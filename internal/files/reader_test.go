package files

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const pemKey = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACDzBASEKEYBODYLINEXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX
-----END OPENSSH PRIVATE KEY-----`

func numbered(n int) string {
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	return sb.String()
}

func TestReadWindow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p := f.write("public/many.txt", numbered(1000))

	res, err := f.reader.Read(ctx, p, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.StartLine != 1 || res.Lines != 200 || !res.Truncated || res.SizeBytes != int64(len(numbered(1000))) || res.Path != p {
		t.Errorf("default window: %+v", res)
	}
	if !strings.HasPrefix(res.Content, "line 1\nline 2\n") || !strings.HasSuffix(res.Content, "line 200") {
		t.Errorf("content: %q", res.Content[:40])
	}
	if _, err := time.Parse(time.RFC3339, res.Modified); err != nil {
		t.Errorf("modified %q: %v", res.Modified, err)
	}

	res, _ = f.reader.Read(ctx, p, 995, 10)
	if res.Lines != 6 || res.Truncated || res.Content != "line 995\nline 996\nline 997\nline 998\nline 999\nline 1000" {
		t.Errorf("tail window: %+v", res)
	}
	res, _ = f.reader.Read(ctx, p, 1001, 10)
	if res.Lines != 0 || res.Content != "" || res.Truncated {
		t.Errorf("past the end: %+v", res)
	}
	res, _ = f.reader.Read(ctx, p, 1000, 1)
	if res.Lines != 1 || res.Truncated {
		t.Errorf("last line: %+v", res)
	}
	res, _ = f.reader.Read(ctx, p, 1, 2000)
	if res.Lines != 1000 || res.Truncated {
		t.Errorf("whole file: %+v", res)
	}
	for _, c := range [][2]int{{-1, 0}, {0, -1}, {1, 2001}} {
		if _, err := f.reader.Read(ctx, p, c[0], c[1]); !errors.Is(err, ErrBadRequest) {
			t.Errorf("offset %d lines %d: %v", c[0], c[1], err)
		}
	}
	noEOL := f.write("public/noeol.txt", "a\r\nb")
	res, _ = f.reader.Read(ctx, noEOL, 0, 0)
	if res.Content != "a\nb" || res.Lines != 2 || res.Truncated {
		t.Errorf("no final newline: %+v", res)
	}
}

func TestReadByteBounds(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	lim := DefaultLimits()
	long := f.write("public/long.txt", strings.Repeat("x", 1<<20)+"\nnext\n")
	res, err := f.reader.Read(ctx, long, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != lim.ReadBytes || !res.Truncated || res.Lines != 1 {
		t.Errorf("long line: %d bytes, %+v", len(res.Content), res.Truncated)
	}
	res, _ = f.reader.Read(ctx, long, 2, 0)
	if res.Content != "next" || res.Lines != 1 {
		t.Errorf("line after a long line: %q", res.Content)
	}
	many := f.write("public/wide.txt", strings.Repeat(strings.Repeat("y", 999)+"\n", 2000))
	res, _ = f.reader.Read(ctx, many, 0, 2000)
	if len(res.Content) > lim.ReadBytes || res.Lines >= 2000 || !res.Truncated {
		t.Errorf("byte cap: %d bytes, %d lines", len(res.Content), res.Lines)
	}

	small := NewReader(f.guard, Limits{ReadLines: 10, MaxReadLines: 10, ReadBytes: 100, SkipBytes: 50, SniffBytes: 8})
	p := f.write("public/n.txt", numbered(100))
	if _, err := small.Read(ctx, p, 50, 1); !errors.Is(err, ErrBadRequest) {
		t.Errorf("skip budget: %v", err)
	}
}

func TestReadBinaryRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bin := f.write("public/a.bin", "ELF\x00\x01\x02 text")
	if _, err := f.reader.Read(ctx, bin, 0, 0); !errors.Is(err, ErrBinary) {
		t.Errorf("binary: %v", err)
	}
	late := f.write("public/late.bin", strings.Repeat("a", 9000)+"\x00")
	if _, err := f.reader.Read(ctx, late, 0, 0); err != nil {
		t.Errorf("NUL after 8 KiB is not sniffed: %v", err)
	}
	empty := f.write("public/empty.txt", "")
	if res, err := f.reader.Read(ctx, empty, 0, 0); err != nil || res.Lines != 0 || res.Content != "" {
		t.Errorf("empty: %+v %v", res, err)
	}
}

func TestReadRedactsAndSanitizes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p := f.write("public/app.conf", "name=web\nDB_PASSWORD=hunter2\n"+pemKey+"\nafter=1\n\x1b[31mred\x1b[0m\u202eevil\n")
	res, err := f.reader.Read(ctx, p, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"hunter2", "b3BlbnNz", "QyNTUxOQ", "BASEKEY", "\x1b", "\u202e"} {
		if strings.Contains(res.Content, leak) {
			t.Errorf("content leaks %q:\n%s", leak, res.Content)
		}
	}
	if !strings.Contains(res.Content, "DB_PASSWORD=[hidden]") || !strings.Contains(res.Content, "after=1") || !strings.Contains(res.Content, "redevil") {
		t.Errorf("content:\n%s", res.Content)
	}

	// A window that starts inside the key must not show its body.
	for start := 3; start <= 6; start++ {
		res, err := f.reader.Read(ctx, p, start, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, leak := range []string{"b3BlbnNz", "QyNTUxOQ", "BASEKEY"} {
			if strings.Contains(res.Content, leak) {
				t.Errorf("window at %d leaks %q: %q", start, leak, res.Content)
			}
		}
	}
	res, _ = f.reader.Read(ctx, p, 7, 1)
	if res.Content != "after=1" {
		t.Errorf("window after the key: %q", res.Content)
	}
}

func TestList(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	dir := f.mkdir("public/list")
	f.write("public/list/b.txt", "bb")
	f.write("public/list/a.txt", "a")
	f.mkdir("public/list/sub")
	f.write("public/list/.env", "SECRET=1")
	f.write("public/list/tls.key", "k")
	f.write("public/list/x.secret", "k")
	f.symlink(f.path("private/secret.txt"), "public/list/link")
	f.symlink("http://user:pa55word@host/\x1b[2J", "public/list/odd")
	f.mkdir("public/list/.ssh")
	f.write("public/list/.ssh/id_ed25519", "k")
	f.write("public/list/.ssh/id_ed25519.pub", "k")
	os.Chmod(f.path("public/list/b.txt"), 0o640)

	res, err := f.reader.List(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	byName := map[string]Entry{}
	for _, e := range res.Entries {
		names = append(names, e.Name)
		byName[e.Name] = e
	}
	if strings.Join(names, ",") != ".ssh,a.txt,b.txt,link,odd,sub" || res.Truncated {
		t.Errorf("entries: %v truncated=%v", names, res.Truncated)
	}
	if e := byName["b.txt"]; e.Type != "file" || e.Size != 2 || e.Mode != "0640" || e.Modified == "" {
		t.Errorf("b.txt: %+v", e)
	}
	if e := byName["sub"]; e.Type != "dir" {
		t.Errorf("sub: %+v", e)
	}
	if e := byName["link"]; e.Type != "symlink" || !strings.HasSuffix(e.Target, "private/secret.txt") || e.Size == int64(len("s3cret\n")) {
		t.Errorf("link must be reported, not followed: %+v", e)
	}
	if e := byName["odd"]; strings.Contains(e.Target, "pa55word") || strings.Contains(e.Target, "\x1b") {
		t.Errorf("symlink target not sanitized: %q", e.Target)
	}
	ssh, _ := f.reader.List(ctx, f.path("public/list/.ssh"))
	if len(ssh.Entries) != 1 || ssh.Entries[0].Name != "id_ed25519.pub" {
		t.Errorf(".ssh listing: %+v", ssh.Entries)
	}

	// A listing of a parent never shows the blocked directories in it.
	root, _ := f.reader.List(ctx, f.root)
	for _, e := range root.Entries {
		if e.Name == "private" || e.Name == "container-data" {
			t.Errorf("blocked entry listed: %s", e.Name)
		}
	}
	if len(root.Entries) == 0 {
		t.Error("root listing is empty")
	}
	if cfgDir, _ := f.reader.List(ctx, f.path("etc-latch")); len(cfgDir.Entries) != 0 {
		t.Errorf("config file listed: %+v", cfgDir.Entries)
	}
	if home, _ := f.reader.List(ctx, f.path("home")); len(home.Entries) != 0 {
		t.Errorf("agent home listed: %+v", home.Entries)
	}
	if _, err := f.reader.List(ctx, f.blocked); !errors.Is(err, ErrBlocked) {
		t.Errorf("list blocked dir: %v", err)
	}
	if _, err := f.reader.List(ctx, "/proc"); !errors.Is(err, ErrBlocked) {
		t.Errorf("list /proc: %v", err)
	}
}

func TestListCap(t *testing.T) {
	f := newFixture(t)
	dir := f.mkdir("public/big")
	for i := range 30 {
		f.write(fmt.Sprintf("public/big/f%02d", i), "")
	}
	lim := DefaultLimits()
	lim.ListEntries = 10
	r := NewReader(f.guard, lim)
	res, err := r.List(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 10 || !res.Truncated || res.Entries[0].Name != "f00" || res.Entries[9].Name != "f09" {
		t.Errorf("got %d entries, truncated=%v", len(res.Entries), res.Truncated)
	}
	lim = DefaultLimits()
	lim.MaxVisited = 5
	res, _ = NewReader(f.guard, lim).List(context.Background(), dir)
	if len(res.Entries) != 5 || !res.Truncated {
		t.Errorf("visit cap: %d entries, truncated=%v", len(res.Entries), res.Truncated)
	}
}
