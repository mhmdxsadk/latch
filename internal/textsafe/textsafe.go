// Package textsafe makes text from the host safe to show an agent: valid
// UTF-8 with no terminal escapes, control characters or invisible characters
// that could disguise it or smuggle hidden data. Pair it with package redact,
// which hides secret-looking values.
package textsafe

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// escape matches terminal escape sequences: CSI, terminated string
// sequences (OSC, DCS, SOS, PM, APC) and two-character escapes. An
// unterminated string sequence loses only its introducer, so it cannot hide
// the rest of the line.
var escape = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|[\]PX^_][^\x07\x1b]*(?:\x07|\x1b\\)|[ -/]*[0-~])`)

// Line makes s valid UTF-8 and removes escape sequences, control
// characters other than tab, and invisible characters (bidi overrides,
// zero-width and tag characters, variation selectors, fillers) that could
// disguise the text or smuggle hidden data. Line and paragraph separators
// become spaces so one entry cannot pose as several.
func Line(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if strings.IndexByte(s, 0x1b) >= 0 {
		s = escape.ReplaceAllString(s, "")
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 0x20 && r < 0x7f: // printable ASCII, the common case
			return r
		case r == '\t':
			return r
		case unicode.In(r, unicode.Zl, unicode.Zp):
			return ' '
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Variation_Selector, unicode.Other_Default_Ignorable_Code_Point):
			return -1
		}
		return r
	}, s)
}

// Head returns the first n bytes of s, ending on a UTF-8 boundary.
func Head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// s is valid UTF-8, so at most a few bytes of a cut rune need dropping.
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// Tail returns the last n bytes of s, starting on a UTF-8 boundary.
func Tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// Lines applies Line to every line of s, keeping the line breaks (a trailing
// "\r" on each line is dropped).
func Lines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = Line(strings.TrimSuffix(l, "\r"))
	}
	return strings.Join(lines, "\n")
}
