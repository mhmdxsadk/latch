package textsafe

import "testing"

func TestLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{"safe\u202etxt.exe\u200b!", "safetxt.exe!"},                 // bidi override, zero-width space
		{"a\U000E0041\U000E0042b", "ab"},                             // tag characters
		{"ok\U0001F600\ufe0f\U000E0100\U000E01EF!", "ok\U0001F600!"}, // variation selectors hiding data
		{"x\u3164\u115f\u034fy", "xy"},                               // fillers, grapheme joiner
		{"one\u2028two\u2029three", "one two three"},                 // fake line breaks
		{"\x1b[31mred\x1b[0m", "red"},                                // CSI
		{"\x1b]0;title\x07text", "text"},                             // OSC with BEL
		{"\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\", "link"},         // OSC 8 hyperlink
		{"\x1bPdata\x1b\\after", "after"},                            // DCS
		{"\x1b(Bplain\x1b=", "plain"},                                // two-character escapes
		{"\x1b]unterminated rest", "unterminated rest"},              // cannot hide the line
		{"bad\xffutf8", "bad\ufffdutf8"},
		{"tab\tkept\r\x00\u009b", "tab\tkept"},
		{"\u00e9moji \U0001F44D \u4e2d\u6587 \u05e2\u05d1\u05e8\u05d9\u05ea", "\u00e9moji \U0001F44D \u4e2d\u6587 \u05e2\u05d1\u05e8\u05d9\u05ea"}, // ordinary text untouched
	}
	for _, c := range cases {
		if got := Line(c.in); got != c.want {
			t.Errorf("Line(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLines(t *testing.T) {
	if got := Lines("a\x1b[1m\r\nb\u200b\n"); got != "a\nb\n" {
		t.Errorf("got %q", got)
	}
}
