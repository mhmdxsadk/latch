package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhmdxsadk/latch/internal/excluded"
	"github.com/mhmdxsadk/latch/internal/files"
)

type fakeFolders struct{ err error }

func (f fakeFolders) Get(context.Context) (excluded.Snapshot, error) {
	return excluded.Snapshot{}, f.err
}

// fileHarness serves a temp tree with "private" blocked.
func fileHarness(t *testing.T, folders files.Folders) (*harness, string) {
	t.Helper()
	root := t.TempDir()
	for name, content := range map[string]string{
		"pub/app.conf":       "listen 80\nDB_PASSWORD=hunter2\n",
		"pub/sub/notes.txt":  "notes\n",
		"private/secret.txt": "top secret\n",
		"pub/.env":           "TOKEN=x\n",
	} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rules := files.DefaultRules(filepath.Join(root, "latch.yaml"), nil, []string{filepath.Join(root, "private")})
	h := newHarnessWith(t, func(d *Deps) {
		d.Files = files.NewReader(files.NewGuard(rules, folders), files.DefaultLimits())
	})
	return h, root
}

func TestFileTools(t *testing.T) {
	h, root := fileHarness(t, fakeFolders{})
	conf := filepath.Join(root, "pub/app.conf")

	rd := structured[files.ReadResult](t, h, "file_read", map[string]any{"path": conf})
	if rd.Content != "listen 80\nDB_PASSWORD=[hidden]" || rd.Lines != 2 || rd.StartLine != 1 || rd.Path != conf {
		t.Errorf("file_read: %+v", rd)
	}
	rd = structured[files.ReadResult](t, h, "file_read", map[string]any{"path": conf, "offset": 2, "lines": 1})
	if rd.Content != "DB_PASSWORD=[hidden]" {
		t.Errorf("file_read window: %+v", rd)
	}
	ls := structured[files.ListResult](t, h, "file_list", map[string]any{"path": filepath.Join(root, "pub")})
	if len(ls.Entries) != 2 || ls.Entries[0].Name != "app.conf" || ls.Entries[1].Name != "sub" {
		t.Errorf("file_list: %+v", ls)
	}
	fd := structured[files.FindResult](t, h, "file_find", map[string]any{"path": root, "name": "*.txt"})
	if len(fd.Matches) != 1 || fd.Matches[0].Path != filepath.Join(root, "pub/sub/notes.txt") {
		t.Errorf("file_find: %+v", fd)
	}
	gr := structured[files.GrepResult](t, h, "file_grep", map[string]any{"path": root, "text": "listen"})
	if len(gr.Matches) != 1 || gr.Matches[0].Line != 1 {
		t.Errorf("file_grep: %+v", gr)
	}
	if gr := structured[files.GrepResult](t, h, "file_grep", map[string]any{"path": root, "text": "hunter2"}); len(gr.Matches) != 0 {
		t.Errorf("file_grep found a hidden value: %+v", gr)
	}
	a := h.audit.String()
	if !strings.Contains(a, `"tool":"file_read","resource":"`+conf+`","outcome":"ok"`) {
		t.Errorf("audit: %s", a)
	}
	if strings.Contains(a, "hunter2") || strings.Contains(a, "listen 80") {
		t.Errorf("audit logged file content: %s", a)
	}
}

func TestFileBlockedLooksLikeMissing(t *testing.T) {
	h, root := fileHarness(t, fakeFolders{})
	texts := map[string]string{}
	for name, p := range map[string]string{
		"blocked":      filepath.Join(root, "private/secret.txt"),
		"blocked-name": filepath.Join(root, "pub/.env"),
		"shadow":       "/etc/shadow",
		"missing":      filepath.Join(root, "pub/nope.txt"),
		"missing-dir":  filepath.Join(root, "private/nope.txt"),
	} {
		res, err := h.call("file_read", map[string]any{"path": p})
		if err != nil || !res.IsError {
			t.Fatalf("%s: %v %+v", name, err, res)
		}
		texts[name] = resultText(res)
		if strings.Contains(texts[name], "secret") || strings.Contains(texts[name], root) {
			t.Errorf("%s: message echoes input: %q", name, texts[name])
		}
	}
	for name, text := range texts {
		if text != texts["missing"] {
			t.Errorf("%s says %q, missing says %q", name, text, texts["missing"])
		}
	}
	a := h.audit.String()
	if !strings.Contains(a, `"reason":"not_allowed"`) || !strings.Contains(a, `"reason":"not_found"`) {
		t.Errorf("audit reasons: %s", a)
	}
}

func TestFileToolRejections(t *testing.T) {
	h, root := fileHarness(t, fakeFolders{})
	conf := filepath.Join(root, "pub/app.conf")
	dir := filepath.Join(root, "pub")
	cases := []struct {
		tool string
		args any
	}{
		{"file_read", map[string]any{"path": "etc/passwd"}},
		{"file_read", map[string]any{"path": "/etc/../etc/shadow"}},
		{"file_read", map[string]any{"path": "/etc//shadow"}},
		{"file_read", map[string]any{"path": dir + "/"}},
		{"file_read", map[string]any{"path": conf + "\x00"}},
		{"file_read", map[string]any{"path": "/" + strings.Repeat("a", 5000)}},
		{"file_read", map[string]any{"path": ""}},
		{"file_read", map[string]any{}},
		{"file_read", map[string]any{"path": conf, "lines": 2001}},
		{"file_read", map[string]any{"path": conf, "lines": -1}},
		{"file_read", map[string]any{"path": conf, "offset": -5}},
		{"file_read", map[string]any{"path": conf, "lines": "all"}},
		{"file_read", map[string]any{"path": conf, "follow": true}},
		{"file_read", map[string]any{"path": conf, "max_bytes": 1 << 30}},
		{"file_read", map[string]any{"path": dir}},
		{"file_list", map[string]any{"path": conf}},
		{"file_list", map[string]any{"path": dir, "recursive": true}},
		{"file_list", map[string]any{"path": "/proc"}},
		{"file_find", map[string]any{"path": dir}},
		{"file_find", map[string]any{"path": dir, "name": "a/b"}},
		{"file_find", map[string]any{"path": dir, "name": "[a-z]*"}},
		{"file_find", map[string]any{"path": dir, "name": "*", "depth": 100}},
		{"file_grep", map[string]any{"path": dir}},
		{"file_grep", map[string]any{"path": dir, "text": ""}},
		{"file_grep", map[string]any{"path": dir, "text": strings.Repeat("x", 201)}},
		{"file_grep", map[string]any{"path": dir, "text": "a\nb"}},
		{"file_grep", map[string]any{"path": dir, "text": "x", "regex": true}},
	}
	for _, c := range cases {
		res, err := h.call(c.tool, c.args)
		if err == nil && !res.IsError {
			t.Errorf("%s %v: expected rejection", c.tool, c.args)
		}
	}
	a := h.audit.String()
	if strings.Contains(a, "etc/passwd") || strings.Contains(a, "/etc/../etc") || strings.Contains(a, "aaaaaaaa") {
		t.Errorf("audit echoed an invalid path: %s", a)
	}
	if !strings.Contains(a, `"reason":"invalid_path"`) {
		t.Errorf("audit: %s", a)
	}
}

func TestFileToolsRefuseWhenExcludedUnknown(t *testing.T) {
	h, root := fileHarness(t, fakeFolders{err: excluded.ErrUnknown})
	for _, c := range []struct {
		tool string
		args any
	}{
		{"file_read", map[string]any{"path": filepath.Join(root, "pub/app.conf")}},
		{"file_list", map[string]any{"path": root}},
		{"file_find", map[string]any{"path": root, "name": "*"}},
		{"file_grep", map[string]any{"path": root, "text": "listen"}},
	} {
		res, err := h.call(c.tool, c.args)
		if err != nil || !res.IsError || resultText(res) != errUnavailable.msg {
			t.Errorf("%s: %v %s", c.tool, err, resultText(res))
		}
	}
	if !strings.Contains(h.audit.String(), `"reason":"excluded_unknown"`) {
		t.Errorf("audit: %s", h.audit)
	}
}
