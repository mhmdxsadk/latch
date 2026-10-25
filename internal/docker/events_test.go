package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func ev(typ, action, name, extra string, ts int64) string {
	return fmt.Sprintf(`{"Type":%q,"Action":%q,"Actor":{"ID":"abc","Attributes":{"name":%q,"image":"nginx","password":"SECRET-LABEL"%s}},"time":%d,"timeNano":%d}`,
		typ, action, name, extra, ts, ts*1_000_000_000)
}

func eventsServer(t *testing.T, lines ...string) (*Engine, *recorder) {
	return newFake(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Join(lines, "\n") + "\n"))
	})
}

var (
	t0 = time.Unix(1_700_000_000, 0)
	t1 = t0.Add(time.Hour)
)

func all(string) bool { return true }

func TestEventsProjection(t *testing.T) {
	e, rec := eventsServer(t,
		ev("container", "die", "web", `,"exitCode":"137"`, 1_700_000_010),
		ev("container", "health_status: unhealthy", "web", "", 1_700_000_020),
		ev("container", "health_status", "web", "", 1_700_000_021),
		ev("container", "exec_die", "web", `,"exitCode":"1"`, 1_700_000_022),
		ev("container", "oom", "web", "", 1_700_000_030),
		ev("network", "start", "web", "", 1_700_000_031),
		ev("container", "start", "bad name!", "", 1_700_000_032),
		ev("container", "start", "../x", "", 1_700_000_033),
		ev("container", "health_status: evil", "web", "", 1_700_000_034),
		ev("container", "stop", "vault", "", 1_700_000_035),
	)
	got, err := e.Events(context.Background(), t0, t1, func(n string) bool { return n != "vault" }, 200)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	s := string(b)
	for _, bad := range []string{"SECRET-LABEL", "nginx", "vault", "exec_die", "bad name", "../x", "evil"} {
		if strings.Contains(s, bad) {
			t.Errorf("output contains %q: %s", bad, s)
		}
	}
	if len(got.Events) != 5 || got.Truncated {
		t.Fatalf("got %s", s)
	}
	d := got.Events[0]
	if d.Action != "die" || d.ExitCode == nil || *d.ExitCode != 137 || d.Time != "2023-11-14T22:13:30Z" || d.Container != "web" {
		t.Errorf("die: %+v", d)
	}
	if h := got.Events[1]; h.Action != "health_status" || h.Health != "unhealthy" || h.ExitCode != nil {
		t.Errorf("health: %+v", h)
	}
	if h := got.Events[2]; h.Action != "health_status" || h.Health != "" {
		t.Errorf("bare health: %+v", h)
	}
	last := rec.reqs[len(rec.reqs)-1]
	q := last.URL.Query()
	if last.URL.Path != "/events" || q.Get("since") != fmt.Sprint(t0.Unix()) || q.Get("until") != fmt.Sprint(t1.Unix()) {
		t.Errorf("request %s", last.URL)
	}
	var f map[string][]string
	if err := json.Unmarshal([]byte(q.Get("filters")), &f); err != nil || f["type"][0] != "container" || len(f["type"]) != 1 {
		t.Errorf("filters %q", q.Get("filters"))
	}
}

func TestEventsKeepNewestAndLimit(t *testing.T) {
	var lines []string
	for i := range 50 {
		lines = append(lines, ev("container", "start", "web", "", 1_700_000_000+int64(i)))
	}
	e, _ := eventsServer(t, lines...)
	got, err := e.Events(context.Background(), t0, t1, all, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 10 || !got.Truncated || got.Events[9].Time != time.Unix(1_700_000_049, 0).UTC().Format(time.RFC3339) {
		t.Errorf("got %d truncated=%v last=%+v", len(got.Events), got.Truncated, got.Events[len(got.Events)-1])
	}
	// Dropped events of hidden containers must not count toward the limit.
	lines = []string{ev("container", "start", "web", "", 1_700_000_001)}
	for range 30 {
		lines = append(lines, ev("container", "start", "vault", "", 1_700_000_002))
	}
	e, _ = eventsServer(t, lines...)
	got, _ = e.Events(context.Background(), t0, t1, func(n string) bool { return n == "web" }, 5)
	if len(got.Events) != 1 || got.Truncated {
		t.Errorf("got %+v", got)
	}
}

func TestEventsEmptyNonNilAndBadArgs(t *testing.T) {
	e, rec := newFake(t, func(w http.ResponseWriter, r *http.Request) {})
	got, err := e.Events(context.Background(), t0, t1, all, 5)
	if err != nil || got.Events == nil || len(got.Events) != 0 {
		t.Errorf("got %+v %v", got, err)
	}
	n := rec.count()
	for _, c := range []struct {
		since, until time.Time
		limit        int
	}{{t1, t0, 5}, {t0, t0, 5}, {t0, t1, 0}, {t0, t1, 5000}} {
		if _, err := e.Events(context.Background(), c.since, c.until, all, c.limit); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
	if rec.count() != n {
		t.Error("bad arguments reached the daemon")
	}
}

func TestEventsMalformed(t *testing.T) {
	e, _ := eventsServer(t, `{"Type":`)
	if _, err := e.Events(context.Background(), t0, t1, all, 5); err == nil {
		t.Error("malformed stream accepted")
	}
	e, _ = eventsServer(t, "not json")
	if _, err := e.Events(context.Background(), t0, t1, all, 5); err == nil {
		t.Error("garbage accepted")
	}
}

func TestIDs(t *testing.T) {
	id2 := strings.Repeat("a", 64)
	e, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[{"Id":%q,"Names":["/web"]},{"Id":%q,"Names":["/x/link","/db"]},{"Id":"short","Names":["/zz"]},{"Id":%q,"Names":["/bad name"]}]`,
			testID, id2, strings.Repeat("b", 64))
	})
	ids, err := e.IDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[testID] != "web" || ids[id2] != "db" {
		t.Errorf("got %v", ids)
	}
}
