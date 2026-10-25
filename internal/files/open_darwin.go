package files

import (
	"errors"

	"golang.org/x/sys/unix"
)

// openNoFollow opens a resolved path for reading without following a symlink
// in any component (O_NOFOLLOW_ANY, macOS 11+).
func openNoFollow(p string, dir bool) (int, error) {
	// O_NOFOLLOW_ANY includes O_NOFOLLOW; the kernel rejects the two together.
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK | unix.O_NOFOLLOW_ANY | unix.O_NOCTTY
	if dir {
		flags |= unix.O_DIRECTORY
	}
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

// fdPath is not implemented on macOS, which is only a development platform:
// O_NOFOLLOW_ANY already refuses every symlink, which is the race fdPath
// guards against on Linux.
func fdPath(int) (string, error) { return "", errNoFDPath }

// checkFS has nothing to refuse on macOS, which has no procfs or sysfs.
func checkFS(int) error { return nil }
