// Command latch-telegram connects the owner's Telegram chat to Latch: change
// requests show up with Approve and Deny buttons, and (when an agent is
// configured) messages go to the agent team, which answers in the same chat.
//
// It runs as root because the admin socket only accepts root, and starts each
// agent turn as the agent's own account. The latch daemon never talks to
// Telegram and never sees the bot token.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // time_zone works even where the system has no zone files

	"github.com/mhmdxsadk/latch/internal/admin"
	"github.com/mhmdxsadk/latch/internal/approval"
	"github.com/mhmdxsadk/latch/internal/textsafe"
	"github.com/mhmdxsadk/latch/internal/tgapi"
	"github.com/mhmdxsadk/latch/internal/tgapprove"
	"github.com/mhmdxsadk/latch/internal/tgchat"
)

// Set by GoReleaser via -ldflags.
var (
	version = "dev"
	commit  = "none"
)

const usage = `Usage: latch-telegram <command> [flags]

Commands:
  serve      Start the Telegram bridge
  pair       Link your Telegram account: writes owner_id
  validate   Validate the configuration without starting
  doctor     Check the token, the admin socket and the agent setup
  version    Print version information

Run "latch-telegram <command> -h" for command flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return cmdServe(args[1:], stderr)
	case "pair":
		return cmdPair(args[1:], os.Stdin, stdout, stderr)
	case "validate":
		return cmdValidate(args[1:], stdout, stderr)
	case "doctor":
		return cmdDoctor(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		c := commit
		if len(c) > 7 {
			c = c[:7]
		}
		fmt.Fprintf(stdout, "latch-telegram %s\ncommit %s, %s/%s\n", version, c, runtime.GOOS, runtime.GOARCH)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "latch-telegram: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func configFlag(fs *flag.FlagSet) *string {
	return fs.String("config", defaultConfig, "path to the configuration file")
}

func load(name string, args []string, stderr io.Writer) (*Config, string, bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return nil, "", false
	}
	cfg, err := loadConfig(*path)
	if err != nil {
		printProblems(stderr, *path, err)
		return nil, *path, false
	}
	return cfg, *path, true
}

func printProblems(w io.Writer, path string, err error) {
	for _, line := range splitLines(err.Error()) {
		fmt.Fprintf(w, "latch-telegram: %s: %s\n", path, line)
	}
}

func cmdValidate(args []string, stdout, stderr io.Writer) int {
	_, path, ok := load("validate", args, stderr)
	if !ok {
		return 1
	}
	fmt.Fprintf(stdout, "latch-telegram: %s: configuration OK\n", path)
	return 0
}

func cmdServe(args []string, stderr io.Writer) int {
	cfg, _, ok := load("serve", args, stderr)
	if !ok {
		return 1
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	fail := func(msg string, err error) int {
		log.Error(msg, "err", err)
		return 1
	}
	if os.Geteuid() != 0 {
		return fail("latch-telegram must run as root", errors.New("the admin socket only accepts root"))
	}
	tok, err := readToken(cfg)
	if err != nil {
		return fail("reading the bot token failed", err)
	}
	if err := stateDir(cfg.StateDir); err != nil {
		return fail("state directory", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	bot := tgapi.New(tgapi.DefaultBaseURL, tok)
	me, err := bot.Me(ctx)
	switch {
	case ctx.Err() != nil:
		return 0 // stopped while starting; nothing is wrong
	case err != nil:
		return fail("could not reach Telegram with the bot token", err)
	}

	b, err := newBridge(cfg, bot, log)
	if err != nil {
		return fail("starting failed", err)
	}
	if err := bot.SetCommands(ctx, b.commands()); err != nil {
		log.Warn("setting the command menu failed", "err", err)
	}
	long, short := b.description()
	if err := bot.SetDescription(ctx, long, short); err != nil {
		log.Warn("setting the bot description failed", "err", err)
	}
	log.Info("latch-telegram started", "version", version, "bot_id", me.ID, "chat", b.chat != nil)
	b.run(ctx)
	log.Info("latch-telegram stopped")
	return 0
}

// newBridge wires the approvals and, when configured, the chat.
func newBridge(cfg *Config, bot *tgapi.Client, log *slog.Logger) (*bridge, error) {
	b := &bridge{bot: bot, owner: cfg.OwnerID, log: log, now: time.Now, loc: time.Local}
	if cfg.TimeZone != "" {
		loc, err := time.LoadLocation(cfg.TimeZone)
		if err != nil {
			return nil, err
		}
		b.loc = loc
	}
	var agentUID *uint32
	if a := cfg.Agent; a != nil {
		acct, err := tgchat.LookupAccount(a.User)
		if err != nil {
			return nil, err
		}
		agentUID = &acct.UID
		var checks tgchat.Runner
		var every time.Duration
		if ch := a.Checks; ch != nil {
			verify := func() error { return checkReadOnly(ch.Config, acct.Home) }
			if err := verify(); err != nil {
				log.Error("scheduled checks are off", "err", err)
			} else {
				// Always restricted: read-only tools run, everything
				// else is refused without asking. The config is
				// verified again before every run, so editing it later
				// cannot add a change tool.
				checks = guardedRunner{
					Runner: &tgchat.ExecRunner{Command: a.Command, Config: ch.Config, ExtraArgs: []string{"--safety", "restricted"}, Account: &acct},
					verify: verify,
				}
				every = time.Duration(ch.Every)
			}
		}
		b.chat, err = tgchat.New(tgchat.Options{
			CheckRunner: checks,
			CheckEvery:  every,
			Bot:         bot,
			Chat:        cfg.OwnerID,
			Runner: &tgchat.ExecRunner{
				Command: a.Command, Config: a.Config, ExtraArgs: a.Args, Account: &acct,
			},
			IndexPath: filepath.Join(cfg.StateDir, "sessions.json"),
			Name:      a.Name,
			Timeout:   time.Duration(a.Timeout),
			Location:  b.loc,
			Host:      hostname(),
			Logger:    log,
		})
		if err != nil {
			return nil, err
		}
	}
	client := admin.NewClient(cfg.AdminSocket)
	client.Via = approval.ViaTelegram
	opts := tgapprove.Options{
		Admin:     client,
		Bot:       bot,
		Chat:      cfg.OwnerID,
		StatePath: filepath.Join(cfg.StateDir, "approvals.json"),
		Location:  b.loc,
		AgentUID:  agentUID,
		Logger:    log,
	}
	if a := cfg.Agent; a != nil {
		opts.AgentName = a.Name
		opts.OnFinished = func(r approval.Request, message int64) {
			b.chat.Notify(outcome(r), "", message)
		}
	}
	var err error
	if b.approvals, err = tgapprove.New(opts); err != nil {
		return nil, err
	}
	return b, nil
}

// guardedRunner runs a check only when verify passes, every time.
type guardedRunner struct {
	tgchat.Runner
	verify func() error
}

func (g guardedRunner) Run(ctx context.Context, session, message string, progress func(tgchat.Progress)) (string, error) {
	if err := g.verify(); err != nil {
		return "", fmt.Errorf("check refused: %w", err)
	}
	return g.Runner.Run(ctx, session, message, progress)
}

// hostname is the server's short name, for the chat's welcome text.
func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	h, _, _ = strings.Cut(h, ".")
	return textsafe.Line(h)
}

// stateDir creates the state directory, root only, and refuses a symlink.
func stateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	switch {
	case err != nil:
		return err
	case !fi.IsDir():
		return fmt.Errorf("%s is not a directory (a symlink?)", dir)
	case fi.Mode().Perm()&0o077 != 0:
		return os.Chmod(dir, 0o700) //nolint:gosec // a directory needs the search bit
	}
	return nil
}

func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
