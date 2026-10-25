package files

import (
	"bytes"
	"errors"
	"strings"

	"github.com/mhmdxsadk/latch/internal/redact"
	"github.com/mhmdxsadk/latch/internal/textsafe"
)

// Edit limits.
const (
	// MaxEditBytes is the largest file Latch edits, creates or restores.
	MaxEditBytes = 1 << 20
	// MaxEditText bounds the old and new text of one edit.
	MaxEditText = 64 << 10
)

// Errors of the edit planner and the writer. The mcp package maps each to a
// fixed message.
var (
	ErrTooLarge        = errors.New("file too large to edit")
	ErrNoMatch         = errors.New("old text not found")
	ErrManyMatches     = errors.New("old text found more than once")
	ErrMultiLineHidden = errors.New("file has hidden values spanning lines")
	ErrNewHidden       = errors.New("new text contains [hidden]")
	ErrNoChange        = errors.New("edit changes nothing")
	ErrBadText         = errors.New("text is not plain text")
	ErrChanged         = errors.New("file changed since the request")
	ErrExists          = errors.New("file exists")
	ErrSpecialBits     = errors.New("file has setuid, setgid or sticky bits")
)

// View is what the agent sees of a file's content: the same text file_read
// returns, as one string (lines joined by "\n", no final newline).
func View(content []byte) string {
	body, _ := cutFinalNewline(content)
	return redact.Text(textsafe.Lines(string(body)))
}

// ViewLines is View split into lines.
func ViewLines(content []byte) []string { return strings.Split(View(content), "\n") }

func cutFinalNewline(b []byte) ([]byte, bool) {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		return b[:n-1], true
	}
	return b, false
}

// PlainText reports whether s is text Latch may write: valid UTF-8 with no
// control characters other than tab and newline, and no invisible
// characters, so what the owner reviews is exactly what is written. With
// allowCR a "\r" before a newline (or at the end) is accepted too.
func PlainText(s string, allowCR bool) bool {
	for _, l := range strings.Split(s, "\n") {
		if allowCR {
			l = strings.TrimSuffix(l, "\r")
		}
		if textsafe.Line(l) != l {
			return false
		}
	}
	return true
}

// EditPlan is the outcome of planning an edit on the agent's view.
type EditPlan struct {
	// Content is the new real content of the file. It holds the file's
	// real secret values: never show or log it.
	Content []byte
	// Before and After are the agent's view of the file before and after
	// the edit (redacted text only).
	Before, After []string
	// Kept counts the lines in the edited part whose hidden values were
	// restored from the real file.
	Kept int
}

// PlanEdit replaces the one occurrence of old in the agent's view of real
// with new and maps the result back onto the real file: lines outside the
// edited part are copied byte for byte, and an edited line identical to a
// replaced line that showed [hidden] gets that line's real text back. Line
// endings (LF or CRLF) and the presence of a final newline are kept.
func PlanEdit(real []byte, old, new string) (EditPlan, error) {
	switch {
	case len(real) > MaxEditBytes:
		return EditPlan{}, ErrTooLarge
	case bytes.IndexByte(real, 0) >= 0:
		return EditPlan{}, ErrBinary
	case old == "" || len(old) > MaxEditText || len(new) > MaxEditText:
		return EditPlan{}, ErrBadRequest
	case !PlainText(new, false) || !PlainText(old, false):
		return EditPlan{}, ErrBadText
	}
	body, finalNL := cutFinalNewline(real)
	rawLines := strings.Split(string(body), "\n")
	realLines := make([]string, len(rawLines)) // without a trailing "\r"
	crlf := len(rawLines) > 1 || finalNL
	for i, l := range rawLines {
		realLines[i] = strings.TrimSuffix(l, "\r")
		terminated := i < len(rawLines)-1 || finalNL
		if terminated && realLines[i] == l {
			crlf = false
		}
	}
	view := ViewLines(real)
	if len(view) != len(realLines) {
		return EditPlan{}, ErrMultiLineHidden
	}

	text := strings.Join(view, "\n")
	if finalNL {
		text += "\n"
	}
	s := strings.Index(text, old)
	if s < 0 {
		return EditPlan{}, ErrNoMatch
	}
	if strings.Contains(text[s+1:], old) {
		return EditPlan{}, ErrManyMatches
	}
	e := s + len(old)

	// starts[i] is the offset of line i in text.
	starts := make([]int, len(view))
	off := 0
	for i, l := range view {
		starts[i] = off
		off += len(l) + 1
	}
	lineAt := func(pos int) int {
		i := len(starts) - 1
		for i > 0 && starts[i] > pos {
			i--
		}
		return i
	}
	a, b := lineAt(s), lineAt(e-1)
	withFinal := false
	if e-1 == starts[b]+len(view[b]) {
		// The match ends with line b's newline: the next line joins the
		// edited part, or the final newline does.
		if b+1 < len(view) {
			b++
		} else {
			withFinal = true
		}
	}
	seg := strings.Join(view[a:b+1], "\n")
	if withFinal {
		seg += "\n"
	}
	seg = seg[:s-starts[a]] + new + seg[e-starts[a]:]
	newFinalNL := finalNL
	if withFinal {
		newFinalNL = strings.HasSuffix(seg, "\n")
		seg = strings.TrimSuffix(seg, "\n")
	}
	edited := strings.Split(seg, "\n")

	// Replaced lines whose view differs from the real text (hidden values,
	// removed control characters) can be restored; others are kept as is.
	hidden := map[string][]int{}
	plain := map[string]bool{}
	for i := a; i <= b; i++ {
		if view[i] != realLines[i] {
			hidden[view[i]] = append(hidden[view[i]], i)
		} else {
			plain[view[i]] = true
		}
	}
	used := map[string]int{}
	out := make([]string, len(edited))
	kept := 0
	for i, l := range edited {
		if idx, ok := hidden[l]; ok {
			n := min(used[l], len(idx)-1)
			used[l]++
			out[i] = realLines[idx[n]]
			kept++
			continue
		}
		if strings.Contains(l, redact.Hidden) && !plain[l] {
			return EditPlan{}, ErrNewHidden
		}
		out[i] = l
	}

	var buf bytes.Buffer
	total := a + len(out) + len(rawLines) - b - 1
	n := 0
	put := func(line string, raw bool) {
		n++
		terminated := n < total || newFinalNL
		if raw {
			// rawLines keep their own "\r".
			buf.WriteString(line)
			if terminated {
				buf.WriteByte('\n')
			}
			return
		}
		buf.WriteString(line)
		if terminated {
			if crlf {
				buf.WriteByte('\r')
			}
			buf.WriteByte('\n')
		}
	}
	for _, l := range rawLines[:a] {
		put(l, true)
	}
	for _, l := range out {
		put(l, false)
	}
	for _, l := range rawLines[b+1:] {
		put(l, true)
	}
	content := buf.Bytes()
	if len(content) > MaxEditBytes {
		return EditPlan{}, ErrTooLarge
	}
	if bytes.Equal(content, real) {
		return EditPlan{}, ErrNoChange
	}
	after := make([]string, 0, total)
	after = append(after, view[:a]...)
	after = append(after, edited...)
	after = append(after, view[b+1:]...)
	return EditPlan{Content: content, Before: view, After: after, Kept: kept}, nil
}
