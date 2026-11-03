package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRunner struct {
	mu   sync.Mutex
	dir  string
	args []string
	res  ComposeResult
	err  error
}

func (f *fakeRunner) run(_ context.Context, dir string, args []string) (ComposeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dir, f.args = dir, args
	return f.res, f.err
}

// newActionsFake serves h and fails the test on any request outside the
// endpoints Actions may use: exact-name inspect, the three POSTs and the DELETE on the
// full ID, and the label-filtered container list.
func newActionsFake(t *testing.T, h http.HandlerFunc) (*Actions, *recorder, *fakeRunner) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		var ok bool
		switch r.Method {
		case http.MethodGet:
			ok = r.URL.Path == "/containers/web/json" ||
				(r.URL.Path == "/containers/json" && r.URL.Query().Get("filters") != "")
		case http.MethodPost:
			ok = r.URL.Path == "/containers/"+testID+"/restart" ||
				r.URL.Path == "/containers/"+testID+"/stop" ||
				r.URL.Path == "/containers/"+testID+"/start"
		case http.MethodDelete:
			ok = r.URL.Path == "/containers/"+testID
		}
		if !ok {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusTeapot)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	fr := &fakeRunner{}
	return newActions(srv.URL, srv.Client().Transport, fr), rec, fr
}

func answerInspect(w http.ResponseWriter, r *http.Request, body string, post int) bool {
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/web/json") {
		_, _ = w.Write([]byte(body))
		return true
	}
	if r.Method == http.MethodPost {
		w.WriteHeader(post)
		return true
	}
	return false
}

func TestActionsPostByFullID(t *testing.T) {
	for _, tc := range []struct {
		verb  string
		call  func(*Actions) (bool, error)
		query string
	}{
		{"restart", func(a *Actions) (bool, error) { return true, a.Restart(context.Background(), "web") }, "t=30"},
		{"stop", func(a *Actions) (bool, error) { return a.Stop(context.Background(), "web") }, "t=30"},
		{"start", func(a *Actions) (bool, error) { return a.Start(context.Background(), "web") }, ""},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			a, rec, _ := newActionsFake(t, func(w http.ResponseWriter, r *http.Request) {
				answerInspect(w, r, fixture(false), http.StatusNoContent)
			})
			changed, err := tc.call(a)
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			if rec.count() != 2 {
				t.Fatalf("requests = %d", rec.count())
			}
			post := rec.reqs[1]
			if post.Method != http.MethodPost || post.URL.Path != "/containers/"+testID+"/"+tc.verb || post.URL.RawQuery != tc.query {
				t.Errorf("got %s %s", post.Method, post.URL)
			}
			if post.ContentLength > 0 {
				t.Error("POST must have no body")
			}
		})
	}
}

func TestActionsRemove(t *testing.T) {
	stoppedFixture := strings.Replace(fixture(false), `"Running": true`, `"Running": false`, 1)
	for _, tc := range []struct {
		name        string
		body        string
		wantStopped bool
		wantPaths   []string
	}{
		{"running", fixture(false), true, []string{"GET /containers/web/json", "POST /containers/" + testID + "/stop", "DELETE /containers/" + testID}},
		{"stopped", stoppedFixture, false, []string{"GET /containers/web/json", "DELETE /containers/" + testID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, rec, _ := newActionsFake(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				answerInspect(w, r, tc.body, http.StatusNoContent)
			})
			stopped, err := a.Remove(context.Background(), "web")
			if err != nil || stopped != tc.wantStopped {
				t.Fatalf("stopped=%v err=%v", stopped, err)
			}
			var got []string
			for _, r := range rec.reqs {
				got = append(got, r.Method+" "+r.URL.Path)
			}
			if !slices.Equal(got, tc.wantPaths) {
				t.Fatalf("requests %v, want %v", got, tc.wantPaths)
			}
			del := rec.reqs[len(rec.reqs)-1]
			q := del.URL.Query()
			if q.Get("v") != "0" || q.Get("force") != "0" || q.Get("link") != "0" {
				t.Errorf("DELETE query %q must keep volumes and not force", del.URL.RawQuery)
			}
			if del.ContentLength > 0 {
				t.Error("DELETE must have no body")
			}
		})
	}
}

func TestActionsRemoveErrors(t *testing.T) {
	for _, tc := range []struct {
		status   int
		notFound bool
	}{{404, true}, {409, false}, {500, false}} {
		a, _, _ := newActionsFake(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				w.WriteHeader(tc.status)
				return
			}
			answerInspect(w, r, fixture(false), http.StatusNoContent)
		})
		_, err := a.Remove(context.Background(), "web")
		if err == nil || errors.Is(err, ErrNotFound) != tc.notFound {
			t.Errorf("status %d: %v", tc.status, err)
		}
	}
}

// A failed stop must not be followed by a DELETE.
func TestActionsRemoveStopFailure(t *testing.T) {
	a, rec, _ := newActionsFake(t, func(w http.ResponseWriter, r *http.Request) {
		answerInspect(w, r, fixture(false), http.StatusInternalServerError)
	})
	if _, err := a.Remove(context.Background(), "web"); err == nil {
		t.Fatal("expected an error")
	}
	for _, r := range rec.reqs {
		if r.Method == http.MethodDelete {
			t.Error("DELETE sent after the stop failed")
		}
	}
}

func TestActionsNotModified(t *testing.T) {
	a, _, _ := newActionsFake(t, func(w http.ResponseWriter, r *http.Request) {
		answerInspect(w, r, fixture(false), http.StatusNotModified)
	})
	if changed, err := a.Stop(context.Background(), "web"); err != nil || changed {
		t.Errorf("stop: %v %v", changed, err)
	}
	if changed, err := a.Start(context.Background(), "web"); err != nil || changed {
		t.Errorf("start: %v %v", changed, err)
	}
	if err := a.Restart(context.Background(), "web"); err == nil {
		t.Error("restart has no not-modified answer")
	}
}

func TestActionsErrors(t *testing.T) {
	for _, tc := range []struct {
		status   int
		notFound bool
	}{{404, true}, {409, false}, {500, false}, {307, false}} {
		a, _, _ := newActionsFake(t, func(w http.ResponseWriter, r *http.Request) {
			if tc.status == 307 && r.Method == http.MethodPost {
				http.Redirect(w, r, "/containers/other/kill", http.StatusTemporaryRedirect)
				return
			}
			answerInspect(w, r, fixture(false), tc.status)
		})
		err := a.Restart(context.Background(), "web")
		if err == nil || errors.Is(err, ErrNotFound) != tc.notFound {
			t.Errorf("status %d: %v", tc.status, err)
		}
	}
}

// An inspect answer for another container (ID prefix match, recreated
// container) must stop the action before any POST.
func TestActionsRequireExactName(t *testing.T) {
	other := strings.Replace(fixture(false), `"/web"`, `"/webapp"`, 1)
	a, rec, _ := newActionsFake(t, func(w http.ResponseWriter, r *http.Request) {
		answerInspect(w, r, other, http.StatusNoContent)
	})
	if err := a.Restart(context.Background(), "web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := a.ComposeTarget(context.Background(), "web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	for _, r := range rec.reqs {
		if r.Method != http.MethodGet {
			t.Errorf("sent %s %s after a name mismatch", r.Method, r.URL)
		}
	}
	bad := strings.Replace(fixture(false), testID, "../../x", 1)
	a, rec, _ = newActionsFake(t, func(w http.ResponseWriter, r *http.Request) {
		answerInspect(w, r, bad, http.StatusNoContent)
	})
	if err := a.Restart(context.Background(), "web"); err == nil {
		t.Error("malformed ID accepted")
	}
	if rec.count() != 1 {
		t.Errorf("requests = %d", rec.count())
	}
}

func TestActionsInvalidNamesRejectedBeforeRequest(t *testing.T) {
	a, rec, _ := newActionsFake(t, func(w http.ResponseWriter, r *http.Request) {})
	for _, n := range []string{"", "../x", "a/b", "-x", "a b", testID[:12] + "/../x", "a\nb", strings.Repeat("a", 129)} {
		if err := a.Restart(context.Background(), n); err == nil {
			t.Errorf("Restart(%q) accepted", n)
		}
		if _, err := a.Stop(context.Background(), n); err == nil {
			t.Errorf("Stop(%q) accepted", n)
		}
		if _, err := a.Start(context.Background(), n); err == nil {
			t.Errorf("Start(%q) accepted", n)
		}
		if _, err := a.Remove(context.Background(), n); err == nil {
			t.Errorf("Remove(%q) accepted", n)
		}
		if _, err := a.ComposeTarget(context.Background(), n); err == nil {
			t.Errorf("ComposeTarget(%q) accepted", n)
		}
	}
	if rec.count() != 0 {
		t.Errorf("%d requests sent", rec.count())
	}
}

// composeFixture writes a compose project and returns labels for it.
func composeFixture(t *testing.T) (map[string]string, string, string) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "compose.yaml")
	override := filepath.Join(dir, "compose.override.yaml")
	for _, f := range []string{file, override} {
		if err := os.WriteFile(f, []byte("services: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return map[string]string{
		labelProject:     "media",
		labelService:     "web",
		labelWorkingDir:  dir,
		labelConfigFiles: file + "," + override,
		"password":       "hunter2",
	}, dir, file
}

func withLabels(labels map[string]string) string {
	b, _ := json.Marshal(labels)
	return strings.Replace(fixture(false), `"Labels": {"password": "hunter2"}`, `"Labels": `+string(b), 1)
}

func TestComposeTargetFromContainer(t *testing.T) {
	labels, dir, file := composeFixture(t)
	var filters string
	a, _, _ := newActionsFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/containers/json" {
			filters = r.URL.Query().Get("filters")
			_, _ = w.Write([]byte(`[{"Names":["/web"]},{"Names":["/media-web-2"]},{"Names":["/a/link"]}]`))
			return
		}
		answerInspect(w, r, withLabels(labels), http.StatusNoContent)
	})
	tg, err := a.ComposeTarget(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	want := ComposeTarget{Project: "media", Service: "web", WorkingDir: dir,
		ConfigFiles: []string{file, filepath.Join(dir, "compose.override.yaml")},
		Siblings:    []string{"web", "media-web-2"}}
	if !tg.Same(want) || !slices.Equal(tg.Siblings, want.Siblings) {
		t.Errorf("got %+v, want %+v", tg, want)
	}
	var f map[string][]string
	if err := json.Unmarshal([]byte(filters), &f); err != nil ||
		!slices.Equal(f["label"], []string{"com.docker.compose.project=media", "com.docker.compose.service=web"}) {
		t.Errorf("filters = %s", filters)
	}
}

func TestComposeTargetNoLabels(t *testing.T) {
	a, _, _ := newActionsFake(t, func(w http.ResponseWriter, r *http.Request) {
		answerInspect(w, r, fixture(false), http.StatusNoContent)
	})
	if _, err := a.ComposeTarget(context.Background(), "web"); !errors.Is(err, ErrNotCompose) {
		t.Fatalf("err = %v", err)
	}
}

func TestParseComposeLabels(t *testing.T) {
	good := map[string]string{
		labelProject: "media", labelService: "web",
		labelWorkingDir: "/srv/media", labelConfigFiles: "/srv/media/compose.yaml",
	}
	tg, err := ParseComposeLabels(good)
	if err != nil || tg.Project != "media" || tg.Service != "web" || len(tg.ConfigFiles) != 1 {
		t.Fatalf("%v %+v", err, tg)
	}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range good {
			m[a] = b
		}
		if v == "<delete>" {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	many := make([]string, MaxComposeFiles+1)
	for i := range many {
		many[i] = "/srv/media/c" + string(rune('a'+i)) + ".yaml"
	}
	for _, tc := range []struct {
		name   string
		labels map[string]string
	}{
		{"missing project", with(labelProject, "<delete>")},
		{"missing service", with(labelService, "<delete>")},
		{"missing working dir", with(labelWorkingDir, "<delete>")},
		{"missing files", with(labelConfigFiles, "<delete>")},
		{"empty files", with(labelConfigFiles, "")},
		{"uppercase project", with(labelProject, "Media")},
		{"project as flag", with(labelProject, "-p")},
		{"project with space", with(labelProject, "a b")},
		{"project with slash", with(labelProject, "a/b")},
		{"service as flag", with(labelService, "--build")},
		{"service with space", with(labelService, "web db")},
		{"service with semicolon", with(labelService, "web;id")},
		{"service too long", with(labelService, strings.Repeat("a", 129))},
		{"relative working dir", with(labelWorkingDir, "srv/media")},
		{"working dir with dotdot", with(labelWorkingDir, "/srv/../etc")},
		{"working dir not clean", with(labelWorkingDir, "/srv/media/")},
		{"relative file", with(labelConfigFiles, "compose.yaml")},
		{"file with dotdot", with(labelConfigFiles, "/srv/media/../../etc/x.yaml")},
		{"file list with empty entry", with(labelConfigFiles, "/srv/media/compose.yaml,")},
		{"file with newline", with(labelConfigFiles, "/srv/media/a\n.yaml")},
		{"file with escape", with(labelConfigFiles, "/srv/media/\x1b[2Ja.yaml")},
		{"too many files", with(labelConfigFiles, strings.Join(many, ","))},
	} {
		if _, err := ParseComposeLabels(tc.labels); !errors.Is(err, ErrBadCompose) {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
	if _, err := ParseComposeLabels(map[string]string{"other": "x"}); !errors.Is(err, ErrNotCompose) {
		t.Errorf("no compose labels: %v", err)
	}
	if _, err := ParseComposeLabels(with(labelConfigFiles, strings.Join(many[:MaxComposeFiles], ","))); err != nil {
		t.Errorf("%d files must be accepted: %v", MaxComposeFiles, err)
	}
}

func TestCheckComposeFiles(t *testing.T) {
	labels, dir, file := composeFixture(t)
	tg, err := ParseComposeLabels(labels)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckComposeFiles(tg, osStat); err != nil {
		t.Fatalf("valid project refused: %v", err)
	}
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o700)
	for _, tc := range []struct {
		name string
		mut  func(*ComposeTarget)
	}{
		{"missing file", func(c *ComposeTarget) { c.ConfigFiles = []string{filepath.Join(dir, "nope.yaml")} }},
		{"directory as file", func(c *ComposeTarget) { c.ConfigFiles = []string{sub} }},
		{"missing working dir", func(c *ComposeTarget) { c.WorkingDir = filepath.Join(dir, "gone") }},
		{"file as working dir", func(c *ComposeTarget) { c.WorkingDir = file }},
		{"no files", func(c *ComposeTarget) { c.ConfigFiles = nil }},
		{"too many files", func(c *ComposeTarget) { c.ConfigFiles = slices.Repeat([]string{file}, MaxComposeFiles+1) }},
	} {
		c := tg
		c.ConfigFiles = slices.Clone(tg.ConfigFiles)
		tc.mut(&c)
		if err := CheckComposeFiles(c, osStat); !errors.Is(err, ErrBadCompose) {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
	// A device or socket is not a compose file.
	if err := CheckComposeFiles(ComposeTarget{WorkingDir: "/", ConfigFiles: []string{"/dev/null"}}, osStat); !errors.Is(err, ErrBadCompose) {
		t.Errorf("/dev/null accepted: %v", err)
	}
}

func TestComposeArgsExact(t *testing.T) {
	tg := ComposeTarget{Project: "media", Service: "web", WorkingDir: "/srv/media",
		ConfigFiles: []string{"/srv/media/compose.yaml", "/srv/media/compose.override.yaml"}}
	want := []string{"compose", "--project-name", "media", "--project-directory", "/srv/media",
		"-f", "/srv/media/compose.yaml", "-f", "/srv/media/compose.override.yaml",
		"up", "-d", "--no-deps", "web"}
	if got := ComposeArgs(tg, false); !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	wantPull := append(slices.Clone(want[:len(want)-1]), "--pull", "always", "web")
	if got := ComposeArgs(tg, true); !slices.Equal(got, wantPull) {
		t.Errorf("got  %q\nwant %q", got, wantPull)
	}
}

func TestComposeEnvIsFixed(t *testing.T) {
	want := []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root", "DOCKER_HOST=unix:///var/run/docker.sock"}
	if got := ComposeEnv("/var/run/docker.sock"); !slices.Equal(got, want) {
		t.Errorf("got %q", got)
	}
}

func TestComposeUpRevalidatesAndUsesFixedArgs(t *testing.T) {
	labels, dir, _ := composeFixture(t)
	a, rec, fr := newActionsFake(t, func(http.ResponseWriter, *http.Request) {})
	tg, _ := ParseComposeLabels(labels)
	fr.res = ComposeResult{Output: []string{"Container web Started"}}
	res, err := a.ComposeUp(context.Background(), tg, true)
	if err != nil || len(res.Output) != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	if fr.dir != dir || !slices.Equal(fr.args, ComposeArgs(tg, true)) {
		t.Errorf("ran %q in %s", fr.args, fr.dir)
	}
	fr.args = nil
	for _, mut := range []func(*ComposeTarget){
		func(c *ComposeTarget) { c.Service = "--build" },
		func(c *ComposeTarget) { c.Project = "x;id" },
		func(c *ComposeTarget) { c.WorkingDir = "relative" },
		func(c *ComposeTarget) { c.ConfigFiles = []string{dir + "/missing.yaml"} },
	} {
		c := tg
		mut(&c)
		if _, err := a.ComposeUp(context.Background(), c, false); err == nil {
			t.Errorf("tampered target %+v accepted", c)
		}
	}
	if fr.args != nil {
		t.Errorf("runner called for a tampered target: %q", fr.args)
	}
	if rec.count() != 0 {
		t.Error("ComposeUp must not call the Engine API")
	}
}

func TestOutputTailSanitizedRedactedBounded(t *testing.T) {
	var b strings.Builder
	for i := range 100 {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", i%3))
		b.WriteString("\n")
	}
	b.WriteString("\x1b[31mred\x1b[0m PASSWORD=hunter2\r\n\n\n")
	b.WriteString("-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----\n")
	b.WriteString(strings.Repeat("y", 5000) + "\n")
	out := outputTail(b.String(), ComposeOutputLines)
	if len(out) != ComposeOutputLines {
		t.Fatalf("got %d lines", len(out))
	}
	joined := strings.Join(out, "\n")
	for _, bad := range []string{"\x1b", "hunter2", "abc", "\r"} {
		if strings.Contains(joined, bad) {
			t.Errorf("output contains %q:\n%s", bad, joined)
		}
	}
	if !strings.Contains(joined, "red PASSWORD=[hidden]") {
		t.Errorf("output:\n%s", joined)
	}
	if last := out[len(out)-1]; len(last) != maxComposeLine {
		t.Errorf("long line not capped: %d", len(last))
	}
}

func TestTailBufferKeepsNewest(t *testing.T) {
	tb := &tailBuffer{max: 8}
	tb.Write([]byte("abcdef"))
	tb.Write([]byte("ghij"))
	if tb.String() != "cdefghij" {
		t.Errorf("got %q", tb.String())
	}
	tb.Write([]byte("0123456789"))
	if tb.String() != "23456789" {
		t.Errorf("got %q", tb.String())
	}
}

func TestFindDockerCLI(t *testing.T) {
	dir := t.TempDir()
	open := filepath.Join(dir, "docker-open")
	os.WriteFile(open, []byte("#!/bin/sh\n"), 0o777)
	os.Chmod(open, 0o777)
	mine := filepath.Join(dir, "docker-mine")
	os.WriteFile(mine, []byte("#!/bin/sh\n"), 0o755)
	if _, err := FindDockerCLI([]string{filepath.Join(dir, "missing"), open, dir}); !errors.Is(err, ErrNoDockerCLI) {
		t.Errorf("writable or missing CLI accepted: %v", err)
	}
	_, err := FindDockerCLI([]string{mine})
	if os.Geteuid() != 0 && !errors.Is(err, ErrNoDockerCLI) {
		t.Errorf("CLI not owned by root accepted: %v", err)
	}
}

func TestRunCLIEnvironmentAndNoShell(t *testing.T) {
	env := "/usr/bin/env"
	if _, err := os.Stat(env); err != nil {
		t.Skip("no /usr/bin/env")
	}
	t.Setenv("LATCH_SECRET_TEST", "hunter2")
	out := &tailBuffer{max: 4096}
	code, err := runCLI(context.Background(), env, t.TempDir(), nil, "/run/d.sock", out)
	if err != nil || code != 0 {
		t.Fatalf("%d %v", code, err)
	}
	got := strings.Split(strings.TrimSpace(out.String()), "\n")
	if !slices.Equal(got, ComposeEnv("/run/d.sock")) {
		t.Errorf("environment = %q", got)
	}
	// Arguments are passed verbatim, never through a shell.
	out = &tailBuffer{max: 4096}
	if _, err := runCLI(context.Background(), "/bin/echo", "/", []string{"$(id)", ";", "`id`"}, "/s", out); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "$(id) ; `id`" {
		t.Errorf("echo printed %q", out.String())
	}
}

func TestRunCLITimeoutKillsProcessGroup(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	out := &tailBuffer{max: 4096}
	// The child keeps stdout open; only killing the group ends the run.
	code, _ := runCLI(ctx, "/bin/sh", "/", []string{"-c", "sleep 30 & sleep 30"}, "/s", out)
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout not enforced (%v)", time.Since(start))
	}
	if code == 0 {
		t.Error("killed run reported success")
	}
}

func TestProjectName(t *testing.T) {
	for dir, want := range map[string]string{
		"/srv/services/streaming-stack": "streaming-stack",
		"/srv/My_Stack.v2":              "my_stackv2",
		"/srv/9lives":                   "9lives",
	} {
		if got, err := ProjectName(dir); err != nil || got != want {
			t.Errorf("ProjectName(%q) = %q, %v; want %q", dir, got, err, want)
		}
	}
	for _, dir := range []string{"/", "/srv/---", "/srv/_x", "/srv/...", "/srv/" + strings.Repeat("a", 129)} {
		if _, err := ProjectName(dir); !errors.Is(err, ErrBadProject) {
			t.Errorf("ProjectName(%q) accepted", dir)
		}
	}
}

func TestProjectArgsAreFixed(t *testing.T) {
	tg := ProjectTarget{Project: "stack", Dir: "/srv/stack", File: "/srv/stack/compose.yaml"}
	want := []string{"compose", "--project-name", "stack", "--project-directory", "/srv/stack", "-f", "/srv/stack/compose.yaml", "up", "-d"}
	if got := ProjectArgs(tg, false); !slices.Equal(got, want) {
		t.Errorf("got %q", got)
	}
	if got := ProjectArgs(tg, true); !slices.Equal(got, append(want, "--pull", "always")) {
		t.Errorf("got %q", got)
	}
}

// projectServer answers the label-filtered lists with the containers whose
// labels match the filter.
func projectServer(t *testing.T, all []map[string]any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		var f struct{ Label []string }
		if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &f); err != nil {
			t.Error(err)
		}
		var out []map[string]any
		for _, c := range all {
			labels, _ := c["Labels"].(map[string]string)
			match := true
			for _, l := range f.Label {
				k, v, _ := strings.Cut(l, "=")
				match = match && labels[k] == v
			}
			if match {
				out = append(out, c)
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	}
}

func TestProjectTarget(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stack")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(file, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mine := map[string]string{labelProject: "stack", labelWorkingDir: dir}
	elsewhere := map[string]string{labelProject: "stack", labelWorkingDir: "/srv/other"}
	otherProject := map[string]string{labelProject: "other", labelWorkingDir: dir}
	unrelated := map[string]string{labelProject: "unrelated", labelWorkingDir: "/srv/unrelated"}

	for _, tc := range []struct {
		name string
		all  []map[string]any
		want []string
		err  error
	}{
		{"new project", []map[string]any{{"Names": []string{"/x"}, "Labels": unrelated}}, nil, nil},
		{"existing", []map[string]any{
			{"Names": []string{"/b"}, "Labels": mine}, {"Names": []string{"/a"}, "Labels": mine},
			{"Names": []string{"/x"}, "Labels": unrelated}}, []string{"a", "b"}, nil},
		{"same name, other folder", []map[string]any{{"Names": []string{"/a"}, "Labels": elsewhere}}, nil, ErrProjectConflict},
		{"same folder, other project", []map[string]any{{"Names": []string{"/a"}, "Labels": otherProject}}, nil, ErrProjectConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _ := newActionsFake(t, projectServer(t, tc.all))
			got, err := a.ProjectTarget(context.Background(), dir, file)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if err == nil && (got.Project != "stack" || got.Dir != dir || got.File != file || !slices.Equal(got.Existing, tc.want)) {
				t.Errorf("got %+v", got)
			}
		})
	}

	a, rec, _ := newActionsFake(t, projectServer(t, nil))
	for _, c := range []struct{ dir, file string }{
		{dir, filepath.Join(dir, "missing.yaml")},
		{dir, filepath.Join(dir, "other.yaml")},
		{dir, "/etc/passwd"},
		{dir, dir + "/../x/compose.yaml"},
		{"relative", "relative/compose.yaml"},
		{"/", "/compose.yaml"},
	} {
		if _, err := a.ProjectTarget(context.Background(), c.dir, c.file); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
	if rec.count() != 0 {
		t.Error("a bad target reached the Engine API")
	}
}

func TestComposeProjectUpRevalidatesAndUsesFixedArgs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stack")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(file, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, rec, fr := newActionsFake(t, func(http.ResponseWriter, *http.Request) {})
	tg := ProjectTarget{Project: "stack", Dir: dir, File: file}
	fr.res = ComposeResult{Output: []string{"Container web Started"}}
	if res, err := a.ComposeProjectUp(context.Background(), tg, true); err != nil || len(res.Output) != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	if fr.dir != dir || !slices.Equal(fr.args, ProjectArgs(tg, true)) {
		t.Errorf("ran %q in %s", fr.args, fr.dir)
	}
	fr.args = nil
	for _, mut := range []func(*ProjectTarget){
		func(c *ProjectTarget) { c.Project = "x;id" },
		func(c *ProjectTarget) { c.Project = "--build" },
		func(c *ProjectTarget) { c.Dir = "relative" },
		func(c *ProjectTarget) { c.File = filepath.Join(dir, "missing.yml") },
		func(c *ProjectTarget) { c.File = "/etc/passwd" },
		func(c *ProjectTarget) { c.File = filepath.Join(dir, "..", "compose.yml") },
	} {
		c := tg
		mut(&c)
		if _, err := a.ComposeProjectUp(context.Background(), c, false); err == nil {
			t.Errorf("tampered target %+v accepted", c)
		}
	}
	if fr.args != nil {
		t.Errorf("runner called for a tampered target: %q", fr.args)
	}
	if rec.count() != 0 {
		t.Error("ComposeProjectUp must not call the Engine API")
	}
}
