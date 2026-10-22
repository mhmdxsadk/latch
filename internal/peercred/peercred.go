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

// Allowed reports whether c matches one of the allowed UIDs or GIDs.
// Empty allow lists match nobody.
func Allowed(c Cred, uids, gids []uint32) bool {
	for _, u := range uids {
		if c.UID == u {
			return true
		}
	}
	for _, g := range gids {
		for _, cg := range c.GIDs {
			if cg == g {
				return true
			}
		}
	}
	return false
}
