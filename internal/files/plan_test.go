package files

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

const planSecret = "Sup3rS3cretValue"

func plan(t *testing.T, real, old, new string) EditPlan {
	t.Helper()
	p, err := PlanEdit([]byte(real), old, new)
	if err != nil {
		t.Fatalf("PlanEdit(%q, %q, %q): %v", real, old, new, err)
	}
	return p
}

func TestPlanEditMatchesExactlyOnce(t *testing.T) {
	real := "a=1\nb=2\na=1\naaa\n"
	for _, tc := range []struct {
		old  string
		want error
	}{
		{"zzz", ErrNoMatch},
		{"a=1", ErrManyMatches},
		{"aa", ErrManyMatches}, // overlapping occurrences in "aaa"
		{"=", ErrManyMatches},
		{"b=2\n", nil},
		{"", ErrBadRequest},
		{strings.Repeat("x", MaxEditText+1), ErrBadRequest},
	} {
		_, err := PlanEdit([]byte(real), tc.old, "c")
		if !errors.Is(err, tc.want) && (tc.want != nil || err != nil) {
			t.Errorf("old %q: err %v, want %v", tc.old, err, tc.want)
		}
	}
	if _, err := PlanEdit([]byte(real), "b=2", strings.Repeat("y", MaxEditText+1)); !errors.Is(err, ErrBadRequest) {
		t.Errorf("long new: %v", err)
	}
}

func TestPlanEditShapes(t *testing.T) {
	for _, tc := range []struct{ name, real, old, new, want string }{
		{"within a line", "listen 80;\nroot /srv;\n", "80", "8080", "listen 8080;\nroot /srv;\n"},
		{"whole lines", "a\nb\nc\n", "b\nc", "x", "a\nx\n"},
		{"insert lines", "a\nb\nc\n", "b", "b\nb2\nb3", "a\nb\nb2\nb3\nc\n"},
		{"delete a line with its newline", "a\nb\nc\n", "a\n", "", "b\nc\n"},
		{"delete the last line", "a\nb\n", "\nb", "", "a\n"},
		{"drop the final newline", "a\nb\n", "b\n", "b", "a\nb"},
		{"add a final newline", "a\nb", "b", "b\n", "a\nb\n"},
		{"no final newline kept", "a\nb", "a", "z", "z\nb"},
		{"join lines", "a\nb\n", "a\nb", "ab", "ab\n"},
		{"crlf kept", "a\r\nb\r\nc\r\n", "b", "x\ny", "a\r\nx\r\ny\r\nc\r\n"},
		{"crlf without final newline", "a\r\nb", "b", "c", "a\r\nc"},
		{"crlf middle line", "a\r\nb\r\nc", "a\nb", "q", "q\r\nc"},
		{"mixed endings: new lines get LF", "a\r\nb\nc\n", "b", "x", "a\r\nx\nc\n"},
		{"untouched lines keep their bytes", "a\r\nb\nc\x01d\n", "b", "x", "a\r\nx\nc\x01d\n"},
	} {
		got := plan(t, tc.real, tc.old, tc.new)
		if string(got.Content) != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got.Content, tc.want)
		}
	}
}

func TestPlanEditRestoresHiddenValues(t *testing.T) {
	real := "user=bob\npassword=" + planSecret + "\nport=1\ntoken: " + planSecret + "2\n"
	view := strings.Join(ViewLines([]byte(real)), "\n")
	if strings.Contains(view, planSecret) || !strings.Contains(view, "password=[hidden]") {
		t.Fatalf("view %q", view)
	}

	p := plan(t, real, "user=bob\npassword=[hidden]\nport=1", "user=alice\npassword=[hidden]\nport=2")
	want := "user=alice\npassword=" + planSecret + "\nport=2\ntoken: " + planSecret + "2\n"
	if string(p.Content) != want || p.Kept != 1 {
		t.Fatalf("got %q kept %d", p.Content, p.Kept)
	}
	for _, l := range append(slices.Clone(p.Before), p.After...) {
		if strings.Contains(l, planSecret) {
			t.Errorf("view leaks the secret: %q", l)
		}
	}
	if !slices.Equal(p.After, []string{"user=alice", "password=[hidden]", "port=2", "token: [hidden]"}) {
		t.Errorf("after %q", p.After)
	}

	// Moving a hidden line keeps its value; two different hidden lines with
	// the same view are restored in order.
	two := "a\npassword=one-" + planSecret + "\npassword=two-" + planSecret + "\nb\n"
	p = plan(t, two, "a\npassword=[hidden]\npassword=[hidden]\nb", "b\npassword=[hidden]\npassword=[hidden]\na")
	if string(p.Content) != "b\npassword=one-"+planSecret+"\npassword=two-"+planSecret+"\na\n" || p.Kept != 2 {
		t.Errorf("moved: %q kept %d", p.Content, p.Kept)
	}

	// A line with control characters removed in the view is restored too.
	ctl := "x=\x07bell\ny=1\n"
	p = plan(t, ctl, "x=bell\ny=1", "x=bell\ny=2")
	if string(p.Content) != "x=\x07bell\ny=2\n" {
		t.Errorf("control: %q", p.Content)
	}
}

func TestPlanEditRefusesNewHidden(t *testing.T) {
	real := "user=bob\npassword=" + planSecret + "\n"
	for _, n := range []string{
		"password=[hidden]x",       // changed a hidden line
		"password=[hidden] # note", // appended to it
		"password=[hidden]\nalso=[hidden]",
		"pass=[hidden]",
	} {
		if _, err := PlanEdit([]byte(real), "password=[hidden]", n); !errors.Is(err, ErrNewHidden) {
			t.Errorf("new %q: %v", n, err)
		}
	}
	// A new [hidden] in a line that never had one is refused as well.
	if _, err := PlanEdit([]byte(real), "user=bob", "user=[hidden]"); !errors.Is(err, ErrNewHidden) {
		t.Errorf("new hidden in plain line: %v", err)
	}
	// A file that literally contains [hidden] can keep that line.
	lit := "note=[hidden] is a word\nx=1\n"
	if p := plan(t, lit, "note=[hidden] is a word\nx=1", "note=[hidden] is a word\nx=2"); string(p.Content) != "note=[hidden] is a word\nx=2\n" {
		t.Errorf("literal: %q", p.Content)
	}
}

func TestPlanEditRefusesMultiLineHidden(t *testing.T) {
	real := "name=web\n" + pemKey + "\nport=1\n"
	if _, err := PlanEdit([]byte(real), "port=1", "port=2"); !errors.Is(err, ErrMultiLineHidden) {
		t.Errorf("err %v", err)
	}
}

func TestPlanEditRefusesBadInput(t *testing.T) {
	for _, tc := range []struct {
		real, old, new string
		want           error
	}{
		{"a\x00b\n", "a", "b", ErrBinary},
		{strings.Repeat("a", MaxEditBytes+1), "a", "b", ErrTooLarge},
		{"abc\n", "b", "b", ErrNoChange},
		{"abc\n", "b", "\x1b[31mred", ErrBadText},
		{"abc\n", "b", "x\r\ny", ErrBadText},
		{"abc\n", "b", "evil\u202etxt", ErrBadText},
		{"abc\n", "b", "zero\u200bwidth", ErrBadText},
		{"abc\n", "b", "bad\xffutf8", ErrBadText},
		{strings.Repeat("a", MaxEditBytes-10) + "\nb\n", "b", strings.Repeat("c", 100), ErrTooLarge},
	} {
		if _, err := PlanEdit([]byte(tc.real), tc.old, tc.new); !errors.Is(err, tc.want) {
			t.Errorf("new %q: %v, want %v", tc.new, err, tc.want)
		}
	}
	if p := plan(t, "a\tb\n", "a\tb", "a\tcé"); string(p.Content) != "a\tcé\n" {
		t.Errorf("tab and accents: %q", p.Content)
	}
}

func TestPlainText(t *testing.T) {
	if !PlainText("a\tb\nc", false) || PlainText("a\r\nb", false) || !PlainText("a\r\nb\r", true) || PlainText("a\rb", true) {
		t.Error("PlainText")
	}
}

func TestDiff(t *testing.T) {
	a := []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12"}
	b := []string{"1", "2", "3", "4", "five", "6", "7", "8", "9", "10", "11", "12", "13"}
	got := Diff(a, b, 3)
	want := []string{
		"@@ -2,7 +2,7 @@", " 2", " 3", " 4", "-5", "+five", " 6", " 7", " 8",
		"@@ -10,3 +10,4 @@", " 10", " 11", " 12", "+13",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got\n%s", strings.Join(got, "\n"))
	}
	if Diff(a, a, 3) != nil {
		t.Error("equal input")
	}
	if got := Diff(nil, []string{"x"}, 3); !slices.Equal(got, []string{"@@ -0,0 +1 @@", "+x"}) {
		t.Errorf("from empty: %q", got)
	}
	if got := Diff([]string{"a", "b"}, []string{"b"}, 3); !slices.Equal(got, []string{"@@ -1,2 +1 @@", "-a", " b"}) {
		t.Errorf("delete first: %q", got)
	}
}
