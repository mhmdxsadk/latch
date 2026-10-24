//go:build linux

package peercred

import (
	"errors"
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/unix"
)

// maxGroups is the kernel's NGROUPS_MAX.
const maxGroups = 65536

func fromUnixConn(uc *net.UnixConn) (Cred, error) {
	raw, err := uc.SyscallConn()
	if err != nil {
		return Cred{}, err
	}
	var (
		ucred  *unix.Ucred
		groups []uint32
		serr   error
		gerr   error
	)
	if err := raw.Control(func(fd uintptr) {
		ucred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		groups, gerr = peerGroups(int(fd))
	}); err != nil {
		return Cred{}, err
	}
	if serr != nil {
		return Cred{}, fmt.Errorf("SO_PEERCRED: %w", serr)
	}
	// Kernels older than 4.13 lack SO_PEERGROUPS; only the primary group is
	// known there. Any other failure is a rejection.
	if gerr != nil && !errors.Is(gerr, unix.ENOPROTOOPT) {
		return Cred{}, fmt.Errorf("SO_PEERGROUPS: %w", gerr)
	}
	return Cred{UID: ucred.Uid, GIDs: append([]uint32{ucred.Gid}, groups...), PID: ucred.Pid}, nil
}

// peerGroups returns the peer's supplementary groups, captured by the kernel
// at connect time. x/sys/unix has no wrapper for this option.
func peerGroups(fd int) ([]uint32, error) {
	n := 64
	for {
		buf := make([]uint32, n)
		size := uint32(n * 4)
		//nolint:gosec // getsockopt(2) needs raw pointers; buf and size outlive the call
		_, _, e := unix.Syscall6(unix.SYS_GETSOCKOPT, uintptr(fd), unix.SOL_SOCKET, unix.SO_PEERGROUPS,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0)
		switch {
		case e == unix.ERANGE && size > uint32(n*4) && size <= maxGroups*4:
			n = int(size / 4)
		case e != 0:
			return nil, e
		default:
			return buf[:size/4], nil
		}
	}
}
