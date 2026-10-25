package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhmdxsadk/latch/internal/admin"
	"github.com/mhmdxsadk/latch/internal/approval"
	"github.com/mhmdxsadk/latch/internal/audit"
)

type stopAction struct {
	name string
	runs *atomic.Int32
}

func (a stopAction) Kind() string { return "docker_stop" }
func (a stopAction) Resource() approval.Resource {
	return approval.Resource{Type: "container", Name: a.name}
}
func (a stopAction) Params() []approval.Param    { return nil }
func (a stopAction) Describe() []string          { return []string{"stop container " + a.name} }
func (a stopAction) Mode() approval.Mode         { return approval.Always }
func (a stopAction) Check(context.Context) error { return nil }
func (a stopAction) Execute(context.Context) (approval.Result, error) {
	a.runs.Add(1)
	return approval.Result{Summary: "stopped"}, nil
}

// testAdmin serves the admin API for a fresh store on a Unix socket, with
// every connection treated as root.
func testAdmin(t *testing.T) (string, *approval.Store) {
	t.Helper()
	dir, err := os.MkdirTemp("", "lc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	store := approval.New(approval.Options{})
	srv := &http.Server{
		Handler:           admin.Handler(store),
		ReadHeaderTimeout: time.Second,
		ConnContext: func(ctx context.Context, _ net.Conn) context.Context {
			return audit.WithPeer(ctx, audit.Peer{UID: 0})
		},
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close(); store.Wait() })
	return sock, store
}

func TestPendingApproveDenyCommands(t *testing.T) {
	sock, store := testAdmin(t)
	var out, errb bytes.Buffer
	if code := run([]string{"pending", "--admin-socket", sock}, &out, &errb); code != 0 || out.String() != "latch: no pending requests\n" {
		t.Fatalf("empty: %d %q %q", code, out.String(), errb.String())
	}

	var runs atomic.Int32
	who := approval.Requester{UID: 1000, PID: 4242}
	a, _ := store.Submit(context.Background(), who, stopAction{"jellyfin", &runs})
	b, _ := store.Submit(context.Background(), who, stopAction{"caddy", &runs})

	out.Reset()
	if code := run([]string{"pending", "--admin-socket", sock}, &out, &errb); code != 0 {
		t.Fatalf("pending: %d %s", code, errb.String())
	}
	want := "request 1  docker_stop  jellyfin\n" +
		"  asked by uid 1000 (pid 4242) less than a minute ago, expires in 59 minutes\n" +
		"  stop container jellyfin\n" +
		"\n" +
		"request 2  docker_stop  caddy\n"
	if !strings.HasPrefix(out.String(), want) {
		t.Errorf("got:\n%s\nwant prefix:\n%s", out.String(), want)
	}

	out.Reset()
	if code := run([]string{"approve", "--admin-socket", sock, "1"}, &out, &errb); code != 0 || out.String() != "latch: request 1 approved\n" {
		t.Fatalf("approve: %d %q %q", code, out.String(), errb.String())
	}
	errb.Reset()
	if code := run([]string{"approve", "--admin-socket", sock, "1"}, &out, &errb); code != 1 || errb.String() != "latch: request 1: not pending\n" {
		t.Errorf("approve twice: %d %q", code, errb.String())
	}
	out.Reset()
	if code := run([]string{"deny", "--admin-socket", sock, "2"}, &out, &errb); code != 0 || out.String() != "latch: request 2 denied\n" {
		t.Errorf("deny: %d %q %q", code, out.String(), errb.String())
	}
	errb.Reset()
	if code := run([]string{"deny", "--admin-socket", sock, "77"}, &out, &errb); code != 1 || errb.String() != "latch: request 77: not found or expired\n" {
		t.Errorf("unknown: %d %q", code, errb.String())
	}
	store.Wait()
	if runs.Load() != 1 || a.ID != 1 || b.ID != 2 {
		t.Errorf("runs = %d", runs.Load())
	}

	// latch requests shows how each ended.
	out.Reset()
	if code := run([]string{"requests", "--admin-socket", sock}, &out, &errb); code != 0 {
		t.Fatalf("requests: %d %s", code, errb.String())
	}
	for _, want := range []string{
		"request 1  docker_stop  jellyfin  done\n  asked less than a minute ago, finished less than a minute ago\n  result: stopped\n",
		"request 2  docker_stop  caddy  denied\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("requests output missing %q:\n%s", want, out.String())
		}
	}
}

func TestDecideArgumentErrors(t *testing.T) {
	sock, _ := testAdmin(t)
	for _, args := range [][]string{
		{"approve", "--admin-socket", sock},
		{"approve", "--admin-socket", sock, "1", "2"},
		{"approve", "--admin-socket", sock, "abc"},
		{"approve", "--admin-socket", sock, "0"},
		{"deny", "--admin-socket", sock, "-3"},
		{"pending", "--admin-socket", sock, "extra"},
	} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 2 {
			t.Errorf("%v: code %d", args, code)
		}
	}
}

func TestAdminSocketFromConfig(t *testing.T) {
	sock, _ := testAdmin(t)
	cfg := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(cfg, []byte("server: {socket: /run/l.sock, socket_group: g, admin_socket: "+sock+"}\n"), 0o600)
	var out, errb bytes.Buffer
	if code := run([]string{"pending", "--config", cfg}, &out, &errb); code != 0 {
		t.Fatalf("%d %s", code, errb.String())
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	os.WriteFile(bad, []byte("server: {socket: /run/l.sock, socket_group: g, admin_socket: rel}\n"), 0o600)
	errb.Reset()
	if code := run([]string{"approve", "--config", bad, "1"}, &out, &errb); code != 1 || !strings.HasPrefix(errb.String(), "latch: "+bad+": server.admin_socket") {
		t.Errorf("%d %q", code, errb.String())
	}
}

func TestAdminSocketMissing(t *testing.T) {
	var out, errb bytes.Buffer
	missing := filepath.Join(t.TempDir(), "none.sock")
	if code := run([]string{"pending", "--admin-socket", missing}, &out, &errb); code != 1 || !strings.HasPrefix(errb.String(), "latch: "+missing+": ") || !strings.Contains(errb.String(), "running?") {
		t.Errorf("%d %q", code, errb.String())
	}
}

func TestSpan(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second: "less than a minute", time.Minute: "1 minute", 3 * time.Minute: "3 minutes",
		59*time.Minute + 59*time.Second: "59 minutes", time.Hour: "1 hour", 5 * time.Hour: "5 hours",
		-time.Minute: "less than a minute",
	} {
		if got := span(d); got != want {
			t.Errorf("span(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestPendingShowsUndoRunsByItself(t *testing.T) {
	var out bytes.Buffer
	now := time.Now()
	printPending(&out, []approval.Request{{
		ID: 4, Action: "file_restore", Resource: approval.Resource{Name: "/etc/ssh/sshd_config"}, RunsOnTimeout: true,
		CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(9*time.Minute + 30*time.Second),
	}}, now)
	if !strings.Contains(out.String(), "  filed by Latch 1 minute ago, RUNS BY ITSELF in 9 minutes unless denied\n") {
		t.Errorf("got %q", out.String())
	}
}

func TestPendingOutputIsSanitized(t *testing.T) {
	var out bytes.Buffer
	now := time.Now()
	printPending(&out, []approval.Request{{
		ID: 3, Action: "docker_compose_up", Resource: approval.Resource{Name: "web"},
		Details:   []string{"line \x1b[2J with escape"},
		Params:    []approval.Param{{Name: "pull", Value: "true"}},
		CreatedAt: now.Add(-3 * time.Minute), ExpiresAt: now.Add(57 * time.Minute),
	}}, now)
	if strings.Contains(out.String(), "\x1b") {
		t.Errorf("escape printed: %q", out.String())
	}
	if !strings.Contains(out.String(), "3 minutes ago, expires in 57 minutes\n") || !strings.Contains(out.String(), "  pull: true\n") {
		t.Errorf("got %q", out.String())
	}
}
