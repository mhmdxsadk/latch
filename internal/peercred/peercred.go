// Package peercred reads kernel-verified credentials of a Unix socket peer.
//
// Filesystem permissions on the socket are the first gate; peer credentials
// are a second, independent one that survives a mis-set mode or a socket that
// was passed to an unexpected process.
package peercred

import (
	"errors"
	"net"
)

// Cred is the identity of the process on the other end of a Unix socket.
type Cred struct {
	UID  uint32
	GIDs []uint32 // primary group first where the platform provides it
	PID  int32
}

// ErrUnsupported is returned on platforms without a supported mechanism.
// Callers must treat it as a denial (fail closed).
var ErrUnsupported = errors.New("peer credentials unsupported on this platform")

// FromConn returns the peer credentials of conn, which must be a *net.UnixConn.
func FromConn(conn net.Conn) (Cred, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return Cred{}, errors.New("not a unix connection")
	}
	return fromUnixConn(uc)
}

// InGroup reports whether c belongs to group gid. Group 0 matches nobody, so
// root is never let in through its own group.
func InGroup(c Cred, gid uint32) bool {
	if gid == 0 {
		return false
	}
	for _, g := range c.GIDs {
		if g == gid {
			return true
		}
	}
	return false
}
