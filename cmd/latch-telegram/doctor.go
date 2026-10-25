package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/mhmdxsadk/latch/internal/admin"
	"github.com/mhmdxsadk/latch/internal/tgapi"
	"github.com/mhmdxsadk/latch/internal/tgchat"
)

// checks are what doctor exercises; tests substitute fakes.
type checks struct {
	euid    func() int
	token   func(*Config) (string, error)
	botMe   func(ctx context.Context, token string) error
	pending func(ctx context.Context, socket string) error
	account func(name string) (tgchat.Account, error)
	stat    func(path string) (os.FileInfo, error)
}

func cmdDoctor(args []string, stdout, stderr io.Writer) int {
	cfg, _, ok := load("doctor", args, stderr)
	if !ok {
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	in := checks{
		euid:  os.Geteuid,
		token: readToken,
		botMe: func(ctx context.Context, token string) error {
			_, err := tgapi.New(tgapi.DefaultBaseURL, token).Me(ctx)
			return err
		},
		pending: func(ctx context.Context, socket string) error {
			_, err := admin.NewClient(socket).Pending(ctx)
			return err
		},
		account: tgchat.LookupAccount,
		stat:    os.Stat,
	}
	if !doctor(ctx, cfg, in, stdout) {
		return 1
	}
	return 0
}

// doctor prints one plain OK or FAIL line per check, like latch doctor.
func doctor(ctx context.Context, cfg *Config, in checks, out io.Writer) bool {
	failed := 0
	check := func(name string, err error, hint string) {
		if err == nil {
			fmt.Fprintf(out, "OK    %s\n", name)
			return
		}
		failed++
		fmt.Fprintf(out, "FAIL  %s\n      %s\n", name, err)
		if hint != "" {
			fmt.Fprintf(out, "      fix: %s\n", hint)
		}
	}
	fmt.Fprintln(out, "latch-telegram doctor")
	fmt.Fprintln(out)
	check("config valid", nil, "")

	var rootErr error
	if uid := in.euid(); uid != 0 {
		rootErr = fmt.Errorf("running as uid %d, not root", uid)
	}
	check("running as root", rootErr, "run doctor with sudo; the service runs as root to reach the admin socket")

	tok, err := in.token(cfg)
	hint := "put the token from @BotFather in " + cfg.TokenFile + " (root, mode 0600)"
	if cfg.TokenKey != "" {
		hint = "add " + cfg.TokenKey + " to " + cfg.TokenFile + " (root, mode 0600), for example through your secrets manager"
	}
	check("bot token", err, hint)
	if err == nil {
		check("bot token accepted by Telegram", in.botMe(ctx, tok), "check the token, and that this server can reach api.telegram.org")
	}
	check("admin socket "+cfg.AdminSocket, in.pending(ctx, cfg.AdminSocket), "is the latch service running? run doctor as root")

	switch fi, err := in.stat(cfg.StateDir); {
	case errors.Is(err, os.ErrNotExist):
		check("state directory "+cfg.StateDir+" (created on start)", nil, "")
	case err != nil:
		check("state directory "+cfg.StateDir, err, "")
	case !fi.IsDir():
		check("state directory "+cfg.StateDir, errors.New("not a directory"), "remove it; latch-telegram creates the directory")
	case fi.Mode().Perm()&0o077 != 0:
		check("state directory "+cfg.StateDir, fmt.Errorf("mode %04o", fi.Mode().Perm()), "chmod 700 "+cfg.StateDir)
	default:
		check("state directory "+cfg.StateDir, nil, "")
	}

	if a := cfg.Agent; a == nil {
		fmt.Fprintln(out, "OK    chat with the agent is off (no agent section); approvals only")
	} else {
		_, err := in.account(a.User)
		check("agent account "+a.User, err, "create the account, or set agent.user to the account the agent runs as")
		switch fi, err := in.stat(a.Command); {
		case err != nil:
			check("agent command "+a.Command, err, "install Docker Agent there, or set agent.command")
		case fi.IsDir() || fi.Mode().Perm()&0o111 == 0:
			check("agent command "+a.Command, errors.New("not an executable file"), "")
		default:
			check("agent command "+a.Command, nil, "")
		}
		_, err = in.stat(a.Config)
		check("agent config "+a.Config, err, "set agent.config to the agent team file, for example /home/"+a.User+"/keeper.yaml")
	}

	fmt.Fprintln(out)
	switch failed {
	case 0:
		fmt.Fprintln(out, "All checks passed.")
	case 1:
		fmt.Fprintln(out, "1 problem found.")
	default:
		fmt.Fprintf(out, "%d problems found.\n", failed)
	}
	return failed == 0
}
