//go:build !linux && !darwin

package peercred

import "net"

func fromUnixConn(*net.UnixConn) (Cred, error) { return Cred{}, ErrUnsupported }
