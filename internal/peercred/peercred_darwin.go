//go:build darwin

package peercred

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// macOS support exists for local development only.
func fromUnixConn(uc *net.UnixConn) (Cred, error) {
	raw, err := uc.SyscallConn()
	if err != nil {
		return Cred{}, err
	}
	var (
		xu   *unix.Xucred
		pid  int
		serr error
	)
	if err := raw.Control(func(fd uintptr) {
		xu, serr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if serr == nil {
			pid, _ = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		}
	}); err != nil {
		return Cred{}, err
	}
	if serr != nil {
		return Cred{}, fmt.Errorf("LOCAL_PEERCRED: %w", serr)
	}
	n := int(xu.Ngroups)
	if n > len(xu.Groups) {
		n = len(xu.Groups)
	}
	return Cred{UID: xu.Uid, GIDs: append([]uint32(nil), xu.Groups[:n]...), PID: int32(pid)}, nil //nolint:gosec // pids fit in int32
}
