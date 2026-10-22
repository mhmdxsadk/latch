package peercred

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestAllowed(t *testing.T) {
	c := Cred{UID: 1000, GIDs: []uint32{1000, 44}}
	tests := []struct {
		name     string
		uids     []uint32
		gids     []uint32
		expected bool
	}{
		{"empty lists match nobody", nil, nil, false},
		{"uid match", []uint32{1000}, nil, true},
		{"uid mismatch", []uint32{1001}, nil, false},
		{"gid match on secondary group", nil, []uint32{44}, true},
		{"gid mismatch", nil, []uint32{45}, false},
		{"root is not implicitly allowed", []uint32{1}, []uint32{2}, false},
	}
	for _, tt := range tests {
		if got := Allowed(c, tt.uids, tt.gids); got != tt.expected {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.expected)
		}
	}
	if Allowed(Cred{UID: 0}, nil, nil) {
		t.Error("uid 0 must not be implicitly allowed")
	}
}

func TestFromConnReportsOwnIdentity(t *testing.T) {
	dir, err := os.MkdirTemp("", "latch")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := net.Dial("unix", path)
		if err == nil {
			defer c.Close()
			buf := make([]byte, 1)
			c.Read(buf)
		}
	}()
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cred, err := FromConn(conn)
	if err != nil {
		t.Fatalf("FromConn: %v", err)
	}
	if cred.UID != uint32(os.Getuid()) {
		t.Errorf("uid = %d, want %d", cred.UID, os.Getuid())
	}
}

func TestFromConnRejectsNonUnix(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if _, err := FromConn(a); err == nil {
		t.Fatal("expected error for non-unix conn")
	}
}
