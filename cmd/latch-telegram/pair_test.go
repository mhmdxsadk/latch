package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mhmdxsadk/latch/internal/tgapi"
	"github.com/mhmdxsadk/latch/internal/tgapi/tgtest"
)

// pairingSetup is an unpaired config and its state directory.
func pairingSetup(t *testing.T) (cfgPath, stateDir string) {
	t.Helper()
	stateDir = t.TempDir()
	return writeConfig(t, "owner_id: 0\nstate_dir: "+stateDir+"\n"), stateDir
}

// startDesk runs awaitOwner against the fake server and returns what it ends with.
func startDesk(t *testing.T, tg *tgtest.Server, cfgPath, stateDir string) <-chan *Config {
	t.Helper()
	out := make(chan *Config, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		c, _ := awaitOwner(ctx, tg.Client(), cfgPath, stateDir, 20*time.Millisecond, discard())
		out <- c
	}()
	return out
}

// codeSentTo returns the pairing code in the reply the fake server recorded
// for chat, or fails.
func codeSentTo(t *testing.T, tg *tgtest.Server, chat int64) string {
	t.Helper()
	var got string
	tgtest.WaitFor(t, "the pairing reply", func() bool {
		for _, m := range tg.Sent() {
			if m.Chat != chat {
				continue
			}
			for _, line := range strings.Split(m.Text, "\n") {
				if strings.Contains(line, "latch-telegram pair") {
					f := strings.Fields(line)
					got = f[len(f)-1]
					return true
				}
			}
		}
		return false
	})
	return got
}

func TestPairingFlow(t *testing.T) {
	tg := tgtest.New(t)
	cfgPath, dir := pairingSetup(t)
	done := startDesk(t, tg, cfgPath, dir)

	tg.Update(tgapi.Update{Message: &tgapi.Message{
		From: &tgapi.User{ID: owner, FirstName: "Moe", Username: "moe"},
		Chat: tgapi.Chat{ID: owner, Type: "private"}, Text: "/start", Date: time.Now().Unix(),
	}})
	code := codeSentTo(t, tg, owner)
	if len(normalize(code)) != codeLength {
		t.Fatalf("code %q", code)
	}
	var reply tgtest.Sent
	for _, m := range tg.Sent() {
		reply = m
	}
	cmd := pairCommand(cfgPath, code)
	if len(reply.Keyboard) != 1 || reply.Keyboard[0][0].Copy == nil || reply.Keyboard[0][0].Copy.Text != cmd {
		t.Errorf("no copy button for the command: %+v", reply.Keyboard)
	}
	f := reply.Formatted()
	var coded, hidden bool
	for _, e := range f.Entities {
		coded = coded || e.Type == "code" && f.Slice(e) == pairCommand(cfgPath, "")
		hidden = hidden || e.Type == "spoiler" && f.Slice(e) == code
	}
	if !coded || !hidden {
		t.Errorf("want the command in monospace and the code hidden: %+v", f.Entities)
	}
	want := "Pairing\nTo pair this bot, run this command on the server:\n\nsudo latch-telegram pair --config " + cfgPath + " " + code + "\n\nThis code works for 10 minutes."
	if reply.Text != want {
		t.Errorf("text:\n%q\nwant:\n%q", reply.Text, want)
	}
	if len(f.Entities) == 0 || f.Entities[0].Type != "bold" || f.Slice(f.Entities[0]) != "Pairing" {
		t.Errorf("the first line must be bold: %+v", f.Entities)
	}
	for _, unwanted := range []string{"ignore", "If you did not"} {
		if strings.Contains(reply.Text, unwanted) {
			t.Errorf("message still says %q: %q", unwanted, reply.Text)
		}
	}

	// The state file holds the hash, not the code.
	raw, err := os.ReadFile(filepath.Join(dir, pairingFile))
	if err != nil || strings.Contains(string(raw), normalize(code)) {
		t.Fatalf("pairing file: %v %s", err, raw)
	}

	// A second message gets the same code, not a new one.
	tg.Message(owner, "hello?")
	tgtest.WaitFor(t, "the resent code", func() bool { return len(tg.Sent()) == 2 })
	if !strings.Contains(tg.Sent()[1].Text, code) {
		t.Errorf("a repeat message must get the same code: %q", tg.Sent()[1].Text)
	}

	var out, errb bytes.Buffer
	args := []string{"--config", cfgPath, "--yes", strings.ToLower(code)}
	if rc := cmdPair(args, strings.NewReader(""), &out, &errb); rc != 0 {
		t.Fatalf("pair: %d %q %q", rc, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "paired with Telegram user 4242 (Moe @moe)") {
		t.Errorf("output: %q", out.String())
	}
	select {
	case c := <-done:
		if c == nil || c.OwnerID != owner {
			t.Fatalf("the bridge did not pick up the owner: %+v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the bridge kept waiting after pair finished")
	}
	if _, err := os.Stat(filepath.Join(dir, pairingFile)); !os.IsNotExist(err) {
		t.Errorf("waiting codes must be removed: %v", err)
	}
}

func TestPairWrongCodeAndConfirmation(t *testing.T) {
	tg := tgtest.New(t)
	cfgPath, dir := pairingSetup(t)
	startDesk(t, tg, cfgPath, dir)
	tg.Message(owner, "hi")
	code := codeSentTo(t, tg, owner)

	var out, errb bytes.Buffer
	if rc := cmdPair([]string{"--config", cfgPath, "--yes", "AAAA-AAAA"}, strings.NewReader(""), &out, &errb); rc != 1 || !strings.Contains(errb.String(), "no waiting request") {
		t.Fatalf("wrong code: %d %q", rc, errb.String())
	}
	if rc := cmdPair([]string{"--config", cfgPath, "--yes", "junk"}, strings.NewReader(""), &out, &errb); rc != 1 || !strings.Contains(errb.String(), "not a pairing code") {
		t.Fatalf("junk: %d %q", rc, errb.String())
	}
	// Declining leaves the config alone; the code is read from stdin when
	// it is not an argument.
	out.Reset()
	errb.Reset()
	if rc := cmdPair([]string{"--config", cfgPath}, strings.NewReader(code+"\nn\n"), &out, &errb); rc != 1 || !strings.Contains(out.String(), "This code was sent to Telegram user 4242") {
		t.Fatalf("declined: %d %q %q", rc, out.String(), errb.String())
	}
	if c, _ := loadConfig(cfgPath); c.OwnerID != 0 {
		t.Fatalf("declining must not pair: %+v", c)
	}
	out.Reset()
	if rc := cmdPair([]string{"--config", cfgPath}, strings.NewReader(code+"\ny\n"), &out, &errb); rc != 0 {
		t.Fatalf("confirmed: %d %q", rc, errb.String())
	}
	if c, _ := loadConfig(cfgPath); c.OwnerID != owner {
		t.Fatalf("owner not written: %+v", c)
	}
	// Paired already: nothing to do.
	errb.Reset()
	if rc := cmdPair([]string{"--config", cfgPath, "--yes", code}, strings.NewReader(""), &out, &errb); rc != 1 || !strings.Contains(errb.String(), "already paired") {
		t.Fatalf("again: %d %q", rc, errb.String())
	}
}

func TestPairingIgnoresGroupsAndBots(t *testing.T) {
	tg := tgtest.New(t)
	cfgPath, dir := pairingSetup(t)
	startDesk(t, tg, cfgPath, dir)
	tg.Update(tgapi.Update{Message: &tgapi.Message{From: &tgapi.User{ID: 9, IsBot: true}, Chat: tgapi.Chat{ID: 9, Type: "private"}, Text: "x", Date: time.Now().Unix()}})
	tg.Update(tgapi.Update{Message: &tgapi.Message{From: &tgapi.User{ID: 9}, Chat: tgapi.Chat{ID: -100, Type: "group"}, Text: "x", Date: time.Now().Unix()}})
	tg.Message(owner, "hi")
	codeSentTo(t, tg, owner)
	if n := len(tg.Sent()); n != 1 {
		t.Errorf("only the private message from a person gets a reply, got %d", n)
	}
}

func TestExpiredCodeIsRefused(t *testing.T) {
	dir := t.TempDir()
	code := "ABCD-EFGH"
	if err := writeWaiting(dir, []waiting{{UserID: 7, Hash: hashCode(code), Expires: time.Now().Add(-time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	cfgPath := writeConfig(t, "owner_id: 0\nstate_dir: "+dir+"\n")
	var out, errb bytes.Buffer
	if rc := cmdPair([]string{"--config", cfgPath, "--yes", code}, strings.NewReader(""), &out, &errb); rc != 1 || !strings.Contains(errb.String(), "expired") {
		t.Fatalf("%d %q", rc, errb.String())
	}
}

func TestPairCommand(t *testing.T) {
	if got := pairCommand(defaultConfig, "ABCD-EFGH"); got != "sudo latch-telegram pair ABCD-EFGH" {
		t.Errorf("default: %q", got)
	}
	if got := pairCommand("/srv/x/t.yaml", "ABCD-EFGH"); got != "sudo latch-telegram pair --config /srv/x/t.yaml ABCD-EFGH" {
		t.Errorf("custom: %q", got)
	}
	if got := pairCommand("/srv/a b;rm/t.yaml", "ABCD-EFGH"); strings.Contains(got, "rm") {
		t.Errorf("an unsafe path must not reach the shell: %q", got)
	}
}

func TestPairedMessage(t *testing.T) {
	plain := pairedMessage(false)
	if plain.S != "Paired." {
		t.Errorf("without login: %q", plain.S)
	}
	withLogin := pairedMessage(true)
	if withLogin.S != "Paired.\n\nYou can now send /login to sign in to ChatGPT." {
		t.Errorf("with login: %q", withLogin.S)
	}
	if len(withLogin.Entities) != 1 || withLogin.Entities[0].Type != "bold" || withLogin.Slice(withLogin.Entities[0]) != "Paired." {
		t.Errorf("the first line must be bold: %+v", withLogin.Entities)
	}
	if tgtest.CheckEntities(withLogin.S, withLogin.Entities) != "" {
		t.Errorf("entities: %+v", withLogin.Entities)
	}
}
