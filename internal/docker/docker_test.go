package docker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

const testID = "4f3c2b1a4f3c2b1a4f3c2b1a4f3c2b1a4f3c2b1a4f3c2b1a4f3c2b1a4f3c2b1a"

const inspectFixture = `{
  "Id": "4f3c2b1a4f3c2b1a4f3c2b1a4f3c2b1a4f3c2b1a4f3c2b1a4f3c2b1a4f3c2b1a",
  "Name": "/web",
  "Path": "/bin/secret-tool",
  "Args": ["--token=hunter2"],
  "RestartCount": 3,
  "Mounts": [{"Source": "/var/lib/secrets", "Destination": "/data"}],
  "HostConfig": {"Binds": ["/var/lib/x:/y"]},
  "NetworkSettings": {"IPAddress": "10.0.0.2"},
  "State": {
    "Status": "running", "Running": true, "Paused": false, "Restarting": false,
    "OOMKilled": false, "ExitCode": 0,
    "StartedAt": "2024-05-01T10:00:00.5+02:00",
    "FinishedAt": "0001-01-01T00:00:00Z",
    "Health": {
      "Status": "unhealthy", "FailingStreak": 4,
      "Log": [
        {"Start": "2024-05-01T10:00:00Z", "End": "2024-05-01T10:00:01Z", "ExitCode": 0, "Output": "SECRET old"},
        {"Start": "2024-05-01T10:01:00Z", "End": "2024-05-01T10:01:02.25Z", "ExitCode": 1, "Output": "curl: (7) refused\r\n\u001b[31mport 3000\u001b[0m\n"}
      ]
    }
  },
  "Config": {
    "Image": "nginx:1.27",
    "Tty": %TTY%,
    "Env": ["SECRET=hunter2", "PATH=/usr/bin"],
    "Labels": {"password": "hunter2"},
    "Cmd": ["run", "--password=hunter2"],
    "Entrypoint": ["/var/lib/entry"]
  }
}`

func fixture(tty bool) string {
	v := "false"
	if tty {
		v = "true"
	}
	return strings.ReplaceAll(inspectFixture, "%TTY%", v)
}

type recorder struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (r *recorder) add(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req.Clone(context.Background()))
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

// newFake serves h behind an Engine and records every request. It fails the
// test on any non-GET method or path outside the allowed endpoints.
func newFake(t *testing.T, h http.HandlerFunc) (*Engine, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		ok := r.Method == http.MethodGet &&
			(r.URL.Path == "/_ping" || r.URL.Path == "/containers/json" || r.URL.Path == "/containers/web/json" ||
				r.URL.Path == "/containers/"+testID+"/json" ||
				r.URL.Path == "/containers/"+testID+"/logs")
		if !ok {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return newEngine(srv.URL, srv.Client().Transport), rec
}

func frame(stream byte, s string) []byte {
	b := make([]byte, 8, 8+len(s))
	b[0] = stream
	binary.BigEndian.PutUint32(b[4:], uint32(len(s)))
	return append(b, s...)
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// logServer answers inspect with the fixture and logs with body.
func logServer(t *testing.T, tty bool, body []byte) (*Engine, *recorder) {
	return newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/json") {
			_, _ = w.Write([]byte(fixture(tty)))
			return
		}
		_, _ = w.Write(body)
	})
}

func TestStatusProjection(t *testing.T) {
	e, rec := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(fixture(false)))
	})
	st, err := e.Status(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	want := Status{
		State: "running", Running: true, RestartCount: 3, Image: "nginx:1.27",
		StartedAt: "2024-05-01T08:00:00.5Z",
		Health: Health{
			Status: "unhealthy", FailingStreak: 4,
			LastCheckAt: "2024-05-01T10:01:02.25Z", LastExitCode: 1,
			LastOutput: "curl: (7) refused\nport 3000",
		},
	}
	if st != want {
		t.Errorf("got %+v\nwant %+v", st, want)
	}
	b, _ := json.Marshal(st)
	for _, bad := range []string{"SECRET", "hunter2", "/var/lib", "10.0.0.2", "PATH", "password"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("output leaks %q: %s", bad, b)
		}
	}
	if strings.Contains(string(b), "finished_at") {
		t.Errorf("zero finished time should be omitted: %s", b)
	}
	if rec.count() != 1 || rec.reqs[0].URL.Path != "/containers/web/json" || rec.reqs[0].URL.RawQuery != "" {
		t.Errorf("unexpected requests: %v", rec.reqs)
	}
}

func TestStatusHealthAndTimes(t *testing.T) {
	tests := []struct {
		name  string
		state string
		want  Status
	}{
		{
			"no healthcheck",
			`{"State":{"Status":"exited","ExitCode":137,"OOMKilled":true,"StartedAt":"0001-01-01T00:00:00Z","FinishedAt":"2024-01-02T03:04:05Z"},"Config":{"Image":"x"}}`,
			Status{State: "exited", ExitCode: 137, OOMKilled: true, FinishedAt: "2024-01-02T03:04:05Z", Image: "x", Health: Health{Status: "none"}},
		},
		{
			"healthy no log",
			`{"State":{"Status":"running","Running":true,"Health":{"Status":"healthy","FailingStreak":0}},"Config":{"Image":"x"}}`,
			Status{State: "running", Running: true, Image: "x", Health: Health{Status: "healthy"}},
		},
		{
			"starting",
			`{"State":{"Status":"restarting","Restarting":true,"Health":{"Status":"starting","FailingStreak":2}},"Config":{"Image":"x"}}`,
			Status{State: "restarting", Restarting: true, Image: "x", Health: Health{Status: "starting", FailingStreak: 2}},
		},
		{
			"garbage time",
			`{"State":{"Status":"paused","Paused":true,"StartedAt":"yesterday"},"Config":{"Image":"x"}}`,
			Status{State: "paused", Paused: true, Image: "x", Health: Health{Status: "none"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"Id":"` + testID + `","Name":"/web",` + tc.state[1:]
			e, _ := newFake(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
			got, err := e.Status(context.Background(), "web")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestStatusErrors(t *testing.T) {
	const secret = "TOP-SECRET-BODY"
	tests := []struct {
		name   string
		status int
		body   string
		notFnd bool
	}{
		{"404", 404, `{"message":"` + secret + `"}`, true},
		{"500", 500, `{"message":"` + secret + `"}`, false},
		{"403", 403, secret, false},
		{"bad json", 200, `{"State": ` + secret, false},
		{"204", 204, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := newFake(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := e.Status(context.Background(), "web")
			if err == nil {
				t.Fatal("expected error")
			}
			if errors.Is(err, ErrNotFound) != tc.notFnd {
				t.Errorf("ErrNotFound = %v, want %v (%v)", !tc.notFnd, tc.notFnd, err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error leaks body: %v", err)
			}
		})
	}
}

func TestInvalidNamesRejectedBeforeRequest(t *testing.T) {
	names := []string{"", "../x", "a/b", "x?y", "-x", ".x", "_x", "a b", "a\x00b", "a%2fb", "a#b", "a\nb",
		strings.Repeat("a", 129), "é", "a:b", "a@b"}
	e, rec := newFake(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(fixture(false))) })
	for _, n := range names {
		if _, err := e.Status(context.Background(), n); err == nil {
			t.Errorf("Status(%q) accepted", n)
		}
		if _, err := e.Logs(context.Background(), n, 10, 100); err == nil {
			t.Errorf("Logs(%q) accepted", n)
		}
	}
	if n := rec.count(); n != 0 {
		t.Errorf("%d requests sent for invalid names", n)
	}
}

func TestValidNameAccepted(t *testing.T) {
	e, rec := newFake(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(fixture(false))) })
	if _, err := e.Status(context.Background(), "web"); err != nil {
		t.Fatal(err)
	}
	if rec.count() != 1 {
		t.Fatal("no request")
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	var hits []string
	var mu sync.Mutex
	e, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/containers/web/json" {
			http.Redirect(w, r, "/containers/web/other", http.StatusFound)
			return
		}
		t.Errorf("redirect target fetched: %s", r.URL.Path)
	})
	_, err := e.Status(context.Background(), "web")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("want generic error, got %v", err)
	}
	if len(hits) != 1 {
		t.Errorf("hits = %v", hits)
	}
}

func TestPing(t *testing.T) {
	tests := []struct {
		status  int
		wantErr bool
	}{{200, false}, {500, true}, {404, true}, {302, true}, {204, true}}
	for _, tc := range tests {
		e, _ := newFake(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte("OK"))
		})
		if err := e.Ping(context.Background()); (err != nil) != tc.wantErr {
			t.Errorf("status %d: err = %v", tc.status, err)
		}
	}
	// Unreachable daemon.
	srv := httptest.NewServer(http.NotFoundHandler())
	e := newEngine(srv.URL, srv.Client().Transport)
	srv.Close()
	if err := e.Ping(context.Background()); err == nil {
		t.Error("expected error for closed server")
	}
}

func TestLogsRequestShape(t *testing.T) {
	e, rec := logServer(t, false, nil)
	if _, err := e.Logs(context.Background(), "web", 42, 1000); err != nil {
		t.Fatal(err)
	}
	if rec.count() != 2 {
		t.Fatalf("requests = %d", rec.count())
	}
	if p := rec.reqs[0].URL.Path; p != "/containers/web/json" {
		t.Errorf("first request %s, want inspect", p)
	}
	r := rec.reqs[1]
	q := r.URL.Query()
	if r.Method != http.MethodGet || r.URL.Path != "/containers/"+testID+"/logs" {
		t.Errorf("bad request %s %s", r.Method, r.URL.Path)
	}
	if q.Get("tail") != "42" || q.Get("timestamps") != "1" || q.Get("stdout") != "1" || q.Get("stderr") != "1" {
		t.Errorf("bad query %v", q)
	}
	if _, ok := q["follow"]; ok {
		t.Error("follow must not be set")
	}
}

func TestLogsNotFound(t *testing.T) {
	e, _ := newFake(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
	if _, err := e.Logs(context.Background(), "web", 10, 100); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestLogsRangeRejected(t *testing.T) {
	e, rec := logServer(t, false, nil)
	tests := []struct{ lines, max int }{
		{0, 100}, {-1, 100}, {1001, 100}, {10, 0}, {10, -5}, {10, 262145},
	}
	for _, tc := range tests {
		if _, err := e.Logs(context.Background(), "web", tc.lines, tc.max); err == nil {
			t.Errorf("Logs(%d,%d) accepted", tc.lines, tc.max)
		}
	}
	if rec.count() != 0 {
		t.Errorf("%d requests for out-of-range args", rec.count())
	}
	if _, err := e.Logs(context.Background(), "web", 1000, 262144); err != nil {
		t.Errorf("max bounds rejected: %v", err)
	}
}

func TestLogsDemux(t *testing.T) {
	const ts1, ts2, ts3 = "2024-05-01T10:00:00.000000001Z", "2024-05-01T10:00:01Z", "2024-05-01T10:00:02.5Z"
	tests := []struct {
		name string
		tty  bool
		body []byte
		want []LogEntry
	}{
		{
			"mixed streams",
			false,
			cat(frame(1, ts1+" out1\n"), frame(2, ts2+" err1\n"), frame(1, ts3+" out2\n")),
			[]LogEntry{
				{"stdout", "2024-05-01T10:00:00.000000001Z", "out1"},
				{"stderr", "2024-05-01T10:00:01Z", "err1"},
				{"stdout", "2024-05-01T10:00:02.5Z", "out2"},
			},
		},
		{
			"line split across frames, stderr interleaved",
			false,
			cat(frame(1, ts1+" hel"), frame(2, ts2+" e\n"), frame(1, "lo\n")),
			[]LogEntry{
				{"stderr", "2024-05-01T10:00:01Z", "e"},
				{"stdout", "2024-05-01T10:00:00.000000001Z", "hello"},
			},
		},
		{
			"unterminated final line, CRLF, empty text",
			false,
			cat(frame(1, ts1+" a\r\n"+ts2+" \n"+ts3+" tail")),
			[]LogEntry{
				{"stdout", "2024-05-01T10:00:00.000000001Z", "a"},
				{"stdout", "2024-05-01T10:00:01Z", ""},
				{"stdout", "2024-05-01T10:00:02.5Z", "tail"},
			},
		},
		{
			"stdin frame ignored",
			false,
			cat(frame(0, "ignored\n"), frame(1, ts1+" x\n")),
			[]LogEntry{{"stdout", "2024-05-01T10:00:00.000000001Z", "x"}},
		},
		{
			"no timestamp",
			false,
			cat(frame(1, "plain words\nnotatime here\n")),
			[]LogEntry{{"stdout", "", "plain words"}, {"stdout", "", "notatime here"}},
		},
		{
			"ansi, control chars, tabs",
			false,
			cat(frame(1, ts1+" \x1b[31mred\x1b[0m\ta\x00b\x07c\x1b]0;x\x07d\x7f\n")),
			[]LogEntry{{"stdout", "2024-05-01T10:00:00.000000001Z", "red\tabcd"}},
		},
		{
			"invalid utf8",
			false,
			cat(frame(1, ts1+" ok\xff\xfe bad\xc3\n")),
			[]LogEntry{{"stdout", "2024-05-01T10:00:00.000000001Z", "ok\uFFFD bad\uFFFD"}},
		},
		{
			"tty raw",
			true,
			[]byte(ts1 + " one\r\n" + ts2 + " \x1b[1mtwo\x1b[0m\n"),
			[]LogEntry{
				{"tty", "2024-05-01T10:00:00.000000001Z", "one"},
				{"tty", "2024-05-01T10:00:01Z", "two"},
			},
		},
		{
			"tty does not demux",
			true,
			frame(1, ts1+" x\n"),
			nil, // header bytes are treated as text; only checked below for no error
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := logServer(t, tc.tty, tc.body)
			got, err := e.Logs(context.Background(), "web", 100, 10000)
			if err != nil {
				t.Fatal(err)
			}
			if got.Entries == nil {
				t.Fatal("Entries is nil")
			}
			if tc.want == nil {
				return
			}
			if got.Truncated {
				t.Error("unexpected truncation")
			}
			if len(got.Entries) != len(tc.want) {
				t.Fatalf("got %+v\nwant %+v", got.Entries, tc.want)
			}
			for i := range tc.want {
				if got.Entries[i] != tc.want[i] {
					t.Errorf("entry %d = %+v, want %+v", i, got.Entries[i], tc.want[i])
				}
			}
		})
	}
}

func TestLogsEmptyNonNil(t *testing.T) {
	for _, tty := range []bool{false, true} {
		e, _ := logServer(t, tty, nil)
		got, err := e.Logs(context.Background(), "web", 10, 100)
		if err != nil {
			t.Fatal(err)
		}
		if got.Entries == nil || len(got.Entries) != 0 || got.Truncated {
			t.Errorf("tty=%v: %+v", tty, got)
		}
		b, _ := json.Marshal(got)
		if !strings.Contains(string(b), `"entries":[]`) {
			t.Errorf("marshalled: %s", b)
		}
	}
}

func TestLogsMalformedStreams(t *testing.T) {
	huge := make([]byte, 8)
	huge[0] = 1
	binary.BigEndian.PutUint32(huge[4:], maxFrame+1)
	hugeMax := make([]byte, 8)
	hugeMax[0] = 1
	binary.BigEndian.PutUint32(hugeMax[4:], 0xFFFFFFFF)
	tests := []struct {
		name string
		body []byte
	}{
		{"oversized frame", huge},
		{"max uint32 frame", hugeMax},
		{"unknown stream type", frame(7, "x\n")},
		{"truncated payload", frame(1, "hello\n")[:12]},
		{"truncated header", []byte{1, 0, 0}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := logServer(t, false, tc.body)
			if _, err := e.Logs(context.Background(), "web", 10, 100); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestLogsMaxBytesKeepsNewest(t *testing.T) {
	body := cat(frame(1, "2024-05-01T10:00:00Z aaaa\n2024-05-01T10:00:01Z bbbb\n2024-05-01T10:00:02Z cccc\n"))
	tests := []struct {
		max       int
		want      []string
		truncated bool
	}{
		{12, []string{"aaaa", "bbbb", "cccc"}, false},
		{11, []string{"bbbb", "cccc"}, true},
		{8, []string{"bbbb", "cccc"}, true},
		{4, []string{"cccc"}, true},
		{3, []string{"ccc"}, true},
	}
	for _, tc := range tests {
		e, _ := logServer(t, false, body)
		got, err := e.Logs(context.Background(), "web", 10, tc.max)
		if err != nil {
			t.Fatal(err)
		}
		var texts []string
		total := 0
		for _, en := range got.Entries {
			texts = append(texts, en.Text)
			total += len(en.Text)
		}
		if strings.Join(texts, ",") != strings.Join(tc.want, ",") || got.Truncated != tc.truncated || total > tc.max {
			t.Errorf("max=%d: got %v truncated=%v", tc.max, texts, got.Truncated)
		}
	}
}

func TestLogsSingleHugeLineTailTruncated(t *testing.T) {
	// 'é' is 2 bytes; a cut in the middle of one must land on a boundary.
	line := "2024-05-01T10:00:00Z " + strings.Repeat("a", 5000) + strings.Repeat("é", 10) + "END"
	e, _ := logServer(t, false, frame(1, line+"\n"))
	got, err := e.Logs(context.Background(), "web", 10, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 1 || !got.Truncated {
		t.Fatalf("got %+v", got)
	}
	txt := got.Entries[0].Text
	if txt != "éééEND" && txt != "ééEND" {
		t.Errorf("text = %q", txt)
	}
	if len(txt) > 8 || strings.Contains(txt, "\uFFFD") {
		t.Errorf("bad tail %q", txt)
	}
}

func TestLogsLinesLimit(t *testing.T) {
	// A misbehaving daemon returning more than tail still yields <= lines.
	var b strings.Builder
	for _, s := range []string{"a", "b", "c", "d"} {
		b.WriteString("2024-05-01T10:00:00Z " + s + "\n")
	}
	e, _ := logServer(t, false, frame(1, b.String()))
	got, err := e.Logs(context.Background(), "web", 2, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 2 || got.Entries[0].Text != "c" || got.Entries[1].Text != "d" || !got.Truncated {
		t.Errorf("got %+v", got)
	}
}

func TestLogsReadCap(t *testing.T) {
	// Stream far more than the 8 MiB cap using 1 MiB frames of one long line.
	e, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/json") {
			_, _ = w.Write([]byte(fixture(false)))
			return
		}
		chunk := frame(1, strings.Repeat("x", maxFrame))
		for range 12 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	got, err := e.Logs(context.Background(), "web", 10, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || len(got.Entries) != 1 || len(got.Entries[0].Text) != 100 {
		t.Errorf("entries=%d truncated=%v", len(got.Entries), got.Truncated)
	}
}

func TestContextTimeout(t *testing.T) {
	e, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := e.Status(ctx, "web"); err == nil {
		t.Fatal("expected timeout error")
	}
	if time.Since(start) > 5*time.Second {
		t.Error("timeout not honoured")
	}
}

func TestUnixSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "dk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	srv := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/_ping" {
				_, _ = w.Write([]byte("OK"))
				return
			}
			_, _ = w.Write([]byte(fixture(false)))
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	e := NewEngine(sock)
	if err := e.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := e.Status(context.Background(), "web")
	if err != nil || st.Image != "nginx:1.27" {
		t.Fatalf("st=%+v err=%v", st, err)
	}
	if e.client.Transport.(*http.Transport).Proxy != nil {
		t.Error("proxy must be disabled")
	}
	if err := NewEngine(filepath.Join(dir, "missing.sock")).Ping(context.Background()); err == nil {
		t.Error("expected dial error")
	}
}

// Docker resolves names as ID prefixes too; an inspect answer for any other
// container must be treated as not found, and nothing else fetched.
func TestInspectMustMatchConfiguredName(t *testing.T) {
	for _, body := range []string{
		strings.Replace(fixture(false), `"/web"`, `"/webapp-other"`, 1),
		strings.Replace(fixture(false), `"Name": "/web",`, ``, 1),
	} {
		e, rec := newFake(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
		if _, err := e.Status(context.Background(), "web"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Status err = %v", err)
		}
		if _, err := e.Logs(context.Background(), "web", 10, 100); !errors.Is(err, ErrNotFound) {
			t.Errorf("Logs err = %v", err)
		}
		if rec.count() != 2 {
			t.Errorf("requests = %d; logs must not be fetched after a mismatch", rec.count())
		}
	}
	bad := strings.Replace(fixture(false), testID, "../../x", 1)
	e, _ := newFake(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(bad)) })
	if _, err := e.Logs(context.Background(), "web", 10, 100); err == nil {
		t.Error("malformed ID accepted")
	}
}

// When the stream exceeds the byte budget, the newest lines are returned.
func TestLogsKeepsNewestOfLargeStream(t *testing.T) {
	var body []byte
	for i := range 1000 {
		line := fmt.Sprintf("2024-05-01T10:00:00Z %04d %s\n", i, strings.Repeat("x", 16<<10))
		body = append(body, frame(1, line)...)
	}
	e, _ := logServer(t, false, body)
	got, err := e.Logs(context.Background(), "web", 1000, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	last := got.Entries[len(got.Entries)-1].Text
	if !got.Truncated || !strings.HasPrefix(last, "0999 ") {
		t.Errorf("truncated=%v last=%.10q", got.Truncated, last)
	}
}

// Docker splits long lines into frames that each repeat the timestamp.
func TestLogsContinuationTimestampsDropped(t *testing.T) {
	body := cat(frame(1, "2024-05-01T10:00:00Z abc"), frame(1, "2024-05-01T10:00:00Z def\n"))
	e, _ := logServer(t, false, body)
	got, err := e.Logs(context.Background(), "web", 10, 100)
	if err != nil || len(got.Entries) != 1 || got.Entries[0].Text != "abcdef" {
		t.Errorf("got %+v %v", got, err)
	}
}

func TestLogsLongLineMemoryBounded(t *testing.T) {
	chunk := frame(1, strings.Repeat("y", 16<<10))
	var body []byte
	for range 1024 { // one 16 MiB line without a newline
		body = append(body, chunk...)
	}
	e, _ := logServer(t, false, body)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got, err := e.Logs(context.Background(), "web", 10, 1000)
	runtime.ReadMemStats(&after)
	if err != nil || len(got.Entries) != 1 || len(got.Entries[0].Text) != 1000 || !got.Truncated {
		t.Fatalf("got %d entries, %v", len(got.Entries), err)
	}
	// The test server and transport allocate too; the old quadratic
	// collector allocated gigabytes here.
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 256<<20 {
		t.Errorf("allocated %d MiB", alloc>>20)
	}
}

func TestHealthOutputBounded(t *testing.T) {
	long := strings.Repeat("a", maxHealthOutput-1) + "étail"
	got := healthOutput(long)
	if len(got) != maxHealthOutput-1 || !utf8.ValidString(got) {
		t.Errorf("len %d, valid %v", len(got), utf8.ValidString(got))
	}
	if got := healthOutput("x\u202ey\n\n"); got != "xy" {
		t.Errorf("got %q", got)
	}
}

func TestOutputIsRedacted(t *testing.T) {
	if got := healthOutput("curl failed\nDB_PASSWORD=hunter2"); got != "curl failed\nDB_PASSWORD=[hidden]" {
		t.Errorf("health output %q", got)
	}
	body := cat(frame(1, "2024-05-01T10:00:00Z start token=abc123 ok\n"), frame(2, "2024-05-01T10:00:01Z \u001b[31mAuthorization: Bearer abcdef123456\u001b[0m\n"))
	e, _ := logServer(t, false, body)
	got, err := e.Logs(context.Background(), "web", 10, 1000)
	if err != nil || len(got.Entries) != 2 {
		t.Fatalf("got %+v %v", got, err)
	}
	if got.Entries[0].Text != "start token=[hidden] ok" || got.Entries[1].Text != "Authorization: Bearer [hidden]" {
		t.Errorf("entries %+v", got.Entries)
	}
}

// A container must be reachable by name only: its ID resolves in Docker but
// would let an excluded container be addressed under a different string.
func TestFullIDIsNotAName(t *testing.T) {
	e, _ := newFake(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(fixture(false))) })
	if _, err := e.Status(context.Background(), testID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Status by ID err = %v", err)
	}
	if _, err := e.Logs(context.Background(), testID, 10, 100); !errors.Is(err, ErrNotFound) {
		t.Errorf("Logs by ID err = %v", err)
	}
}

func TestList(t *testing.T) {
	e, rec := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "all=true" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`[
			{"Id":"x","Names":["/zeta"],"Env":["SECRET=hunter2"]},
			{"Id":"y","Names":["/web","/other/web"]},
			{"Id":"z","Names":["/other/alias","/linked"]},
			{"Id":"w","Names":["/bad name"]},
			{"Id":"v","Names":["/web"]}
		]`))
	})
	got, err := e.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := "linked,web,zeta"; strings.Join(got, ",") != want {
		t.Errorf("got %v, want %s", got, want)
	}
	if rec.count() != 1 {
		t.Errorf("requests = %d", rec.count())
	}
}

func TestListErrors(t *testing.T) {
	e, _ := newFake(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"not":"a list"}`)) })
	if _, err := e.List(context.Background()); err == nil {
		t.Error("malformed list accepted")
	}
	e, _ = newFake(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	if _, err := e.List(context.Background()); err == nil {
		t.Errorf("err = %v", err)
	}
}

const settingsFixture = `{
  "Id": "4f3c2b1a4f3c2b1a4f3c2b1a4f3c2b1a4f3c2b1a4f3c2b1a4f3c2b1a4f3c2b1a",
  "Name": "/web",
  "Config": {
    "Image": "ghcr.io/example/web:1.2",
    "Env": ["API_KEY=envsecret", "PATH=/usr/bin"],
    "Cmd": ["serve", "--password=cmdsecret"],
    "Entrypoint": ["/entry", "--token=entrysecret"],
    "Labels": {
      "com.docker.compose.project": "media",
      "com.docker.compose.project.config_files": "/srv/media/compose.yaml",
      "db_password": "labelsecret",
      "note": "token=abc123",
      "bad\u202ekey": "ok"
    },
    "Healthcheck": {
      "Test": ["CMD-SHELL", "curl -fsS -u admin:pw http://localhost:3000/health?token=hcsecret || exit 1"],
      "Interval": 30000000000, "Timeout": 5000000000, "Retries": 3
    }
  },
  "HostConfig": {"NetworkMode": "media_default", "RestartPolicy": {"Name": "on-failure", "MaximumRetryCount": 5}},
  "NetworkSettings": {
    "Ports": {"3000/tcp": [{"HostIp": "0.0.0.0", "HostPort": "8080"}], "9000/tcp": null},
    "Networks": {"media_default": {"IPAddress": "172.18.0.4"}}
  },
  "Mounts": [
    {"Type": "bind", "Source": "/srv/media/config", "Destination": "/config", "RW": true},
    {"Type": "volume", "Name": "cache", "Source": "/var/lib/docker/volumes/cache/_data", "Destination": "/cache", "RW": false}
  ]
}`

func TestSettings(t *testing.T) {
	e, _ := newFake(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(settingsFixture)) })
	got, err := e.Settings(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if got.Image != "ghcr.io/example/web:1.2" || got.RestartPolicy != "on-failure:5" || got.NetworkMode != "media_default" {
		t.Errorf("basics %+v", got)
	}
	if len(got.Ports) != 1 || got.Ports[0] != (Port{Container: "3000/tcp", HostIP: "0.0.0.0", HostPort: "8080"}) {
		t.Errorf("ports %+v", got.Ports)
	}
	wantMounts := []Mount{
		{Type: "bind", Source: "/srv/media/config", Destination: "/config"},
		{Type: "volume", Source: "cache", Destination: "/cache", ReadOnly: true},
	}
	if !slices.Equal(got.Mounts, wantMounts) {
		t.Errorf("mounts %+v", got.Mounts)
	}
	if len(got.Networks) != 1 || got.Networks[0] != (Network{Name: "media_default", IPAddress: "172.18.0.4"}) {
		t.Errorf("networks %+v", got.Networks)
	}
	if got.Labels["com.docker.compose.project.config_files"] != "/srv/media/compose.yaml" ||
		got.Labels["db_password"] != "[hidden]" || got.Labels["note"] != "[hidden]" || got.Labels["badkey"] != "ok" {
		t.Errorf("labels %+v", got.Labels)
	}
	hc := got.HealthCheck
	if hc == nil || hc.Kind != "CMD-SHELL" || hc.Interval != "30s" || hc.Timeout != "5s" || hc.StartPeriod != "" || hc.Retries != 3 {
		t.Fatalf("health check %+v", hc)
	}
	if !strings.Contains(hc.Command, "http://localhost:3000/health?token=[hidden]") || strings.Contains(hc.Command, "hcsecret") {
		t.Errorf("health check command %q", hc.Command)
	}

	// Environment variables and command lines must never come back.
	b, _ := json.Marshal(got)
	for _, secret := range []string{"envsecret", "cmdsecret", "entrysecret", "labelsecret", "hcsecret", "abc123", "PATH=", "/entry"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("output contains %q: %s", secret, b)
		}
	}
}

func TestSettingsMinimal(t *testing.T) {
	e, _ := newFake(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(fixture(false))) })
	got, err := e.Settings(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if got.RestartPolicy != "no" || got.HealthCheck != nil || got.Ports == nil || got.Mounts == nil || got.Networks == nil || got.Labels == nil {
		t.Errorf("got %+v", got)
	}
}

func TestSettingsBounded(t *testing.T) {
	labels := make([]string, 0, 100)
	for i := range 100 {
		labels = append(labels, fmt.Sprintf("%q: %q", fmt.Sprintf("l%03d", i), strings.Repeat("v", 2000)))
	}
	doc := `{"Id": "` + testID + `", "Name": "/web", "Config": {"Labels": {` + strings.Join(labels, ",") + `}}}`
	e, _ := newFake(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(doc)) })
	got, err := e.Settings(context.Background(), "web")
	if err != nil || !got.Truncated || len(got.Labels) != maxLabels || len(got.Labels["l000"]) != maxSettingText {
		t.Errorf("truncated=%v labels=%d len=%d err=%v", got.Truncated, len(got.Labels), len(got.Labels["l000"]), err)
	}
}

func TestSettingsNotFound(t *testing.T) {
	e, _ := newFake(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	if _, err := e.Settings(context.Background(), "web"); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v", err)
	}
}
