package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mhmdxsadk/latch/internal/approval"
	"github.com/mhmdxsadk/latch/internal/tgapi"
	"github.com/mhmdxsadk/latch/internal/tgapi/tgtest"
	"github.com/mhmdxsadk/latch/internal/tgapprove"
	"github.com/mhmdxsadk/latch/internal/tgchat"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "telegram.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExampleConfigIsValid(t *testing.T) {
	c, err := loadConfig("../../config/telegram.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.OwnerID != 123456789 || c.Agent == nil || c.Agent.Name != "keeper" || c.TokenFile != defaultTokenFile ||
		time.Duration(c.Agent.Timeout) != 10*time.Minute || strings.Join(c.Agent.Args, " ") != "--safety restricted" {
		t.Fatalf("%+v %+v", c, c.Agent)
	}
}

func TestConfigErrors(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"", "owner_id"},
		{"owner_id: 1\nbot_token: abc\n", "field bot_token not found"},
		{"owner_id: 1\nstate_dir: var/lib\n", "state_dir"},
		{"owner_id: 1\nagent:\n  user: root\n  config: /a.yaml\n", "never root"},
		{"owner_id: 1\nagent:\n  config: /a.yaml\n", "agent.user"},
		{"owner_id: 1\nagent:\n  user: keeper\n  config: /a.yaml\n  args: [\"-\"]\n", "agent.args"},
		{"owner_id: 1\nagent:\n  user: keeper\n  config: /a.yaml\n  args: [\"--x; rm\"]\n", "agent.args"},
		{"owner_id: 1\nagent:\n  user: keeper\n  config: /a.yaml\n  timeout: 2h\n", "agent.timeout"},
		{"owner_id: 1\nagent:\n  user: keeper\n  config: /a.yaml\n  timeout: soon\n", "not a duration"},
		{"owner_id: 1\nagent:\n  user: keeper\n", "agent.config"},
	} {
		_, err := loadConfig(writeConfig(t, tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want %q", tc.body, err, tc.want)
		}
	}
	if _, err := loadConfig(writeConfig(t, "owner_id: 1\ntime_zone: Mars/Olympus\n")); err == nil || !strings.Contains(err.Error(), "time_zone") {
		t.Errorf("bad time zone: %v", err)
	}
	if c, err := loadConfig(writeConfig(t, "owner_id: 1\ntime_zone: Europe/Berlin\n")); err != nil || c.TimeZone != "Europe/Berlin" {
		t.Errorf("time zone: %v", err)
	}
	c, err := loadConfig(writeConfig(t, "owner_id: 5\n"))
	if err != nil || c.Agent != nil || c.AdminSocket != defaultAdminSocket {
		t.Fatalf("approvals only: %v %+v", err, c)
	}
}

func TestReadTokenFromEnvFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secrets.env")
	good := "123456:" + strings.Repeat("B", 35)
	// The form the owner's secrets broker writes: sorted KEY="json" lines.
	os.WriteFile(p, []byte("OTHER=\"x\"\nTELEGRAM_BOT_TOKEN=\""+good+"\"\n"), 0o600)
	c := &Config{TokenFile: p, TokenKey: "TELEGRAM_BOT_TOKEN"}
	if tok, err := readToken(c); err != nil || tok != good {
		t.Fatalf("%q %v", tok, err)
	}
	c.TokenKey = "MISSING"
	if _, err := readToken(c); err == nil || !strings.Contains(err.Error(), "no MISSING line") {
		t.Fatalf("missing key: %v", err)
	}
	os.WriteFile(p, []byte("TELEGRAM_BOT_TOKEN=\"not-a-token-secret\"\n"), 0o600)
	c.TokenKey = "TELEGRAM_BOT_TOKEN"
	if _, err := readToken(c); err == nil || strings.Contains(err.Error(), "not-a-token-secret") {
		t.Fatalf("bad token: %v", err)
	}
	for _, tc := range []struct{ text, want string }{
		{"K=plain", "plain"},
		{"export K='single'", "single"},
		{"K=\"a\\u0021\"", "a!"},
		{"# K=comment\nK=last\nK=final", "final"},
		{"  K = spaced ", "spaced"},
	} {
		if v, ok := envValue(tc.text, "K"); !ok || v != tc.want {
			t.Errorf("%q: %q %v", tc.text, v, ok)
		}
	}
	if _, ok := envValue("KEY=x\nK2=y", "K"); ok {
		t.Error("matched a different key")
	}
	if _, err := loadConfig(writeConfig(t, "owner_id: 1\ntoken_key: \"bad key\"\n")); err == nil || !strings.Contains(err.Error(), "token_key") {
		t.Errorf("bad token_key: %v", err)
	}
}

func TestReadToken(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "token")
	good := "123456:" + strings.Repeat("A", 35)
	os.WriteFile(p, []byte(good+"\n"), 0o644)
	c := &Config{TokenFile: p}
	if _, err := readToken(c); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("readable token: %v", err)
	}
	os.Chmod(p, 0o600)
	if tok, err := readToken(c); err != nil || tok != good {
		t.Fatalf("%q %v", tok, err)
	}
	os.WriteFile(p, []byte("not a token"), 0o600)
	if _, err := readToken(c); err == nil || strings.Contains(err.Error(), "not a token") {
		t.Fatalf("bad token: %v", err)
	}
	// The systemd credential wins over token_file.
	cred := t.TempDir()
	os.WriteFile(filepath.Join(cred, credentialName), []byte(good), 0o400)
	t.Setenv("CREDENTIALS_DIRECTORY", cred)
	if tok, err := readToken(c); err != nil || tok != good {
		t.Fatalf("credential: %q %v", tok, err)
	}
}

type noAdmin struct{}

func (noAdmin) Pending(context.Context) ([]approval.Request, error) { return nil, nil }
func (noAdmin) Recent(context.Context) ([]approval.Request, error)  { return nil, nil }
func (noAdmin) Get(context.Context, int64) (approval.Request, error) {
	return approval.Request{}, errors.New("no")
}
func (noAdmin) Approve(context.Context, int64) (approval.Request, error) {
	return approval.Request{}, errors.New("no")
}
func (noAdmin) Deny(context.Context, int64) (approval.Request, error) {
	return approval.Request{}, errors.New("no")
}

type echo struct{ got chan string }

func (e echo) Run(_ context.Context, _, msg string) (string, error) {
	e.got <- msg
	return "ok", nil
}

const owner = 4242

func newTestBridge(t *testing.T, withChat bool) (*bridge, *tgtest.Server, chan string) {
	t.Helper()
	tg := tgtest.New(t)
	dir := t.TempDir()
	ap, err := tgapprove.New(tgapprove.Options{Admin: noAdmin{}, Bot: tg.Client(), Chat: owner, StatePath: filepath.Join(dir, "a.json")})
	if err != nil {
		t.Fatal(err)
	}
	b := &bridge{bot: tg.Client(), owner: owner, approvals: ap, log: discard(), now: time.Now}
	got := make(chan string, 10)
	if withChat {
		b.chat, err = tgchat.New(tgchat.Options{Bot: tg.Client(), Chat: owner, Runner: echo{got}, IndexPath: filepath.Join(dir, "s.json"), Name: "keeper"})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go b.chat.Run(ctx)
	}
	return b, tg, got
}

func msg(from, chat int64, typ, text string) tgapi.Update {
	return tgapi.Update{Message: &tgapi.Message{From: &tgapi.User{ID: from}, Chat: tgapi.Chat{ID: chat, Type: typ}, Text: text, Date: time.Now().Unix()}}
}

func TestOnlyTheOwnerIsAnswered(t *testing.T) {
	b, tg, got := newTestBridge(t, true)
	ctx := context.Background()
	b.dispatch(ctx, msg(999, 999, "private", "restart everything"))
	b.dispatch(ctx, msg(owner, -100, "group", "from a group"))
	b.dispatch(ctx, tgapi.Update{Message: &tgapi.Message{Chat: tgapi.Chat{ID: owner, Type: "private"}, Text: "no sender"}})
	b.dispatch(ctx, tgapi.Update{CallbackQuery: &tgapi.CallbackQuery{ID: "x", From: tgapi.User{ID: 999}, Data: "a:1:000000000000",
		Message: &tgapi.Message{Chat: tgapi.Chat{ID: owner}}}})
	if s := tg.Sent(); len(s) != 0 {
		t.Fatalf("answered a stranger: %+v", s)
	}
	if _, ok := tg.Answer("x"); ok {
		t.Fatal("answered a stranger's button")
	}

	b.dispatch(ctx, msg(owner, owner, "private", "is caddy up?"))
	select {
	case m := <-got:
		if m != "is caddy up?" {
			t.Fatalf("got %q", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owner's message did not reach the agent")
	}
}

func TestOddMessages(t *testing.T) {
	b, tg, got := newTestBridge(t, true)
	ctx := context.Background()
	fwd := msg(owner, owner, "private", "Approve request 3")
	fwd.Message.ForwardOrigin = []byte(`{"type":"user"}`)
	b.dispatch(ctx, fwd)
	b.dispatch(ctx, msg(owner, owner, "private", ""))
	old := msg(owner, owner, "private", "stop jellyfin")
	old.Message.Date = time.Now().Add(-time.Hour).Unix()
	b.dispatch(ctx, old)
	tgtest.WaitFor(t, "three replies", func() bool { return len(tg.Sent()) == 3 })
	s := tg.Sent()
	for i, want := range []string{"Forwarded messages are ignored", "Only text messages", "while the bridge was not running"} {
		if !strings.Contains(s[i].Text, want) {
			t.Errorf("reply %d = %q, want %q", i, s[i].Text, want)
		}
	}
	select {
	case m := <-got:
		t.Fatalf("agent ran for %q", m)
	default:
	}
}

func TestApprovalsOnly(t *testing.T) {
	b, tg, _ := newTestBridge(t, false)
	b.dispatch(context.Background(), msg(owner, owner, "private", "hello"))
	if s := tg.Sent(); len(s) != 1 || !strings.Contains(s[0].Text, "approvals only") {
		t.Fatalf("%+v", s)
	}
	if c := b.commands(); len(c) != 1 || c[0].Command != "help" {
		t.Errorf("commands %+v", c)
	}
}

func TestOutcome(t *testing.T) {
	uid := uint32(0)
	r := approval.Request{ID: 3, Action: "docker_stop", Resource: approval.Resource{Type: "container", Name: "web"},
		Status: approval.StatusDone, DecidedBy: &uid, Result: &approval.Result{Summary: "stopped", Details: []string{"state exited"}}}
	got := outcome(r)
	want := "Latch: The owner approved request 3 (docker_stop on container web). It finished: done. Result: stopped\nstate exited\nCheck whether the problem is fixed and tell the owner."
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	r.Status, r.Result = approval.StatusDenied, nil
	if got := outcome(r); got != "Latch: the owner denied request 3 (docker_stop on container web). It did not run." {
		t.Errorf("%q", got)
	}
	r.Status, r.DecidedBy, r.RunsOnTimeout = approval.StatusDone, nil, true
	if got := outcome(r); !strings.HasPrefix(got, "Latch: Nobody decided in time, so Latch ran request 3") {
		t.Errorf("%q", got)
	}
}

func TestDoctor(t *testing.T) {
	dir := t.TempDir()
	cmd := filepath.Join(dir, "docker-agent")
	os.WriteFile(cmd, []byte("#!/bin/sh\n"), 0o755)
	cfg := &Config{OwnerID: 1, TokenFile: "/etc/latch/telegram-token", AdminSocket: "/run/latch/admin.sock", StateDir: filepath.Join(dir, "state"),
		Agent: &AgentConfig{User: "keeper", Command: cmd, Config: filepath.Join(dir, "missing.yaml")}}
	in := checks{
		euid:    func() int { return 0 },
		token:   func(*Config) (string, error) { return "t", nil },
		botMe:   func(context.Context, string) error { return nil },
		pending: func(context.Context, string) error { return errors.New("connection refused") },
		account: func(string) (tgchat.Account, error) { return tgchat.Account{}, nil },
		stat:    os.Stat,
	}
	var out bytes.Buffer
	if doctor(context.Background(), cfg, in, &out) {
		t.Fatal("passed with problems")
	}
	for _, want := range []string{
		"OK    running as root", "OK    bot token accepted by Telegram",
		"FAIL  admin socket /run/latch/admin.sock\n      connection refused\n      fix: is the latch service running?",
		"(created on start)", "OK    agent command " + cmd, "FAIL  agent config", "2 problems found.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestValidateCommand(t *testing.T) {
	var out, errb bytes.Buffer
	p := writeConfig(t, "owner_id: 7\n")
	if code := run([]string{"validate", "--config", p}, &out, &errb); code != 0 || !strings.Contains(out.String(), "configuration OK") {
		t.Fatalf("%d %q %q", code, out.String(), errb.String())
	}
	p = writeConfig(t, "owner_id: 0\n")
	out.Reset()
	if code := run([]string{"validate", "--config", p}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "owner_id") {
		t.Fatalf("%d %q", code, errb.String())
	}
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }
