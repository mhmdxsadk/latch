package chatgptauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func jwt(t *testing.T, payload map[string]any) string {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

func TestURLHasPKCEAndFixedClient(t *testing.T) {
	l, err := Begin()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(l.URL())
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	sum := sha256.Sum256([]byte(l.verifier))
	for k, want := range map[string]string{
		"response_type": "code", "client_id": clientID, "redirect_uri": "http://localhost:1455/auth/callback",
		"code_challenge_method": "S256", "state": l.state, "originator": "codex_cli_rs",
		"code_challenge": base64.RawURLEncoding.EncodeToString(sum[:]),
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	if !strings.HasPrefix(l.URL(), "https://auth.openai.com/oauth/authorize?") {
		t.Errorf("issuer: %s", l.URL())
	}
	l2, _ := Begin()
	if l2.state == l.state || l2.verifier == l.verifier {
		t.Error("two sign-ins must not share a state or verifier")
	}
}

func TestCodeAcceptsWhatTheBrowserShows(t *testing.T) {
	l, _ := Begin()
	good := "code=abc123&state=" + l.state
	for _, in := range []string{
		"http://localhost:1455/auth/callback?" + good,
		"  localhost:1455/auth/callback?" + good + "\n",
		"<http://127.0.0.1:1455/auth/callback?" + good + ">",
		good,
	} {
		if code, err := l.Code(in); err != nil || code != "abc123" {
			t.Errorf("%q: %q %v", in, code, err)
		}
	}
	for in, want := range map[string]string{
		"hello": "not the address",
		"http://localhost:1455/auth/callback?code=x&state=other":                                        "different sign-in",
		"http://localhost:1455/auth/callback?state=" + l.state:                                          "not the address",
		"http://evil.example/auth/callback?" + good:                                                     "not the address",
		"http://localhost:1455/other?" + good:                                                           "not the address",
		"http://localhost:1455/auth/callback?error=access_denied&error_description=no&state=" + l.state: "refused the sign-in: access_denied: no",
	} {
		if _, err := l.Code(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", in, err, want)
		}
	}
}

func TestComplete(t *testing.T) {
	var form url.Values
	access := jwt(t, map[string]any{
		"email": "moe@example.com", "exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-1", "chatgpt_plan_type": "plus"},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		form = r.PostForm
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": access, "refresh_token": "r1", "id_token": access, "expires_in": 3600})
	}))
	defer srv.Close()
	old := authBase
	authBase = srv.URL
	defer func() { authBase = old }()

	l, _ := Begin()
	c, err := l.Complete(context.Background(), "http://localhost:1455/auth/callback?code=thecode&state="+l.state)
	if err != nil {
		t.Fatal(err)
	}
	if form.Get("grant_type") != "authorization_code" || form.Get("code") != "thecode" || form.Get("code_verifier") != l.verifier ||
		form.Get("client_id") != clientID || form.Get("redirect_uri") != redirectURI {
		t.Errorf("token request: %v", form)
	}
	if c.AccessToken != access || c.RefreshToken != "r1" || c.Email != "moe@example.com" || c.Plan != "plus" || c.AccountID != "acct-1" {
		t.Errorf("credentials: %+v", c)
	}
	if d := time.Until(c.ExpiresAt); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("expiry %v", c.ExpiresAt)
	}
	b, _ := c.JSON()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || m["access_token"] != access || m["refresh_token"] != "r1" || m["email"] != "moe@example.com" {
		t.Errorf("file layout: %s", b)
	}
}

func TestCompleteFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("{\"error\":\"invalid_grant\"}\n\x1b[31m"))
	}))
	defer srv.Close()
	old := authBase
	authBase = srv.URL
	defer func() { authBase = old }()

	l, _ := Begin()
	_, err := l.Complete(context.Background(), "code=x&state="+l.state)
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") || strings.ContainsRune(err.Error(), 0x1b) {
		t.Errorf("refused code: %v", err)
	}
	l.now = func() time.Time { return time.Now().Add(Valid + time.Minute) }
	if _, err := l.Complete(context.Background(), "code=x&state="+l.state); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired: %v", err)
	}
}
