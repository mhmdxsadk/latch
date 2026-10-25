// Package docker inspects and, through the separate Actions type, changes
// containers through the Docker Engine API.
//
// Engine is strictly read-only: it speaks plain HTTP over the Docker Unix socket,
// issues only GET requests to four fixed endpoints (/_ping, /containers/json,
// /containers/{name}/json, /containers/{name}/logs), never follows redirects
// or uses proxies, and caps every response read. Inspect output is decoded into
// a private struct and projected onto Status and Settings, so environment
// variables and command lines are never decoded and never returned. The newest
// health-check output is returned, bounded and
// sanitized and redacted like logs, because it is the container's own explanation of why it
// is unhealthy. Container names come from policy but
// are re-validated here as defence in depth.
package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mhmdxsadk/latch/internal/redact"
)

// ErrNotFound is returned when the configured container does not exist (HTTP 404).
var ErrNotFound = errors.New("container not found")

const (
	defaultTimeout = 10 * time.Second
	maxInspectBody = 4 << 20
	maxListBody    = 8 << 20
	// maxHealthOutput bounds the returned health-check output; Docker itself
	// keeps at most 4096 bytes per probe.
	maxHealthOutput = 4096
	baseURL         = "http://docker"
)

var (
	// containerName mirrors the pattern enforced by config.
	containerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	// containerID is a full Docker container ID. Containers are only ever
	// addressed by name, never by ID, so an excluded container cannot be
	// reached through its ID.
	containerID = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Health is the container health-check state.
type Health struct {
	Status        string `json:"status"` // "starting", "healthy", "unhealthy", or "none" when no healthcheck
	FailingStreak int    `json:"failing_streak"`
	LastCheckAt   string `json:"last_check_at,omitempty"` // RFC3339, end time of most recent probe
	LastExitCode  int    `json:"last_exit_code"`
	LastOutput    string `json:"last_output,omitempty"` // sanitized output of the most recent probe
}

// Status is the projected state of a container.
type Status struct {
	State        string `json:"state"` // created, running, paused, restarting, removing, exited, dead
	Running      bool   `json:"running"`
	Paused       bool   `json:"paused"`
	Restarting   bool   `json:"restarting"`
	OOMKilled    bool   `json:"oom_killed"`
	ExitCode     int    `json:"exit_code"`
	StartedAt    string `json:"started_at,omitempty"` // RFC3339 UTC; "" when Docker reports the zero time
	FinishedAt   string `json:"finished_at,omitempty"`
	RestartCount int    `json:"restart_count"`
	Image        string `json:"image"` // the image reference as configured
	Health       Health `json:"health"`
}

// LogEntry is one sanitized log line.
type LogEntry struct {
	Stream string `json:"stream"`         // "stdout", "stderr", or "tty" for TTY containers
	Time   string `json:"time,omitempty"` // RFC3339Nano timestamp from Docker
	Text   string `json:"text"`
}

// Logs is a bounded tail of container logs.
type Logs struct {
	Entries   []LogEntry `json:"entries"`   // oldest first; never nil
	Truncated bool       `json:"truncated"` // true if entries or text were dropped to respect maxBytes
}

// Client is the Docker interface the MCP layer uses; tests substitute a fake.
type Client interface {
	Ping(ctx context.Context) error
	// List returns the names of all containers (running or not), sorted.
	// The caller must filter the result through policy.
	List(ctx context.Context) ([]string, error)
	// Status and Logs take a container name already resolved by policy.
	Status(ctx context.Context, container string) (Status, error)
	Logs(ctx context.Context, container string, lines, maxBytes int) (Logs, error)
	Settings(ctx context.Context, container string) (Settings, error)
}

// Engine talks to the Docker Engine API over a Unix socket.
type Engine struct {
	client *http.Client
	base   string
}

var _ Client = (*Engine)(nil)

// NewEngine returns an Engine that dials the Docker socket at socketPath.
func NewEngine(socketPath string) *Engine {
	var d net.Dialer
	return newEngine(baseURL, &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "unix", socketPath)
		},
		MaxIdleConns:    2,
		IdleConnTimeout: 30 * time.Second,
	})
}

// newEngine builds an Engine over an arbitrary transport (used by tests).
// Redirects are never followed.
func newEngine(base string, rt http.RoundTripper) *Engine {
	return &Engine{
		base: base,
		client: &http.Client{
			Transport:     rt,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// get issues a GET and returns a 200 response. The caller must close the body
// and call cancel once done reading. Errors never include response bodies.
func (e *Engine) get(ctx context.Context, path, query string) (*http.Response, context.CancelFunc, error) {
	cancel := context.CancelFunc(func() {})
	if _, ok := ctx.Deadline(); !ok {
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
	}
	u := e.base + path
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		cancel()
		return nil, nil, errors.New("docker: bad request")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("docker: request failed: %w", unwrapURL(err))
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		resp.Body.Close()
		cancel()
		return nil, nil, ErrNotFound
	case resp.StatusCode != http.StatusOK:
		resp.Body.Close()
		cancel()
		return nil, nil, fmt.Errorf("docker: unexpected status %d", resp.StatusCode)
	}
	return resp, cancel, nil
}

// unwrapURL drops the URL from *url.Error so messages stay terse.
func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func containerPath(name, leaf string) (string, error) {
	if !containerName.MatchString(name) {
		return "", errors.New("docker: invalid container name")
	}
	return "/containers/" + url.PathEscape(name) + "/" + leaf, nil
}

// Ping checks that the daemon answers.
func (e *Engine) Ping(ctx context.Context) error {
	resp, cancel, err := e.get(ctx, "/_ping", "")
	if err != nil {
		return err
	}
	defer cancel()
	return resp.Body.Close()
}

// List returns the primary name of every container, running or not. Names
// that Docker accepts but Latch would refuse to address are skipped.
func (e *Engine) List(ctx context.Context) ([]string, error) {
	resp, cancel, err := e.get(ctx, "/containers/json", "all=true")
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer resp.Body.Close()
	var items []struct{ Names []string }
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxListBody)).Decode(&items); err != nil {
		return nil, errors.New("docker: malformed list response")
	}
	names := make([]string, 0, len(items))
	for _, it := range items {
		for _, n := range it.Names {
			// Primary names look like "/web"; link aliases look like "/a/b".
			if name, ok := strings.CutPrefix(n, "/"); ok && containerName.MatchString(name) {
				names = append(names, name)
				break
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// inspect is the only part of the inspect document that is ever decoded.
type inspect struct {
	ID    string `json:"Id"`
	Name  string
	State struct {
		Status     string
		Running    bool
		Paused     bool
		Restarting bool
		OOMKilled  bool
		ExitCode   int
		StartedAt  string
		FinishedAt string
		Health     *struct {
			Status        string
			FailingStreak int
			Log           []struct {
				End      string
				ExitCode int
				Output   string
			}
		}
	}
	RestartCount int
	// Config deliberately omits Env, Cmd and Entrypoint: environment
	// variables and command lines are never decoded.
	Config struct {
		Image       string
		Tty         bool
		Labels      map[string]string
		Healthcheck *struct {
			Test        []string
			Interval    int64
			Timeout     int64
			StartPeriod int64
			Retries     int
		}
	}
	HostConfig struct {
		NetworkMode   string
		RestartPolicy struct {
			Name              string
			MaximumRetryCount int
		}
	}
	NetworkSettings struct {
		Ports    map[string][]struct{ HostIp, HostPort string }
		Networks map[string]struct{ IPAddress, GlobalIPv6Address string }
	}
	Mounts []struct {
		Type        string
		Name        string
		Source      string
		Destination string
		Mode        string
		RW          bool
	}
}

func (e *Engine) inspect(ctx context.Context, name string) (inspect, error) {
	var in inspect
	path, err := containerPath(name, "json")
	if err != nil {
		return in, err
	}
	resp, cancel, err := e.get(ctx, path, "")
	if err != nil {
		return in, err
	}
	defer cancel()
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxInspectBody)).Decode(&in); err != nil {
		return in, errors.New("docker: malformed inspect response")
	}
	// Docker also resolves a name as an ID or ID prefix, so if the container
	// is gone an unrelated container whose ID happens to start with the name
	// would answer, and an excluded container could be reached through its
	// ID. Accept only an exact name match.
	if in.Name != "/"+name {
		return inspect{}, ErrNotFound
	}
	if !containerID.MatchString(in.ID) {
		return inspect{}, errors.New("docker: malformed inspect response")
	}
	return in, nil
}

// Status returns the projected state of the container.
func (e *Engine) Status(ctx context.Context, container string) (Status, error) {
	in, err := e.inspect(ctx, container)
	if err != nil {
		return Status{}, err
	}
	s := in.State
	st := Status{
		State:        s.Status,
		Running:      s.Running,
		Paused:       s.Paused,
		Restarting:   s.Restarting,
		OOMKilled:    s.OOMKilled,
		ExitCode:     s.ExitCode,
		StartedAt:    fmtTime(s.StartedAt),
		FinishedAt:   fmtTime(s.FinishedAt),
		RestartCount: in.RestartCount,
		Image:        in.Config.Image,
		Health:       Health{Status: "none"},
	}
	if h := s.Health; h != nil {
		st.Health.Status = h.Status
		st.Health.FailingStreak = h.FailingStreak
		if n := len(h.Log); n > 0 {
			st.Health.LastCheckAt = fmtTime(h.Log[n-1].End)
			st.Health.LastExitCode = h.Log[n-1].ExitCode
			st.Health.LastOutput = healthOutput(h.Log[n-1].Output)
		}
	}
	return st, nil
}

// healthOutput sanitizes probe output line by line, keeping line breaks, and
// keeps at most maxHealthOutput bytes from the start, where errors appear.
func healthOutput(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\r\n"), "\n")
	for i, l := range lines {
		lines[i] = sanitize(strings.TrimSuffix(l, "\r"))
	}
	return headBytes(redact.Text(strings.Join(lines, "\n")), maxHealthOutput)
}

// fmtTime normalizes a Docker timestamp to UTC; zero or unparsable becomes "".
func fmtTime(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() <= 1 {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
