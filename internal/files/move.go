package files

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// Errors of Move that the mcp package turns into fixed messages.
var (
	ErrSymlink     = errors.New("path is a symlink")
	ErrCrossDevice = errors.New("not on one filesystem")
	ErrMoveInside  = errors.New("target is inside the source")
)

// Move is a planned rename of one file or folder: where the entry is now and
// where it will be. Two Moves are equal when nothing changed between planning
// and doing.
type Move struct {
	// From and To are resolved paths: the resolved parent plus the name.
	From, To string
	IsDir    bool
	// dev and ino identify the entry at From.
	dev, ino uint64
}

// ResolveMove checks that from names an existing regular file or folder (not
// a symlink) and to names a path that does not exist yet, in an existing
// folder on the same filesystem.
//
// A folder whose move would let a blocked path go unblocked is refused like a
// blocked path: one that holds a blocked tree, or that is named like a folder
// whose files are blocked by their parent's name (.ssh, .docker, .aws, .kube).
func (e *Editor) ResolveMove(ctx context.Context, from, to string) (Move, error) {
	for _, p := range []string{from, to} {
		if err := ValidatePath(p); err != nil {
			return Move{}, err
		}
		if p == "/" {
			return Move{}, ErrInvalidPath
		}
	}
	c, err := e.guard.session(ctx)
	if err != nil {
		return Move{}, err
	}
	return c.resolveMove(from, to)
}

func (c *check) resolveMove(from, to string) (Move, error) {
	if c.blocked(from) {
		return Move{}, ErrBlocked
	}
	parent, pinfo, err := c.resolve(filepath.Dir(from))
	if err != nil {
		return Move{}, err
	}
	if !pinfo.IsDir() {
		return Move{}, ErrNotDir
	}
	src := filepath.Join(parent, filepath.Base(from))
	if c.blocked(src) {
		return Move{}, ErrBlocked
	}
	st, err := os.Lstat(src)
	if err != nil {
		return Move{}, mapErr(err)
	}
	switch {
	case st.Mode()&fs.ModeSymlink != 0:
		return Move{}, ErrSymlink
	case !st.Mode().IsRegular() && !st.IsDir():
		return Move{}, ErrNotFile
	case st.IsDir() && c.holdsBlocked(src):
		return Move{}, ErrBlocked
	case st.Mode().IsRegular() && nlink(st) > 1 && c.hardLinked(st):
		return Move{}, ErrBlocked
	}
	nf, err := c.resolveNew(to)
	if err != nil {
		return Move{}, err
	}
	if under(nf.Path, src) {
		return Move{}, ErrMoveInside
	}
	tp, err := os.Lstat(filepath.Dir(nf.Path))
	if err != nil {
		return Move{}, mapErr(err)
	}
	dev, ino, ok := identity(st)
	pdev, _, pok := identity(pinfo)
	tdev, _, tok := identity(tp)
	if !ok || !pok || !tok {
		return Move{}, ErrNotFound
	}
	// A different device below the source's folder is a mount point, which
	// rename cannot move.
	if dev != pdev || dev != tdev {
		return Move{}, ErrCrossDevice
	}
	return Move{From: src, To: nf.Path, IsDir: st.IsDir(), dev: dev, ino: ino}, nil
}

// holdsBlocked reports whether moving the folder real would unblock
// something: a blocked tree or folder rule inside it, or a name that makes
// the folder's own files blocked.
func (c *check) holdsBlocked(real string) bool {
	for _, t := range c.g.trees {
		if under(t, real) {
			return true
		}
	}
	for _, t := range c.extra {
		if under(t, real) {
			return true
		}
	}
	for _, d := range c.g.dirEntries {
		if under(d.Dir, real) {
			return true
		}
	}
	name := filepath.Base(real)
	if c.g.ssh && name == ".ssh" {
		return true
	}
	for _, d := range c.g.namedDirEntries {
		if name == d.Dir {
			return true
		}
	}
	return false
}

// identity returns the device and inode of fi.
func identity(fi fs.FileInfo) (dev, ino uint64, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true //nolint:unconvert,gosec // Dev differs in type by platform; only compared for equality
}

// Move renames the entry at from to to. It checks the plan again, never
// replaces anything and never copies: the entry keeps its inode, content,
// owner and permissions.
func (e *Editor) Move(ctx context.Context, from, to string, want Move) error {
	for _, p := range []string{from, to} {
		if err := ValidatePath(p); err != nil {
			return err
		}
	}
	c, err := e.guard.session(ctx)
	if err != nil {
		return err
	}
	cur, err := c.resolveMove(from, to)
	if err != nil {
		return err
	}
	if cur != want {
		return ErrChanged
	}
	src, _, err := c.open(filepath.Dir(cur.From), kindDir)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, _, err := c.open(filepath.Dir(cur.To), kindDir)
	if err != nil {
		return err
	}
	defer dst.Close()
	sfd, dfd := int(src.Fd()), int(dst.Fd()) //nolint:gosec // valid descriptors
	sname, dname := filepath.Base(cur.From), filepath.Base(cur.To)
	if e.beforeCommit != nil {
		if err := e.beforeCommit(); err != nil {
			return err
		}
	}
	// The name must still be the entry that was checked.
	var now unix.Stat_t
	if err := unix.Fstatat(sfd, sname, &now, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		uint64(now.Dev) != cur.dev || uint64(now.Ino) != cur.ino { //nolint:unconvert,gosec // Dev differs in type by platform
		return ErrChanged
	}
	if err := renameNoReplace(sfd, sname, dfd, dname); err != nil {
		switch {
		case errors.Is(err, unix.EEXIST), errors.Is(err, unix.ENOTEMPTY):
			return ErrExists
		case errors.Is(err, unix.EXDEV), errors.Is(err, unix.EBUSY):
			return ErrCrossDevice
		}
		return err
	}
	if err := syncFD(dfd); err != nil {
		return err
	}
	if sfd != dfd {
		return syncFD(sfd)
	}
	return nil
}
