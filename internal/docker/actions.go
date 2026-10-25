package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Actions changes containers. It is separate from Engine so the read client
// stays GET-only: Actions sends exactly three POST requests,
// /containers/{id}/restart, /containers/{id}/stop and /containers/{id}/start,
// plus the GETs it needs to find the container (exact-name inspect) and its
// compose siblings (/containers/json filtered by compose labels). It always
// acts on the full ID it resolved from the exact name, so a container cannot
// be reached through an ID prefix, and runs "docker compose up" only through
// ComposeUp's fixed argument list.
type Actions struct {
	// read is used only for its exact-name inspect and list GETs.
	read *Engine
	// compose runs the docker CLI (execRunner in production).
	compose composeRunner
}

// Mutator is the change interface the MCP layer uses; tests substitute a fake.
// Names must already have been resolved by policy.
type Mutator interface {
	// Restart, Stop and Start report whether anything changed: Docker answers
	// "not modified" when a container is already stopped or running.
	Restart(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) (changed bool, err error)
	Start(ctx context.Context, name string) (changed bool, err error)
	// ComposeTarget reads and validates the compose labels of the container.
	ComposeTarget(ctx context.Context, name string) (ComposeTarget, error)
	// ComposeUp runs "docker compose up -d" for exactly one service.
	ComposeUp(ctx context.Context, t ComposeTarget, pull bool) (ComposeResult, error)
}

var _ Mutator = (*Actions)(nil)

const (
	// StopTimeout is how long Docker waits for a container to stop before it
	// kills it, for stop and restart.
	StopTimeout = 30 * time.Second
	// actionTimeout bounds one POST: the stop timeout plus slack.
	actionTimeout = StopTimeout + 30*time.Second
)

// NewActions returns an Actions that talks to the Docker socket at socketPath.
func NewActions(socketPath string) *Actions {
	return &Actions{read: NewEngine(socketPath), compose: newExecRunner(socketPath)}
}

// newActions builds Actions over an arbitrary transport (tests).
func newActions(base string, rt http.RoundTripper, r composeRunner) *Actions {
	return &Actions{read: newEngine(base, rt), compose: r}
}

// post sends a bodiless POST and accepts 204, or 304 when notModified is
// allowed. It reports whether the answer was 204.
func (a *Actions) post(ctx context.Context, path, query string, notModified bool) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, actionTimeout)
	defer cancel()
	u := a.read.base + path
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return false, errors.New("docker: bad request")
	}
	resp, err := a.read.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("docker: request failed: %w", unwrapURL(err))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusNoContent:
		return true, nil
	case resp.StatusCode == http.StatusNotModified && notModified:
		return false, nil
	case resp.StatusCode == http.StatusNotFound:
		return false, ErrNotFound
	default:
		return false, fmt.Errorf("docker: unexpected status %d", resp.StatusCode)
	}
}

// resolve returns the full ID of the container with exactly this name.
func (a *Actions) resolve(ctx context.Context, name string) (inspect, error) {
	ictx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	return a.read.inspect(ictx, name)
}

func (a *Actions) act(ctx context.Context, name, verb, query string, notModified bool) (bool, error) {
	in, err := a.resolve(ctx, name)
	if err != nil {
		return false, err
	}
	return a.post(ctx, "/containers/"+in.ID+"/"+verb, query, notModified)
}

var stopQuery = fmt.Sprintf("t=%d", int(StopTimeout.Seconds()))

// Restart restarts the container, waiting StopTimeout for it to stop.
func (a *Actions) Restart(ctx context.Context, name string) error {
	_, err := a.act(ctx, name, "restart", stopQuery, false)
	return err
}

// Stop stops the container, waiting StopTimeout before killing it.
func (a *Actions) Stop(ctx context.Context, name string) (bool, error) {
	return a.act(ctx, name, "stop", stopQuery, true)
}

// Start starts the container.
func (a *Actions) Start(ctx context.Context, name string) (bool, error) {
	return a.act(ctx, name, "start", "", true)
}

// ComposeTarget reads the compose labels of the container and validates them
// (see ParseComposeLabels and CheckComposeFiles). Siblings lists every
// container of the same project and service, so callers can refuse when
// one of them is excluded.
func (a *Actions) ComposeTarget(ctx context.Context, name string) (ComposeTarget, error) {
	in, err := a.resolve(ctx, name)
	if err != nil {
		return ComposeTarget{}, err
	}
	t, err := ParseComposeLabels(in.Config.Labels)
	if err != nil {
		return ComposeTarget{}, err
	}
	if err := CheckComposeFiles(t, osStat); err != nil {
		return ComposeTarget{}, err
	}
	t.Siblings, err = a.serviceContainers(ctx, t.Project, t.Service)
	if err != nil {
		return ComposeTarget{}, err
	}
	return t, nil
}

// serviceContainers lists the names of every container (running or not)
// labelled with the compose project and service.
func (a *Actions) serviceContainers(ctx context.Context, project, service string) ([]string, error) {
	filters, err := json.Marshal(map[string][]string{"label": {
		labelProject + "=" + project,
		labelService + "=" + service,
	}})
	if err != nil {
		return nil, errors.New("docker: bad filter")
	}
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	resp, done, err := a.read.get(ctx, "/containers/json", "all=true&filters="+url.QueryEscape(string(filters)))
	if err != nil {
		return nil, err
	}
	defer done()
	defer resp.Body.Close()
	var items []struct{ Names []string }
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxListBody)).Decode(&items); err != nil {
		return nil, errors.New("docker: malformed list response")
	}
	var names []string
	for _, it := range items {
		for _, n := range it.Names {
			if name, ok := strings.CutPrefix(n, "/"); ok && !strings.Contains(name, "/") {
				// Keep names Latch would refuse to address too: the
				// caller must see every container the command touches.
				names = append(names, name)
				break
			}
		}
	}
	return names, nil
}

// ComposeUp runs "docker compose up -d --no-deps" for the target's service.
// A non-zero exit status is returned as ErrComposeFailed together with the
// result.
func (a *Actions) ComposeUp(ctx context.Context, t ComposeTarget, pull bool) (ComposeResult, error) {
	if _, err := ParseComposeLabels(t.labels()); err != nil {
		return ComposeResult{}, err
	}
	if err := CheckComposeFiles(t, osStat); err != nil {
		return ComposeResult{}, err
	}
	return a.compose.run(ctx, t.WorkingDir, ComposeArgs(t, pull))
}
