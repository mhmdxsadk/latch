package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mhmdxsadk/latch/internal/tgapi"
	"github.com/mhmdxsadk/latch/internal/tgapprove"
)

const (
	pairWait     = 10 * time.Minute
	pairAttempts = 3
	// codeAlphabet leaves out look-alikes (0/O, 1/I/L).
	codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
)

// pairing is what pair needs from Telegram; tests substitute a fake.
type pairing interface {
	Me(ctx context.Context) (tgapi.User, error)
	Updates(ctx context.Context, offset int64) ([]tgapi.Update, error)
	Confirm(ctx context.Context, offset int64) error
	Send(ctx context.Context, chat int64, t tgapi.Text, kb tgapi.Keyboard) (tgapi.Message, error)
}

// cmdPair finds the owner's Telegram account and writes owner_id: the first
// private message to the bot names an account, the bot sends that account a
// code, and the owner types the code here. Someone else who messages the bot
// first cannot pair, because the owner never sees their code.
func cmdPair(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfigForPairing(*path)
	if err != nil {
		printProblems(stderr, *path, err)
		return 1
	}
	tok, err := readToken(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "latch-telegram: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), pairWait+time.Minute)
	defer cancel()
	id, err := pair(ctx, tgapi.New(tgapi.DefaultBaseURL, tok), cfg.OwnerID, bufio.NewReader(stdin), stdout)
	if err != nil {
		fmt.Fprintf(stderr, "latch-telegram: %v\n", err)
		return 1
	}
	if err := setOwnerID(*path, id); err != nil {
		fmt.Fprintf(stderr, "latch-telegram: %s: %v\n", *path, err)
		return 1
	}
	fmt.Fprintf(stdout, "latch-telegram: paired with Telegram user %d; owner_id written to %s\n", id, *path)
	fmt.Fprintln(stdout, "Restart the bridge to use it: systemctl restart latch-telegram")
	return 0
}

func pair(ctx context.Context, bot pairing, current int64, in *bufio.Reader, out io.Writer) (int64, error) {
	me, err := bot.Me(ctx)
	if err != nil {
		return 0, fmt.Errorf("the bot token does not work: %w", err)
	}
	if current > 0 {
		fmt.Fprintf(out, "owner_id is %d now; pairing replaces it.\n", current)
	}
	fmt.Fprintf(out, "Send any message to your bot (id %d) in Telegram now. Waiting up to %s.\n", me.ID, pairWait)
	user, err := firstSender(ctx, bot)
	if err != nil {
		return 0, err
	}
	code, err := newCode()
	if err != nil {
		return 0, err
	}
	var b tgapi.Builder
	// The code is a spoiler, hidden until tapped.
	b.Bold("Latch pairing code: ").Styled("spoiler", code)
	b.Plain("\nTap to show it, or use Copy code. Type it into latch-telegram pair on the server. If you did not start pairing, ignore this message.")
	kb := tgapi.Keyboard{{{Text: "Copy code", Copy: &tgapi.CopyText{Text: code}}}}
	if _, err := bot.Send(ctx, user, b.Done(), kb); err != nil {
		return 0, fmt.Errorf("sending the code failed: %w", err)
	}
	fmt.Fprintf(out, "A message came from Telegram user %d. The bot sent that account a code.\n", user)
	for range pairAttempts {
		fmt.Fprint(out, "Code from Telegram: ")
		line, err := in.ReadString('\n')
		got := strings.ToUpper(strings.Join(strings.Fields(line), ""))
		if subtle.ConstantTimeCompare([]byte(got), []byte(code)) == 1 {
			var ok tgapi.Builder
			ok.Bold("Paired.").Plain(" This chat now answers for Latch and approves its changes.")
			_, _ = bot.Send(ctx, user, ok.Done(), nil)
			return user, nil
		}
		if err != nil {
			return 0, errors.New("no code entered; nothing changed")
		}
		fmt.Fprintln(out, "That code does not match.")
	}
	return 0, errors.New("the code did not match; nothing changed. If the code never arrived, someone else messaged the bot first: run pair again")
}

// firstSender waits for the first private text message from a person and
// returns their user id. Updates up to it are marked handled.
func firstSender(ctx context.Context, bot pairing) (int64, error) {
	deadline := time.Now().Add(pairWait)
	var offset int64
	for time.Now().Before(deadline) {
		ups, err := bot.Updates(ctx, offset)
		if err != nil {
			var te *tgapi.Error
			if errors.As(err, &te) && te.Code == 409 {
				return 0, errors.New("the bridge is running and reads the bot's messages; stop it first: systemctl stop latch-telegram")
			}
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			time.Sleep(2 * time.Second)
			continue
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			m := u.Message
			if m == nil || m.From == nil || m.From.IsBot || m.Chat.Type != "private" || m.Chat.ID != m.From.ID {
				continue
			}
			_ = bot.Confirm(ctx, offset)
			return m.From.ID, nil
		}
	}
	return 0, errors.New("no message arrived in time; nothing changed")
}

func newCode() (string, error) {
	var b strings.Builder
	for i := range 8 {
		if i == 4 {
			b.WriteByte('-')
		}
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(codeAlphabet))))
		if err != nil {
			return "", err
		}
		b.WriteByte(codeAlphabet[n.Int64()])
	}
	return b.String(), nil
}

var ownerLine = regexp.MustCompile(`(?m)^owner_id:.*$`)

// setOwnerID writes owner_id into the config file, keeping everything else
// (comments included): it replaces an owner_id line, or adds one at the top.
// The file stays root-only and is checked before it replaces the old one.
func setOwnerID(path string, id int64) error {
	b, err := os.ReadFile(path) //nolint:gosec // owner-supplied config location
	if err != nil {
		return err
	}
	line := "owner_id: " + strconv.FormatInt(id, 10)
	s := string(b)
	if ownerLine.MatchString(s) {
		s = ownerLine.ReplaceAllLiteralString(s, line)
	} else {
		s = line + "\n" + s
	}
	tmp := path + ".pairing"
	if err := os.WriteFile(tmp, []byte(s), 0o600); err != nil { //nolint:gosec // next to the owner-supplied config
		return err
	}
	defer os.Remove(tmp)
	if _, err := loadConfig(tmp); err != nil {
		return fmt.Errorf("the updated config would not load: %w", err)
	}
	return tgapprove.WriteFile(path, []byte(s))
}

// loadConfigForPairing loads the config but allows a missing owner_id,
// which is what pair fills in.
func loadConfigForPairing(path string) (*Config, error) { return parseConfig(path, false) }
