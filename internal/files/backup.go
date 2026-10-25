package files

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// DefaultBackupRoot is where Latch keeps copies of the files it changes.
// /var/lib/latch is on the built-in block list, so the agent never reads
// them.
const DefaultBackupRoot = "/var/lib/latch/backups"

// BackupKeep is how many backups are kept per file.
const BackupKeep = 3

// Backup errors.
var (
	// ErrBackupUnsafe means a directory of the backup tree is a symlink, or
	// has the wrong owner or permissions, so Latch refuses to use it.
	ErrBackupUnsafe = errors.New("backup directory is not safe")
	// ErrNoBackup means there is no backup with that id.
	ErrNoBackup = errors.New("no such backup")
)

// stampLayout is the UTC timestamp in backup names, e.g. 20261025T143000Z.
const stampLayout = "20060102T150405Z"

// backupID matches a backup id: a timestamp and, after a collision, a
// counter (20261025T143000Z-2).
var backupID = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z(?:-([0-9]{1,3}))?$`)

// ValidBackupID reports whether id has the shape of a backup id.
func ValidBackupID(id string) bool { return backupID.MatchString(id) }

// Backup describes one stored copy.
type Backup struct {
	ID   string    `json:"id"`
	Time time.Time `json:"-"`
	Size int64     `json:"size"`
	n    int       // collision counter, 1 for none
}

// Backups stores copies of files under a root directory, at
// <root>/<original full path>.<timestamp>. Directories are 0700 and files
// 0600, owned by the uid Latch runs as. Every directory is opened without
// following symlinks and checked before use.
type Backups struct {
	root string
	uid  int
	gid  int
	keep int
	now  func() time.Time
}

// NewBackups returns Backups under root, a clean absolute path without
// symlinks.
func NewBackups(root string) *Backups {
	return &Backups{root: root, uid: os.Geteuid(), gid: os.Getegid(), keep: BackupKeep, now: time.Now}
}

// Root is the backup directory.
func (b *Backups) Root() string { return b.root }

// components splits a clean absolute path into its names.
func components(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// openDirAt opens name (inside the directory at fd) without following a
// symlink. With create, a missing directory is made 0700.
func openDirAt(fd int, name string, create bool, uid, gid int) (int, bool, error) {
	const flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	made := false
	for {
		nfd, err := unix.Openat(fd, name, flags, 0)
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.ENOENT) && create && !made:
			if err := unix.Mkdirat(fd, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
				return -1, false, err
			}
			made = true
			continue
		case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
			return -1, false, ErrBackupUnsafe
		case err != nil:
			return -1, false, err
		}
		if made {
			// Set owner and mode explicitly; the umask and the parent's
			// setgid bit must not decide them.
			if err := unix.Fchown(nfd, uid, gid); err != nil {
				_ = unix.Close(nfd)
				return -1, false, err
			}
			if err := unix.Fchmod(nfd, 0o700); err != nil {
				_ = unix.Close(nfd)
				return -1, false, err
			}
		}
		return nfd, made, nil
	}
}

// openTree opens the directory <root>/<rel...>. The root's ancestors must be
// real directories owned by root or by Latch's uid that others cannot write
// to (unless sticky, like /tmp); the root and everything below it must be
// owned by Latch's uid with no group or other permissions. With create,
// missing directories (including the root's parents, such as
// /var/lib/latch) are made 0700; without, a missing one returns
// fs.ErrNotExist.
func (b *Backups) openTree(rel []string, create bool) (int, error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	rootParts := components(b.root)
	parts := append(slices.Clone(rootParts), rel...)
	for i, name := range parts {
		inside := i >= len(rootParts)-1
		nfd, _, err := openDirAt(fd, name, create, b.uid, b.gid)
		_ = unix.Close(fd)
		if errors.Is(err, unix.ENOENT) {
			return -1, os.ErrNotExist
		}
		if err != nil {
			return -1, err
		}
		fd = nfd
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			_ = unix.Close(fd)
			return -1, err
		}
		if !dirSafe(st, inside, b.uid) {
			_ = unix.Close(fd)
			return -1, ErrBackupUnsafe
		}
	}
	return fd, nil
}

func dirSafe(st unix.Stat_t, inside bool, uid int) bool {
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return false
	}
	owner := int(st.Uid)
	perm := st.Mode & 0o7777
	if inside {
		return owner == uid && perm&0o077 == 0
	}
	if owner != 0 && owner != uid {
		return false
	}
	return perm&0o022 == 0 || perm&unix.S_ISVTX != 0
}

// CheckBackupRoot reports whether the backup directory is usable: absent
// (it is created on the first change), or a real directory owned by uid with
// no group or other permissions, inside real directories.
func CheckBackupRoot(root string, uid int) (exists bool, err error) {
	b := &Backups{root: root, uid: uid}
	fd, err := b.openTree(nil, false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	_ = unix.Close(fd)
	return true, nil
}

// split returns the directory components and the file name of the clean
// absolute path p.
func split(p string) ([]string, string, error) {
	parts := components(p)
	if len(parts) == 0 {
		return nil, "", ErrInvalidPath
	}
	return parts[:len(parts)-1], parts[len(parts)-1], nil
}

// Save stores content as a new backup of the file at orig (a resolved path)
// and drops the oldest backups beyond the newest three. It returns the new
// backup's id.
func (b *Backups) Save(orig string, content []byte) (string, error) {
	dirs, name, err := split(orig)
	if err != nil {
		return "", err
	}
	dfd, err := b.openTree(dirs, true)
	if err != nil {
		return "", err
	}
	defer func() { _ = unix.Close(dfd) }()

	stamp := b.now().UTC().Format(stampLayout)
	var (
		fd int
		id string
	)
	for n := 1; ; n++ {
		if n > 999 {
			return "", errors.New("too many backups with the same timestamp")
		}
		id = stamp
		if n > 1 {
			id += "-" + strconv.Itoa(n)
		}
		fd, err = unix.Openat(dfd, name+"."+id, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if errors.Is(err, unix.EEXIST) || errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return "", err
		}
		break
	}
	f := os.NewFile(uintptr(fd), name) //nolint:gosec // fd is a valid descriptor from openat
	werr := func() error {
		if err := unix.Fchmod(fd, 0o600); err != nil {
			return err
		}
		if _, err := f.Write(content); err != nil {
			return err
		}
		return f.Sync()
	}()
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = unix.Unlinkat(dfd, name+"."+id, 0)
		return "", werr
	}
	if err := syncFD(dfd); err != nil {
		return "", err
	}
	// Prune only after the new backup is safely written.
	list, err := listAt(dfd, name)
	if err != nil {
		return id, nil //nolint:nilerr // the backup exists; pruning is retried next time
	}
	for len(list) > b.keep {
		_ = unix.Unlinkat(dfd, name+"."+list[len(list)-1].ID, 0)
		list = list[:len(list)-1]
	}
	return id, nil
}

// List returns the backups of the file at orig, newest first.
func (b *Backups) List(orig string) ([]Backup, error) {
	dirs, name, err := split(orig)
	if err != nil {
		return nil, err
	}
	dfd, err := b.openTree(dirs, false)
	if errors.Is(err, os.ErrNotExist) {
		return []Backup{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(dfd) }()
	return listAt(dfd, name)
}

// listAt lists the backups of name in the directory at dfd, newest first.
func listAt(dfd int, name string) ([]Backup, error) {
	// Duplicate the descriptor: os.File closes what it wraps.
	dup, err := unix.Dup(dfd)
	if err != nil {
		return nil, err
	}
	d := os.NewFile(uintptr(dup), "backups") //nolint:gosec // dup is a valid descriptor
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	out := []Backup{}
	for _, n := range names {
		id, ok := strings.CutPrefix(n, name+".")
		if !ok {
			continue
		}
		m := backupID.FindStringSubmatch(id)
		if m == nil {
			continue
		}
		t, err := time.Parse(stampLayout, id[:16])
		if err != nil {
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstatat(dfd, n, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
			continue
		}
		c := 1
		if m[1] != "" {
			c, _ = strconv.Atoi(m[1])
		}
		out = append(out, Backup{ID: id, Time: t, Size: st.Size, n: c})
	}
	slices.SortFunc(out, func(x, y Backup) int {
		if c := y.Time.Compare(x.Time); c != 0 {
			return c
		}
		return y.n - x.n
	})
	return out, nil
}

// Read returns the content of backup id of the file at orig. Backups larger
// than MaxEditBytes are refused.
func (b *Backups) Read(orig, id string) ([]byte, error) {
	if !ValidBackupID(id) {
		return nil, ErrNoBackup
	}
	dirs, name, err := split(orig)
	if err != nil {
		return nil, err
	}
	dfd, err := b.openTree(dirs, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoBackup
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(dfd) }()
	fd, err := unix.Openat(dfd, name+"."+id, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil, ErrNoBackup
	case errors.Is(err, unix.ELOOP):
		return nil, ErrBackupUnsafe
	case err != nil:
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name) //nolint:gosec // fd is a valid descriptor from openat
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || int(st.Uid) != b.uid {
		return nil, ErrBackupUnsafe
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxEditBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxEditBytes {
		return nil, ErrTooLarge
	}
	return data, nil
}

// syncFD flushes a directory. Filesystems that cannot sync a directory are
// not an error.
func syncFD(fd int) error {
	err := unix.Fsync(fd)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EBADF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}
