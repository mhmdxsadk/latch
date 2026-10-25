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
	m, err := c.Send(ctx, 42, "hello <b>", kb)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Edit(ctx, 42, m.MessageID, "done", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Edit(ctx, 42, m.MessageID, "done", nil); err != nil {
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
	u, err = c.Updates(context.Background(), u[1].UpdateID+1)
	if err != nil || len(u) != 0 {
		t.Fatalf("after offset: %v %+v", err, u)
	}
}

func TestErrorsNeverShowTheToken(t *testing.T) {
	c := tgapi.New("http://127.0.0.1:1", "999:secret-token-value")
	_, err := c.Send(context.Background(), 1, "x", nil)
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
