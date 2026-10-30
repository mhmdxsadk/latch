// Package chatgptauth signs in to a ChatGPT account the way OpenAI's Codex
// CLI does, in a form that works from a phone: Begin makes the sign-in link,
// the owner approves in any browser, and Complete takes the address the
// browser then fails to load and turns its code into credentials.
//
// OpenAI registered http://localhost:1455/auth/callback as the only redirect
// for this public client, so the browser always lands on its own machine's
// port 1455, never on the server. The code is in that address, which the
// owner pastes back. The credentials use the file layout Docker Agent reads
// (chatgpt-auth.json in its config directory) and refreshes on its own.
package chatgptauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// FileName is the credentials file in Docker Agent's config directory.
	FileName = "chatgpt-auth.json"

	// Valid is how long a sign-in link works.
	Valid = 10 * time.Minute

	// clientID is OpenAI's public OAuth client for Codex CLI sign-in. It is
	// not a secret; PKCE protects the exchange.
	clientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	originator  = "codex_cli_rs"
	redirectURI = "http://localhost:1455/auth/callback"
	scope       = "openid profile email offline_access"
	maxBody     = 1 << 16
)

// authBase is the OAuth issuer. A variable so tests can use a local server.
var authBase = "https://auth.openai.com"

// Credentials is the file Docker Agent reads. Field names match its layout.
type Credentials struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	IDToken      string    `json:"id_token,omitempty"`
	AccountID    string    `json:"account_id,omitempty"`
	Email        string    `json:"email,omitempty"`
	Plan         string    `json:"plan,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"`
	LastRefresh  time.Time `json:"last_refresh,omitzero"`
}

// JSON is the file's content.
func (c *Credentials) JSON() ([]byte, error) {
	return json.MarshalIndent(c, "", "  ") //nolint:gosec // this is the credentials file itself
}

// Login is one sign-in in progress. It works once.
type Login struct {
	verifier string
	state    string
	made     time.Time
	now      func() time.Time
}

// Begin starts a sign-in.
func Begin() (*Login, error) {
	verifier, err := random(32)
	if err != nil {
		return nil, err
	}
	state, err := random(32)
	if err != nil {
		return nil, err
	}
	return &Login{verifier: verifier, state: state, made: time.Now(), now: time.Now}, nil
}

func random(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("making a random value failed: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// URL is the link the owner opens to approve the sign-in.
func (l *Login) URL() string {
	sum := sha256.Sum256([]byte(l.verifier))
	q := url.Values{
		"response_type":              {"code"},
		"client_id":                  {clientID},
		"redirect_uri":               {redirectURI},
		"scope":                      {scope},
		"code_challenge":             {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method":      {"S256"},
		"state":                      {l.state},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"originator":                 {originator},
	}
	return authBase + "/oauth/authorize?" + q.Encode()
}

// Expired reports whether the link has passed its time.
func (l *Login) Expired() bool { return l.now().Sub(l.made) > Valid }

// ErrNotCallback means the pasted text is not the browser's redirect
// address. The sign-in is still waiting; the owner can paste again.
var ErrNotCallback = errors.New("that is not the address from the error page; it starts with http://localhost:1455/auth/callback?code=")

// Code reads the authorization code from what the owner pasted: the full
// address, or just its query. It checks the state, so a code from another
// sign-in is refused.
func (l *Login) Code(pasted string) (string, error) {
	s := strings.Trim(strings.TrimSpace(pasted), "<>\"'`")
	var q url.Values
	switch {
	case strings.HasPrefix(s, "code=") || strings.HasPrefix(s, "state=") || strings.HasPrefix(s, "error="):
		v, err := url.ParseQuery(s)
		if err != nil {
			return "", ErrNotCallback
		}
		q = v
	default:
		if !strings.Contains(s, "://") {
			s = "http://" + s
		}
		u, err := url.Parse(s)
		if err != nil || u.Path != "/auth/callback" || (u.Host != "localhost:1455" && u.Host != "127.0.0.1:1455") {
			return "", ErrNotCallback
		}
		q = u.Query()
	}
	if e := q.Get("error"); e != "" {
		return "", fmt.Errorf("OpenAI refused the sign-in: %s", clean(e+": "+q.Get("error_description")))
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(l.state)) != 1 {
		return "", errors.New("that address belongs to a different sign-in; start again with /login")
	}
	code := q.Get("code")
	if code == "" {
		return "", ErrNotCallback
	}
	return code, nil
}

// Complete trades the code in pasted for credentials. The code works once,
// so a failure here ends this sign-in.
func (l *Login) Complete(ctx context.Context, pasted string) (*Credentials, error) {
	code, err := l.Code(pasted)
	if err != nil {
		return nil, err
	}
	if l.Expired() {
		return nil, errors.New("that sign-in link expired; start again with /login")
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {l.verifier},
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authBase+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // the fixed OpenAI issuer
	if err != nil {
		return nil, fmt.Errorf("could not reach OpenAI: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("reading OpenAI's answer failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The body can echo request values; show only a short clean excerpt.
		return nil, fmt.Errorf("OpenAI refused the code (HTTP %d: %s)", resp.StatusCode, excerpt(body))
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
		return nil, errors.New("OpenAI's answer had no access token")
	}
	now := l.now()
	c := &Credentials{AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, IDToken: tok.IDToken, LastRefresh: now}
	for _, t := range []string{tok.AccessToken, tok.IDToken} {
		cl := parseClaims(t)
		if cl.AccountID != "" {
			c.AccountID = cl.AccountID
		}
		if cl.Email != "" {
			c.Email = cl.Email
		}
		if cl.Plan != "" {
			c.Plan = cl.Plan
		}
		if t == tok.AccessToken && !cl.Expires.IsZero() {
			c.ExpiresAt = cl.Expires
		}
	}
	if tok.ExpiresIn > 0 {
		c.ExpiresAt = now.Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	return c, nil
}

type claims struct {
	AccountID, Email, Plan string
	Expires                time.Time
}

// parseClaims reads OpenAI's claims from a JWT without checking its
// signature. The token came straight from the issuer over TLS and is only
// read here to label the sign-in, never trusted for access.
func parseClaims(token string) claims {
	var out claims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return out
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return out
	}
	var p struct {
		Email string `json:"email"`
		Exp   int64  `json:"exp"`
		Auth  struct {
			AccountID string `json:"chatgpt_account_id"`
			Plan      string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return out
	}
	out.Email, out.AccountID, out.Plan = p.Email, p.Auth.AccountID, p.Auth.Plan
	if p.Exp > 0 {
		out.Expires = time.Unix(p.Exp, 0)
	}
	return out
}

// clean makes text from the network fit on one short line.
func clean(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func excerpt(b []byte) string { return clean(string(b)) }
