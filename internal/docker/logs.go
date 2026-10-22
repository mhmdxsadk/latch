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
	maxLines     = 1000
	maxLogBytes  = 262144
	maxFrame     = 1 << 20 // largest accepted multiplexed frame
	maxLogSource = 8 << 20 // most bytes ever read from a logs body
)

// csi matches ANSI CSI escape sequences.
var csi = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

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
	path, err := containerPath(container, "logs")
	if err != nil {
		return Logs{}, err
	}
	resp, cancel, err := e.get(ctx, path, fmt.Sprintf("stdout=1&stderr=1&timestamps=1&tail=%d", lines))
	if err != nil {
		return Logs{}, err
	}
	defer cancel()
	defer resp.Body.Close()

	src := &io.LimitedReader{R: resp.Body, N: maxLogSource}
	var c collector
	if in.Config.Tty {
		data, err := io.ReadAll(src)
		if err != nil && src.N > 0 {
			return Logs{}, errors.New("docker: reading logs failed")
		}
		c.add("tty", data)
	} else if err := demux(src, &c); err != nil {
		return Logs{}, err
	}
	c.flush()
	out := bound(c.entries, lines, maxBytes)
	out.Truncated = out.Truncated || src.N == 0
	return out, nil
}

// demux reads Docker's multiplexed stream (8-byte header per frame) into c.
// Hitting the read cap ends the stream quietly; the caller marks truncation.
func demux(src *io.LimitedReader, c *collector) error {
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(src, hdr[:]); err != nil {
			return readEnd(src, err)
		}
		size := binary.BigEndian.Uint32(hdr[4:])
		if size > maxFrame {
			return errors.New("docker: oversized log frame")
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(src, buf); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF // EOF inside a frame is never clean
			}
			return readEnd(src, err)
		}
		switch hdr[0] {
		case 0: // stdin: ignore
		case 1:
			c.add("stdout", buf)
		case 2:
			c.add("stderr", buf)
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

// collector splits per-stream bytes into sanitized entries; a line may span frames.
type collector struct {
	pending map[string][]byte
	entries []LogEntry
}

func (c *collector) add(stream string, data []byte) {
	if c.pending == nil {
		c.pending = map[string][]byte{}
	}
	buf := append(c.pending[stream], data...)
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		c.entries = append(c.entries, parseLine(stream, buf[:i]))
		buf = buf[i+1:]
	}
	c.pending[stream] = append([]byte(nil), buf...)
}

// flush emits any unterminated trailing lines.
func (c *collector) flush() {
	for _, s := range []string{"tty", "stdout", "stderr"} {
		if b := c.pending[s]; len(b) > 0 {
			c.entries = append(c.entries, parseLine(s, b))
		}
		delete(c.pending, s)
	}
}

// parseLine splits "<RFC3339Nano> <text>" and sanitizes the text.
func parseLine(stream string, line []byte) LogEntry {
	s := strings.TrimSuffix(string(line), "\r")
	e := LogEntry{Stream: stream}
	if ts, rest, ok := strings.Cut(s, " "); ok {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			e.Time = t.UTC().Format(time.RFC3339Nano)
			s = rest
		}
	}
	e.Text = sanitize(s)
	return e
}

// sanitize makes s valid UTF-8 and removes escape sequences and control
// characters other than tab.
func sanitize(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = csi.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		if r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// bound keeps the newest entries within lines and maxBytes of text.
func bound(all []LogEntry, lines, maxBytes int) Logs {
	out := Logs{Entries: []LogEntry{}}
	if len(all) > lines {
		all = all[len(all)-lines:]
		out.Truncated = true
	}
	total := 0
	var kept []LogEntry
	for i := len(all) - 1; i >= 0; i-- {
		e := all[i]
		if total+len(e.Text) > maxBytes {
			out.Truncated = true
			if len(kept) == 0 {
				e.Text = tailBytes(e.Text, maxBytes)
				kept = append(kept, e)
			}
			break
		}
		total += len(e.Text)
		kept = append(kept, e)
	}
	for i := len(kept) - 1; i >= 0; i-- {
		out.Entries = append(out.Entries, kept[i])
	}
	return out
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
