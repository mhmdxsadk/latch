package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhmdxsadk/latch/internal/chatgptauth"
	"github.com/mhmdxsadk/latch/internal/tgapi"
	"github.com/mhmdxsadk/latch/internal/tgapi/tgtest"
)

const goodAddress = "http://localhost:1455/auth/callback?code=thecode&state=thestate"

// fakeSignIn accepts goodAddress and returns fixed credentials.
type fakeSignIn struct {
	mu        sync.Mutex
	completed int
	failWith  error
}

func (f *fakeSignIn) URL() string { return "https://auth.openai.com/oauth/authorize?state=thestate" }

func (f *fakeSignIn) Code(pasted string) (string, error) {
	if !strings.Contains(pasted, "state=thestate") {
		return "", chatgptauth.ErrNotCallback
	}
	return "thecode", nil
}

func (f *fakeSignIn) Complete(_ context.Context, pasted string) (*chatgptauth.Credentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed++
	if f.failWith != nil {
		return nil, f.failWith
	}
	return &chatgptauth.Credentials{AccessToken: "access-secret", RefreshToken: "refresh-secret", Email: "moe@example.com", Plan: "plus"}, nil
}

type loginHarness struct {
	tg     *tgtest.Server
	desk   *loginDesk
	flow   *fakeSignIn
	mu     sync.Mutex
	files  [][]byte
	verify error
}

func newLoginHarness(t *testing.T) *loginHarness { return newLoginHarnessOn(t, tgtest.New(t)) }

func newLoginHarnessOn(t *testing.T, tg *tgtest.Server) *loginHarness {
	t.Helper()
	h := &loginHarness{tg: tg, flow: &fakeSignIn{}}
	h.desk = newLoginDesk(h.tg.Client(), owner, discard(),
		func(_ context.Context, data []byte) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.files = append(h.files, data)
			return nil
		},
		func(context.Context) error { return h.verify })
	h.desk.begin = func() (signIn, error) { return h.flow, nil }
	return h
}

func (h *loginHarness) paste(text string) (messageID int64) {
	messageID = h.tg.MessageID(owner, text)
	h.desk.Handle(context.Background(), tgapi.Message{MessageID: messageID, Text: text, From: &tgapi.User{ID: owner}, Chat: tgapi.Chat{ID: owner, Type: "private"}})
	return messageID
}

func (h *loginHarness) waitSaid(t *testing.T, want string) {
	t.Helper()
	tgtest.WaitFor(t, want, func() bool {
		for _, m := range h.tg.Sent() {
			if strings.Contains(m.Text, want) {
				return true
			}
		}
		return false
	})
}

func TestLoginSignsIn(t *testing.T) {
	h := newLoginHarness(t)
	h.desk.Start(context.Background())
	link := h.tg.Sent()[0]
	if len(link.Keyboard) != 1 || link.Keyboard[0][0].URL != h.flow.URL() || link.Keyboard[0][0].Text != "Sign in with ChatGPT" {
		t.Fatalf("no sign-in button: %+v", link.Keyboard)
	}
	if strings.Contains(link.Text, "oauth/authorize") {
		t.Errorf("the link belongs on the button, not in the text: %q", link.Text)
	}

	id := h.paste(goodAddress)
	h.waitSaid(t, "Signed in as moe@example.com (plus). The agent can use the model now.")
	if !h.tg.Deleted(id) {
		t.Error("the pasted address, which holds the code, was not deleted")
	}
	h.mu.Lock()
	files := h.files
	h.mu.Unlock()
	if len(files) != 1 || !strings.Contains(string(files[0]), `"access_token": "access-secret"`) {
		t.Fatalf("credentials file: %q", files)
	}
	for _, m := range h.tg.Sent() {
		if strings.Contains(m.Text, "secret") || strings.Contains(m.Text, "thecode") {
			t.Errorf("a credential reached the chat: %q", m.Text)
		}
	}

	// The sign-in is used up: pasting again does not exchange the code twice.
	id2 := h.paste(goodAddress)
	h.waitSaid(t, "No sign-in is waiting")
	h.flow.mu.Lock()
	defer h.flow.mu.Unlock()
	if !h.tg.Deleted(id2) || h.flow.completed != 1 {
		t.Errorf("second paste: deleted %v, exchanged %d times", h.tg.Deleted(id2), h.flow.completed)
	}
}

func TestLoginWrongPasteCanBeRetried(t *testing.T) {
	h := newLoginHarness(t)
	h.desk.Start(context.Background())
	// Contains the port, so it is treated as a pasted address, but it is not the right one.
	id := h.paste("http://localhost:1455/auth/callback?code=x")
	h.waitSaid(t, "That is not the address from the error page")
	if !h.tg.Deleted(id) {
		t.Error("a wrong paste may still hold a code and must be deleted")
	}
	h.paste(goodAddress)
	h.waitSaid(t, "Signed in as moe@example.com")
}

func TestLoginFailuresSayWhatHappened(t *testing.T) {
	h := newLoginHarness(t)
	h.flow.failWith = errors.New("OpenAI refused the code (HTTP 400: invalid_grant)")
	h.desk.Start(context.Background())
	h.paste(goodAddress)
	h.waitSaid(t, "Sign-in failed: OpenAI refused the code (HTTP 400: invalid_grant). Send /login to try again.")

	h = newLoginHarness(t)
	h.verify = errors.New("exit status 1: no credentials")
	h.desk.Start(context.Background())
	h.paste(goodAddress)
	h.waitSaid(t, "Signed in as moe@example.com (plus), but Docker Agent reports a problem with the setup: Exit status 1: no credentials.")
}

func TestLoginIgnoresOrdinaryMessages(t *testing.T) {
	h := newLoginHarness(t)
	if h.desk.Handle(context.Background(), tgapi.Message{MessageID: 1, Text: "is caddy up?"}) {
		t.Error("an ordinary message must go to the agent")
	}
	if !isLoginCommand("/login") || !isLoginCommand("/Login@keeper_bot") || isLoginCommand("/logins") || isLoginCommand("login") {
		t.Error("command matching")
	}
	if looksLikeCallback("what is localhost:80") || !looksLikeCallback("see http://localhost:1455/auth/callback?code=a") || !looksLikeCallback("code=a&state=b") {
		t.Error("callback matching")
	}
}

func TestLoginThroughTheBridge(t *testing.T) {
	b, tg, got := newTestBridge(t, true)
	h := newLoginHarnessOn(t, tg)
	b.login = h.desk
	ctx := context.Background()
	b.dispatch(ctx, msg(owner, owner, "private", "/login"))
	tgtest.WaitFor(t, "the link", func() bool { return len(tg.Sent()) == 1 })
	b.dispatch(ctx, msg(owner, owner, "private", goodAddress))
	h.waitSaid(t, "Signed in as")
	select {
	case m := <-got:
		t.Fatalf("the pasted address reached the agent: %q", m)
	case <-time.After(200 * time.Millisecond):
	}
	cmds := b.commands()
	if cmds[len(cmds)-1].Command != "login" {
		t.Errorf("/login missing from the menu: %+v", cmds)
	}
	// Without a login desk the command goes to the chat like any other.
	b.login = nil
	if cmds := b.commands(); cmds[len(cmds)-1].Command == "login" {
		t.Errorf("/login listed without agent.login: %+v", cmds)
	}
}

func TestConfigLogin(t *testing.T) {
	base := "owner_id: 1\nagent:\n  user: keeper\n  config: /home/keeper/keeper.yaml\n"
	if c, err := loadConfig(writeConfig(t, base+"  login: chatgpt\n")); err != nil || c.Agent.Login != "chatgpt" {
		t.Errorf("chatgpt: %v", err)
	}
	if _, err := loadConfig(writeConfig(t, base+"  login: gemini\n")); err == nil || !strings.Contains(err.Error(), "agent.login") {
		t.Errorf("unsupported: %v", err)
	}
}

func TestConfigDirFor(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "/home/keeper/.config/cagent"},
		{[]string{"--safety", "restricted"}, "/home/keeper/.config/cagent"},
		{[]string{"--config-dir", "/srv/agent/cfg"}, "/srv/agent/cfg"},
		{[]string{"--config-dir=cfg"}, "/home/keeper/cfg"},
	} {
		if got := configDirFor("/home/keeper", tc.args); got != tc.want {
			t.Errorf("%v: %q, want %q", tc.args, got, tc.want)
		}
	}
}
