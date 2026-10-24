package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxLines    = 1000
	maxLogBytes = 262144
	maxFrame    = 1 << 20 // largest accepted multiplexed frame
	// maxLogSource bounds the bytes read from a logs body. Memory does not
	// grow with it (the collector keeps only the newest lines within
	// maxBytes); it only bounds work when lines are pathologically long.
	maxLogSource = 64 << 20
)

// escape matches terminal escape sequences: CSI, terminated string
// sequences (OSC, DCS, SOS, PM, APC) and two-character escapes. An
// unterminated string sequence loses only its introducer, so it cannot hide
// the rest of the line.
var escape = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|[\]PX^_][^\x07\x1b]*(?:\x07|\x1b\\)|[ -/]*[0-~])`)

// Logs returns the newest log lines of the container, at most lines entries
// and maxBytes bytes of text in total.
func (e *Engine) Logs(ctx context.Context, container string, lines, maxBytes int) (Logs, error) {
	if lines < 1 || lines > maxLines || maxBytes < 1 || maxBytes > maxLogBytes {
		return Logs{}, errors.New("docker: lines or max_bytes out of range")
	}
	in, err := e.inspect(ctx, container)
	if err != nil {
		return Logs{}, err
	}
	// Fetch by the verified full ID so a container recreated under the same
	// name between the two requests cannot answer.
	resp, cancel, err := e.get(ctx, "/containers/"+in.ID+"/logs",
		fmt.Sprintf("stdout=1&stderr=1&timestamps=1&tail=%d", lines))
	if err != nil {
		return Logs{}, err
	}
	defer cancel()
	defer resp.Body.Close()

	src := &io.LimitedReader{R: resp.Body, N: maxLogSource}
	c := newCollector(lines, maxBytes)
	if in.Config.Tty {
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			c.add("tty", buf[:n], false)
			if err != nil {
				if err := readEnd(src, err); err != nil {
					return Logs{}, err
				}
				break
			}
		}
	} else if err := demux(src, c); err != nil {
		return Logs{}, err
	}
	if src.N == 0 {
		c.truncated = true
	}
	return c.result(), nil
}

// demux reads Docker's multiplexed stream (8-byte header per frame) into c.
// Hitting the read cap ends the stream quietly; the caller marks truncation.
func demux(src *io.LimitedReader, c *collector) error {
	var hdr [8]byte
	buf := make([]byte, 0, 32<<10)
	for {
		if _, err := io.ReadFull(src, hdr[:]); err != nil {
			return readEnd(src, err)
		}
		size := binary.BigEndian.Uint32(hdr[4:])
		if size > maxFrame {
			return errors.New("docker: oversized log frame")
		}
		if cap(buf) < int(size) {
			buf = make([]byte, size)
		}
		buf = buf[:size]
		if _, err := io.ReadFull(src, buf); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF // EOF inside a frame is never clean
			}
			return readEnd(src, err)
		}
		switch hdr[0] {
		case 0: // stdin: ignore
		case 1:
			c.add("stdout", buf, true)
		case 2:
			c.add("stderr", buf, true)
		default:
			return errors.New("docker: unknown log stream type")
		}
	}
}

// readEnd classifies a read error: a clean EOF between frames or hitting the
// read cap is the normal end; anything else is a failure.
func readEnd(src *io.LimitedReader, err error) error {
	if errors.Is(err, io.EOF) || src.N == 0 {
		return nil
	}
	return errors.New("docker: reading logs failed")
}

// partial is an unterminated line of one stream.
type partial struct {
	time string
	buf  []byte // raw text after the timestamp; at most ~2*maxBytes
	open bool   // a line has started
}

// collector turns stream bytes into sanitized entries, keeping only the
// newest entries that fit within lines and maxBytes. Memory is bounded by
// those limits regardless of how much the daemon sends.
type collector struct {
	lines, maxBytes int
	pending         map[string]*partial
	entries         []LogEntry
	total           int
	truncated       bool
}

func newCollector(lines, maxBytes int) *collector {
	return &collector{lines: lines, maxBytes: maxBytes, pending: map[string]*partial{}}
}

// add consumes one chunk of a stream. framed is true for multiplexed
// streams, where Docker splits long lines into frames that each repeat the
// timestamp; the repeat is dropped when continuing a line.
func (c *collector) add(stream string, data []byte, framed bool) {
	p := c.pending[stream]
	if p == nil {
		p = &partial{}
		c.pending[stream] = p
	}
	if framed && p.open {
		if _, rest, ok := cutTimestamp(data); ok {
			data = rest
		}
	}
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		seg := data
		if i >= 0 {
			seg = data[:i]
		}
		if !p.open {
			p.open = true
			if ts, rest, ok := cutTimestamp(seg); ok {
				p.time, seg = ts, rest
			}
		}
		c.appendBounded(p, seg)
		if i < 0 {
			return
		}
		c.emit(stream, p)
		data = data[i+1:]
	}
}

// appendBounded appends to the pending line, keeping only its last maxBytes
// (compacting occasionally so the cost stays linear).
func (c *collector) appendBounded(p *partial, seg []byte) {
	p.buf = append(p.buf, seg...)
	if len(p.buf) > 2*c.maxBytes {
		p.buf = append(p.buf[:0:0], p.buf[len(p.buf)-c.maxBytes:]...)
		c.truncated = true
	}
}

func (c *collector) emit(stream string, p *partial) {
	text := strings.TrimSuffix(string(p.buf), "\r")
	if len(text) > c.maxBytes {
		text = tailBytes(text, c.maxBytes)
		c.truncated = true
	}
	text = sanitize(text)
	if len(text) > c.maxBytes { // replacement characters can grow the text
		text = tailBytes(text, c.maxBytes)
	}
	c.entries = append(c.entries, LogEntry{Stream: stream, Time: p.time, Text: text})
	c.total += len(text)
	*p = partial{buf: p.buf[:0]}
	for len(c.entries) > 1 && (len(c.entries) > c.lines || c.total > c.maxBytes) {
		c.total -= len(c.entries[0].Text)
		c.entries = c.entries[1:]
		c.truncated = true
	}
}

// result flushes unterminated lines and returns the bounded entries.
func (c *collector) result() Logs {
	for _, s := range []string{"tty", "stdout", "stderr"} {
		if p := c.pending[s]; p != nil && p.open {
			c.emit(s, p)
		}
	}
	return Logs{Entries: append([]LogEntry{}, c.entries...), Truncated: c.truncated}
}

// cutTimestamp splits a leading "<RFC3339Nano> " and returns it normalized.
func cutTimestamp(b []byte) (string, []byte, bool) {
	i := bytes.IndexByte(b, ' ')
	if i < 0 || i > 40 {
		return "", b, false
	}
	t, err := time.Parse(time.RFC3339Nano, string(b[:i]))
	if err != nil {
		return "", b, false
	}
	return t.UTC().Format(time.RFC3339Nano), b[i+1:], true
}

// sanitize makes s valid UTF-8 and removes escape sequences, control
// characters other than tab, and invisible characters (bidi overrides,
// zero-width and tag characters, variation selectors, fillers) that could
// disguise the text or smuggle hidden data. Line and paragraph separators
// become spaces so one entry cannot pose as several.
func sanitize(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = escape.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return r
		case unicode.In(r, unicode.Zl, unicode.Zp):
			return ' '
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Variation_Selector, unicode.Other_Default_Ignorable_Code_Point):
			return -1
		}
		return r
	}, s)
}

// headBytes returns the first n bytes of s, ending on a UTF-8 boundary.
func headBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// s is valid UTF-8, so at most a few bytes of a cut rune need dropping.
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// tailBytes returns the last n bytes of s, starting on a UTF-8 boundary.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}
