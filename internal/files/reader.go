package files

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mhmdxsadk/latch/internal/redact"
	"github.com/mhmdxsadk/latch/internal/textsafe"
)

// Limits bound the work and output of one call.
type Limits struct {
	ReadLines     int           // default lines for Read
	MaxReadLines  int           // maximum lines for Read
	ReadBytes     int           // maximum bytes of content Read returns
	SkipBytes     int64         // maximum bytes Read scans to reach the start line
	SniffBytes    int           // a NUL in the first SniffBytes marks a file binary
	ListEntries   int           // maximum entries List returns
	MaxDepth      int           // directory depth Find and Grep descend
	MaxResults    int           // maximum results of Find and Grep
	MaxVisited    int           // maximum directory entries one call looks at
	Budget        time.Duration // time budget of Find and Grep
	GrepFileBytes int64         // larger files are skipped by Grep
	GrepLineBytes int           // matching lines are cut to this many bytes
	MaxGrepText   int           // longest search text
}

// DefaultLimits are the production limits.
func DefaultLimits() Limits {
	return Limits{
		ReadLines:     200,
		MaxReadLines:  2000,
		ReadBytes:     256 << 10,
		SkipBytes:     64 << 20,
		SniffBytes:    8 << 10,
		ListEntries:   500,
		MaxDepth:      8,
		MaxResults:    200,
		MaxVisited:    20000,
		Budget:        5 * time.Second,
		GrepFileBytes: 4 << 20,
		GrepLineBytes: 300,
		MaxGrepText:   200,
	}
}

// Service is what the MCP tools need; Reader implements it.
type Service interface {
	Read(ctx context.Context, path string, offset, lines int) (ReadResult, error)
	List(ctx context.Context, path string) (ListResult, error)
	Find(ctx context.Context, path, name string) (FindResult, error)
	Grep(ctx context.Context, path, text string) (GrepResult, error)
}

// Reader implements the read-only file tools on top of a Guard.
type Reader struct {
	guard *Guard
	lim   Limits
}

// NewReader returns a Reader enforcing g and lim.
func NewReader(g *Guard, lim Limits) *Reader { return &Reader{guard: g, lim: lim} }

// ReadResult is a window of a text file.
type ReadResult struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	StartLine int    `json:"start_line"`
	Lines     int    `json:"lines"`
	SizeBytes int64  `json:"size_bytes"`
	Truncated bool   `json:"truncated"`
	Modified  string `json:"modified"`
}

// Read returns up to lines lines of the text file at p, starting at line
// offset (1-based). Zero means the default for either.
func (r *Reader) Read(ctx context.Context, p string, offset, lines int) (ReadResult, error) {
	if err := ValidatePath(p); err != nil {
		return ReadResult{}, err
	}
	if offset == 0 {
		offset = 1
	}
	if lines == 0 {
		lines = r.lim.ReadLines
	}
	if offset < 1 || lines < 1 || lines > r.lim.MaxReadLines {
		return ReadResult{}, ErrBadRequest
	}
	c, err := r.guard.session(ctx)
	if err != nil {
		return ReadResult{}, err
	}
	real, _, err := c.resolve(p)
	if err != nil {
		return ReadResult{}, err
	}
	f, st, err := c.open(real, kindFile)
	if err != nil {
		return ReadResult{}, err
	}
	defer f.Close()
	if bin, err := isBinary(f, r.lim.SniffBytes); err != nil {
		return ReadResult{}, mapErr(err)
	} else if bin {
		return ReadResult{}, ErrBinary
	}

	out := ReadResult{Path: p, StartLine: offset, SizeBytes: st.Size(), Modified: modTime(st)}
	br := bufio.NewReaderSize(f, 64<<10)
	var pem pemState
	var skipped int64
	for n := 1; n < offset; n++ {
		if n%1024 == 0 && ctx.Err() != nil {
			return ReadResult{}, ctx.Err()
		}
		line, used, _, err := readLine(br, 64<<10)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return ReadResult{}, mapErr(err)
		}
		pem.feed(line)
		if skipped += used; skipped > r.lim.SkipBytes {
			return ReadResult{}, ErrBadRequest
		}
	}

	// A window that starts inside a private key block gets a synthetic BEGIN
	// line, so redaction hides the key body instead of showing base64.
	var text strings.Builder
	if pem.in {
		text.WriteString("-----BEGIN PRIVATE KEY-----\n")
	}
	budget := r.lim.ReadBytes
	for out.Lines < lines && budget > 0 {
		line, _, cut, err := readLine(br, budget)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ReadResult{}, mapErr(err)
		}
		if out.Lines > 0 {
			text.WriteByte('\n')
		}
		text.Write(line)
		out.Lines++
		budget -= len(line) + 1
		if cut {
			out.Truncated = true
		}
	}
	if !out.Truncated {
		if _, err := br.Peek(1); err == nil {
			out.Truncated = true
		}
	}
	out.Content = redact.Text(textsafe.Lines(text.String()))
	return out, nil
}

// isBinary reports whether the first n bytes of f contain a NUL.
func isBinary(f *os.File, n int) (bool, error) {
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return bytes.IndexByte(buf[:got], 0) >= 0, nil
}

// readLine reads one line without its "\n", keeping at most keep bytes. It
// returns the kept bytes, the bytes consumed, whether some were dropped, and
// io.EOF when the input ended before a new line began.
func readLine(br *bufio.Reader, keep int) (line []byte, used int64, cut bool, err error) {
	started := false
	for {
		chunk, err := br.ReadSlice('\n')
		used += int64(len(chunk))
		if len(chunk) > 0 {
			started = true
		}
		data := chunk
		if err == nil {
			data = data[:len(data)-1]
		}
		if room := keep - len(line); room >= len(data) {
			line = append(line, data...)
		} else {
			line = append(line, data[:max(room, 0)]...)
			cut = true
		}
		switch {
		case err == nil:
			return line, used, cut, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if !started {
				return nil, used, false, io.EOF
			}
			return line, used, cut, nil
		default:
			return nil, used, false, err
		}
	}
}

var (
	pemBegin = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	pemEnd   = regexp.MustCompile(`-----END [A-Z ]*PRIVATE KEY-----`)
)

// pemState follows private key blocks line by line, so text taken from the
// middle of a file (a window, a grep match) is not shown when it belongs to
// a key whose BEGIN line is out of view.
type pemState struct{ in bool }

// feed advances the state over one line and reports whether any of the line
// belongs to a private key block.
func (s *pemState) feed(line []byte) bool {
	was := s.in
	if !bytes.Contains(line, []byte("PRIVATE KEY-----")) {
		return was
	}
	b, e := lastIndex(pemBegin, line), lastIndex(pemEnd, line)
	switch {
	case b > e:
		s.in = true
	case e >= 0:
		s.in = false
	}
	return true
}

func lastIndex(re *regexp.Regexp, b []byte) int {
	all := re.FindAllIndex(b, -1)
	if len(all) == 0 {
		return -1
	}
	return all[len(all)-1][0]
}

func modTime(fi fs.FileInfo) string { return fi.ModTime().UTC().Format(time.RFC3339) }

// Entry is one directory entry.
type Entry struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // file, dir, symlink or other
	Size     int64  `json:"size"`
	Mode     string `json:"mode"` // octal permission bits, e.g. "0644"
	Modified string `json:"modified"`
	// Target is a symlink's target as written, not followed.
	Target string `json:"target,omitempty"`
}

// ListResult is the content of a directory.
type ListResult struct {
	Path      string  `json:"path"`
	Entries   []Entry `json:"entries"`
	Truncated bool    `json:"truncated"`
}

// List returns the entries of the directory at p, sorted by name. Blocked
// entries are left out, as if they did not exist.
func (r *Reader) List(ctx context.Context, p string) (ListResult, error) {
	if err := ValidatePath(p); err != nil {
		return ListResult{}, err
	}
	c, err := r.guard.session(ctx)
	if err != nil {
		return ListResult{}, err
	}
	real, _, err := c.resolve(p)
	if err != nil {
		return ListResult{}, err
	}
	ents, truncated, err := c.readDir(real, r.lim.MaxVisited)
	if err != nil {
		return ListResult{}, err
	}
	out := ListResult{Path: p, Entries: []Entry{}, Truncated: truncated}
	for _, e := range ents {
		if len(out.Entries) == r.lim.ListEntries {
			out.Truncated = true
			break
		}
		fi, err := e.Info() // lstat: never follows a symlink
		if err != nil {
			continue // removed since it was listed
		}
		ent := Entry{
			Name:     textsafe.Line(e.Name()),
			Type:     typeName(fi.Mode()),
			Size:     fi.Size(),
			Mode:     modeString(fi.Mode()),
			Modified: modTime(fi),
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			if t, err := os.Readlink(filepath.Join(real, e.Name())); err == nil {
				ent.Target = redact.Text(textsafe.Line(t))
			}
		}
		out.Entries = append(out.Entries, ent)
	}
	return out, nil
}

// readDir opens the resolved directory dir and returns up to limit of its
// entries that are not blocked, sorted by name. truncated reports that the
// directory had more entries than limit.
func (c *check) readDir(dir string, limit int) ([]fs.DirEntry, bool, error) {
	f, _, err := c.open(dir, kindDir)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	var ents []fs.DirEntry
	truncated := false
	for {
		batch, err := f.ReadDir(256)
		ents = append(ents, batch...)
		if len(ents) >= limit {
			truncated = len(ents) > limit
			if !truncated {
				more, _ := f.ReadDir(1)
				truncated = len(more) > 0
			}
			ents = ents[:limit]
			break
		}
		if errors.Is(err, io.EOF) || len(batch) == 0 {
			break
		}
		if err != nil {
			return nil, false, mapErr(err)
		}
	}
	ents = slices.DeleteFunc(ents, func(e fs.DirEntry) bool { return c.blocked(filepath.Join(dir, e.Name())) })
	slices.SortFunc(ents, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return ents, truncated, nil
}

func typeName(m fs.FileMode) string {
	switch {
	case m.IsRegular():
		return "file"
	case m.IsDir():
		return "dir"
	case m&fs.ModeSymlink != 0:
		return "symlink"
	default:
		return "other"
	}
}

func modeString(m fs.FileMode) string {
	v := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		v |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		v |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		v |= 0o1000
	}
	return fmt.Sprintf("%04o", v)
}
