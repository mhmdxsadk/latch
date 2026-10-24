package files

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/mhmdxsadk/latch/internal/redact"
	"github.com/mhmdxsadk/latch/internal/textsafe"
)

// walker visits a directory tree within the call's budgets. It never follows
// symlinks: directory entries are typed from the directory itself, and every
// directory is opened with openNoFollow after a block check.
type walker struct {
	c         *check
	lim       Limits
	ctx       context.Context
	visited   int
	truncated bool
}

// walk calls fn for every entry under the resolved directory dir that is not
// blocked, depth first in name order. fn returns false to stop the walk.
// display is the path shown for dir (the path the agent asked for).
func (w *walker) walk(dir, display string, depth int, fn func(real, display string, e fs.DirEntry) bool) bool {
	ents, truncated, err := w.c.readDir(dir, w.lim.MaxVisited-w.visited)
	if err != nil {
		return true // unreadable or vanished: skip it
	}
	if truncated {
		w.truncated = true
	}
	for _, e := range ents {
		if w.visited >= w.lim.MaxVisited || w.ctx.Err() != nil {
			w.truncated = true
			return false
		}
		w.visited++
		real, shown := filepath.Join(dir, e.Name()), path.Join(display, e.Name())
		if !fn(real, shown, e) {
			return false
		}
		if e.IsDir() { // from the directory entry: a symlink is never a dir here
			if depth+1 >= w.lim.MaxDepth {
				w.truncated = true
				continue
			}
			if !w.walk(real, shown, depth+1, fn) {
				return false
			}
		}
	}
	return true
}

// FindResult lists matching paths.
type FindResult struct {
	Path      string      `json:"path"`
	Matches   []FoundPath `json:"matches"`
	Truncated bool        `json:"truncated"`
}

// FoundPath is one Find match.
type FoundPath struct {
	Path string `json:"path"`
	Type string `json:"type"` // file, dir, symlink or other
}

// ValidNamePattern reports whether pat is a usable Find pattern: 1 to 255
// bytes of valid UTF-8 with no slash, backslash, brackets or control
// characters. Only the * and ? wildcards are special.
func ValidNamePattern(pat string) bool {
	if pat == "" || len(pat) > 255 || !utf8.ValidString(pat) || strings.ContainsAny(pat, "/\\[]") {
		return false
	}
	for i := 0; i < len(pat); i++ {
		if pat[i] < 0x20 || pat[i] == 0x7f {
			return false
		}
	}
	return true
}

// Find returns the paths under the directory p whose name matches the
// pattern name (* and ? wildcards). Paths are shown under p as requested,
// even when p is reached through a symlink.
func (r *Reader) Find(ctx context.Context, p, name string) (FindResult, error) {
	if err := ValidatePath(p); err != nil {
		return FindResult{}, err
	}
	if !ValidNamePattern(name) {
		return FindResult{}, ErrBadRequest
	}
	c, err := r.guard.session(ctx)
	if err != nil {
		return FindResult{}, err
	}
	real, info, err := c.resolve(p)
	if err != nil {
		return FindResult{}, err
	}
	if !info.IsDir() {
		return FindResult{}, ErrNotDir
	}
	ctx, cancel := context.WithTimeout(ctx, r.lim.Budget)
	defer cancel()
	w := &walker{c: c, lim: r.lim, ctx: ctx}
	out := FindResult{Path: p, Matches: []FoundPath{}}
	w.walk(real, p, 0, func(_, shown string, e fs.DirEntry) bool {
		if ok, _ := path.Match(name, e.Name()); !ok {
			return true
		}
		if len(out.Matches) == r.lim.MaxResults {
			out.Truncated = true
			return false
		}
		out.Matches = append(out.Matches, FoundPath{Path: textsafe.Line(shown), Type: typeName(e.Type())})
		return true
	})
	out.Truncated = out.Truncated || w.truncated
	return out, nil
}

// GrepResult lists matching lines.
type GrepResult struct {
	Path      string  `json:"path"`
	Matches   []Match `json:"matches"`
	Truncated bool    `json:"truncated"`
}

// Match is one matching line.
type Match struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// ValidSearchText reports whether text is a usable Grep search: 1 to max
// bytes of valid UTF-8 with no control characters other than tab.
func ValidSearchText(text string, maxLen int) bool {
	if text == "" || len(text) > maxLen || !utf8.ValidString(text) {
		return false
	}
	for i := 0; i < len(text); i++ {
		if (text[i] < 0x20 && text[i] != '\t') || text[i] == 0x7f {
			return false
		}
	}
	return true
}

// Grep finds lines containing text (a fixed, case-sensitive string) in the
// file p or in the text files under the directory p. Binary files, files over
// GrepFileBytes and blocked files are skipped in a directory; a single large
// file is searched up to GrepFileBytes. Lines inside private key blocks are
// never matched, and a match whose line no longer contains text once
// secret-looking values are hidden is dropped, so a search cannot confirm a
// hidden value.
func (r *Reader) Grep(ctx context.Context, p, text string) (GrepResult, error) {
	if err := ValidatePath(p); err != nil {
		return GrepResult{}, err
	}
	if !ValidSearchText(text, r.lim.MaxGrepText) {
		return GrepResult{}, ErrBadRequest
	}
	c, err := r.guard.session(ctx)
	if err != nil {
		return GrepResult{}, err
	}
	real, info, err := c.resolve(p)
	if err != nil {
		return GrepResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.lim.Budget)
	defer cancel()
	g := &grep{lim: r.lim, ctx: ctx, text: []byte(text), out: GrepResult{Path: p, Matches: []Match{}}}

	switch {
	case info.Mode().IsRegular():
		f, st, err := c.open(real, kindFile)
		if err != nil {
			return GrepResult{}, err
		}
		defer f.Close()
		if st.Size() > r.lim.GrepFileBytes {
			g.out.Truncated = true
		}
		data, err := io.ReadAll(io.LimitReader(f, r.lim.GrepFileBytes))
		if err != nil {
			return GrepResult{}, mapErr(err)
		}
		if bytes.IndexByte(data[:min(len(data), r.lim.SniffBytes)], 0) >= 0 {
			return GrepResult{}, ErrBinary
		}
		g.file(p, data)
	case info.IsDir():
		w := &walker{c: c, lim: r.lim, ctx: ctx}
		w.walk(real, p, 0, func(realPath, shown string, e fs.DirEntry) bool {
			if !e.Type().IsRegular() {
				return true
			}
			f, st, err := c.open(realPath, kindFile)
			if err != nil {
				return true
			}
			defer f.Close()
			if st.Size() > r.lim.GrepFileBytes {
				return true
			}
			data, err := io.ReadAll(io.LimitReader(f, r.lim.GrepFileBytes))
			if err != nil || bytes.IndexByte(data[:min(len(data), r.lim.SniffBytes)], 0) >= 0 {
				return true
			}
			return g.file(shown, data)
		})
		g.out.Truncated = g.out.Truncated || w.truncated
	default:
		return GrepResult{}, ErrNotFile
	}
	if ctx.Err() != nil {
		g.out.Truncated = true
	}
	return g.out, nil
}

type grep struct {
	lim  Limits
	ctx  context.Context
	text []byte
	out  GrepResult
}

// file searches one file's content and reports whether to go on.
func (g *grep) file(shown string, data []byte) bool {
	if !bytes.Contains(data, g.text) {
		return true
	}
	shown = textsafe.Line(shown)
	needle := string(g.text)
	var pem pemState
	for n := 1; len(data) > 0; n++ {
		var line []byte
		line, data, _ = bytes.Cut(data, []byte("\n"))
		if pem.feed(line) || !bytes.Contains(line, g.text) {
			continue
		}
		safe := redact.Text(textsafe.Line(strings.TrimSuffix(string(line), "\r")))
		if !strings.Contains(safe, needle) {
			continue // the match was in a hidden value
		}
		if len(g.out.Matches) == g.lim.MaxResults {
			g.out.Truncated = true
			return false
		}
		g.out.Matches = append(g.out.Matches, Match{Path: shown, Line: n, Text: textsafe.Head(safe, g.lim.GrepLineBytes)})
		if g.ctx.Err() != nil {
			g.out.Truncated = true
			return false
		}
	}
	return true
}
