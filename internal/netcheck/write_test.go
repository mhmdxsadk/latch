package netcheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestHTTPRequestSendsOnlyMethodBodyAndType(t *testing.T) {
	e := newEnv(t)
	var (
		got  *http.Request
		body string
	)
	s, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=abc")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id": 7, "token": "ghp_abcdefghijklmnopqrstuvwxyz0123456789"}`)
	})
	r, err := e.p.HTTPRequest(context.Background(), http.MethodPost, s.URL+"/items?key=v", "application/json", []byte(`{"name":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPost || body != `{"name":"x"}` || got.Header.Get("Content-Type") != "application/json" || got.Header.Get("User-Agent") != "latch/test" {
		t.Errorf("request %s %q %v", got.Method, body, got.Header)
	}
	for k := range got.Header {
		switch k {
		case "User-Agent", "Content-Type", "Content-Length", "Accept-Encoding", "Connection":
		default:
			t.Errorf("unexpected request header %q", k)
		}
	}
	if r.StatusCode != 201 || strings.Contains(r.Body, "ghp_") || !strings.Contains(r.Body, `"id": 7`) || r.Headers["Set-Cookie"] != "" {
		t.Errorf("result %+v", r)
	}
	if !strings.HasSuffix(r.URL, "/items?key=[hidden]") {
		t.Errorf("url %q", r.URL)
	}
	for _, m := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if r, err := e.p.HTTPRequest(context.Background(), m, s.URL+"/", "", nil); err != nil || got.Method != m || r.StatusCode != 201 {
			t.Errorf("%s: %v %+v", m, err, r)
		}
	}
	if got.Header.Get("Content-Type") != "" {
		t.Errorf("content type sent without one: %v", got.Header)
	}
}

func TestHTTPRequestDoesNotFollowRedirects(t *testing.T) {
	e := newEnv(t)
	s, hits := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/next" {
			t.Error("redirect followed")
		}
		w.Header().Set("Location", "/next?token=abc")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	r, err := e.p.HTTPRequest(context.Background(), http.MethodPost, s.URL+"/", "text/plain", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != 307 || !strings.HasSuffix(r.Headers["Location"], "/next?token=[hidden]") || len(r.Redirects) != 0 || hits.Load() != 1 {
		t.Errorf("got %+v hits %d", r, hits.Load())
	}
}

func TestHTTPRequestRefusals(t *testing.T) {
	e := newEnv(t)
	other, otherHits := server(t, func(http.ResponseWriter, *http.Request) {})
	e.ex.snap.Ports = append(e.ex.snap.Ports, uint16(portOf(t, other)))
	e.name("container.test", addrs("172.18.0.5"))
	e.name("rebind.test", addrs("127.0.0.1"), addrs("172.18.0.5"))
	ctx := context.Background()
	for _, u := range []string{
		"http://127.0.0.1:" + strconv.Itoa(portOf(t, other)) + "/",
		"http://172.18.0.5/", "http://container.test/", "http://blocked.example/", "http://10.9.9.9/",
	} {
		if _, err := e.p.CheckHTTPTarget(ctx, u); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("check %s: %v", u, err)
		}
		if _, err := e.p.HTTPRequest(ctx, http.MethodPost, u, "", nil); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("send %s: %v", u, err)
		}
	}
	for _, u := range []string{"ftp://x/", "http://user:pw@127.0.0.1/", "", "http://"} {
		if _, err := e.p.CheckHTTPTarget(ctx, u); !errors.Is(err, ErrInvalid) {
			t.Errorf("check %q: %v", u, err)
		}
	}
	// The name passes when the request is made, then resolves to an
	// excluded container when it is sent.
	s, _ := server(t, func(http.ResponseWriter, *http.Request) {})
	u := "http://rebind.test:" + strconv.Itoa(portOf(t, s)) + "/"
	if _, err := e.p.CheckHTTPTarget(ctx, u); err != nil {
		t.Fatalf("first check: %v", err)
	}
	if _, err := e.p.HTTPRequest(ctx, http.MethodPost, u, "", nil); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("rebinding: %v", err)
	}
	for _, tc := range []struct {
		method, ct string
		body       int
	}{
		{http.MethodGet, "", 0}, {"TRACE", "", 0}, {"post", "", 0}, {http.MethodPost, "text/html", 0},
		{http.MethodPost, "application/json; charset=utf-8", 0}, {http.MethodPost, "", MaxWriteBody + 1},
	} {
		if _, err := e.p.HTTPRequest(ctx, tc.method, s.URL, tc.ct, make([]byte, tc.body)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s %q %d: %v", tc.method, tc.ct, tc.body, err)
		}
	}
	if otherHits.Load() != 0 {
		t.Error("excluded port reached")
	}
	if got := DisplayURL("https://u:p@example.com/a?token=x#f"); got != "https://example.com/a?token=[hidden]" {
		t.Errorf("display %q", got)
	}
}
