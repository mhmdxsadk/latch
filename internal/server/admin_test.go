package server

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mhmdxsadk/latch/internal/admin"
	"github.com/mhmdxsadk/latch/internal/approval"
	"github.com/mhmdxsadk/latch/internal/audit"
	"github.com/mhmdxsadk/latch/internal/peercred"
)

func TestAdminPeerAllowedOnlyRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		cred peercred.Cred
		want bool
	}{
		{"root", peercred.Cred{UID: 0, GIDs: []uint32{0}}, true},
		{"root with other groups", peercred.Cred{UID: 0, GIDs: []uint32{1500}}, true},
		{"agent in socket group", peercred.Cred{UID: 1000, GIDs: []uint32{1000, 1500}}, false},
		{"agent in root group", peercred.Cred{UID: 1000, GIDs: []uint32{0}}, false},
		{"docker group member", peercred.Cred{UID: 999, GIDs: []uint32{999}}, false},
		{"no groups", peercred.Cred{UID: 65534}, false},
	} {
		if got := AdminPeerAllowed(tc.cred); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAgentPeerAllowedNeverMeansAdmin(t *testing.T) {
	allow := AgentPeerAllowed(1500)
	if !allow(peercred.Cred{UID: 1000, GIDs: []uint32{1500}}) {
		t.Error("socket group member refused")
	}
	if allow(peercred.Cred{UID: 0, GIDs: []uint32{0}}) {
		t.Error("root is not implicitly an agent")
	}
}

func TestAdminListenMode0600(t *testing.T) {
	p := filepath.Join(shortTempDir(t), "a.sock")
	ln, err := Listen(Options{Path: p, Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}
	if _, err := Listen(Options{Path: filepath.Join(shortTempDir(t), "b.sock"), Admin: true, Group: "latch-agents"}); err == nil {
		t.Error("an admin socket with a group must be refused")
	}
}

func startAdmin(t *testing.T, allow func(peercred.Cred) bool) (string, *lockedBuffer, *approval.Store) {
	t.Helper()
	buf := &lockedBuffer{}
	al := audit.New(slog.New(slog.NewJSONHandler(buf, nil)))
	store := approval.New(approval.Options{Audit: al})
	p := filepath.Join(shortTempDir(t), "a.sock")
	ln, err := Listen(Options{Path: p, Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveAdmin(ctx, ln, admin.Handler(store), allow, al) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return p, buf, store
}

// The real admin check against this (non-root) test process: every
// connection is dropped before any request is read.
func TestAdminSocketRefusesNonRootPeer(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	p, buf, _ := startAdmin(t, AdminPeerAllowed)
	resp, err := unixClient(p).Get("http://latch/v1/requests?status=pending")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("non-root peer was served: %s", resp.Status)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(buf.String(), "peer_not_allowed") {
		if time.Now().After(deadline) {
			t.Fatalf("rejection not audited: %s", buf)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// With a check that lets this process in, the admin API answers and records
// the verified uid as approver.
func TestAdminSocketServesVerifiedPeer(t *testing.T) {
	p, _, _ := startAdmin(t, func(peercred.Cred) bool { return true })
	reqs, err := admin.NewClient(p).Pending(context.Background())
	if err != nil || len(reqs) != 0 {
		t.Fatalf("%v %+v", err, reqs)
	}
}

// The agent socket serves MCP only: the admin routes do not exist there.
func TestAgentSocketHasNoAdminRoutes(t *testing.T) {
	e := start(t, uint32(os.Getgid()))
	c := unixClient(e.path)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/requests"},
		{"GET", "/v1/requests?status=pending"},
		{"POST", "/v1/requests/1/approve"},
		{"POST", "/v1/requests/1/deny"},
		{"POST", "/mcp/v1/requests/1/approve"},
	} {
		req, _ := http.NewRequest(tc.method, "http://latch"+tc.path, nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d on the agent socket, want 404", tc.method, tc.path, resp.StatusCode)
		}
	}
}
