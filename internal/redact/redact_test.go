package redact

import (
	"strings"
	"testing"
)

func TestText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"env", "DB_PASSWORD=hunter2 started", "DB_PASSWORD=[hidden] started"},
		{"colon", "api_key: abc123xyz", "api_key: [hidden]"},
		{"camel", "clientSecret=s3cr3t&x=1", "clientSecret=[hidden]&x=1"},
		{"json", `{"password":"hunter2","user":"bob"}`, `{"password":"[hidden]","user":"bob"}`},
		{"json spaced", `{"access_token": "abc.def"}`, `{"access_token": "[hidden]"}`},
		{"single quotes", `token='abc def'`, `token='[hidden]'`},
		{"query string", "GET /cb?token=abc123&state=ok", "GET /cb?token=[hidden]&state=ok"},
		{"bearer header", "Authorization: Bearer abcdef123456", "Authorization: Bearer [hidden]"},
		{"bearer alone", "sent bearer abcdef123456 to upstream", "sent bearer [hidden] to upstream"},
		{"basic header", "authorization=Basic dXNlcjpwYXNz", "authorization=Basic [hidden]"},
		{"cookie", "Cookie: sessionid=abc123", "Cookie: [hidden]"},
		{"url credentials", "connecting to postgres://app:p4ss@db:5432/app", "connecting to postgres://app:[hidden]@db:5432/app"},
		{"url without password", "see https://example.com:8080/path", "see https://example.com:8080/path"},
		{"github token", "using ghp_" + strings.Repeat("a", 36), "using " + Hidden},
		{"aws key", "key AKIAABCDEFGHIJKLMNOP end", "key " + Hidden + " end"},
		{"jwt", "jwt eyJhbGciOiJIUzI1.eyJzdWIiOiIxMjM0.abcdefghijkl done", "jwt " + Hidden + " done"},
		{"private key", "-----BEGIN RSA PRIVATE KEY-----\nMIIE\nabc\n-----END RSA PRIVATE KEY-----\nafter", "[hidden]\nafter"},
		{"private key without end", "key -----BEGIN PRIVATE KEY----- MIIEvQIBADANB", "key [hidden]"},
		{"already hidden", "password=[hidden]", "password=[hidden]"},
		{"empty value", "password=", "password="},
		{"two on a line", "user=a password=b token=c", "user=a password=[hidden] token=[hidden]"},

		// Ordinary log lines must pass through unchanged.
		{"acme", "authorization finalized for example.com", "authorization finalized for example.com"},
		{"secret flag", "crafty: secret flag set", "crafty: secret flag set"},
		{"stack trace", "at Foo(Guid id, CancellationToken cancellationToken)", "at Foo(Guid id, CancellationToken cancellationToken)"},
		{"plain", "listening on :8080 time=2026-10-24T10:00:00Z", "listening on :8080 time=2026-10-24T10:00:00Z"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Text(c.in); got != c.want {
				t.Errorf("Text(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}

func TestValue(t *testing.T) {
	for _, c := range []struct{ name, value, want string }{
		{"db_password", "hunter2", Hidden},
		{"note", "token=abc123", Hidden},
		{"com.docker.compose.project", "media", "media"},
		{"com.docker.compose.project.config_files", "/srv/media/compose.yaml", "/srv/media/compose.yaml"},
		{"description", "a web server", "a web server"},
	} {
		if got := Value(c.name, c.value); got != c.want {
			t.Errorf("Value(%q, %q) = %q, want %q", c.name, c.value, got, c.want)
		}
	}
}

func TestTextIdempotent(t *testing.T) {
	in := "password=abc Authorization: Bearer abcdef123456 postgres://u:p@h/db"
	once := Text(in)
	if twice := Text(once); twice != once {
		t.Errorf("not idempotent: %q then %q", once, twice)
	}
}

func TestTextLongLineIsFast(t *testing.T) {
	line := strings.Repeat("a-b_c.d=e ", 30000) + "password=x"
	if got := Text(line); !strings.HasSuffix(got, "password=[hidden]") {
		t.Errorf("tail not redacted: %q", got[len(got)-30:])
	}
}

func BenchmarkText(b *testing.B) {
	line := `2026-10-24T10:00:00Z level=info msg="request done" path=/api/items status=200 user=bob duration=12ms`
	for b.Loop() {
		Text(line)
	}
}
