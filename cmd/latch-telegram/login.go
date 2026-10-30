package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mhmdxsadk/latch/internal/chatgptauth"
	"github.com/mhmdxsadk/latch/internal/textsafe"
	"github.com/mhmdxsadk/latch/internal/tgapi"
)

// loginCommand is the menu entry for signing the agent's model in.
var loginCommand = tgapi.Command{Command: "login", Description: "Sign in to the model provider"}

// loginBot is what the sign-in needs from Telegram; tests substitute a fake.
type loginBot interface {
	Send(ctx context.Context, chat int64, t tgapi.Text, kb tgapi.Keyboard) (tgapi.Message, error)
	DeleteMessages(ctx context.Context, chat int64, ids []int64) error
}

// signIn is one ChatGPT sign-in in progress (chatgptauth.Login).
type signIn interface {
	URL() string
	Code(pasted string) (string, error)
	Complete(ctx context.Context, pasted string) (*chatgptauth.Credentials, error)
}

// loginDesk signs the agent's model provider in from the chat, for the first
// login and for every one after it expires. The owner taps a link, approves in
// their browser, and pastes the address the browser then fails to load. The
// address holds a one-time code, so the message is deleted at once. The
// credentials go to the agent's own config directory, written as the agent
// account, where Docker Agent reads and refreshes them.
type loginDesk struct {
	bot  loginBot
	chat int64
	log  *slog.Logger

	begin func() (signIn, error)
	// write stores the credentials file for the agent; verify asks Docker
	// Agent whether it accepts the setup.
	write  func(ctx context.Context, data []byte) error
	verify func(ctx context.Context) error

	mu   sync.Mutex
	flow signIn // the sign-in waiting for its address; nil when none
}

func newLoginDesk(bot loginBot, chat int64, log *slog.Logger, write func(context.Context, []byte) error, verify func(context.Context) error) *loginDesk {
	return &loginDesk{
		bot: bot, chat: chat, log: log, write: write, verify: verify,
		begin: func() (signIn, error) { return chatgptauth.Begin() },
	}
}

// isLoginCommand reports whether text is /login, with or without @botname.
func isLoginCommand(text string) bool {
	head, _, _ := strings.Cut(strings.TrimSpace(text), " ")
	head, _, _ = strings.Cut(head, "@")
	return strings.EqualFold(head, "/login")
}

// looksLikeCallback reports whether text is (or contains) the address the
// browser shows after approving. Those always stay out of the agent's
// conversation: the code in them is a credential.
func looksLikeCallback(text string) bool {
	t := strings.ToLower(text)
	if strings.Contains(t, "localhost:1455") || strings.Contains(t, "127.0.0.1:1455") {
		return true
	}
	t = strings.TrimSpace(t)
	return strings.HasPrefix(t, "code=") && strings.Contains(t, "state=")
}

func (l *loginDesk) say(ctx context.Context, text string) {
	var b tgapi.Builder
	if _, err := l.bot.Send(ctx, l.chat, b.Italic(text).Done(), nil); err != nil {
		l.log.Warn("sending failed", "err", err)
	}
}

// Start answers /login with a sign-in link.
func (l *loginDesk) Start(ctx context.Context) {
	flow, err := l.begin()
	if err != nil {
		l.log.Error("starting a sign-in failed", "err", err)
		l.say(ctx, "Could not start the sign-in; see the latch-telegram log.")
		return
	}
	l.mu.Lock()
	l.flow = flow
	l.mu.Unlock()
	var b tgapi.Builder
	b.Bold("Sign in to ChatGPT")
	b.Plain("\n\n1. Tap the button and approve the sign-in.\n2. The browser then shows an error page. That is expected. Copy the full address from the address bar.\n3. Paste it here. The message is deleted right after.")
	b.Plain(fmt.Sprintf("\n\nThe link works for %d minutes.", int(chatgptauth.Valid.Minutes())))
	kb := tgapi.Keyboard{{{Text: "Sign in with ChatGPT", URL: flow.URL(), Style: tgapi.StylePrimary}}}
	if _, err := l.bot.Send(ctx, l.chat, b.Done(), kb); err != nil {
		l.log.Warn("sending the sign-in link failed", "err", err)
	}
}

// Handle takes the owner's message m when it holds the browser's address and
// reports whether it did. The message is deleted either way, and never
// reaches the agent.
func (l *loginDesk) Handle(ctx context.Context, m tgapi.Message) bool {
	if !looksLikeCallback(m.Text) {
		return false
	}
	if err := l.bot.DeleteMessages(ctx, l.chat, []int64{m.MessageID}); err != nil {
		l.log.Warn("deleting the pasted address failed", "err", err)
	}
	l.mu.Lock()
	flow := l.flow
	l.mu.Unlock()
	if flow == nil {
		l.say(ctx, "No sign-in is waiting. Send /login to start one.")
		return true
	}
	// Check the address before using up the sign-in: a wrong paste can be tried again.
	if _, err := flow.Code(m.Text); err != nil {
		l.say(ctx, sentence(err.Error()))
		return true
	}
	l.mu.Lock()
	if l.flow != flow {
		l.mu.Unlock() // a second paste while the first is being finished
		return true
	}
	l.flow = nil
	l.mu.Unlock()
	// The exchange talks to OpenAI; the polling loop must not wait for it.
	go l.finish(ctx, flow, m.Text)
	return true
}

func (l *loginDesk) finish(ctx context.Context, flow signIn, pasted string) {
	creds, err := flow.Complete(ctx, pasted)
	if err != nil {
		l.log.Warn("signing in failed", "err", err)
		l.say(ctx, "Sign-in failed: "+sentence(err.Error())+" Send /login to try again.")
		return
	}
	data, err := creds.JSON()
	if err == nil {
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = l.write(wctx, data)
		cancel()
	}
	if err != nil {
		l.log.Error("saving the sign-in failed", "err", err)
		l.say(ctx, "Signed in, but saving the login for the agent failed; see the latch-telegram log.")
		return
	}
	who := textsafe.Line(creds.Email)
	if p := textsafe.Line(creds.Plan); p != "" {
		who += " (" + p + ")"
	}
	if who = strings.TrimSpace(who); who == "" {
		who = "your ChatGPT account"
	}
	vctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := l.verify(vctx); err != nil {
		l.log.Warn("docker-agent doctor failed after signing in", "err", err)
		l.say(ctx, "Signed in as "+who+", but Docker Agent reports a problem with the setup: "+sentence(textsafe.Line(err.Error())))
		return
	}
	l.say(ctx, "Signed in as "+who+". The agent can use the model now.")
}

// sentence makes an error message read as a sentence.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// configDirFor is the config directory Docker Agent uses for the agent
// account: --config-dir in its arguments, else .config/cagent in its home.
func configDirFor(home string, args []string) string {
	dir := filepath.Join(home, ".config", "cagent")
	for i, a := range args {
		v := ""
		switch {
		case a == "--config-dir" && i+1 < len(args):
			v = args[i+1]
		case strings.HasPrefix(a, "--config-dir="):
			v = strings.TrimPrefix(a, "--config-dir=")
		}
		if v != "" {
			if !filepath.IsAbs(v) {
				v = filepath.Join(home, v)
			}
			dir = filepath.Clean(v)
		}
	}
	return dir
}
