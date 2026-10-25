package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultConfig      = "/etc/latch/telegram.yaml"
	defaultTokenFile   = "/etc/latch/telegram-token" //nolint:gosec // a path, not a credential
	defaultAdminSocket = "/run/latch/admin.sock"
	defaultStateDir    = "/var/lib/latch-telegram"
	defaultCommand     = "/usr/local/bin/docker-agent"
	// credentialName is a systemd credential that, when a unit override
	// passes one (LoadCredential=telegram-token:PATH), replaces token_file.
	credentialName = "telegram-token"
	maxConfigBytes = 64 << 10
	maxTimeout     = time.Hour
)

// Config is /etc/latch/telegram.yaml. Unknown keys are refused.
type Config struct {
	// OwnerID is the owner's Telegram user id. Nobody else is answered.
	OwnerID   int64  `yaml:"owner_id"`
	TokenFile string `yaml:"token_file"`
	// TokenKey, when set, reads token_file as KEY=value lines (an env file,
	// for example one a secrets manager writes) and takes this key.
	TokenKey    string `yaml:"token_key"`
	AdminSocket string `yaml:"admin_socket"`
	StateDir    string `yaml:"state_dir"`
	// Agent is optional: without it the bot only handles approvals.
	Agent *AgentConfig `yaml:"agent"`
}

// AgentConfig says how to run the agent team for chat messages.
type AgentConfig struct {
	// Name labels the agent's answers in the chat.
	Name string `yaml:"name"`
	// User is the Linux account the agent runs as (never root).
	User    string   `yaml:"user"`
	Command string   `yaml:"command"`
	Config  string   `yaml:"config"`
	Args    []string `yaml:"args"`
	Timeout Duration `yaml:"timeout"`
}

// Duration is a time.Duration written like "10m".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration like 10m", n.Line, n.Value)
	}
	*d = Duration(v)
	return nil
}

var (
	userName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	label    = regexp.MustCompile(`^[A-Za-z0-9 _.-]{1,32}$`)
	token    = regexp.MustCompile(`^[0-9]{1,20}:[A-Za-z0-9_-]{30,100}$`)
	envKey   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	// argPattern keeps extra args to plain flags and values.
	argPattern = regexp.MustCompile(`^[A-Za-z0-9_./=:,@+-]{1,200}$`)
)

func loadConfig(path string) (*Config, error) {
	f, err := os.Open(path) //nolint:gosec // owner-supplied config location
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxConfigBytes {
		return nil, errors.New("config file is too large")
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, cleanYAMLError(err)
	}
	c.defaults()
	return &c, c.validate()
}

func cleanYAMLError(err error) error {
	var te *yaml.TypeError
	if errors.As(err, &te) {
		return errors.New(strings.Join(te.Errors, "; "))
	}
	return errors.New(strings.TrimPrefix(err.Error(), "yaml: "))
}

func (c *Config) defaults() {
	if c.TokenFile == "" {
		c.TokenFile = defaultTokenFile
	}
	if c.AdminSocket == "" {
		c.AdminSocket = defaultAdminSocket
	}
	if c.StateDir == "" {
		c.StateDir = defaultStateDir
	}
	if a := c.Agent; a != nil {
		if a.Command == "" {
			a.Command = defaultCommand
		}
		if a.Name == "" {
			a.Name = a.User
		}
		if a.Timeout == 0 {
			a.Timeout = Duration(10 * time.Minute)
		}
	}
}

func (c *Config) validate() error {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	if c.OwnerID <= 0 {
		add("owner_id: set it to your Telegram user id (a positive number)")
	}
	absPath := func(name, p string) {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			add("%s: %q must be a clean absolute path", name, p)
		}
	}
	absPath("token_file", c.TokenFile)
	if c.TokenKey != "" && !envKey.MatchString(c.TokenKey) {
		add("token_key: %q must be an environment variable name like TELEGRAM_BOT_TOKEN", c.TokenKey)
	}
	absPath("admin_socket", c.AdminSocket)
	absPath("state_dir", c.StateDir)
	if a := c.Agent; a != nil {
		switch {
		case a.User == "":
			add("agent.user: set the Linux account the agent runs as")
		case a.User == "root" || !userName.MatchString(a.User):
			add("agent.user: %q is not allowed (a normal account name, never root)", a.User)
		}
		if !label.MatchString(a.Name) {
			add("agent.name: %q must be 1 to 32 letters, digits, spaces, _ . or -", a.Name)
		}
		absPath("agent.command", a.Command)
		absPath("agent.config", a.Config)
		for _, arg := range a.Args {
			if !argPattern.MatchString(arg) || arg == "-" {
				add("agent.args: %q is not allowed (plain flags and values only)", arg)
			}
		}
		if d := time.Duration(a.Timeout); d < 10*time.Second || d > maxTimeout {
			add("agent.timeout: %s must be between 10s and 1h", d)
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "\n"))
	}
	return nil
}

// readToken reads the bot token: the systemd credential when the unit passes
// one, otherwise token_file, which must be readable by its owner only. With
// token_key the file is KEY=value lines and the token is that key's value.
// Errors never include the file's content.
func readToken(c *Config) (string, error) {
	path := c.TokenFile
	if dir := os.Getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		if p := filepath.Join(dir, credentialName); fileExists(p) {
			path = p
		}
	}
	fi, err := os.Stat(path) //nolint:gosec // owner-supplied token location
	if err != nil {
		return "", err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s: other accounts can read it (mode %04o); run chmod 600 %s", path, fi.Mode().Perm(), path)
	}
	b, err := os.ReadFile(path) //nolint:gosec // owner-supplied token location
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if c.TokenKey != "" {
		v, ok := envValue(string(b), c.TokenKey)
		if !ok {
			return "", fmt.Errorf("%s: no %s line", path, c.TokenKey)
		}
		t = strings.TrimSpace(v)
	}
	if !token.MatchString(t) {
		what := path
		if c.TokenKey != "" {
			what += ": " + c.TokenKey
		}
		return "", fmt.Errorf("%s: does not look like a bot token from @BotFather", what)
	}
	return t, nil
}

// envValue returns key's value from env-file text: KEY=value lines, with
// optional "export ", and a value that is bare, 'single-quoted' or a
// "double-quoted" JSON string (the form secrets managers write with jq @json).
// The last line for key wins.
func envValue(text, key string) (string, bool) {
	var val string
	found := false
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		v = strings.TrimSpace(v)
		switch {
		case len(v) >= 2 && v[0] == '"':
			var s string
			if json.Unmarshal([]byte(v), &s) != nil {
				return "", false
			}
			v = s
		case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
			v = v[1 : len(v)-1]
		}
		val, found = v, true
	}
	return val, found
}

func fileExists(p string) bool {
	_, err := os.Stat(p) //nolint:gosec // path from the service's own environment
	return err == nil
}
