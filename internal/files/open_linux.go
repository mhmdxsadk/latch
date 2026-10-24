package files

import (
	"errors"
	"strconv"

	"golang.org/x/sys/unix"
)

const openFlags = unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK | unix.O_NOFOLLOW | unix.O_NOCTTY

// openNoFollow opens a resolved path for reading without following a symlink
// in any component (openat2 with RESOLVE_NO_SYMLINKS, Linux 5.6+). Where
// openat2 is unavailable (old kernel, or a seccomp filter that refuses it) it
// falls back to O_NOFOLLOW, which covers only the last component; fdPath then
// catches a directory swapped for a symlink.
func openNoFollow(p string, dir bool) (int, error) {
	flags := openFlags
	if dir {
		flags |= unix.O_DIRECTORY
	}
	how := &unix.OpenHow{Flags: uint64(flags), Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS} //nolint:gosec // flags are non-negative constants
	for {
		fd, err := unix.Openat2(unix.AT_FDCWD, p, how)
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EPERM):
			return openLegacy(p, flags)
		}
		return fd, err
	}
}

func openLegacy(p string, flags int) (int, error) {
	for {
		fd, err := unix.Open(p, flags, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return fd, err
	}
}

// errNoFDPath means the platform cannot report an open file's path.
var errNoFDPath = errors.New("file path of descriptor unavailable")

// fdPath returns the path the kernel reports for an open descriptor.
func fdPath(fd int) (string, error) {
	buf := make([]byte, 4097)
	n, err := unix.Readlink("/proc/self/fd/"+strconv.Itoa(fd), buf)
	if err != nil {
		return "", err
	}
	if n >= len(buf) {
		return "", errors.New("descriptor path too long")
	}
	return string(buf[:n]), nil
}

// checkFS refuses kernel pseudo-filesystems wherever they are mounted, so a
// second procfs or sysfs mount cannot be read under another path.
func checkFS(fd int) error {
	var st unix.Statfs_t
	if err := unix.Fstatfs(fd, &st); err != nil {
		return err
	}
	switch st.Type {
	case unix.PROC_SUPER_MAGIC, unix.SYSFS_MAGIC, unix.DEBUGFS_MAGIC, unix.SECURITYFS_MAGIC,
		unix.TRACEFS_MAGIC, unix.CGROUP_SUPER_MAGIC, unix.CGROUP2_SUPER_MAGIC, unix.BPF_FS_MAGIC,
		unix.EFIVARFS_MAGIC, unix.PSTOREFS_MAGIC, unix.BINFMTFS_MAGIC:
		return errors.New("pseudo filesystem")
	}
	return nil
}
