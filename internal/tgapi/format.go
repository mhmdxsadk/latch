package tgapi

import (
	"slices"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// Entity marks formatting over a range of a message, counted in UTF-16 code
// units like Telegram does. Only formatting types are ever built here: no
// links, mentions or custom emoji, so formatted text can never hide where a
// link goes.
type Entity struct {
	// Type is bold, italic, strikethrough, code, pre, blockquote,
	// expandable_blockquote or date_time.
	Type     string `json:"type"`
	Offset   int    `json:"offset"`
	Length   int    `json:"length"`
	Language string `json:"language,omitempty"` // pre only
	// UnixTime and DateTimeFormat are for date_time only: the client shows
	// the time in the reader's own time zone.
	UnixTime       int64  `json:"unix_time,omitempty"`
	DateTimeFormat string `json:"date_time_format,omitempty"`
}

// Text is message text with formatting. The zero value is empty.
type Text struct {
	S        string
	Entities []Entity
}

// Plain is unformatted text.
func Plain(s string) Text { return Text{S: s} }

// Len is the length of t the way Telegram counts it.
func (t Text) Len() int { return TextLen(t.S) }

// Builder appends formatted pieces to a Text.
type Builder struct {
	t Text
	n int // UTF-16 length so far
}

// Plain appends unformatted text.
func (b *Builder) Plain(s string) *Builder {
	b.t.S += s
	b.n += TextLen(s)
	return b
}

// Styled appends s with one formatting type.
func (b *Builder) Styled(typ, s string) *Builder { return b.add(Entity{Type: typ}, s) }

// Bold, Italic and Code append s with that formatting.
func (b *Builder) Bold(s string) *Builder   { return b.Styled("bold", s) }
func (b *Builder) Italic(s string) *Builder { return b.Styled("italic", s) }
func (b *Builder) Code(s string) *Builder   { return b.Styled("code", s) }

// Pre appends s as a code block, with a language for highlighting or "".
func (b *Builder) Pre(s, language string) *Builder {
	return b.add(Entity{Type: "pre", Language: language}, s)
}

// Time appends t as a date_time entity: the reader's app shows it in their
// own time zone, in the given format ("t" short time, "r" relative, "wDT"
// and so on; see Telegram's date-time entity formatting). shown is the text
// for apps that cannot render it.
func (b *Builder) Time(shown string, t time.Time, format string) *Builder {
	return b.add(Entity{Type: "date_time", UnixTime: t.Unix(), DateTimeFormat: format}, shown)
}

// Quote appends t as a block quotation, collapsed until tapped when
// expandable, keeping t's own formatting inside.
func (b *Builder) Quote(t Text, expandable bool) *Builder {
	typ := "blockquote"
	if expandable {
		typ = "expandable_blockquote"
	}
	if t.S == "" {
		return b
	}
	b.t.Entities = append(b.t.Entities, Entity{Type: typ, Offset: b.n, Length: t.Len()})
	return b.Text(t)
}

// Text appends another Text, keeping its formatting.
func (b *Builder) Text(t Text) *Builder {
	for _, e := range t.Entities {
		e.Offset += b.n
		b.t.Entities = append(b.t.Entities, e)
	}
	return b.Plain(t.S)
}

func (b *Builder) add(e Entity, s string) *Builder {
	if s == "" {
		return b
	}
	e.Offset, e.Length = b.n, TextLen(s)
	b.t.Entities = append(b.t.Entities, e)
	return b.Plain(s)
}

// Len is the UTF-16 length built so far.
func (b *Builder) Len() int { return b.n }

// Done returns the built Text.
func (b *Builder) Done() Text {
	t := b.t
	t.Entities = slices.Clone(t.Entities)
	return t
}

// SplitText cuts t into pieces of at most limit UTF-16 code units, like
// Split, carrying formatting along: an entity that crosses a cut continues in
// the next piece.
func SplitText(t Text, limit int) []Text {
	parts := Split(t.S, limit)
	out := make([]Text, 0, len(parts))
	start := 0
	for _, p := range parts {
		n := TextLen(p)
		piece := Text{S: p}
		for _, e := range t.Entities {
			lo, hi := max(e.Offset, start), min(e.Offset+e.Length, start+n)
			if lo >= hi {
				continue
			}
			e.Offset, e.Length = lo-start, hi-lo
			piece.Entities = append(piece.Entities, e)
		}
		out = append(out, trimText(piece))
		start += n
	}
	return out
}

// trimText drops trailing whitespace and the formatting over it.
func trimText(t Text) Text {
	s := strings.TrimRightFunc(t.S, func(r rune) bool { return r == ' ' || r == '\n' || r == '\t' })
	n := TextLen(s)
	kept := t.Entities[:0:0]
	for _, e := range t.Entities {
		if e.Offset >= n {
			continue
		}
		e.Length = min(e.Length, n-e.Offset)
		kept = append(kept, e)
	}
	return Text{S: s, Entities: kept}
}

// byteOffset converts a UTF-16 offset in s to a byte offset.
func byteOffset(s string, u16 int) int {
	n := 0
	for i, r := range s {
		if n >= u16 {
			return i
		}
		n += utf16.RuneLen(r)
	}
	return len(s)
}

// Slice returns the text an entity covers; tests use it.
func (t Text) Slice(e Entity) string {
	lo := byteOffset(t.S, e.Offset)
	hi := byteOffset(t.S, e.Offset+e.Length)
	if !utf8.ValidString(t.S[lo:hi]) {
		return ""
	}
	return t.S[lo:hi]
}
