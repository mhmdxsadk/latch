package tgchat

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mhmdxsadk/latch/internal/textsafe"
	"github.com/mhmdxsadk/latch/internal/tgapi"
)

var (
	fenceLine   = regexp.MustCompile("^\\s*```\\s*([A-Za-z0-9_+-]{0,20})\\s*$")
	headingLine = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$`)
	ruleLine    = regexp.MustCompile(`^\s{0,3}(-(\s*-){2,}|\*(\s*\*){2,}|_(\s*_){2,})\s*$`)
	bulletLine  = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	quoteLine   = regexp.MustCompile(`^\s{0,3}>\s?(.*)$`)
	tableLine   = regexp.MustCompile(`^\s*\|.*\|\s*$`)
	tableRule   = regexp.MustCompile(`^\s*\|?(\s*:?-{3,}:?\s*\|)+\s*:?-*:?\s*$`)
	linkPattern = regexp.MustCompile(`^\[([^\]\n]{1,300})\]\(([^)\s]{1,500})\)`)
)

// Markdown turns the Markdown an agent writes into Telegram formatting:
// italic, strikethrough, quotes and bullets (•). Bold and headings are
// plain text: the owner does not want the first sentence shouting.
// Inline code and short code blocks become plain text, long code blocks a
// collapsed quote: the owner reads answers, they do not copy them, so
// nothing is boxed or given a copy button. Tables become
// a fixed-width block so their columns line up. Links become "text
// (address)", so where a link goes is always visible; no link entity is ever
// made. Every line is cleaned with textsafe first, and emoji are removed:
// the owner wants none.
func Markdown(src string) tgapi.Text {
	src = noEmoji(src)
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = textsafe.Line(l)
	}
	var b tgapi.Builder
	newline := func() {
		if b.Len() > 0 {
			b.Plain("\n")
		}
	}
	blank := false // the last line written was empty
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if strings.TrimSpace(l) == "" || ruleLine.MatchString(l) {
			// Blank lines and horizontal rules: at most one empty line.
			if !blank && b.Len() > 0 {
				b.Plain("\n")
			}
			blank = true
			continue
		}
		blank = false
		switch {
		case fenceLine.MatchString(l):
			var body []string
			for i++; i < len(lines) && !isFenceClose(lines[i]); i++ {
				body = append(body, lines[i])
			}
			newline()
			code := tgapi.Plain(strings.TrimRight(strings.Join(body, "\n"), "\n "))
			if len(body) > collapseAfter {
				b.Quote(code, true)
			} else {
				b.Text(code)
			}
		case tableLine.MatchString(l):
			var rows []string
			for ; i < len(lines) && tableLine.MatchString(lines[i]); i++ {
				if !tableRule.MatchString(lines[i]) {
					rows = append(rows, strings.TrimSpace(lines[i]))
				}
			}
			i--
			newline()
			b.Pre(strings.Join(rows, "\n"), "")
		case quoteLine.MatchString(l):
			var q tgapi.Builder
			for ; i < len(lines) && quoteLine.MatchString(lines[i]); i++ {
				if q.Len() > 0 {
					q.Plain("\n")
				}
				q.Text(inline(quoteLine.FindStringSubmatch(lines[i])[1]))
			}
			i--
			newline()
			b.Text(wrap(q.Done(), "blockquote"))
		case headingLine.MatchString(l):
			newline()
			b.Text(inline(headingLine.FindStringSubmatch(l)[1]))
		case bulletLine.MatchString(l):
			m := bulletLine.FindStringSubmatch(l)
			newline()
			b.Plain(strings.Repeat("  ", min(len(m[1])/2, 4)) + "• ")
			b.Text(inline(m[2]))
		default:
			newline()
			b.Text(inline(l))
		}
	}
	t := b.Done()
	t.S = strings.TrimRight(t.S, "\n ")
	return tidy(t)
}

func isFenceClose(l string) bool {
	t := strings.TrimSpace(l)
	return strings.HasPrefix(t, "```") && strings.Trim(t, "`") == ""
}

// collapseAfter is how many lines a code block may have before it is
// collapsed until tapped.
const collapseAfter = 6

// inline formats one line: **bold** and __bold__ (printed plain), *italic*, _italic_,
// ~~strike~~, links and backslash escapes; `code` loses its backticks.
func inline(s string) tgapi.Text {
	var b tgapi.Builder
	plain := strings.Builder{}
	flush := func() {
		if plain.Len() > 0 {
			b.Plain(plain.String())
			plain.Reset()
		}
	}
	for i := 0; i < len(s); {
		rest := s[i:]
		switch {
		case rest[0] == '\\' && len(rest) > 1 && isPunct(rest[1]):
			plain.WriteByte(rest[1])
			i += 2
			continue
		case rest[0] == '`':
			if j := strings.IndexByte(rest[1:], '`'); j > 0 {
				flush()
				b.Plain(rest[1 : 1+j])
				i += j + 2
				continue
			}
		case strings.HasPrefix(rest, "**"), strings.HasPrefix(rest, "__"):
			if j := strings.Index(rest[2:], rest[:2]); j > 0 && !unicode.IsSpace(rune(rest[2])) {
				flush()
				b.Text(inline(rest[2 : 2+j]))
				i += j + 4
				continue
			}
		case strings.HasPrefix(rest, "~~"):
			if j := strings.Index(rest[2:], "~~"); j > 0 {
				flush()
				b.Text(wrap(inline(rest[2:2+j]), "strikethrough"))
				i += j + 4
				continue
			}
		case rest[0] == '*' || rest[0] == '_':
			if j, ok := emphasisEnd(s, i); ok {
				flush()
				b.Text(wrap(inline(s[i+1:j]), "italic"))
				i = j + 1
				continue
			}
		case rest[0] == '[':
			if m := linkPattern.FindStringSubmatch(rest); m != nil {
				flush()
				if m[1] == m[2] {
					b.Plain(m[2])
				} else {
					b.Plain(m[1] + " (" + m[2] + ")")
				}
				i += len(m[0])
				continue
			}
		}
		_, n := utf8.DecodeRuneInString(rest)
		plain.WriteString(rest[:n])
		i += n
	}
	flush()
	return b.Done()
}

// emphasisEnd finds the closing marker of *italic* or _italic_ starting at
// i. Underscores only count at word edges, so names like file_create stay
// as they are.
func emphasisEnd(s string, i int) (int, bool) {
	m := s[i]
	if i+1 >= len(s) || unicode.IsSpace(rune(s[i+1])) || s[i+1] == m {
		return 0, false
	}
	if m == '_' && i > 0 && isWord(s[i-1]) {
		return 0, false
	}
	for j := i + 1; j < len(s); j++ {
		if s[j] != m || unicode.IsSpace(rune(s[j-1])) {
			continue
		}
		if m == '_' && j+1 < len(s) && isWord(s[j+1]) {
			continue
		}
		return j, true
	}
	return 0, false
}

func isWord(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isPunct(c byte) bool { return strings.IndexByte("\\`*_{}[]()#+-.!|~>", c) >= 0 }

// wrap formats all of t as typ, keeping t's own formatting inside.
func wrap(t tgapi.Text, typ string) tgapi.Text {
	if t.S == "" {
		return t
	}
	t.Entities = append([]tgapi.Entity{{Type: typ, Offset: 0, Length: t.Len()}}, t.Entities...)
	return t
}

// tidy drops empty or out-of-range entities and sorts them, outer first.
func tidy(t tgapi.Text) tgapi.Text {
	n := t.Len()
	kept := t.Entities[:0:0]
	for _, e := range t.Entities {
		if e.Offset >= n || e.Length <= 0 {
			continue
		}
		e.Length = min(e.Length, n-e.Offset)
		kept = append(kept, e)
	}
	slices.SortStableFunc(kept, func(a, b tgapi.Entity) int {
		if a.Offset != b.Offset {
			return a.Offset - b.Offset
		}
		return b.Length - a.Length
	})
	t.Entities = kept
	return t
}

// noEmoji removes emoji: pictographs, symbols and dingbats drawn as emoji,
// flags, keycaps and the joiners and selectors that build them. Plain
// punctuation such as arrows, bullets and dashes stays.
func noEmoji(s string) string {
	if utf8.ValidString(s) && !strings.ContainsFunc(s, isEmoji) {
		return s
	}
	var b strings.Builder
	removed := false // just removed an emoji
	last := '\n'     // the last rune written; a line start counts as space
	for _, r := range s {
		switch {
		case isEmoji(r):
			removed = true
			continue
		case removed && r == ' ' && (last == ' ' || last == '\n'):
			// "done ✅ next" becomes "done next", not "done  next".
			continue
		}
		removed = false
		b.WriteRune(r)
		last = r
	}
	return b.String()
}

func isEmoji(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF, // pictographs, emoticons, transport, symbols
		r >= 0x2600 && r <= 0x27BF,   // miscellaneous symbols and dingbats
		r >= 0x2B05 && r <= 0x2B55,   // arrows and shapes drawn as emoji
		r >= 0x1F1E6 && r <= 0x1F1FF, // flags
		r == 0x20E3,                  // keycap
		r == 0x200D,                  // zero-width joiner
		r >= 0xFE00 && r <= 0xFE0F,   // variation selectors
		r >= 0xE0020 && r <= 0xE007F, // tag characters
		r == 0x231A, r == 0x231B, r == 0x23F0, r == 0x23F3, r >= 0x23E9 && r <= 0x23EC,
		r == 0x2139, r == 0x24C2, r == 0x3030, r == 0x303D, r == 0x3297, r == 0x3299:
		return true
	}
	return false
}
