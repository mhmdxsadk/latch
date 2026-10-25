package systemd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/mhmdxsadk/latch/internal/host"
	"github.com/mhmdxsadk/latch/internal/redact"
	"github.com/mhmdxsadk/latch/internal/textsafe"
)

// Reading the journal is the one place Latch runs another program. The
// program is journalctl, found at a fixed absolute path, started with fixed
// flags and one validated unit name, no shell, a cleared environment and a
// deadline. Its output is read through a size limit and its error output is
// discarded, so nothing it prints about the host reaches the agent.

const (
	maxJournalLines  = 1000
	maxJournalBytes  = 262144
	maxJournalSource = 32 << 20 // bytes read from journalctl in total
	maxJournalEntry  = 1 << 20  // bytes of one JSON entry; larger ones are skipped
	journalTimeout   = 15 * time.Second
	missingMessage   = "[message not shown by the journal: too long]"
)

// journalctlPaths are the only locations journalctl is run from.
var journalctlPaths = []string{"/usr/bin/journalctl", "/bin/journalctl"}

// journalEnv is the whole environment journalctl gets.
var journalEnv = []string{"LANG=C.UTF-8", "SYSTEMD_COLORS=0", "SYSTEMD_PAGER="}

var priorityNames = [...]string{"emerg", "alert", "crit", "err", "warning", "notice", "info", "debug"}

// LogEntry is one sanitized journal entry.
type LogEntry struct {
	Time         string `json:"time,omitempty"` // RFC3339Nano UTC
	Priority     int    `json:"priority"`       // syslog level 0 (emerg) to 7 (debug); -1 if unknown
	PriorityName string `json:"priority_name"`
	PID          int    `json:"pid"` // 0 if unknown
	Text         string `json:"text"`
}

// Logs is a bounded tail of a unit's journal.
type Logs struct {
	Entries   []LogEntry `json:"entries"`   // oldest first; never nil
	Truncated bool       `json:"truncated"` // true if entries or text were dropped
}

// runner starts journalctl with args and returns its stdout and a function
// that waits for it to exit. Tests substitute a fake.
type runner func(ctx context.Context, args []string) (io.ReadCloser, func() error, error)

// Logs returns the newest journal entries of unit.
func (b *Bus) Logs(ctx context.Context, unit string, lines, maxBytes int) (Logs, error) {
	if runtime.GOOS != "linux" {
		return Logs{}, host.ErrUnsupported
	}
	run := b.run
	if run == nil {
		run = execJournalctl
	}
	return readJournal(ctx, run, unit, lines, maxBytes)
}

// journalArgs builds the fixed argument list. The unit travels inside a
// "--unit=" argument, so it can never be read as a separate option.
func journalArgs(unit string, lines int) []string {
	return []string{
		"--unit=" + unit,
		"--lines=" + strconv.Itoa(lines),
		"--output=json",
		"--no-pager",
		"--quiet",
		"--output-fields=MESSAGE,PRIORITY,_PID,__REALTIME_TIMESTAMP,CONTAINER_ID",
	}
}

func execJournalctl(ctx context.Context, args []string) (io.ReadCloser, func() error, error) {
	var path string
	for _, p := range journalctlPaths {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			path = p
			break
		}
	}
	if path == "" {
		return nil, nil, errors.New("journalctl not found")
	}
	cmd := exec.CommandContext(ctx, path, args...) //nolint:gosec // fixed path, fixed flags, validated operand, no shell
	cmd.Env = append([]string(nil), journalEnv...)
	cmd.Dir = "/"
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, errors.New("journalctl: start failed")
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, errors.New("journalctl: start failed")
	}
	return out, cmd.Wait, nil
}

func readJournal(ctx context.Context, run runner, unit string, lines, maxBytes int) (Logs, error) {
	if !unitName.MatchString(unit) {
		return Logs{}, errors.New("invalid unit name")
	}
	if lines < 1 || lines > maxJournalLines || maxBytes < 1 || maxBytes > maxJournalBytes {
		return Logs{}, errors.New("journal: lines or max_bytes out of range")
	}
	ctx, cancel := context.WithTimeout(ctx, journalTimeout)
	defer cancel()
	rc, wait, err := run(ctx, journalArgs(unit, lines))
	if err != nil {
		return Logs{}, err
	}
	waited := false
	defer func() {
		if !waited {
			cancel() // stop journalctl if we stopped reading early
			_ = rc.Close()
			_ = wait()
		}
	}()

	src := &io.LimitedReader{R: rc, N: maxJournalSource}
	br := bufio.NewReaderSize(src, 64<<10)
	c := &journalCollector{lines: lines, maxBytes: maxBytes}
	for {
		line, tooLong, err := readEntryLine(br)
		if len(line) > 0 || tooLong {
			c.add(line, tooLong)
		}
		if err != nil {
			if errors.Is(err, io.EOF) || src.N == 0 {
				break
			}
			return Logs{}, errors.New("journal: reading failed")
		}
	}
	if src.N == 0 {
		c.truncated = true
		return c.result(), nil // deferred cleanup stops journalctl
	}
	waited = true
	if err := wait(); err != nil {
		return Logs{}, errors.New("journal: journalctl failed")
	}
	return c.result(), nil
}

// readEntryLine reads one newline-terminated line of at most maxJournalEntry
// bytes. A longer line is discarded and reported as tooLong.
func readEntryLine(br *bufio.Reader) (line []byte, tooLong bool, err error) {
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > maxJournalEntry {
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		switch {
		case err == nil:
			return bytes.TrimRight(line, "\r\n"), tooLong, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return bytes.TrimRight(line, "\r\n"), tooLong, err
		}
	}
}

// journalDoc is the only part of a journal entry that is decoded.
type journalDoc struct {
	Message  json.RawMessage `json:"MESSAGE"`
	Priority json.RawMessage `json:"PRIORITY"`
	PID      json.RawMessage `json:"_PID"`
	Realtime json.RawMessage `json:"__REALTIME_TIMESTAMP"`
	// ContainerID is set on lines a container wrote through Docker's
	// journald log driver; they are logged under docker.service.
	ContainerID json.RawMessage `json:"CONTAINER_ID"`
}

type journalCollector struct {
	lines, maxBytes int
	entries         []LogEntry
	total           int
	truncated       bool
}

func (c *journalCollector) add(line []byte, tooLong bool) {
	if tooLong {
		c.truncated = true
		return
	}
	var d journalDoc
	if err := json.Unmarshal(line, &d); err != nil {
		return // not an entry; nothing from it is passed on
	}
	if id := bytes.TrimSpace(d.ContainerID); len(id) > 0 && string(id) != "null" {
		// Container output belongs to docker_logs, where exclusion applies;
		// here it would show excluded containers' logs.
		return
	}
	e := LogEntry{Priority: -1, PriorityName: "unknown"}
	if p, ok := fieldInt(d.Priority); ok && p >= 0 && p < len(priorityNames) {
		e.Priority, e.PriorityName = p, priorityNames[p]
	}
	if pid, ok := fieldInt(d.PID); ok && pid > 0 {
		e.PID = pid
	}
	if us, ok := fieldInt(d.Realtime); ok && us > 0 {
		e.Time = time.UnixMicro(int64(us)).UTC().Format(time.RFC3339Nano)
	}
	msg, ok := fieldString(d.Message)
	if !ok && string(bytes.TrimSpace(d.Message)) == "null" {
		msg = missingMessage
	}
	text := redact.Text(textsafe.Lines(strings.TrimSuffix(msg, "\n")))
	if len(text) > c.maxBytes { // also covers growth from replacement characters and [hidden]
		text = textsafe.Tail(text, c.maxBytes)
		c.truncated = true
	}
	e.Text = text
	c.entries = append(c.entries, e)
	c.total += len(text)
	for len(c.entries) > 1 && (len(c.entries) > c.lines || c.total > c.maxBytes) {
		c.total -= len(c.entries[0].Text)
		c.entries = c.entries[1:]
		c.truncated = true
	}
}

func (c *journalCollector) result() Logs {
	return Logs{Entries: append([]LogEntry{}, c.entries...), Truncated: c.truncated}
}

// fieldString decodes a journal field value. The journal writes text as a
// JSON string, binary data as an array of byte values and a field that
// occurs more than once as an array of values; the first value is used.
func fieldString(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", false
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", false
		}
		return s, true
	case '[':
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return "", false
		}
		if len(items) == 0 {
			return "", true
		}
		if first := bytes.TrimSpace(items[0]); len(first) > 0 && first[0] == '"' {
			return fieldString(first)
		}
		buf := make([]byte, 0, len(items))
		for _, it := range items {
			n, err := strconv.ParseUint(string(bytes.TrimSpace(it)), 10, 8)
			if err != nil {
				return "", false
			}
			buf = append(buf, byte(n))
		}
		return string(buf), true
	}
	return "", false
}

// fieldInt decodes a numeric journal field, which is normally a string.
func fieldInt(raw json.RawMessage) (int, bool) {
	s, ok := fieldString(raw)
	if !ok {
		s = strings.TrimSpace(string(raw))
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n > 1<<62 {
		return 0, false
	}
	return int(n), true
}
