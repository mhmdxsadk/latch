// Package redact hides values that look like secrets in text Latch returns to
// the agent: passwords and tokens in "KEY=value" and "key: value" pairs, JSON
// fields, URL credentials, authorization headers, private key blocks and
// well-known token formats.
//
// It is best effort and works on patterns, so it cannot catch a secret that
// looks like any other text. It is a second line of defence behind excluding
// the resources that hold secrets.
package redact

import (
	"regexp"
	"strings"
)

// Hidden replaces every redacted value.
const Hidden = "[hidden]"

const (
	// sep is the separator after a name, with any closing quote of the name.
	sep = `(["']?[ \t]*[:=][ \t]*)`
	// value is a quoted value, or an unquoted one up to a delimiter, with an
	// optional authorization scheme.
	value = `("[^"\n]*"|'[^'\n]*'|(?:(?:bearer|basic)[ \t]+)?(?:\[hidden\]|[^\s,;&"'}\])]+))`
)

var (
	// pair matches "name<sep>value" where the name contains a secret-looking
	// word. Groups: 1 name, 2 separator (with any closing quote of the name),
	// 3 value.
	pair = regexp.MustCompile(`(?i)([a-z0-9_.-]*(?:passwords?|passwd|passphrase|pwd|secrets?|tokens?|api[_-]?key|apikey|access[_-]?key|private[_-]?key|key[_-]?data|credentials?|authorization|cookie|session[_-]?(?:id|key))[a-z0-9_.-]*)` + sep + value)

	// shortPair is pair for names that END in the short words "pass" or
	// "auth" (DB_PASS, "auth" in .docker/config.json), which would match far
	// too much inside longer words (bypass, author).
	shortPair = regexp.MustCompile(`(?i)\b((?:[a-z0-9]+[_.-])*(?:pass|auth))` + sep + value)

	// netrc matches the password of a .netrc line ("login bob password x")
	// or a line that starts with "password".
	netrc = regexp.MustCompile(`(?im)(^[ \t]*password[ \t]+|\blogin[ \t]+\S+[ \t]+password[ \t]+)(\S+)`)

	// scheme matches an authorization scheme followed by a credential.
	scheme = regexp.MustCompile(`(?i)\b(bearer|basic)([ \t]+)[a-z0-9._~+/=-]{8,}`)

	// urlCreds matches the password in scheme://user:password@host.
	urlCreds = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]*:)[^\s@/]+(@)`)

	// privateKey matches a PEM private key block, or the rest of the text when
	// the end marker is missing (a line of a log, a truncated file).
	privateKey = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`)

	// known matches credentials with a recognisable shape.
	known = regexp.MustCompile(strings.Join([]string{
		`\bgh[pousr]_[A-Za-z0-9]{30,}`,
		`\bgithub_pat_[A-Za-z0-9_]{30,}`,
		`\bglpat-[A-Za-z0-9_-]{16,}`,
		`\bxox[abprs]-[A-Za-z0-9-]{10,}`,
		`\b[sr]k_(?:live|test)_[A-Za-z0-9]{16,}`,
		`\bsk-[A-Za-z0-9_-]{20,}`,
		`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`,
		`\bAIza[0-9A-Za-z_-]{35}\b`,
		`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`,
	}, "|"))
)

// Text returns s with secret-looking values replaced by Hidden.
func Text(s string) string {
	if s == "" {
		return s
	}
	if strings.Contains(s, "-----BEGIN") {
		s = privateKey.ReplaceAllString(s, Hidden)
	}
	// The patterns are slow next to a substring scan and most lines match
	// none of them, so a lowercase copy is checked for their literals first.
	lower := strings.ToLower(s)
	if containsAny(lower, pairWords) {
		for _, re := range []*regexp.Regexp{pair, shortPair} {
			s = re.ReplaceAllStringFunc(s, func(m string) string {
				sub := re.FindStringSubmatch(m)
				return sub[1] + sub[2] + hide(sub[3])
			})
		}
		if strings.Contains(lower, "password") {
			s = netrc.ReplaceAllStringFunc(s, func(m string) string {
				sub := netrc.FindStringSubmatch(m)
				return sub[1] + hide(sub[2])
			})
		}
	}
	if strings.Contains(lower, "bearer") || strings.Contains(lower, "basic") {
		s = scheme.ReplaceAllString(s, "${1}${2}"+Hidden)
	}
	if strings.Contains(s, "://") {
		s = urlCreds.ReplaceAllString(s, "${1}"+Hidden+"${2}")
	}
	if containsAny(lower, knownPrefixes) {
		s = known.ReplaceAllString(s, Hidden)
	}
	return s
}

// pairWords are the literals pair needs one of; knownPrefixes likewise for
// known (lowercase, since they are searched in a lowercase copy).
var (
	pairWords     = []string{"pass", "pwd", "secret", "token", "key", "credential", "auth", "cookie", "session"}
	knownPrefixes = []string{"gh", "github_pat_", "glpat-", "xox", "_live_", "_test_", "sk-", "akia", "asia", "aiza", "eyj"}
)

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// hide replaces a matched value, keeping its quotes and any authorization
// scheme so JSON stays valid and the text stays readable.
func hide(v string) string {
	if v == Hidden {
		return v
	}
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		if v[1:len(v)-1] == Hidden {
			return v
		}
		return v[:1] + Hidden + v[len(v)-1:]
	}
	if f := strings.Fields(v); len(f) == 2 {
		return f[0] + " " + Hidden
	}
	return Hidden
}

// Value redacts the value of a named setting, such as a container label: it
// returns Hidden when the name or the value looks like a secret.
func Value(name, value string) string {
	if pair := name + "=" + value; Text(pair) != pair {
		return Hidden
	}
	return value
}
