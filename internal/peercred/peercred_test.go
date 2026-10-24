package peercred

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestInGroup(t *testing.T) {
	c := Cred{UID: 1000, GIDs: []uint32{1000, 44}}
	tests := []struct {
		name     string
		gid      uint32
		expected bool
	}{
		{"primary group", 1000, true},
		{"secondary group", 44, true},
		{"other group", 45, false},
		{"uid is not a group", 999, false},
	}
	for _, tt := range tests {
		if got := InGroup(c, tt.gid); got != tt.expected {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.expected)
		}
	}
	if InGroup(Cred{UID: 0, GIDs: []uint32{0}}, 0) {
		t.Error("group 0 must match nobody")
	}
	if InGroup(Cred{UID: 1000}, 1000) {
		t.Error("a peer without groups must match nothing")
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
	if len(cred.GIDs) == 0 || cred.GIDs[0] != uint32(os.Getgid()) {
		t.Errorf("gids = %v, want primary group %d first", cred.GIDs, os.Getgid())
	}
	if runtime.GOOS == "linux" {
		groups, err := os.Getgroups()
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range groups {
			if !InGroup(cred, uint32(g)) {
				t.Errorf("supplementary group %d missing from %v", g, cred.GIDs)
			}
		}
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
