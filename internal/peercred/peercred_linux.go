//go:build linux

package peercred

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

func fromUnixConn(uc *net.UnixConn) (Cred, error) {
	raw, err := uc.SyscallConn()
	if err != nil {
		return Cred{}, err
	}
	var (
		ucred *unix.Ucred
		serr  error
	)
	if err := raw.Control(func(fd uintptr) {
		ucred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return Cred{}, err
	}
	if serr != nil {
		return Cred{}, fmt.Errorf("SO_PEERCRED: %w", serr)
	}
	return Cred{UID: ucred.Uid, GIDs: []uint32{ucred.Gid}, PID: ucred.Pid}, nil
}
