package admin

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhmdxsadk/latch/internal/approval"
	"github.com/mhmdxsadk/latch/internal/audit"
)

type action struct {
	name string
	mode approval.Mode
	runs *atomic.Int32
}

func (a action) Kind() string { return "docker_stop" }
func (a action) Resource() approval.Resource {
	return approval.Resource{Type: "container", Name: a.name}
}
func (a action) Params() []approval.Param    { return nil }
func (a action) Describe() []string          { return []string{"stop container " + a.name} }
func (a action) Mode() approval.Mode         { return a.mode }
func (a action) Check(context.Context) error { return nil }
func (a action) Execute(context.Context) (approval.Result, error) {
	a.runs.Add(1)
	return approval.Result{Summary: "stopped"}, nil
}

// serve runs the handler on a Unix socket whose connections carry peer.
func serve(t *testing.T, store *approval.Store, peer *audit.Peer) *Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "la")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "a.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler:           Handler(store),
		ReadHeaderTimeout: time.Second,
		ConnContext: func(ctx context.Context, _ net.Conn) context.Context {
			if peer != nil {
				return audit.WithPeer(ctx, *peer)
			}
			return ctx
		},
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return NewClient(path)
}

func TestApproveDenyAndList(t *testing.T) {
	store := approval.New(approval.Options{})
	t.Cleanup(store.Wait)
	var runs atomic.Int32
	ctx := context.Background()
	who := approval.Requester{UID: 1000, PID: 7}
	a, _ := store.Submit(ctx, who, action{name: "web", runs: &runs})
	b, _ := store.Submit(ctx, who, action{name: "db", runs: &runs})
	c := serve(t, store, &audit.Peer{UID: 0})

	p, err := c.Pending(ctx)
	if err != nil || len(p) != 2 || p[0].ID != a.ID || p[0].RequestedBy.UID != 1000 || p[0].Details[0] != "stop container web" {
		t.Fatalf("%v %+v", err, p)
	}
	got, err := c.Approve(ctx, a.ID)
	if err != nil || got.Status != approval.StatusRunning {
		t.Fatalf("%v %+v", err, got)
	}
	if _, err := c.Approve(ctx, a.ID); err == nil || IsNotFound(err) || !strings.Contains(err.Error(), "not pending") {
		t.Errorf("second approve: %v", err)
	}
	if d, err := c.Deny(ctx, b.ID); err != nil || d.Status != approval.StatusDenied {
		t.Fatalf("%v %+v", err, d)
	}
	store.Wait()
	if runs.Load() != 1 {
		t.Errorf("runs = %d", runs.Load())
	}
	if _, err := c.Approve(ctx, 99); !IsNotFound(err) || err.Error() != "not found or expired" {
		t.Errorf("unknown id: %v", err)
	}
	if p, _ := c.Pending(ctx); len(p) != 0 {
		t.Errorf("pending: %+v", p)
	}
	r, err := c.Recent(ctx)
	if err != nil || len(r) != 2 {
		t.Errorf("recent: %v %+v", err, r)
	}
	one, err := c.Get(ctx, a.ID)
	if err != nil || one.Status != approval.StatusDone || one.DecidedBy == nil || *one.DecidedBy != 0 {
		t.Errorf("get: %v %+v", err, one)
	}
}

func TestDecisionsNeedAVerifiedPeer(t *testing.T) {
	store := approval.New(approval.Options{})
	var runs atomic.Int32
	a, _ := store.Submit(context.Background(), approval.Requester{}, action{name: "web", runs: &runs})
	c := serve(t, store, nil)
	var ae *APIError
	if _, err := c.Approve(context.Background(), a.ID); !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
		t.Fatalf("err = %v", err)
	}
	store.Wait()
	if runs.Load() != 0 {
		t.Error("ran without a verified approver")
	}
}

func TestRoutesAreFixed(t *testing.T) {
	store := approval.New(approval.Options{})
	var runs atomic.Int32
	a, _ := store.Submit(context.Background(), approval.Requester{}, action{name: "web", runs: &runs})
	c := serve(t, store, &audit.Peer{UID: 0})
	id := strconv.FormatInt(a.ID, 10)
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/v1/requests/" + id + "/approve", http.StatusMethodNotAllowed}, // approving needs POST
		{"POST", "/v1/requests", http.StatusMethodNotAllowed},
		{"GET", "/v1/requests/abc", http.StatusBadRequest},
		{"GET", "/v1/requests/0", http.StatusBadRequest},
		{"GET", "/v1/requests/-1", http.StatusBadRequest},
		{"GET", "/v1/requests?status=all", http.StatusBadRequest},
		{"POST", "/v1/requests/" + id + "/execute", http.StatusNotFound},
		{"GET", "/mcp", http.StatusNotFound},
		{"GET", "/", http.StatusNotFound},
	} {
		req, _ := http.NewRequest(tc.method, "http://latch-admin"+tc.path, nil)
		resp, err := c.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
		}
	}
	store.Wait()
	if runs.Load() != 0 {
		t.Error("a GET ran a change")
	}
}
