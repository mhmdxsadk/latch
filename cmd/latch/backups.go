package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/mhmdxsadk/latch/internal/files"
)

// cmdBackups runs "latch backups PATH": it lists the backups Latch made of
// a file, newest first. It reads the backup folder directly, so it needs
// root, like the folder.
func cmdBackups(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backups", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("backup-dir", files.DefaultBackupRoot, "folder where Latch keeps backups")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "latch: usage: latch backups PATH")
		return 2
	}
	p := fs.Arg(0)
	if !filepath.IsAbs(p) {
		fmt.Fprintf(stderr, "latch: %s: path must be absolute\n", p)
		return 2
	}
	p = filepath.Clean(p)
	// Backups are kept under the file's real path.
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	list, err := files.NewBackups(*root).List(p)
	switch {
	case errors.Is(err, files.ErrBackupUnsafe):
		fmt.Fprintf(stderr, "latch: %s: not safe to use (a symlink, or not owned by root with mode 0700); run latch doctor\n", *root)
		return 1
	case errors.Is(err, os.ErrPermission):
		fmt.Fprintf(stderr, "latch: %s: permission denied (run as root)\n", *root)
		return 1
	case err != nil:
		fmt.Fprintf(stderr, "latch: %s: %v\n", p, err)
		return 1
	case len(list) == 0:
		fmt.Fprintf(stdout, "latch: %s: no backups\n", p)
		return 0
	}
	fmt.Fprintf(stdout, "latch: %s: %d backup(s), newest first\n", p, len(list))
	for _, b := range list {
		fmt.Fprintf(stdout, "%s  %d bytes\n", b.ID, b.Size)
	}
	return 0
}
