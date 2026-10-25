package netcheck

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"time"
)

// runFunc runs a program and returns its bounded stdout and stderr. stderr
// is only used to classify failures; it never reaches the agent.
type runFunc func(ctx context.Context, path string, args []string) (stdout, stderr []byte, err error)

// errBadOutput means a program's output could not be parsed.
var errBadOutput = errors.New("unexpected program output")

const (
	maxProgramOutput = 64 << 10
	maxProgramErrors = 4 << 10
)

// Program locations. Only these fixed absolute paths are ever run, and only
// on Linux, whose iputils and traceroute output the parsers understand.
var (
	defaultPingPaths  = linuxOnly("/usr/bin/ping", "/bin/ping")
	defaultTracePaths = linuxOnly("/usr/bin/traceroute", "/usr/sbin/traceroute", "/usr/bin/tracepath")
)

func linuxOnly(paths ...string) []string {
	if runtime.GOOS != "linux" {
		return nil
	}
	return paths
}

// findProgram returns the first path that is an executable regular file
// (after following symlinks, as Debian's alternatives use them).
func findProgram(paths []string) string {
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// runProgram runs path with args: no shell, a fixed minimal environment,
// stdin from /dev/null, bounded output, killed when ctx ends.
func runProgram(ctx context.Context, path string, args []string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, path, args...) //nolint:gosec // fixed absolute path; args are fixed flags and a checked IP literal
	cmd.Env = []string{"LC_ALL=C"}
	cmd.Dir = "/"
	stdout := &capped{max: maxProgramOutput}
	stderr := &capped{max: maxProgramErrors}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	return stdout.buf.Bytes(), stderr.buf.Bytes(), err
}

// capped keeps the first max bytes written and discards the rest, so a
// chatty program cannot use unbounded memory.
type capped struct {
	buf bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// permissionDenied reports whether a program's stderr says it could not
// open its socket.
func permissionDenied(stderr []byte) bool {
	for _, s := range []string{"Operation not permitted", "Permission denied", "socket:"} {
		if bytes.Contains(stderr, []byte(s)) {
			return true
		}
	}
	return false
}

func unreachable(stderr []byte) bool {
	return bytes.Contains(stderr, []byte("unreachable")) || bytes.Contains(stderr, []byte("No route to host"))
}

// familyFlag selects IPv4 or IPv6 explicitly, so the program never guesses.
func familyFlag(isV4 bool) string {
	if isV4 {
		return "-4"
	}
	return "-6"
}
