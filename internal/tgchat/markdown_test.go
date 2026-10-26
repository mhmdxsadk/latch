package tgchat

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mhmdxsadk/latch/internal/tgapi"
	"github.com/mhmdxsadk/latch/internal/tgapi/tgtest"
)

// show renders t as text with [type:...] around each entity, for tests.
func show(t tgapi.Text) string {
	var parts []string
	for _, e := range t.Entities {
		parts = append(parts, fmt.Sprintf("%s=%q", e.Type, t.Slice(e)))
	}
	return t.S + " | " + strings.Join(parts, " ")
}

func TestMarkdown(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain text", `plain text | `},
		{"Request **5** is waiting", `Request 5 is waiting | bold="5"`},
		{"run `latch approve 5` now", `run latch approve 5 now | `},
		{"Fixed it 🎉✅ - all good 👍🏽 → next 🇪🇸", `Fixed it - all good → next | `},
		{"🚨 Jellyfin is down", `Jellyfin is down | `},
		{"plain • bullets · dots", `plain • bullets · dots | `},
		{"*italic* and _also_", `italic and also | italic="italic" italic="also"`},
		{"file_create and some_name_here stay", `file_create and some_name_here stay | `},
		{"2 * 3 * 4", `2 * 3 * 4 | `},
		{"~~old~~ new", `old new | strikethrough="old"`},
		{"**bold with `code`**", `bold with code | bold="bold with code"`},
		{"```\n1\n2\n3\n4\n5\n6\n7\n```", "1\n2\n3\n4\n5\n6\n7 | expandable_blockquote=\"1\\n2\\n3\\n4\\n5\\n6\\n7\""},
		{"## Summary", `Summary | bold="Summary"`},
		{"- one\n- two\n  - nested", "• one\n• two\n  • nested | "},
		{"see [the docs](https://example.com/x)", `see the docs (https://example.com/x) | `},
		{"[https://a.b](https://a.b)", `https://a.b | `},
		{"```bash\nlatch approve 5\n```", `latch approve 5 | `},
		{"```\nnot closed", `not closed | `},
		{"> quoted **line**\n> two", "quoted line\ntwo | blockquote=\"quoted line\\ntwo\" bold=\"line\""},
		{"| a | b |\n|---|---|\n| 1 | 2 |", "| a | b |\n| 1 | 2 | | pre=\"| a | b |\\n| 1 | 2 |\""},
		{"above\n\n---\n\nbelow", "above\n\nbelow | "},
		{"one\n\n\n\ntwo", "one\n\ntwo | "},
		{`\*not italic\*`, `*not italic* | `},
		{"\x1b[31mred\x1b[0m", `red | `},
	} {
		got := Markdown(tc.in)
		if s := show(got); s != tc.want {
			t.Errorf("Markdown(%q)\n got  %s\n want %s", tc.in, s, tc.want)
		}
		if msg := tgtest.CheckEntities(got.S, got.Entities); msg != "" {
			t.Errorf("Markdown(%q): Telegram would refuse: %s", tc.in, msg)
		}
	}
}

func TestMarkdownNeverMakesLinks(t *testing.T) {
	got := Markdown("[click](https://evil.example/steal) <a href=x>y</a> [x](javascript:alert)")
	for _, e := range got.Entities {
		if e.Type != "bold" && e.Type != "italic" && e.Type != "code" && e.Type != "pre" && e.Type != "strikethrough" && e.Type != "blockquote" {
			t.Errorf("entity %s", e.Type)
		}
	}
	if !strings.Contains(got.S, "click (https://evil.example/steal)") {
		t.Errorf("link target hidden: %q", got.S)
	}
}
