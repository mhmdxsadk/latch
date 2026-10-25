package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhmdxsadk/latch/internal/files"
	"github.com/mhmdxsadk/latch/internal/netcheck"
)

// planted is a secret value that must never reach the agent, the audit log
// or "latch pending".
const planted = "hunter2-planted-secret"

type editHarness struct {
	*actHarness
	dir     string
	backups string
	http    *fakeSender
}

func newEditHarness(t *testing.T) *editHarness {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "srv")
	if err := os.MkdirAll(filepath.Join(dir, "blocked"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir, _ = filepath.EvalSymlinks(dir) // macOS: /var is a symlink
	backups := filepath.Join(filepath.Dir(dir), "backups")
	fh := &editHarness{dir: dir, backups: backups, http: &fakeSender{}}
	fh.actHarness = newActHarness(t, 5*time.Second, func(d *Deps) {
		g := files.NewGuard(files.Rules{Trees: []string{filepath.Join(dir, "blocked"), backups}}, nil)
		d.Editor = files.NewEditor(g, files.NewBackups(backups))
		d.HTTP = fh.http
		d.RestartSettle = time.Millisecond
	})
	return fh
}

func (f *editHarness) path(name string) string { return filepath.Join(f.dir, name) }

func (f *editHarness) write(t *testing.T, name, content string) string {
	t.Helper()
	p := f.path(name)
	if err := os.WriteFile(p, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// noSecret fails if the planted secret appears in anything the agent, the
// audit log or the owner's pending view can see.
func (f *editHarness) noSecret(t *testing.T, id int64, outs ...actionOut) {
	t.Helper()
	r, err := f.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	seen = append(seen, r.Details...)
	for _, p := range r.Params {
		seen = append(seen, p.Value)
	}
	if r.Result != nil {
		seen = append(seen, r.Result.Summary)
		seen = append(seen, r.Result.Details...)
	}
	for _, o := range outs {
		seen = append(seen, o.Message, o.Resource)
		if o.Result != nil {
			seen = append(seen, o.Result.Summary)
			seen = append(seen, o.Result.Details...)
		}
	}
	f.store.Wait()
	seen = append(seen, f.audit.String())
	for _, s := range seen {
		if strings.Contains(s, planted) {
			t.Errorf("the real secret leaked: %q", s)
		}
	}
}

func TestFileEditKeepsHiddenValues(t *testing.T) {
	f := newEditHarness(t)
	p := f.write(t, "app.env.conf", "# app\nDB_PASSWORD="+planted+"\nPORT=3000\n")
	out := f.act(t, "file_edit", map[string]any{
		"path": p,
		"old":  "DB_PASSWORD=[hidden]\nPORT=3000",
		"new":  "DB_PASSWORD=[hidden]\nPORT=4000",
	})
	if out.Status != "pending" || out.Resource != p {
		t.Fatalf("got %+v", out)
	}
	r, _ := f.store.Get(out.ID)
	joined := strings.Join(r.Details, "\n")
	if !strings.Contains(joined, "-PORT=3000") || !strings.Contains(joined, "+PORT=4000") || !strings.Contains(joined, "keep their real value") {
		t.Errorf("pending view:\n%s", joined)
	}
	if read(t, p) != "# app\nDB_PASSWORD="+planted+"\nPORT=3000\n" {
		t.Fatal("the file changed before approval")
	}
	f.approve(t, out.ID)
	if got := read(t, p); got != "# app\nDB_PASSWORD="+planted+"\nPORT=4000\n" {
		t.Errorf("content %q", got)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", fi.Mode())
	}
	done := f.act(t, "action_status", map[string]any{"id": out.ID})
	if done.Status != "done" || !strings.Contains(done.Result.Summary, "edited "+p+", backup ") {
		t.Errorf("status %+v", done)
	}
	bk := structured[fileBackupsOut](t, f.harness, "file_backups", map[string]any{"path": p})
	if len(bk.Backups) != 1 {
		t.Fatalf("backups %+v", bk)
	}
	f.noSecret(t, out.ID, out, done)
}

func TestFileEditRefusals(t *testing.T) {
	f := newEditHarness(t)
	p := f.write(t, "a.conf", "x=1\nx=1\nTOKEN="+planted+"\n")
	blocked := f.write(t, "blocked/b.conf", "y=1\n")
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"path": p, "old": "nope", "new": "z"}, "not found"},
		{map[string]any{"path": p, "old": "x=1", "new": "z"}, "more than once"},
		{map[string]any{"path": p, "old": "TOKEN=[hidden]", "new": "TOKEN=[hidden] # note"}, "[hidden]"},
		{map[string]any{"path": blocked, "old": "y=1", "new": "y=2"}, "does not exist or is not allowed"},
		{map[string]any{"path": f.path("missing.conf"), "old": "y=1", "new": "y=2"}, "does not exist or is not allowed"},
		{map[string]any{"path": "relative/a.conf", "old": "y", "new": "z"}, "invalid path"},
		{map[string]any{"path": p, "old": "x", "new": "y", "restart": map[string]any{"container": "secret-vault"}}, "does not exist or is not allowed"},
		{map[string]any{"path": p, "old": "x", "new": "y", "restart": map[string]any{"unit": "latch.service"}}, "latch.service"},
		{map[string]any{"path": p, "old": "x", "new": "y", "mode": "0777"}, ""}, // unknown field
	} {
		got := f.refusal(t, "file_edit", c.args)
		if !strings.Contains(got, c.want) || strings.Contains(got, planted) {
			t.Errorf("%v: %q", c.args, got)
		}
	}
	if a, b := f.refusal(t, "file_edit", map[string]any{"path": blocked, "old": "y", "new": "z"}),
		f.refusal(t, "file_edit", map[string]any{"path": f.path("missing.conf"), "old": "y", "new": "z"}); a != b {
		t.Errorf("blocked %q differs from missing %q", a, b)
	}
}

// A change made to the file between the request and the approval must stop
// the edit: the owner approved a diff against the old content.
func TestFileEditRefusesChangedFile(t *testing.T) {
	f := newEditHarness(t)
	p := f.write(t, "a.conf", "a=1\n")
	out := f.act(t, "file_edit", map[string]any{"path": p, "old": "a=1", "new": "a=2"})
	f.write(t, "a.conf", "a=1\nb=1\n")
	f.approve(t, out.ID)
	done := f.act(t, "action_status", map[string]any{"id": out.ID})
	if done.Status != "failed" || !strings.Contains(done.Result.Summary, "changed") || read(t, p) != "a=1\nb=1\n" {
		t.Errorf("got %+v, file %q", done, read(t, p))
	}
}

// A symlink swapped in after the request must not redirect the write.
func TestFileEditRefusesSymlinkSwap(t *testing.T) {
	f := newEditHarness(t)
	p := f.write(t, "a.conf", "a=1\n")
	other := f.write(t, "blocked/target.conf", "a=1\n")
	out := f.act(t, "file_edit", map[string]any{"path": p, "old": "a=1", "new": "a=2"})
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, p); err != nil {
		t.Fatal(err)
	}
	f.approve(t, out.ID)
	done := f.act(t, "action_status", map[string]any{"id": out.ID})
	if done.Status != "failed" || read(t, other) != "a=1\n" {
		t.Errorf("got %+v, target %q", done, read(t, other))
	}
}

func TestFileEditRestartUpAndRollback(t *testing.T) {
	f := newEditHarness(t)
	p := f.write(t, "web.conf", "listen 80\n")

	// The container comes back up: the edit stays.
	out := f.act(t, "file_edit", map[string]any{"path": p, "old": "listen 80", "new": "listen 81", "restart": map[string]any{"container": "my-web"}})
	f.approve(t, out.ID)
	done := f.act(t, "action_status", map[string]any{"id": out.ID})
	if done.Status != "done" || !strings.Contains(done.Result.Summary, "restarted container my-web, up") || read(t, p) != "listen 81\n" {
		t.Fatalf("up: %+v %q", done, read(t, p))
	}

	// The unit is not active after the restart: the old content comes back.
	out = f.act(t, "file_edit", map[string]any{"path": p, "old": "listen 81", "new": "listen 82", "restart": map[string]any{"unit": "nginx.service"}})
	f.approve(t, out.ID)
	done = f.act(t, "action_status", map[string]any{"id": out.ID})
	if done.Status != "failed" || !strings.Contains(done.Result.Summary, "rolled back") || read(t, p) != "listen 81\n" {
		t.Errorf("rollback: %+v %q", done, read(t, p))
	}
	if calls := f.sa.Calls(); len(calls) != 2 || calls[0] != "restart nginx.service" || calls[1] != "restart nginx.service" {
		t.Errorf("systemd calls %v", calls)
	}
}

func TestFileCreate(t *testing.T) {
	f := newEditHarness(t)
	p := f.path("new.conf")
	out := f.act(t, "file_create", map[string]any{"path": p, "content": "a=1\n", "mode": "0600"})
	if out.Status != "pending" {
		t.Fatalf("got %+v", out)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("created before approval")
	}
	f.approve(t, out.ID)
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 || read(t, p) != "a=1\n" {
		t.Errorf("file %v %v", fi, err)
	}
	existing := f.write(t, "old.conf", "x\n")
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"path": existing, "content": "y\n"}, "exists"},
		{map[string]any{"path": f.path("b.conf"), "content": "y\n", "mode": "0666"}, "mode"},
		{map[string]any{"path": f.path("b.conf"), "content": "y\n", "mode": "4755"}, "mode"},
		{map[string]any{"path": f.path("b.conf"), "content": "k=[hidden]\n"}, "[hidden]"},
		{map[string]any{"path": f.path("blocked/b.conf"), "content": "y\n"}, "does not exist or is not allowed"},
		{map[string]any{"path": f.path("nofolder/b.conf"), "content": "y\n"}, "does not exist or is not allowed"},
	} {
		if got := f.refusal(t, "file_create", c.args); !strings.Contains(got, c.want) {
			t.Errorf("%v: %q", c.args, got)
		}
	}
}

func TestFileRestore(t *testing.T) {
	f := newEditHarness(t)
	p := f.write(t, "a.conf", "v=1\nSECRET="+planted+"\n")
	out := f.act(t, "file_edit", map[string]any{"path": p, "old": "v=1", "new": "v=2"})
	f.approve(t, out.ID)
	bk := structured[fileBackupsOut](t, f.harness, "file_backups", map[string]any{"path": p})
	if len(bk.Backups) != 1 {
		t.Fatalf("backups %+v", bk)
	}
	if f.refusal(t, "file_restore", map[string]any{"path": p, "backup": "../../etc/passwd"}) == "" {
		t.Error("bad backup id accepted")
	}
	out = f.act(t, "file_restore", map[string]any{"path": p, "backup": bk.Backups[0].ID})
	if out.Status != "pending" {
		t.Fatalf("got %+v", out)
	}
	f.approve(t, out.ID)
	if got := read(t, p); got != "v=1\nSECRET="+planted+"\n" {
		t.Errorf("content %q", got)
	}
	// The restore made a backup of what it replaced.
	if bk := structured[fileBackupsOut](t, f.harness, "file_backups", map[string]any{"path": p}); len(bk.Backups) != 2 {
		t.Errorf("backups after restore %+v", bk)
	}
	f.noSecret(t, out.ID, out)
}

type fakeSender struct {
	mu     sync.Mutex
	sent   []string
	target error
}

func (s *fakeSender) CheckHTTPTarget(_ context.Context, raw string) (string, error) {
	if s.target != nil {
		return "", s.target
	}
	if strings.Contains(raw, "blocked") {
		return "", netcheck.ErrNotAllowed
	}
	return "api.example", nil
}

func (s *fakeSender) HTTPRequest(_ context.Context, method, raw, ctype string, body []byte) (netcheck.HTTPResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, method+" "+raw+" "+ctype+" "+string(body))
	return netcheck.HTTPResult{URL: "https://api.example/v1/x", StatusCode: 201, Headers: map[string]string{}, Body: "created\nid=7"}, nil
}

func TestHTTPRequestNeedsApproval(t *testing.T) {
	f := newEditHarness(t)
	out := f.act(t, "http_request", map[string]any{"method": "post", "url": "https://api.example/v1/x?token=abc", "body": `{"a":1}`, "content_type": "application/json"})
	if out.Status != "pending" || out.Resource != "api.example" {
		t.Fatalf("got %+v", out)
	}
	if len(f.http.sent) != 0 {
		t.Fatal("sent before approval")
	}
	r, _ := f.store.Get(out.ID)
	if !strings.Contains(strings.Join(r.Details, "\n"), `{"a":1}`) {
		t.Errorf("owner does not see the body: %v", r.Details)
	}
	for _, p := range r.Params {
		if strings.Contains(p.Value, "abc") {
			t.Errorf("param shows the query value: %v", p)
		}
	}
	f.approve(t, out.ID)
	done := f.act(t, "action_status", map[string]any{"id": out.ID})
	if done.Status != "done" || done.Result.Summary != "POST https://api.example/v1/x: status 201" || len(f.http.sent) != 1 {
		t.Errorf("got %+v %v", done, f.http.sent)
	}

	for _, c := range []map[string]any{
		{"method": "GET", "url": "https://api.example/"},
		{"method": "POST", "url": "https://blocked.example/"},
		{"method": "POST", "url": "https://api.example/", "content_type": "text/html"},
		{"method": "POST", "url": "https://api.example/", "body": "a\x1b[31mb"},
		{"method": "POST", "url": "https://api.example/", "body": strings.Repeat("a", netcheck.MaxWriteBody+1)},
		{"method": "POST", "url": "https://api.example/", "headers": map[string]any{"Authorization": "x"}},
	} {
		f.refusal(t, "http_request", c)
	}

	// The target is checked again when the request runs.
	out = f.act(t, "http_request", map[string]any{"method": "DELETE", "url": "https://api.example/v1/x"})
	f.http.target = netcheck.ErrNotAllowed
	f.approve(t, out.ID)
	if done := f.act(t, "action_status", map[string]any{"id": out.ID}); done.Status != "failed" || len(f.http.sent) != 1 {
		t.Errorf("re-check: %+v", done)
	}
}
