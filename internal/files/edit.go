package files

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Editor changes files for approved actions. Every operation resolves the
// agent's path through the Guard again, opens files and directories without
// following symlinks, and writes through a temporary file in the same
// directory that is renamed (or linked) into place, so a reader never sees a
// half-written file and a hard link to another file is never written
// through.
type Editor struct {
	guard   *Guard
	backups *Backups
	// beforeCommit runs right before the temporary file is put in place.
	// Tests use it to inject failures.
	beforeCommit func() error
}

// NewEditor returns an Editor that checks paths with g and keeps backups in
// b.
func NewEditor(g *Guard, b *Backups) *Editor { return &Editor{guard: g, backups: b} }

// Backups returns the backup store.
func (e *Editor) Backups() *Backups { return e.backups }

// File is a text file read for an edit.
type File struct {
	// Path is the resolved path (no symlinks).
	Path string
	// Content is the real content, secrets included: never show or log it.
	Content []byte
	Hash    [sha256.Size]byte
	UID     int
	GID     int
	Mode    fs.FileMode // permission bits
}

// Load reads the regular text file at p (at most MaxEditBytes, no NUL).
// Files with setuid, setgid or sticky bits are refused: Latch's service
// cannot set those bits again.
func (e *Editor) Load(ctx context.Context, p string) (File, error) {
	if err := ValidatePath(p); err != nil {
		return File{}, err
	}
	c, err := e.guard.session(ctx)
	if err != nil {
		return File{}, err
	}
	real, _, err := c.resolve(p)
	if err != nil {
		return File{}, err
	}
	f, st, err := c.open(real, kindFile)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	return readEditable(f, st, real)
}

func readEditable(f *os.File, st fs.FileInfo, real string) (File, error) {
	if st.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
		return File{}, ErrSpecialBits
	}
	if st.Size() > MaxEditBytes {
		return File{}, ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxEditBytes+1))
	if err != nil {
		return File{}, mapErr(err)
	}
	if len(data) > MaxEditBytes {
		return File{}, ErrTooLarge
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return File{}, ErrBinary
	}
	uid, gid := owner(st)
	return File{Path: real, Content: data, Hash: sha256.Sum256(data), UID: uid, GID: gid, Mode: st.Mode().Perm()}, nil
}

func owner(fi fs.FileInfo) (int, int) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid)
	}
	return -1, -1
}

// ReplaceResult says what Replace did.
type ReplaceResult struct {
	// BackupID is the backup made of the old content, or "".
	BackupID string
	// Old is the content that was replaced (equal to the File passed in).
	Old []byte
}

// Replace writes content over the file at p, which must still resolve to
// want.Path and hold exactly want's content. With backup, the old content
// is saved first. Owner, group and permission bits are those of the file
// when it is replaced.
func (e *Editor) Replace(ctx context.Context, p string, want File, content []byte, backup bool) (ReplaceResult, error) {
	if len(content) > MaxEditBytes {
		return ReplaceResult{}, ErrTooLarge
	}
	c, err := e.guard.session(ctx)
	if err != nil {
		return ReplaceResult{}, err
	}
	real, _, err := c.resolve(p)
	if err != nil {
		return ReplaceResult{}, err
	}
	if real != want.Path {
		return ReplaceResult{}, ErrChanged
	}
	f, st, err := c.open(real, kindFile)
	if err != nil {
		return ReplaceResult{}, err
	}
	defer f.Close()
	cur, err := readEditable(f, st, real)
	if err != nil {
		return ReplaceResult{}, err
	}
	if cur.Hash != want.Hash {
		return ReplaceResult{}, ErrChanged
	}
	dir, _, err := c.open(filepath.Dir(real), kindDir)
	if err != nil {
		return ReplaceResult{}, err
	}
	defer dir.Close()
	dfd := int(dir.Fd()) //nolint:gosec // a valid descriptor
	name := filepath.Base(real)
	if err := sameEntry(dfd, name, st); err != nil {
		return ReplaceResult{}, err
	}
	res := ReplaceResult{Old: cur.Content}
	if backup {
		if res.BackupID, err = e.backups.Save(real, cur.Content); err != nil {
			return ReplaceResult{}, err
		}
	}
	tmp, err := writeTemp(dfd, name, content, cur.UID, cur.GID, cur.Mode)
	if err != nil {
		return res, err
	}
	commit := func() error {
		if e.beforeCommit != nil {
			if err := e.beforeCommit(); err != nil {
				return err
			}
		}
		// The name must still be the file that was checked.
		if err := sameEntry(dfd, name, st); err != nil {
			return err
		}
		return unix.Renameat(dfd, tmp, dfd, name)
	}
	if err := commit(); err != nil {
		_ = unix.Unlinkat(dfd, tmp, 0)
		return res, err
	}
	return res, syncFD(dfd)
}

// sameEntry checks that name in the directory at dfd is the file st.
func sameEntry(dfd int, name string, st fs.FileInfo) error {
	var cur unix.Stat_t
	if err := unix.Fstatat(dfd, name, &cur, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return ErrChanged
	}
	want, ok := st.Sys().(*syscall.Stat_t)
	if !ok || uint64(want.Dev) != uint64(cur.Dev) || uint64(want.Ino) != uint64(cur.Ino) { //nolint:unconvert // Dev differs in type by platform
		return ErrChanged
	}
	return nil
}

// writeTemp writes content to a new temporary file next to name and gives
// it owner uid:gid and permission bits perm. It returns the temporary name.
// The file is created 0600 and only gets its final owner and mode once its
// content is on disk.
func writeTemp(dfd int, name string, content []byte, uid, gid int, perm fs.FileMode) (string, error) {
	var (
		fd  int
		tmp string
		err error
	)
	for range 10 {
		var r [6]byte
		_, _ = rand.Read(r[:])
		tmp = "." + name + ".latch-" + hex.EncodeToString(r[:])
		if len(tmp) > 255 {
			tmp = ".latch-" + hex.EncodeToString(r[:])
		}
		fd, err = unix.Openat(dfd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if !errors.Is(err, unix.EEXIST) && !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), tmp) //nolint:gosec // fd is a valid descriptor from openat
	werr := func() error {
		if _, err := f.Write(content); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		if uid >= 0 && gid >= 0 {
			if err := unix.Fchown(fd, uid, gid); err != nil {
				return err
			}
		}
		// After chown, which may clear mode bits.
		return unix.Fchmod(fd, uint32(perm.Perm()))
	}()
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = unix.Unlinkat(dfd, tmp, 0)
		return "", werr
	}
	return tmp, nil
}

// NewFile is where Create will put a new file.
type NewFile struct {
	// Path is the resolved path: the resolved parent plus the name.
	Path string
	UID  int // the parent directory's owner
	GID  int // the parent directory's group
}

// ResolveNew checks that p names a file that does not exist yet, in an
// existing directory, and that the resolved path is not blocked.
func (e *Editor) ResolveNew(ctx context.Context, p string) (NewFile, error) {
	if err := ValidatePath(p); err != nil {
		return NewFile{}, err
	}
	if p == "/" {
		return NewFile{}, ErrInvalidPath
	}
	c, err := e.guard.session(ctx)
	if err != nil {
		return NewFile{}, err
	}
	return c.resolveNew(p)
}

func (c *check) resolveNew(p string) (NewFile, error) {
	if c.blocked(p) {
		return NewFile{}, ErrBlocked
	}
	parent, info, err := c.resolve(filepath.Dir(p))
	if err != nil {
		return NewFile{}, err
	}
	if !info.IsDir() {
		return NewFile{}, ErrNotDir
	}
	target := filepath.Join(parent, filepath.Base(p))
	if c.blocked(target) {
		return NewFile{}, ErrBlocked
	}
	if _, err := os.Lstat(target); err == nil {
		return NewFile{}, ErrExists
	} else if !errors.Is(err, fs.ErrNotExist) {
		return NewFile{}, mapErr(err)
	}
	uid, gid := owner(info)
	return NewFile{Path: target, UID: uid, GID: gid}, nil
}

// Locate returns the resolved path of p, which may no longer exist (to list
// its backups). The path must not be blocked.
func (e *Editor) Locate(ctx context.Context, p string) (string, error) {
	real, err := e.guard.Check(ctx, p)
	if !errors.Is(err, ErrNotFound) {
		return real, err
	}
	nf, err := e.ResolveNew(ctx, p)
	if err != nil {
		return "", err
	}
	return nf.Path, nil
}

// Create writes a new file at p with content and permission bits perm,
// owned like its parent directory. It fails with ErrExists if anything
// exists at p when it is created, and never replaces it.
func (e *Editor) Create(ctx context.Context, p string, want NewFile, content []byte, perm fs.FileMode) error {
	if len(content) > MaxEditBytes {
		return ErrTooLarge
	}
	if perm&^fs.ModePerm != 0 {
		return ErrSpecialBits
	}
	if err := ValidatePath(p); err != nil {
		return err
	}
	c, err := e.guard.session(ctx)
	if err != nil {
		return err
	}
	nf, err := c.resolveNew(p)
	if err != nil {
		return err
	}
	if nf != want {
		return ErrChanged
	}
	dir, _, err := c.open(filepath.Dir(nf.Path), kindDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	dfd := int(dir.Fd()) //nolint:gosec // a valid descriptor
	name := filepath.Base(nf.Path)
	tmp, err := writeTemp(dfd, name, content, nf.UID, nf.GID, perm)
	if err != nil {
		return err
	}
	link := func() error {
		if e.beforeCommit != nil {
			if err := e.beforeCommit(); err != nil {
				return err
			}
		}
		// link fails if the name exists, so nothing is ever replaced.
		err := unix.Linkat(dfd, tmp, dfd, name, 0)
		if errors.Is(err, unix.EEXIST) {
			return ErrExists
		}
		return err
	}
	err = link()
	_ = unix.Unlinkat(dfd, tmp, 0)
	if err != nil {
		return err
	}
	return syncFD(dfd)
}

// unitDirs hold systemd unit files; a change below one needs a
// daemon-reload.
var unitDirs = []string{"/etc/systemd/system", "/run/systemd/system", "/usr/lib/systemd/system", "/lib/systemd/system"}

// IsUnitPath reports whether the resolved path real is a systemd unit file
// or drop-in (anything below a unit directory).
func IsUnitPath(real string) bool {
	for _, d := range unitDirs {
		if strings.HasPrefix(real, d+"/") {
			return true
		}
	}
	return false
}
