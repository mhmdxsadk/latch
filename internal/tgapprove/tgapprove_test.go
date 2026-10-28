package tgapprove

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhmdxsadk/latch/internal/admin"
	"github.com/mhmdxsadk/latch/internal/approval"
	"github.com/mhmdxsadk/latch/internal/audit"
	"github.com/mhmdxsadk/latch/internal/tgapi"
	"github.com/mhmdxsadk/latch/internal/tgapi/tgtest"
)

const owner = 4242

type action struct {
	kind    string
	name    string
	mode    approval.Mode
	details []string
	undo    time.Duration
	runs    *atomic.Int32
}

func (a action) Kind() string { return a.kind }
func (a action) Resource() approval.Resource {
	return approval.Resource{Type: "container", Name: a.name}
}
func (a action) Params() []approval.Param    { return []approval.Param{{Name: "pull", Value: "false"}} }
func (a action) Describe() []string          { return a.details }
func (a action) Mode() approval.Mode         { return a.mode }
func (a action) Check(context.Context) error { return nil }
func (a action) Execute(context.Context) (approval.Result, error) {
	a.runs.Add(1)
	return approval.Result{Summary: "stopped", Details: []string{"state exited"}}, nil
}

type undoAction struct{ action }

func (u undoAction) Timeout() time.Duration { return u.undo }

type harness struct {
	t     *testing.T
	store *approval.Store
	tg    *tgtest.Server
	a     *Approver
	runs  atomic.Int32
	sock  string
	state string
	srv   *http.Server

	mu       sync.Mutex
	finished []approval.Request
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir, err := os.MkdirTemp("", "tga")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	h := &harness{t: t, tg: tgtest.New(t), sock: filepath.Join(dir, "a.sock"), state: filepath.Join(dir, "approvals.json")}
	h.serve(approval.New(approval.Options{}))
	h.a = h.approver()
	return h
}

// serve runs the admin API for store on the harness socket, replacing an
// earlier one (a Latch restart).
func (h *harness) serve(store *approval.Store) {
	h.t.Helper()
	if h.srv != nil {
		h.srv.Close()
	}
	os.Remove(h.sock)
	ln, err := net.Listen("unix", h.sock)
	if err != nil {
		h.t.Fatal(err)
	}
	srv := &http.Server{
		Handler:           admin.Handler(store),
		ReadHeaderTimeout: time.Second,
		ConnContext: func(ctx context.Context, _ net.Conn) context.Context {
			return audit.WithPeer(ctx, audit.Peer{UID: 0})
		},
	}
	go srv.Serve(ln)
	h.t.Cleanup(func() { srv.Close(); store.Wait() })
	h.store, h.srv = store, srv
}

func (h *harness) approver() *Approver {
	h.t.Helper()
	c := admin.NewClient(h.sock)
	c.Via = approval.ViaTelegram
	a, err := New(Options{
		Admin: c, Bot: h.tg.Client(), Chat: owner, StatePath: h.state, Location: time.UTC,
		OnFinished: func(r approval.Request, _ int64) {
			h.mu.Lock()
			h.finished = append(h.finished, r)
			h.mu.Unlock()
		},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return a
}

func (h *harness) submit(a approval.Action) approval.Request {
	h.t.Helper()
	r, err := h.store.Submit(context.Background(), approval.Requester{UID: 1000, PID: 7}, a)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func (h *harness) stop(name string) action {
	return action{kind: "docker_stop", name: name, mode: approval.Always, details: []string{"stop container " + name}, runs: &h.runs}
}

func (h *harness) poll() {
	h.t.Helper()
	if err := h.a.Poll(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) press(m tgtest.Sent, data string) string {
	h.t.Helper()
	answer := h.a.decide(context.Background(), data[0] == 'a', mustID(h.t, data), data[strings.LastIndexByte(data, ':')+1:])
	h.store.Wait()
	return answer
}

func mustID(t *testing.T, data string) int64 {
	_, id, _, ok := parseData(data)
	if !ok {
		t.Fatalf("bad data %q", data)
	}
	return id
}

func TestAnnounceApproveAndUpdate(t *testing.T) {
	h := newHarness(t)
	r := h.submit(h.stop("jellyfin"))
	h.poll()
	sent := h.tg.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %+v", sent)
	}
	m := sent[0]
	for _, want := range []string{"uid 1000 wants to stop container jellyfin\nExpires ", " · request 1\nstop container jellyfin\npull: false"} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("message lacks %q:\n%s", want, m.Text)
		}
	}
	if len(m.Keyboard) != 1 || len(m.Keyboard[0]) != 2 || m.Keyboard[0][0].Text != "Approve" || m.Keyboard[0][1].Text != "Deny" ||
		m.Keyboard[0][0].Style != tgapi.StyleSuccess || m.Keyboard[0][1].Style != tgapi.StyleDanger {
		t.Fatalf("keyboard %+v", m.Keyboard)
	}
	h.poll() // a second round must not announce again
	if n := len(h.tg.Sent()); n != 1 {
		t.Fatalf("announced twice: %d", n)
	}

	if got := h.press(m, m.Keyboard[0][0].Data); got != "Approved request 1." {
		t.Fatalf("answer %q", got)
	}
	if h.runs.Load() != 1 {
		t.Fatalf("runs = %d", h.runs.Load())
	}
	h.poll()
	m = h.tg.Sent()[0]
	// The agent reports the result, so Latch's own line is not repeated.
	if m.Text != "Done: stopped container jellyfin" || m.Keyboard != nil {
		t.Fatalf("after approval: %q %+v", m.Text, m.Keyboard)
	}
	got, _ := h.store.Get(r.ID)
	if got.DecidedVia != approval.ViaTelegram {
		t.Errorf("decided via %q", got.DecidedVia)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.finished) != 1 || h.finished[0].ID != r.ID || h.finished[0].Status != approval.StatusDone {
		t.Errorf("finished %+v", h.finished)
	}
}

func TestDecidedOnTheServer(t *testing.T) {
	h := newHarness(t)
	r := h.submit(h.stop("web"))
	h.poll()
	if _, err := h.store.Deny(context.Background(), r.ID, approval.Decider{Via: approval.ViaCLI}); err != nil {
		t.Fatal(err)
	}
	h.poll()
	m := h.tg.Sent()[0]
	if m.Text != "Denied: stop container web" || m.Keyboard != nil {
		t.Fatalf("%q %+v", m.Text, m.Keyboard)
	}
	// The old button now only explains.
	if got := h.press(m, data(true, r.ID, fingerprint(r))); !strings.Contains(got, "already denied") {
		t.Errorf("answer %q", got)
	}
	if h.runs.Load() != 0 {
		t.Error("ran")
	}
}

func TestStaleButtonDoesNothing(t *testing.T) {
	h := newHarness(t)
	r := h.submit(h.stop("web"))
	h.poll()
	if got := h.press(h.tg.Sent()[0], data(true, r.ID, "000000000000")); !strings.Contains(got, "no longer matches") {
		t.Fatalf("answer %q", got)
	}
	if got := h.press(h.tg.Sent()[0], data(true, 99, fingerprint(r))); !strings.Contains(got, "gone or expired") {
		t.Fatalf("answer %q", got)
	}
	if h.runs.Load() != 0 {
		t.Error("ran")
	}
	if p := h.store.List(true, 0); len(p) != 1 {
		t.Errorf("pending %+v", p)
	}
}

func TestLongChangeHasNoApproveButton(t *testing.T) {
	h := newHarness(t)
	a := h.stop("web")
	for i := range 400 {
		a.details = append(a.details, "+ line of a long file edit number "+strings.Repeat("x", i%40))
	}
	r := h.submit(a)
	h.poll()
	m := h.tg.Sent()[0]
	kb := m.Keyboard
	if len(kb) != 1 || len(kb[0]) != 2 || kb[0][0].Copy == nil || kb[0][0].Copy.Text != "latch approve 1" || kb[0][1].Text != "Deny" {
		t.Fatalf("keyboard %+v", kb)
	}
	if msg := tgtest.CheckKeyboard(kb); msg != "" {
		t.Fatal(msg)
	}
	if !strings.Contains(m.Text, "more block(s) not shown") || !strings.Contains(m.Text, "request 1") || !strings.Contains(m.Text, "approve it on the server") {
		t.Fatalf("text:\n%s", m.Text)
	}
	// A hand-made Approve press is refused too.
	if got := h.press(m, data(true, r.ID, fingerprint(r))); !strings.Contains(got, "Too long to approve here") {
		t.Fatalf("answer %q", got)
	}
	if got := h.press(m, m.Keyboard[0][1].Data); got != "Denied request 1." {
		t.Fatalf("answer %q", got)
	}
	if h.runs.Load() != 0 {
		t.Error("ran")
	}
}

func TestStateSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	h.submit(h.stop("web"))
	h.poll()
	h.a = h.approver() // the bridge restarts
	h.poll()
	if n := len(h.tg.Sent()); n != 1 {
		t.Fatalf("announced again after restart: %d", n)
	}
	st, err := os.Stat(h.state)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v %v", err, st.Mode())
	}
}

func TestLatchRestartMarksRequestsGone(t *testing.T) {
	h := newHarness(t)
	h.submit(h.stop("web"))
	h.poll()
	h.serve(approval.New(approval.Options{})) // Latch restarts and forgets
	h.poll()
	m := h.tg.Sent()[0]
	if !strings.Contains(m.Text, "Request 1 is gone") || m.Keyboard != nil {
		t.Fatalf("%q %+v", m.Text, m.Keyboard)
	}
}

func TestAutomaticRestartNotice(t *testing.T) {
	h := newHarness(t)
	h.submit(action{kind: "docker_restart", name: "caddy", mode: approval.RateLimited, runs: &h.runs})
	h.store.Wait()
	h.poll()
	h.poll()
	sent := h.tg.Sent()
	if len(sent) != 1 || !strings.HasPrefix(sent[0].Text, "uid 1000 restarted container caddy\n") || sent[0].Keyboard != nil {
		t.Fatalf("%+v", sent)
	}
}

func TestUndoRequestShowsDeadline(t *testing.T) {
	h := newHarness(t)
	u := undoAction{h.stop("web")}
	u.kind, u.undo = "file_restore", 10*time.Minute
	r := h.submit(u)
	h.poll()
	m := h.tg.Sent()[0]
	want := "A change did not come up. Undo it?\nLatch will undo the change to web by itself at " + r.ExpiresAt.UTC().Format("15:04") + " unless you deny it."
	if !strings.HasPrefix(m.Text, want) || len(m.Keyboard[0]) != 2 {
		t.Fatalf("%q %+v", m.Text, m.Keyboard)
	}
}

func TestUndoRequestNamesTheAgentWhenThereIsOne(t *testing.T) {
	h := newHarness(t)
	h.a.o.AgentName = "keeper"
	u := undoAction{h.stop("web")}
	u.kind, u.undo = "file_restore", 10*time.Minute
	h.submit(u)
	h.poll()
	if m := h.tg.Sent()[0]; !strings.Contains(m.Text, "\nkeeper will undo the change to web by itself at ") || strings.Contains(m.Text, "Latch") {
		t.Fatalf("%q", m.Text)
	}
}

func TestHandlePressIgnoresOtherButtons(t *testing.T) {
	h := newHarness(t)
	for _, d := range []string{"", "s:abc", "a:1", "a:x:000000000000", "a:0:000000000000", "a:1:short", "x:1:000000000000"} {
		if h.a.HandlePress(context.Background(), tgapi.CallbackQuery{ID: "cb", Data: d}) {
			t.Errorf("%q handled", d)
		}
	}
	r := h.submit(h.stop("web"))
	if !h.a.HandlePress(context.Background(), tgapi.CallbackQuery{ID: "cb1", Data: data(false, r.ID, fingerprint(r))}) {
		t.Fatal("not handled")
	}
	if a, ok := h.tg.Answer("cb1"); !ok || a != "Denied request 1." {
		t.Errorf("answer %q %v", a, ok)
	}
}

type fileAction struct {
	action
	params []approval.Param
}

func (f fileAction) Params() []approval.Param { return f.params }

func TestFileRequestsAreFormatted(t *testing.T) {
	h := newHarness(t)
	uid := uint32(1000)
	h.a.o.AgentName, h.a.o.AgentUID = "keeper", &uid

	create := fileAction{action: action{kind: "file_create", name: "/root/hello.txt", mode: approval.Always, runs: &h.runs,
		details: []string{"create /root/hello.txt (mode 0644, owner uid 0 gid 0, 6 bytes)", "content:", "  hello"}},
		params: []approval.Param{{Name: "path", Value: "/root/hello.txt"}, {Name: "mode", Value: "0644"}}}
	edit := fileAction{action: action{kind: "file_edit", name: "/etc/caddy/Caddyfile", mode: approval.Always, runs: &h.runs,
		details: []string{"edit /etc/caddy/Caddyfile", "--- /etc/caddy/Caddyfile (now)", "+++ /etc/caddy/Caddyfile (after the change)",
			"@@ -1,2 +1,2 @@", " example.com {", "-  respond 404", "+  respond 200", "then restart container caddy and check it"}},
		params: []approval.Param{{Name: "path", Value: "/etc/caddy/Caddyfile"}, {Name: "restart", Value: "container caddy"}, {Name: "backup", Value: "b1"}}}
	h.submit(create)
	h.submit(edit)
	h.poll()
	sent := h.tg.Sent()
	if len(sent) != 2 {
		t.Fatalf("%+v", sent)
	}
	for _, m := range sent {
		if msg := tgtest.CheckEntities(m.Text, m.Entities); msg != "" {
			t.Fatalf("Telegram would refuse %q: %s", m.Text, msg)
		}
	}
	wantCreate := "keeper wants to create /root/hello.txt\nExpires " +
		time.Now().Add(time.Hour).UTC().Format("15:04") +
		" · request 1\ncreate /root/hello.txt (mode 0644, owner uid 0 gid 0, 6 bytes)\nContent:\nhello"
	if sent[0].Text != wantCreate {
		t.Errorf("create:\n%s\nwant:\n%s", sent[0].Text, wantCreate)
	}
	f := sent[0].Formatted()
	var styles []string
	for _, e := range f.Entities {
		styles = append(styles, e.Type+"="+f.Slice(e))
	}
	exp := time.Now().Add(time.Hour).UTC().Format("15:04")
	if got := strings.Join(styles, "|"); got != "bold=keeper wants to create /root/hello.txt|date_time="+exp+
		"|expandable_blockquote=create /root/hello.txt (mode 0644, owner uid 0 gid 0, 6 bytes)\nContent:\nhello" {
		t.Errorf("create styles %s", got)
	}

	e := sent[1].Formatted()
	if !strings.Contains(e.S, " example.com {\n-  respond 404\n+  respond 200\nthen restart container caddy and check it\nbackup: b1") ||
		strings.Contains(e.S, "path:") || strings.Contains(e.S, "restart: container caddy") || strings.Contains(e.S, "+++ ") {
		t.Errorf("edit:\n%s", e.S)
	}
	var diff *tgapi.Entity
	for i := range e.Entities {
		if e.Entities[i].Type == "pre" {
			diff = &e.Entities[i]
		}
	}
	if diff == nil || diff.Language != "diff" || !strings.HasPrefix(e.Slice(*diff), "@@ -1,2 +1,2 @@") {
		t.Errorf("diff block %+v", diff)
	}
}

func TestLongDiffIsCollapsed(t *testing.T) {
	h := newHarness(t)
	details := []string{"edit /etc/x.conf", "--- /etc/x.conf (now)", "+++ /etc/x.conf (after the change)", "@@ -1,12 +1,12 @@"}
	for i := range 12 {
		details = append(details, fmt.Sprintf("-old %d", i), fmt.Sprintf("+new %d", i))
	}
	h.submit(fileAction{action: action{kind: "file_edit", name: "/etc/x.conf", mode: approval.Always, runs: &h.runs, details: details}})
	h.poll()
	m := h.tg.Sent()[0]
	if msg := tgtest.CheckEntities(m.Text, m.Entities); msg != "" {
		t.Fatal(msg)
	}
	f := m.Formatted()
	var quote, pre bool
	for _, e := range f.Entities {
		quote = quote || e.Type == "expandable_blockquote" && strings.Contains(f.Slice(e), "@@ -1,12")
		pre = pre || e.Type == "pre" && e.Language == "diff"
	}
	if !quote || !pre {
		t.Fatalf("not collapsed: %+v", f.Entities)
	}
}

func TestFinishedMessagesLeaveWaitingRequestsAlone(t *testing.T) {
	h := newHarness(t)
	done := h.submit(h.stop("web"))
	h.submit(h.stop("db"))
	h.poll()
	sent := h.tg.Sent()
	if len(sent) != 2 {
		t.Fatalf("%+v", sent)
	}
	h.press(sent[0], data(true, done.ID, fingerprint(done)))
	h.poll()
	// Request 1 is decided, request 2 still waits for the owner.
	got := h.a.FinishedMessages()
	if len(got) != 1 || got[0] != sent[0].ID {
		t.Fatalf("finished messages %v, want only %d", got, sent[0].ID)
	}
	if again := h.a.FinishedMessages(); len(again) != 0 {
		t.Errorf("returned again: %v", again)
	}
	// The waiting request still works after the others were cleared.
	h.poll()
	waiting, _ := h.store.Get(2)
	if got := h.press(sent[1], data(false, 2, fingerprint(waiting))); got != "Denied request 2." {
		t.Errorf("the waiting request does not answer: %q", got)
	}
	// The state survives a restart: a cleared request is not shown again.
	h.a = h.approver()
	if got := h.a.FinishedMessages(); len(got) != 1 || got[0] != sent[1].ID {
		t.Errorf("after restart: %v", got)
	}
}

func answerText(s string) tgapi.Text {
	var b tgapi.Builder
	return b.Plain(s).Done()
}

func TestAnswerJoinsTheRequestItFiled(t *testing.T) {
	for _, announced := range []bool{false, true} {
		t.Run(fmt.Sprintf("announced=%v", announced), func(t *testing.T) {
			h := newHarness(t)
			since := time.Now().Add(-time.Second)
			r := h.submit(h.stop("jellyfin"))
			if announced {
				h.poll() // the approver was faster than the end of the turn
			}
			if !h.a.Attach(context.Background(), since, answerText("Jellyfin is stuck, so I would stop it.")) {
				t.Fatal("answer not taken")
			}
			h.poll()
			sent := h.tg.Visible()
			if len(sent) != 1 {
				t.Fatalf("want one message, got %+v", sent)
			}
			// The answer arrives as a new message (that is what clears the
			// turn's live preview), and the one it replaces is deleted.
			if announced && len(h.tg.Sent()) != 2 {
				t.Fatalf("the announced request was not replaced: %+v", h.tg.Sent())
			}
			m := sent[0]
			want := "Jellyfin is stuck, so I would stop it.\n\nuid 1000 wants to stop container jellyfin"
			if !strings.HasPrefix(m.Text, want) || len(m.Keyboard) != 1 || len(m.Keyboard[0]) != 2 {
				t.Fatalf("message %q %+v", m.Text, m.Keyboard)
			}
			// The agent's words are not part of what the buttons decide.
			if got := h.press(m, m.Keyboard[0][0].Data); got != "Approved request 1." {
				t.Fatalf("answer %q (request %d)", got, r.ID)
			}
			h.poll()
			m = h.tg.Visible()[0]
			if !strings.HasPrefix(m.Text, "Done: stopped container jellyfin") || strings.Contains(m.Text, "I would stop it") || m.Keyboard != nil {
				t.Fatalf("after approval: %q %+v", m.Text, m.Keyboard)
			}
			// The agent's check of the result goes under the outcome.
			if !h.a.Follow(context.Background(), m.ID, answerText("Jellyfin is stopped, as asked.")) {
				t.Fatal("follow-up not taken")
			}
			sent = h.tg.Visible()
			if len(sent) != 1 || !strings.HasSuffix(sent[0].Text, "\n\nJellyfin is stopped, as asked.") {
				t.Fatalf("after follow-up: %+v", sent)
			}
			// A restart keeps what is shown; a poll changes nothing.
			h.a = h.approver()
			h.poll()
			if got := h.tg.Visible()[0].Text; got != sent[0].Text {
				t.Errorf("poll changed the message: %q", got)
			}
		})
	}
}

func TestAnswerStaysSeparateWhenItCannotJoin(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if h.a.Attach(ctx, time.Now().Add(-time.Second), answerText("hello")) {
		t.Error("took an answer when no request was filed")
	}
	h.submit(h.stop("old"))
	if h.a.Attach(ctx, time.Now().Add(time.Second), answerText("hello")) {
		t.Error("took an answer for a request filed before the turn")
	}
	if h.a.Follow(ctx, 99, answerText("hello")) {
		t.Error("followed up on a message that is not a request")
	}
	// An answer too long to share the message is not cut.
	r := h.submit(h.stop("db"))
	long := answerText(strings.Repeat("x", tgapi.MaxText-20))
	if h.a.Attach(ctx, r.CreatedAt.Add(-time.Second), long) {
		t.Error("took an answer that does not fit")
	}
	// A request still waiting takes no follow-up.
	h.poll()
	m := h.tg.Visible()[1]
	if h.a.Follow(ctx, m.ID, answerText("done")) {
		t.Error("followed up on a request that is still waiting")
	}
}

func TestEmptyAnswerLeavesTheRequestAlone(t *testing.T) {
	h := newHarness(t)
	since := time.Now().Add(-time.Second)
	h.submit(h.stop("jellyfin"))
	if !h.a.Attach(context.Background(), since, tgapi.Text{}) {
		t.Fatal("an empty answer should not be sent as a message of its own")
	}
	h.poll()
	if sent := h.tg.Visible(); len(sent) != 1 || !strings.HasPrefix(sent[0].Text, "uid 1000 wants") {
		t.Fatalf("%+v", sent)
	}
}

func TestShortContentIsPlainAndLongContentIsCode(t *testing.T) {
	h := newHarness(t)
	body := []string{"create /root/a (mode 0644, owner uid 0 gid 0, 40 bytes)", "content:"}
	for i := range 8 {
		body = append(body, fmt.Sprintf("  line %d", i))
	}
	h.submit(fileAction{action: action{kind: "file_create", name: "/root/a", mode: approval.Always, runs: &h.runs, details: body}})
	h.poll()
	m := h.tg.Sent()[0]
	if msg := tgtest.CheckEntities(m.Text, m.Entities); msg != "" {
		t.Fatal(msg)
	}
	f := m.Formatted()
	var code string
	for _, e := range f.Entities {
		if e.Type == "pre" {
			code = f.Slice(e)
		}
	}
	if !strings.HasPrefix(code, "line 0\nline 1") || !strings.HasSuffix(code, "line 7") {
		t.Errorf("long content is not a code block: %q", code)
	}
}

func TestResultIsKeptWhereNobodyElseReportsIt(t *testing.T) {
	h := newHarness(t)
	h.submit(h.stop("db"))
	h.poll()
	m := h.tg.Sent()[0]
	h.press(m, m.Keyboard[0][0].Data)

	// Without an agent to report the result, the message keeps it.
	h.a.o.OnFinished = nil
	h.poll()
	if got := h.tg.Sent()[0].Text; got != "Done: stopped container db\nstopped\nstate exited" {
		t.Errorf("approvals only: %q", got)
	}
}
