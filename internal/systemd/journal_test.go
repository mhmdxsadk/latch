package systemd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/mhmdxsadk/latch/internal/host"
)

type fakeRun struct {
	out     string
	startEr error
	exitErr error
	args    []string
	calls   int
	closed  bool
	waited  bool
}

func (f *fakeRun) run(_ context.Context, args []string) (io.ReadCloser, func() error, error) {
	f.calls++
	f.args = args
	if f.startEr != nil {
		return nil, nil, f.startEr
	}
	return &closeRecorder{Reader: strings.NewReader(f.out), f: f}, func() error { f.waited = true; return f.exitErr }, nil
}

type closeRecorder struct {
	io.Reader
	f *fakeRun
}

func (c *closeRecorder) Close() error { c.f.closed = true; return nil }

func entry(ts int64, prio, pid, msg string) string {
	return fmt.Sprintf(`{"__REALTIME_TIMESTAMP":"%d","PRIORITY":%q,"_PID":%q,"MESSAGE":%s,"_CMDLINE":"SECRET-CMDLINE","_HOSTNAME":"h"}`+"\n", ts, prio, pid, msg)
}

func TestJournalArgsAreFixed(t *testing.T) {
	f := &fakeRun{}
	if _, err := readJournal(context.Background(), f.run, "ssh.service", 50, 1000); err != nil {
		t.Fatal(err)
	}
	want := []string{"--unit=ssh.service", "--lines=50", "--output=json", "--no-pager", "--quiet",
		"--output-fields=MESSAGE,PRIORITY,_PID,__REALTIME_TIMESTAMP"}
	if strings.Join(f.args, "|") != strings.Join(want, "|") {
		t.Errorf("args %v", f.args)
	}
	if !f.waited {
		t.Error("journalctl not waited for")
	}
}

func TestJournalInvalidInputRejectedBeforeRun(t *testing.T) {
	f := &fakeRun{}
	for _, u := range []string{"", "ssh", "ssh.service\n", "a b.service", "../x.service", "ssh.service --x"} {
		if _, err := readJournal(context.Background(), f.run, u, 10, 1000); err == nil {
			t.Errorf("unit %q accepted", u)
		}
	}
	for _, c := range [][2]int{{0, 10}, {1001, 10}, {-1, 10}, {10, 0}, {10, maxJournalBytes + 1}} {
		if _, err := readJournal(context.Background(), f.run, "ssh.service", c[0], c[1]); err == nil {
			t.Errorf("limits %v accepted", c)
		}
	}
	if f.calls != 0 {
		t.Errorf("journalctl was started %d times", f.calls)
	}
}

func TestJournalProjection(t *testing.T) {
	out := entry(1_700_000_000_123_456, "3", "42", `"disk failed\n"`) +
		entry(1_700_000_001_000_000, "6", "43", `"login password=hunter2 ok"`) +
		entry(1_700_000_002_000_000, "9", "x", `"odd"`) +
		entry(1_700_000_003_000_000, "7", "0", `[104,105,0,255,10]`) + // binary
		entry(1_700_000_004_000_000, "5", "1", `["first","second"]`) +
		entry(1_700_000_005_000_000, "4", "1", `null`) +
		entry(1_700_000_006_000_000, "4", "1", `"\u001b[31mred\u001b[0m \u202eevil"`) +
		"not json\n"
	f := &fakeRun{out: out}
	got, err := readJournal(context.Background(), f.run, "ssh.service", 100, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if got.Truncated || len(got.Entries) != 7 {
		t.Fatalf("got %+v", got)
	}
	e := got.Entries
	if e[0].Time != "2023-11-14T22:13:20.123456Z" || e[0].Priority != 3 || e[0].PriorityName != "err" || e[0].PID != 42 || e[0].Text != "disk failed" {
		t.Errorf("e0 %+v", e[0])
	}
	if e[1].Text != "login password=[hidden] ok" {
		t.Errorf("redaction: %q", e[1].Text)
	}
	if e[2].Priority != -1 || e[2].PriorityName != "unknown" || e[2].PID != 0 {
		t.Errorf("e2 %+v", e[2])
	}
	if e[3].Text != "hi�" {
		t.Errorf("binary: %q", e[3].Text)
	}
	if e[4].Text != "first" || e[4].PriorityName != "notice" {
		t.Errorf("e4 %+v", e[4])
	}
	if e[5].Text != missingMessage {
		t.Errorf("null message: %q", e[5].Text)
	}
	if strings.ContainsAny(e[6].Text, "\x1b\u202e") || !strings.Contains(e[6].Text, "red") {
		t.Errorf("sanitize: %q", e[6].Text)
	}
	for _, x := range got.Entries {
		if strings.Contains(x.Text, "SECRET-CMDLINE") {
			t.Error("undecoded field leaked")
		}
	}
}

func TestJournalBounds(t *testing.T) {
	var sb strings.Builder
	for i := range 30 {
		sb.WriteString(entry(int64(1_700_000_000_000_000+i), "6", "1", fmt.Sprintf(`"line %02d"`, i)))
	}
	f := &fakeRun{out: sb.String()}

	got, _ := readJournal(context.Background(), f.run, "a.service", 10, 100000)
	if len(got.Entries) != 10 || !got.Truncated || got.Entries[9].Text != "line 29" || got.Entries[0].Text != "line 20" {
		t.Errorf("lines bound: %+v", got)
	}
	f = &fakeRun{out: sb.String()}
	got, _ = readJournal(context.Background(), f.run, "a.service", 100, 28) // 7 bytes per line
	if len(got.Entries) != 4 || !got.Truncated || got.Entries[3].Text != "line 29" {
		t.Errorf("bytes bound: %+v", got)
	}
	// One huge line is cut to its end.
	f = &fakeRun{out: entry(1, "6", "1", `"`+strings.Repeat("x", 5000)+`END"`)}
	got, _ = readJournal(context.Background(), f.run, "a.service", 10, 100)
	if len(got.Entries) != 1 || len(got.Entries[0].Text) != 100 || !strings.HasSuffix(got.Entries[0].Text, "END") || !got.Truncated {
		t.Errorf("huge line: %d %v", len(got.Entries[0].Text), got.Truncated)
	}
	// An entry above the per-entry limit is skipped.
	f = &fakeRun{out: entry(1, "6", "1", `"`+strings.Repeat("y", maxJournalEntry)+`"`) + entry(2, "6", "1", `"after"`)}
	got, _ = readJournal(context.Background(), f.run, "a.service", 10, 100)
	if len(got.Entries) != 1 || got.Entries[0].Text != "after" || !got.Truncated {
		t.Errorf("oversized entry: %+v", got)
	}
	// Empty output is not an error and not nil.
	f = &fakeRun{}
	got, err := readJournal(context.Background(), f.run, "a.service", 10, 100)
	if err != nil || got.Entries == nil || len(got.Entries) != 0 || got.Truncated {
		t.Errorf("empty: %+v %v", got, err)
	}
}

func TestJournalSourceCapStopsProcess(t *testing.T) {
	line := entry(1, "6", "1", `"`+strings.Repeat("z", 500000)+`"`)
	f := &fakeRun{out: strings.Repeat(line, 100)} // ~50 MB, above the source cap
	got, err := readJournal(context.Background(), f.run, "a.service", 5, 1000)
	if err != nil || !got.Truncated {
		t.Fatalf("%v %+v", err, got.Truncated)
	}
	if !f.closed || !f.waited {
		t.Errorf("process not stopped: closed=%v waited=%v", f.closed, f.waited)
	}
}

func TestJournalErrorsAreOpaque(t *testing.T) {
	secret := errors.New("SECRET /var/log/journal denied")
	f := &fakeRun{startEr: secret}
	_, err := readJournal(context.Background(), f.run, "a.service", 5, 100)
	if err == nil {
		t.Fatal("no error")
	}
	f = &fakeRun{out: entry(1, "6", "1", `"x"`), exitErr: secret}
	_, err = readJournal(context.Background(), f.run, "a.service", 5, 100)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("exit error %v", err)
	}
}

func TestBusLogsUnsupportedOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("linux")
	}
	f := &fakeRun{}
	_, err := (&Bus{run: f.run}).Logs(context.Background(), "a.service", 5, 100)
	if !errors.Is(err, host.ErrUnsupported) || f.calls != 0 {
		t.Errorf("err=%v calls=%d", err, f.calls)
	}
}

func TestFieldDecoding(t *testing.T) {
	if s, ok := fieldString([]byte(`[104,105]`)); !ok || s != "hi" {
		t.Errorf("%q %v", s, ok)
	}
	for _, bad := range []string{`[300]`, `[-1]`, `{}`, `5`, ``, `[1,"a"]`} {
		if s, ok := fieldString([]byte(bad)); ok {
			t.Errorf("%s decoded to %q", bad, s)
		}
	}
	if n, ok := fieldInt([]byte(`6`)); !ok || n != 6 {
		t.Errorf("number: %d %v", n, ok)
	}
}
