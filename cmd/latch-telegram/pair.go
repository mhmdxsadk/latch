package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/mhmdxsadk/latch/internal/tgapprove"
)

const (
	// codeTTL is how long a pairing code works.
	codeTTL = 10 * time.Minute
	// maxWaiting caps the codes held at once, so strangers messaging the
	// bot cannot grow the list without limit.
	maxWaiting  = 20
	pairingFile = "pairing.json"
	codeLength  = 8
	// codeAlphabet leaves out look-alikes (0/O, 1/I/L).
	codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
)

// waiting is a pairing code the bridge issued to a Telegram account. The
// state directory keeps only the code's hash, in a root-only file; the
// running bridge keeps the code itself in memory to resend it.
type waiting struct {
	UserID  int64     `json:"user_id"`
	Name    string    `json:"name"`
	Hash    string    `json:"hash"`
	Expires time.Time `json:"expires"`
}

// normalize reduces a typed code to its letters and digits, upper case, so
// "abcd efgh" and "ABCD-EFGH" are the same code.
func normalize(code string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(code) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func hashCode(code string) string {
	sum := sha256.Sum256([]byte(normalize(code)))
	return hex.EncodeToString(sum[:])
}

func newCode() (string, error) {
	var b strings.Builder
	for i := range codeLength {
		if i == codeLength/2 {
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

// readWaiting returns the codes in dir that have not expired. A missing file
// means none are waiting.
func readWaiting(dir string, now time.Time) ([]waiting, error) {
	b, err := os.ReadFile(filepath.Join(dir, pairingFile)) //nolint:gosec // the configured state directory
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var all, live []waiting
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, fmt.Errorf("%s: %w", pairingFile, err)
	}
	for _, w := range all {
		if now.Before(w.Expires) {
			live = append(live, w)
		}
	}
	return live, nil
}

// writeWaiting replaces the list in dir, or removes the file when it is empty.
func writeWaiting(dir string, ws []waiting) error {
	p := filepath.Join(dir, pairingFile)
	if len(ws) == 0 {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.Marshal(ws)
	if err != nil {
		return err
	}
	return tgapprove.WriteFile(p, b)
}

// cmdPair finishes pairing. The running bridge sends a code to whoever
// messages the bot while no owner is set; the owner types that code here,
// as root, and this writes their account into the config as owner_id. The
// bridge notices the change and starts, so nothing needs a restart. Someone
// else who messages the bot first cannot pair: the code goes only to their
// own chat, and this command shows whose code it is before it pairs.
func cmdPair(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: latch-telegram pair [flags] [CODE]\n\nFlags:\n")
		fs.PrintDefaults()
	}
	path := configFlag(fs)
	yes := fs.Bool("yes", false, "pair without asking whether the account is yours")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfig(*path)
	if err != nil {
		printProblems(stderr, *path, err)
		return 1
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "latch-telegram: "+format+"\n", a...)
		return 1
	}
	if cfg.OwnerID != 0 {
		return fail("%s: already paired with Telegram user %d; to pair another account, remove owner_id and restart latch-telegram", *path, cfg.OwnerID)
	}
	in := bufio.NewReader(stdin)
	code := strings.Join(fs.Args(), "")
	if code == "" {
		fmt.Fprint(stdout, "Pairing code: ")
		code, _ = in.ReadString('\n')
	}
	if len(normalize(code)) != codeLength {
		return fail("that is not a pairing code like ABCD-EFGH")
	}
	ws, err := readWaiting(cfg.StateDir, time.Now())
	if err != nil {
		return fail("reading waiting codes in %s: %v (run as root)", cfg.StateDir, err)
	}
	var found *waiting
	want := hashCode(code)
	for i := range ws {
		if subtle.ConstantTimeCompare([]byte(ws[i].Hash), []byte(want)) == 1 {
			found = &ws[i]
		}
	}
	if found == nil {
		return fail("no waiting request has that code, or it expired. Message the bot for a new one; latch-telegram must be running")
	}
	who := "Telegram user " + strconv.FormatInt(found.UserID, 10)
	if found.Name != "" {
		who += " (" + found.Name + ")"
	}
	if !*yes {
		fmt.Fprintf(stdout, "This code was sent to %s.\nPair this account? [y/N] ", who)
		answer, _ := in.ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			return fail("not paired; nothing changed")
		}
	}
	if err := setOwnerID(*path, found.UserID); err != nil {
		return fail("%s: %v", *path, err)
	}
	if err := writeWaiting(cfg.StateDir, nil); err != nil {
		fmt.Fprintf(stderr, "latch-telegram: owner_id is set, but removing the waiting codes failed: %v\n", err)
	}
	fmt.Fprintf(stdout, "latch-telegram: paired with %s; owner_id written to %s\n", who, *path)
	fmt.Fprintln(stdout, "A running bridge starts within a few seconds. If it is not running: systemctl enable --now latch-telegram")
	return 0
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
