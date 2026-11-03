package files

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Errors of Delete that the mcp package turns into fixed messages.
var (
	ErrNotEmpty   = errors.New("folder is not empty")
	ErrMountPoint = errors.New("path is a mount point")
)

// Removal is a planned delete of one file or empty folder. Two Removals are
// equal when nothing changed between planning and doing.
type Removal struct {
	// Path is the resolved path: the resolved parent plus the name.
	Path  string
	IsDir bool
	// Hash is the content hash of a file; zero for a folder.
	Hash     [sha256.Size]byte
	dev, ino uint64
}

// ResolveDelete checks that p names an existing empty folder or a regular
// text file that file_edit could change (at most MaxEditBytes, so Delete can
// keep a backup of it). Symlinks, mount points, blocked paths and the root
// are refused.
func (e *Editor) ResolveDelete(ctx context.Context, p string) (Removal, error) {
	if err := ValidatePath(p); err != nil {
		return Removal{}, err
	}
	if p == "/" {
		return Removal{}, ErrInvalidPath
	}
	c, err := e.guard.session(ctx)
	if err != nil {
		return Removal{}, err
	}
	r, _, err := c.resolveDelete(p)
	return r, err
}

// resolveDelete also returns the file's content, which Delete backs up.
func (c *check) resolveDelete(p string) (Removal, []byte, error) {
	if c.blocked(p) {
		return Removal{}, nil, ErrBlocked
	}
	parent, pinfo, err := c.resolve(filepath.Dir(p))
	if err != nil {
		return Removal{}, nil, err
	}
	if !pinfo.IsDir() {
		return Removal{}, nil, ErrNotDir
	}
	real := filepath.Join(parent, filepath.Base(p))
	if c.blocked(real) {
		return Removal{}, nil, ErrBlocked
	}
	st, err := os.Lstat(real)
	if err != nil {
		return Removal{}, nil, mapErr(err)
	}
	r := Removal{Path: real, IsDir: st.IsDir()}
	var content []byte
	switch {
	case st.Mode()&fs.ModeSymlink != 0:
		return Removal{}, nil, ErrSymlink
	case st.IsDir():
		f, _, err := c.open(real, kindDir)
		if err != nil {
			return Removal{}, nil, err
		}
		defer f.Close()
		if names, err := f.Readdirnames(1); len(names) > 0 {
			return Removal{}, nil, ErrNotEmpty
		} else if err != nil && !errors.Is(err, io.EOF) {
			return Removal{}, nil, mapErr(err)
		}
	case st.Mode().IsRegular():
		f, fst, err := c.open(real, kindFile)
		if err != nil {
			return Removal{}, nil, err
		}
		defer f.Close()
		file, err := readEditable(f, fst, real)
		if err != nil {
			return Removal{}, nil, err
		}
		r.Hash, content = file.Hash, file.Content
	default:
		return Removal{}, nil, ErrNotFile
	}
	dev, ino, ok := identity(st)
	pdev, _, pok := identity(pinfo)
	if !ok || !pok {
		return Removal{}, nil, ErrNotFound
	}
	if dev != pdev {
		return Removal{}, nil, ErrMountPoint
	}
	r.dev, r.ino = dev, ino
	return r, content, nil
}

// Delete removes the file or empty folder at p after checking the plan
// again. A file is backed up first; the backup id is returned. Nothing is
// removed recursively.
func (e *Editor) Delete(ctx context.Context, p string, want Removal) (backupID string, err error) {
	if err := ValidatePath(p); err != nil {
		return "", err
	}
	c, err := e.guard.session(ctx)
	if err != nil {
		return "", err
	}
	cur, content, err := c.resolveDelete(p)
	if err != nil {
		return "", err
	}
	if cur != want {
		return "", ErrChanged
	}
	dir, _, err := c.open(filepath.Dir(cur.Path), kindDir)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	dfd := int(dir.Fd()) //nolint:gosec // a valid descriptor
	name := filepath.Base(cur.Path)
	if !cur.IsDir {
		if backupID, err = e.backups.Save(cur.Path, content); err != nil {
			return "", err
		}
	}
	if e.beforeCommit != nil {
		if err := e.beforeCommit(); err != nil {
			return backupID, err
		}
	}
	// The name must still be the entry that was checked.
	var now unix.Stat_t
	if err := unix.Fstatat(dfd, name, &now, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		uint64(now.Dev) != cur.dev || uint64(now.Ino) != cur.ino { //nolint:unconvert,gosec // Dev differs in type by platform
		return backupID, ErrChanged
	}
	flags := 0
	if cur.IsDir {
		flags = unix.AT_REMOVEDIR
	}
	if err := unix.Unlinkat(dfd, name, flags); err != nil {
		switch {
		case errors.Is(err, unix.ENOTEMPTY), errors.Is(err, unix.EEXIST):
			return backupID, ErrNotEmpty
		case errors.Is(err, unix.EBUSY):
			return backupID, ErrMountPoint
		}
		return backupID, err
	}
	return backupID, syncFD(dfd)
}
