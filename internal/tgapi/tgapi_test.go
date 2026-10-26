package tgapi_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mhmdxsadk/latch/internal/tgapi"
	"github.com/mhmdxsadk/latch/internal/tgapi/tgtest"
)

func TestSendEditAnswer(t *testing.T) {
	srv := tgtest.New(t)
	c := srv.Client()
	ctx := context.Background()
	if _, err := c.Me(ctx); err != nil {
		t.Fatal(err)
	}
	kb := tgapi.Keyboard{{{Text: "Approve", Data: "a:1"}}}
	m, err := c.Send(ctx, 42, tgapi.Plain("hello <b>"), kb)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Edit(ctx, 42, m.MessageID, tgapi.Plain("done"), nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Edit(ctx, 42, m.MessageID, tgapi.Plain("done"), nil); err != nil {
		t.Fatalf("unchanged edit: %v", err)
	}
	got := srv.Sent()
	if len(got) != 1 || got[0].Text != "done" || got[0].Keyboard != nil || got[0].Edits != 1 {
		t.Fatalf("%+v", got)
	}
}

func TestUpdates(t *testing.T) {
	srv := tgtest.New(t)
	c := srv.Client()
	srv.Message(7, "hi")
	srv.Message(7, "there")
	u, err := c.Updates(context.Background(), 0)
	if err != nil || len(u) != 2 || u[0].Message.Text != "hi" {
		t.Fatalf("%v %+v", err, u)
	}
	if err := c.Confirm(context.Background(), u[1].UpdateID+1); err != nil {
		t.Fatal(err)
	}
	u, err = c.Updates(context.Background(), 0)
	if err != nil || len(u) != 0 {
		t.Fatalf("after confirm: %v %+v", err, u)
	}
}

func TestErrorsNeverShowTheToken(t *testing.T) {
	c := tgapi.New("http://127.0.0.1:1", "999:secret-token-value")
	_, err := c.Send(context.Background(), 1, tgapi.Plain("x"), nil)
	if err == nil || strings.Contains(err.Error(), "secret-token-value") {
		t.Fatalf("err = %v", err)
	}
	srv := tgtest.New(t)
	c = tgapi.New(srv.URL, "999:secret-token-value")
	_, err = c.Me(context.Background())
	var te *tgapi.Error
	if !errors.As(err, &te) || te.Code != 401 || strings.Contains(err.Error(), "secret-token-value") {
		t.Fatalf("err = %v", err)
	}
}

func TestSplit(t *testing.T) {
	for _, tc := range []struct {
		in    string
		limit int
		want  []string
	}{
		{"", 10, nil},
		{"short", 10, []string{"short"}},
		{"line one\nline two\n", 12, []string{"line one\n", "line two\n"}},
		{"aaaa bbbb cccc", 10, []string{"aaaa bbbb ", "cccc"}},
		{"abcdefghij", 4, []string{"abcd", "efgh", "ij"}},
		{"😀😀😀", 4, []string{"😀😀", "😀"}}, // two UTF-16 units each
		{"😀", 1, []string{"😀"}},
	} {
		got := tgapi.Split(tc.in, tc.limit)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") || len(got) != len(tc.want) {
			t.Errorf("Split(%q, %d) = %q, want %q", tc.in, tc.limit, got, tc.want)
		}
	}
	if n := tgapi.TextLen("a😀é"); n != 4 {
		t.Errorf("TextLen = %d", n)
	}
}

func TestFormattedTextAndFallback(t *testing.T) {
	srv := tgtest.New(t)
	c := srv.Client()
	ctx := context.Background()
	var b tgapi.Builder
	b.Bold("Request 5").Plain(" on ").Code("/root/hello.txt").Plain("\n").Pre("hello", "")
	if _, err := c.Send(ctx, 1, b.Done(), nil); err != nil {
		t.Fatal(err)
	}
	m := srv.Sent()[0]
	f := m.Formatted()
	if len(f.Entities) != 3 || f.Slice(f.Entities[0]) != "Request 5" || f.Slice(f.Entities[1]) != "/root/hello.txt" || f.Slice(f.Entities[2]) != "hello" {
		t.Fatalf("%+v", f)
	}
	// Formatting Telegram refuses (code inside code) falls back to plain text.
	bad := tgapi.Text{S: "abc", Entities: []tgapi.Entity{{Type: "code", Offset: 0, Length: 3}, {Type: "bold", Offset: 1, Length: 1}}}
	if _, err := c.Send(ctx, 1, bad, nil); err != nil {
		t.Fatal(err)
	}
	if m := srv.Sent()[1]; m.Text != "abc" || len(m.Entities) != 0 {
		t.Fatalf("fallback: %+v", m)
	}
}

func TestSplitTextCarriesFormatting(t *testing.T) {
	var b tgapi.Builder
	b.Bold("aaaa bbbb").Plain(" cccc ").Code("dd😀d")
	parts := tgapi.SplitText(b.Done(), 10)
	if len(parts) != 2 {
		t.Fatalf("%+v", parts)
	}
	for _, p := range parts {
		if msg := tgtest.CheckEntities(p.S, p.Entities); msg != "" {
			t.Errorf("%q: %s", p.S, msg)
		}
	}
	if parts[0].S != "aaaa bbbb" || parts[0].Slice(parts[0].Entities[0]) != "aaaa bbbb" {
		t.Errorf("part 0 %+v", parts[0])
	}
	last := parts[1]
	if last.S != "cccc dd😀d" || len(last.Entities) != 1 || last.Slice(last.Entities[0]) != "dd😀d" {
		t.Errorf("last %+v", last)
	}
}
