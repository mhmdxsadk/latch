package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxEventsBody bounds the bytes read from an events response. Memory does
// not grow with it: only the newest events are kept.
const maxEventsBody = 32 << 20

// Event is one container event. Only these fields are ever returned; other
// event attributes (image, labels, ...) are not decoded.
type Event struct {
	Time      string `json:"time"`      // RFC3339 UTC
	Container string `json:"container"` // container name
	Action    string `json:"action"`    // die, oom, restart, kill, stop, start or health_status
	ExitCode  *int   `json:"exit_code,omitempty"`
	Health    string `json:"health,omitempty"` // healthy, unhealthy or starting, for health_status
}

// Events is a bounded list of container events.
type Events struct {
	Events    []Event `json:"events"`    // oldest first; never nil
	Truncated bool    `json:"truncated"` // true if older events were dropped
}

// eventActions are the actions reported. health_status is reported by
// Docker as "health_status: <state>".
var eventActions = []string{"die", "oom", "restart", "kill", "stop", "start", "health_status"}

var healthStates = map[string]bool{"healthy": true, "unhealthy": true, "starting": true}

// eventDoc is the only part of an event that is decoded.
type eventDoc struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		Attributes struct {
			Name     string `json:"name"`
			ExitCode string `json:"exitCode"`
		} `json:"Attributes"`
	} `json:"Actor"`
	Time     int64 `json:"time"`
	TimeNano int64 `json:"timeNano"`
}

// Events returns container events between since and until, oldest first,
// keeping at most limit of the newest. Events of containers for which
// visible returns false, or whose name is not a valid container name, are
// dropped before the limit is applied.
func (e *Engine) Events(ctx context.Context, since, until time.Time, visible func(string) bool, limit int) (Events, error) {
	if limit < 1 || limit > 1000 || !since.Before(until) {
		return Events{}, errors.New("docker: events range out of bounds")
	}
	filters, err := json.Marshal(map[string][]string{
		"type":  {"container"},
		"event": eventActions,
	})
	if err != nil {
		return Events{}, errors.New("docker: bad request")
	}
	q := url.Values{}
	q.Set("since", strconv.FormatInt(since.Unix(), 10))
	q.Set("until", strconv.FormatInt(until.Unix(), 10))
	q.Set("filters", string(filters))
	resp, cancel, err := e.get(ctx, "/events", q.Encode())
	if err != nil {
		return Events{}, err
	}
	defer cancel()
	defer resp.Body.Close()

	src := &io.LimitedReader{R: resp.Body, N: maxEventsBody}
	dec := json.NewDecoder(src)
	out := Events{Events: []Event{}}
	for {
		var d eventDoc
		if err := dec.Decode(&d); err != nil {
			if errors.Is(err, io.EOF) || src.N == 0 {
				break
			}
			return Events{}, errors.New("docker: malformed events response")
		}
		ev, ok := projectEvent(&d)
		if !ok || !visible(ev.Container) {
			continue
		}
		out.Events = append(out.Events, ev)
		if len(out.Events) > 2*limit {
			out.Events = append(out.Events[:0:0], out.Events[len(out.Events)-limit:]...)
			out.Truncated = true
		}
	}
	if len(out.Events) > limit {
		out.Events = append(out.Events[:0:0], out.Events[len(out.Events)-limit:]...)
		out.Truncated = true
	}
	if src.N == 0 {
		out.Truncated = true
	}
	return out, nil
}

// projectEvent turns a decoded event into an Event, or reports false for
// events that are not wanted or have an unusable container name.
func projectEvent(d *eventDoc) (Event, bool) {
	if d.Type != "container" || !containerName.MatchString(d.Actor.Attributes.Name) {
		return Event{}, false
	}
	action, detail, _ := strings.Cut(d.Action, ":")
	action = strings.TrimSpace(action)
	ev := Event{Container: d.Actor.Attributes.Name, Action: action}
	switch action {
	case "die", "oom", "restart", "kill", "stop", "start":
	case "health_status":
		ev.Health = strings.TrimSpace(detail)
		if !healthStates[ev.Health] {
			ev.Health = ""
		}
	default:
		return Event{}, false
	}
	if n, err := strconv.Atoi(d.Actor.Attributes.ExitCode); err == nil && action == "die" {
		ev.ExitCode = &n
	}
	var t time.Time
	switch {
	case d.TimeNano > 0:
		t = time.Unix(0, d.TimeNano)
	case d.Time > 0:
		t = time.Unix(d.Time, 0)
	}
	if !t.IsZero() {
		ev.Time = t.UTC().Format(time.RFC3339)
	}
	return ev, true
}

// IDs returns the primary name of every container keyed by its full ID.
// Names Latch would refuse to address are skipped.
func (e *Engine) IDs(ctx context.Context) (map[string]string, error) {
	resp, cancel, err := e.get(ctx, "/containers/json", "all=true")
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer resp.Body.Close()
	var items []struct {
		ID    string `json:"Id"`
		Names []string
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxListBody)).Decode(&items); err != nil {
		return nil, errors.New("docker: malformed list response")
	}
	ids := make(map[string]string, len(items))
	for _, it := range items {
		if !containerID.MatchString(it.ID) {
			continue
		}
		for _, n := range it.Names {
			if name, ok := strings.CutPrefix(n, "/"); ok && containerName.MatchString(name) {
				ids[it.ID] = name
				break
			}
		}
	}
	return ids, nil
}
