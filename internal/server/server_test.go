package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mhmdxsadk/latch/internal/audit"
	"github.com/mhmdxsadk/latch/internal/config"
	"github.com/mhmdxsadk/latch/internal/host"
	lmcp "github.com/mhmdxsadk/latch/internal/mcp"
	"github.com/mhmdxsadk/latch/internal/policy"
)

// shortTempDir avoids macOS's ~104-byte sun_path limit.
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "lt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestListenCreatesSocketWithMode0660(t *testing.T) {
	p := filepath.Join(shortTempDir(t), "s.sock")
	ln, err := Listen(Options{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Error("not a socket")
	}
	if got := fi.Mode().Perm(); got != 0o660 {
		t.Errorf("mode = %o, want 660", got)
	}
}

func TestListenRefusesToReplaceNonSocket(t *testing.T) {
	p := filepath.Join(shortTempDir(t), "s.sock")
	if err := os.WriteFile(p, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(Options{Path: p}); err == nil {
		t.Fatal("must not clobber a regular file")
	}
	if b, _ := os.ReadFile(p); string(b) != "precious" {
		t.Error("file was modified")
	}
}

func TestListenRefusesLiveSocketAndReplacesStale(t *testing.T) {
	p := filepath.Join(shortTempDir(t), "s.sock")
	ln, err := Listen(Options{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(Options{Path: p}); err == nil {
		t.Error("must not take over a live socket")
	}
	// Leave a stale socket file behind: close without unlinking.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	ln2, err := Listen(Options{Path: p})
	if err != nil {
		t.Fatalf("stale socket should be replaced: %v", err)
	}
	ln2.Close()
}

func TestListenUnknownGroupFailsClosed(t *testing.T) {
	p := filepath.Join(shortTempDir(t), "s.sock")
	if _, err := Listen(Options{Path: p, Group: "no-such-group-latch"}); err == nil {
		t.Fatal("expected error")
	}
	if _, err := os.Lstat(p); err == nil {
		t.Error("socket must not be left behind")
	}
}

type env struct {
	path  string
	audit *lockedBuffer
}

func start(t *testing.T, gid uint32) env {
	t.Helper()
	cfg, err := config.Parse([]byte("server: {socket: /run/l.sock, socket_group: latch-agents}\n"))
	if err != nil {
		t.Fatal(err)
	}
	buf := &lockedBuffer{}
	al := audit.New(slog.New(slog.NewJSONHandler(buf, nil)))
	srv := lmcp.NewServer(&lmcp.Deps{Policy: policy.New(cfg), Host: host.System{}, Audit: al, Version: "test"})

	p := filepath.Join(shortTempDir(t), "s.sock")
	ln, err := Listen(Options{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeUnix(ctx, ln, Handler(srv), gid, al) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("server did not shut down")
		}
	})
	return env{path: p, audit: buf}
}

func unixClient(path string) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", path)
			},
		},
	}
}

func TestMCPClientOverUnixSocketInGroup(t *testing.T) {
	e := start(t, uint32(os.Getgid()))
	client := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:   "http://latch" + Endpoint,
		HTTPClient: unixClient(e.path),
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "actions_list", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
}

func TestPeerWithDisallowedCredentialsIsRejected(t *testing.T) {
	e := start(t, 4_000_000_000) // a group this test process is not in
	resp, err := unixClient(e.path).Post("http://latch"+Endpoint, "application/json", strings.NewReader("{}"))
	if err == nil {
		resp.Body.Close()
		t.Fatalf("request should have been dropped, got %s", resp.Status)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(e.audit.String(), "peer_not_allowed") {
		if time.Now().After(deadline) {
			t.Fatalf("rejection not audited: %s", e.audit)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestGroupZeroRejectsEveryone(t *testing.T) {
	e := start(t, 0)
	resp, err := unixClient(e.path).Get("http://latch" + Endpoint)
	if err == nil {
		resp.Body.Close()
		t.Fatal("fail closed: group 0 must reject all peers")
	}
}

func TestGroupIDRefusesRootGroup(t *testing.T) {
	g, err := user.LookupGroupId("0")
	if err != nil {
		t.Skip("no gid 0 on this system")
	}
	if _, err := GroupID(g.Name); err == nil {
		t.Fatalf("socket group %q (gid 0) must be refused", g.Name)
	}
}

func TestMCPClientAtSocketRoot(t *testing.T) {
	e := start(t, uint32(os.Getgid()))
	client := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:   "http://latch/",
		HTTPClient: unixClient(e.path),
	}, nil)
	if err != nil {
		t.Fatalf("connect at /: %v", err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "actions_list", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
}

func TestOnlyMCPEndpointIsServed(t *testing.T) {
	e := start(t, uint32(os.Getgid()))
	c := unixClient(e.path)
	for _, path := range []string{"/etc/passwd", "/mcp/../x", "/mcp/", "/metrics", "/debug/pprof/", "/index.html"} {
		resp, err := c.Get("http://latch" + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMovedPermanently {
			t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	e := start(t, uint32(os.Getgid()))
	big := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"x","params":{"p":"` + strings.Repeat("a", 2*maxBodyBytes) + `"}}`)
	req, _ := http.NewRequest("POST", "http://latch"+Endpoint, big)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := unixClient(e.path).Do(req)
	if err != nil {
		return // connection dropped is also an acceptable rejection
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("oversized request was accepted")
	}
}
