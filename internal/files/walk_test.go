package files

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func paths(ms []FoundPath) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Path)
	}
	return out
}

func TestFind(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.write("public/a/nginx.conf", "x")
	f.write("public/a/b/site.conf", "x")
	f.write("public/a/b/.env", "x")
	f.write("public/a/b/c.secret", "x")
	f.symlink(f.blocked, "public/a/into-private")
	f.symlink(f.path("public/a"), "public/a/b/loop")
	f.write("private/found.conf", "x")

	res, err := f.reader.Find(ctx, f.path("public"), "*.conf")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{f.path("public/a/b/site.conf"), f.path("public/a/nginx.conf")}
	if strings.Join(paths(res.Matches), ",") != strings.Join(want, ",") || res.Truncated {
		t.Errorf("got %v truncated=%v", paths(res.Matches), res.Truncated)
	}
	if res.Matches[0].Type != "file" {
		t.Errorf("type: %+v", res.Matches[0])
	}

	// Symlinks are reported but never followed.
	res, _ = f.reader.Find(ctx, f.path("public"), "into-*")
	if len(res.Matches) != 1 || res.Matches[0].Type != "symlink" {
		t.Errorf("symlink match: %+v", res.Matches)
	}
	res, _ = f.reader.Find(ctx, f.path("public"), "secret.txt")
	if len(res.Matches) != 0 {
		t.Errorf("followed a symlink into a blocked tree: %+v", res.Matches)
	}
	for _, name := range []string{".env", "*.secret", "found.conf"} {
		if res, _ := f.reader.Find(ctx, f.root, name); len(res.Matches) != 0 {
			t.Errorf("found blocked %s: %v", name, paths(res.Matches))
		}
	}

	// Paths are shown under the requested path, even through a symlink.
	link := f.symlink(f.path("public/a"), "public-link")
	res, _ = f.reader.Find(ctx, link, "nginx.conf")
	if len(res.Matches) != 1 || res.Matches[0].Path != link+"/nginx.conf" {
		t.Errorf("display path: %+v", res.Matches)
	}

	for _, bad := range []string{"", "a/b", "../x", "[ab]", "a\\*", "a\nb", strings.Repeat("a", 256)} {
		if _, err := f.reader.Find(ctx, f.path("public"), bad); !errors.Is(err, ErrBadRequest) {
			t.Errorf("pattern %q: %v", bad, err)
		}
	}
	if _, err := f.reader.Find(ctx, f.blocked, "*"); !errors.Is(err, ErrBlocked) {
		t.Errorf("find in blocked: %v", err)
	}
}

func TestFindBudgets(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	deep := "public/deep"
	for i := range 12 {
		deep += fmt.Sprintf("/d%d", i)
	}
	f.write(deep+"/bottom.txt", "x")
	f.write("public/deep/d0/d1/d2/d3/d4/d5/d6/at8.txt", "x") // depth 8 below public/deep
	res, _ := f.reader.Find(ctx, f.path("public/deep"), "*.txt")
	if len(res.Matches) != 1 || !strings.HasSuffix(res.Matches[0].Path, "at8.txt") || !res.Truncated {
		t.Errorf("depth: %v truncated=%v", paths(res.Matches), res.Truncated)
	}

	for i := range 50 {
		f.write(fmt.Sprintf("public/flat/f%02d.log", i), "x")
	}
	lim := DefaultLimits()
	lim.MaxResults = 10
	res, _ = NewReader(f.guard, lim).Find(ctx, f.path("public/flat"), "*.log")
	if len(res.Matches) != 10 || !res.Truncated {
		t.Errorf("results cap: %d truncated=%v", len(res.Matches), res.Truncated)
	}
	lim = DefaultLimits()
	lim.MaxVisited = 20
	res, _ = NewReader(f.guard, lim).Find(ctx, f.path("public/flat"), "*.log")
	if len(res.Matches) != 20 || !res.Truncated {
		t.Errorf("visit cap: %d truncated=%v", len(res.Matches), res.Truncated)
	}
	lim = DefaultLimits()
	lim.Budget = 1
	res, _ = NewReader(f.guard, lim).Find(ctx, f.path("public/flat"), "*.log")
	if !res.Truncated || len(res.Matches) == 50 {
		t.Errorf("time budget: %d truncated=%v", len(res.Matches), res.Truncated)
	}
}

func TestGrep(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.write("public/g/app.conf", "listen 80\nserver_name example.com\napi_token=tok-ABCDEF-123\nnote: tok- is a prefix\n")
	f.write("public/g/sub/other.conf", "server_name other.example\n")
	f.write("public/g/key.txt", "intro\n"+pemKey+"\noutro\n")
	f.write("public/g/bin.dat", "server_name\x00binary")
	f.write("public/g/.env", "server_name=leak\n")
	f.write("private/x.conf", "server_name hidden\n")
	f.symlink(f.blocked, "public/g/priv")
	f.symlink(f.path("private/x.conf"), "public/g/x.conf")

	res, err := f.reader.Grep(ctx, f.path("public/g"), "server_name")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range res.Matches {
		rel, _ := filepath.Rel(f.path("public/g"), m.Path)
		got = append(got, fmt.Sprintf("%s:%d:%s", rel, m.Line, m.Text))
	}
	if strings.Join(got, "|") != "app.conf:2:server_name example.com|sub/other.conf:1:server_name other.example" || res.Truncated {
		t.Errorf("got %v truncated=%v", got, res.Truncated)
	}

	// The value of a hidden secret cannot be found.
	for _, probe := range []string{"ABCDEF", "tok-ABC", "=tok", "b3BlbnNz", "QyNTUxOQ", "PRIVATE KEY"} {
		if res, _ := f.reader.Grep(ctx, f.path("public/g"), probe); len(res.Matches) != 0 {
			t.Errorf("grep %q found %+v", probe, res.Matches)
		}
	}
	// A search for visible text on a line with a secret shows the line redacted.
	res, _ = f.reader.Grep(ctx, f.path("public/g/app.conf"), "api_token")
	if len(res.Matches) != 1 || res.Matches[0].Text != "api_token=[hidden]" || res.Matches[0].Path != f.path("public/g/app.conf") {
		t.Errorf("redacted line: %+v", res.Matches)
	}
	res, _ = f.reader.Grep(ctx, f.path("public/g"), "tok-")
	if len(res.Matches) != 1 || res.Matches[0].Line != 4 {
		t.Errorf("visible text next to a secret: %+v", res.Matches)
	}
	res, _ = f.reader.Grep(ctx, f.path("public/g"), "outro")
	if len(res.Matches) != 1 || res.Matches[0].Line != 6 {
		t.Errorf("line after the key: %+v", res.Matches)
	}

	if _, err := f.reader.Grep(ctx, f.path("public/g/bin.dat"), "x"); !errors.Is(err, ErrBinary) {
		t.Errorf("single binary file: %v", err)
	}
	if _, err := f.reader.Grep(ctx, f.path("public/g/x.conf"), "server_name"); !errors.Is(err, ErrBlocked) {
		t.Errorf("single blocked file through symlink: %v", err)
	}
	for _, bad := range []string{"", strings.Repeat("a", 201), "a\nb", "\x00", "\xff"} {
		if _, err := f.reader.Grep(ctx, f.path("public/g"), bad); !errors.Is(err, ErrBadRequest) {
			t.Errorf("text %q: %v", bad, err)
		}
	}
}

func TestGrepBoundsAndSkips(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	lim := DefaultLimits()

	// Redaction happens before the cut: a secret straddling the cut is hidden.
	long := strings.Repeat("a", 280) + " needle password=" + strings.Repeat("S", 100)
	f.write("public/h/long.txt", long+"\n")
	res, _ := f.reader.Grep(ctx, f.path("public/h"), "needle")
	if len(res.Matches) != 1 || len(res.Matches[0].Text) > lim.GrepLineBytes || strings.Contains(res.Matches[0].Text, "SSS") {
		t.Errorf("cut: %+v", res.Matches)
	}

	small := DefaultLimits()
	small.GrepFileBytes = 1000
	f.write("public/h/big.txt", strings.Repeat("x\n", 600)+"needle\n")
	res, _ = NewReader(f.guard, small).Grep(ctx, f.path("public/h"), "needle")
	if len(res.Matches) != 1 || !strings.HasSuffix(res.Matches[0].Path, "long.txt") {
		t.Errorf("large file not skipped: %+v", res.Matches)
	}
	res, _ = NewReader(f.guard, small).Grep(ctx, f.path("public/h/big.txt"), "x")
	if !res.Truncated || len(res.Matches) == 0 {
		t.Errorf("single large file: %d truncated=%v", len(res.Matches), res.Truncated)
	}

	var many strings.Builder
	for range 300 {
		many.WriteString("hit\n")
	}
	f.write("public/i/many.txt", many.String())
	res, _ = f.reader.Grep(ctx, f.path("public/i"), "hit")
	if len(res.Matches) != lim.MaxResults || !res.Truncated {
		t.Errorf("results cap: %d truncated=%v", len(res.Matches), res.Truncated)
	}
	budget := DefaultLimits()
	budget.Budget = 1
	res, _ = NewReader(f.guard, budget).Grep(ctx, f.path("public/i"), "hit")
	if !res.Truncated {
		t.Errorf("time budget: %d truncated=%v", len(res.Matches), res.Truncated)
	}
}
