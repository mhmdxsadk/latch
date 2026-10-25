// Package admin is the owner's JSON API for change requests, served only on
// the root-only admin socket (never on the agent socket). The latch pending,
// approve and deny commands use it, and a chat bridge can use it later.
//
//	GET  /v1/requests?status=pending  requests waiting for approval, oldest first
//	GET  /v1/requests                 the most recent requests (up to 50), oldest first
//	GET  /v1/requests/{id}            one request
//	POST /v1/requests/{id}/approve    approve and start a pending request
//	POST /v1/requests/{id}/deny       deny a pending request
//
// The decide calls take an optional ?via=cli|telegram (default cli) that is
// recorded with the decision and in the audit log.
//
// Lists answer {"requests": [Request...]}; the others answer a Request (see
// approval.Request). Errors answer {"error": "..."} with 400 (bad id), 404
// (not found or expired), 409 (not pending, or the resource is busy) or 403
// (no verified peer). Unknown paths answer 404 and wrong methods 405.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/mhmdxsadk/latch/internal/approval"
	"github.com/mhmdxsadk/latch/internal/audit"
)

// RecentLimit is how many requests GET /v1/requests returns.
const RecentLimit = 50

// List is the body of the list endpoints.
type List struct {
	Requests []approval.Request `json:"requests"`
}

// Error is the body of every error response.
type Error struct {
	Error string `json:"error"`
}

// Handler serves the admin API for store. The approver is the peer that the
// admin socket verified; a request without one is refused.
func Handler(store *approval.Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/requests", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("status") {
		case "":
			reply(w, http.StatusOK, List{Requests: store.List(false, RecentLimit)})
		case "pending":
			reply(w, http.StatusOK, List{Requests: store.List(true, 0)})
		default:
			reply(w, http.StatusBadRequest, Error{"status must be empty or pending"})
		}
	})
	mux.HandleFunc("GET /v1/requests/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r)
		if !ok {
			return
		}
		req, err := store.Get(id)
		respond(w, req, err)
	})
	decide := func(do func(context.Context, int64, approval.Decider) (approval.Request, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			peer, ok := audit.PeerFromContext(r.Context())
			if !ok {
				reply(w, http.StatusForbidden, Error{"no verified peer"})
				return
			}
			id, ok := parseID(w, r)
			if !ok {
				return
			}
			via := r.URL.Query().Get("via")
			switch via {
			case "":
				via = approval.ViaCLI
			case approval.ViaCLI, approval.ViaTelegram:
			default:
				reply(w, http.StatusBadRequest, Error{"via must be cli or telegram"})
				return
			}
			req, err := do(r.Context(), id, approval.Decider{UID: peer.UID, Via: via})
			respond(w, req, err)
		}
	}
	mux.HandleFunc("POST /v1/requests/{id}/approve", decide(store.Approve))
	mux.HandleFunc("POST /v1/requests/{id}/deny", decide(store.Deny))
	return mux
}

func parseID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		reply(w, http.StatusBadRequest, Error{"invalid request id"})
		return 0, false
	}
	return id, true
}

func respond(w http.ResponseWriter, req approval.Request, err error) {
	switch {
	case err == nil:
		reply(w, http.StatusOK, req)
	case errors.Is(err, approval.ErrNotFound):
		reply(w, http.StatusNotFound, Error{approval.ErrNotFound.Error()})
	case errors.Is(err, approval.ErrNotPending):
		reply(w, http.StatusConflict, Error{approval.ErrNotPending.Error()})
	case errors.Is(err, approval.ErrBusy):
		reply(w, http.StatusConflict, Error{approval.ErrBusy.Error()})
	default:
		reply(w, http.StatusInternalServerError, Error{"internal error"})
	}
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Client talks to the admin socket.
type Client struct {
	http *http.Client
	// Via is sent with approvals and denials (approval.ViaCLI when empty).
	Via string
}

// NewClient returns a Client for the admin socket at path.
func NewClient(path string) *Client {
	var d net.Dialer
	return &Client{http: &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return d.DialContext(ctx, "unix", path)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// APIError is an error answer from the server.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return e.Message }

// IsNotFound reports whether err is a "not found or expired" answer.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// Pending lists the requests waiting for approval.
func (c *Client) Pending(ctx context.Context) ([]approval.Request, error) {
	var l List
	err := c.do(ctx, http.MethodGet, "/v1/requests?status=pending", &l)
	return l.Requests, err
}

// Recent lists the most recent requests.
func (c *Client) Recent(ctx context.Context) ([]approval.Request, error) {
	var l List
	err := c.do(ctx, http.MethodGet, "/v1/requests", &l)
	return l.Requests, err
}

// Get returns one request.
func (c *Client) Get(ctx context.Context, id int64) (approval.Request, error) {
	var r approval.Request
	err := c.do(ctx, http.MethodGet, "/v1/requests/"+strconv.FormatInt(id, 10), &r)
	return r, err
}

// Approve approves a pending request.
func (c *Client) Approve(ctx context.Context, id int64) (approval.Request, error) {
	return c.decide(ctx, id, "approve")
}

// Deny denies a pending request.
func (c *Client) Deny(ctx context.Context, id int64) (approval.Request, error) {
	return c.decide(ctx, id, "deny")
}

func (c *Client) decide(ctx context.Context, id int64, verb string) (approval.Request, error) {
	path := "/v1/requests/" + strconv.FormatInt(id, 10) + "/" + verb
	if c.Via != "" {
		path += "?via=" + url.QueryEscape(c.Via)
	}
	var r approval.Request
	err := c.do(ctx, http.MethodPost, path, &r)
	return r, err
}

const maxResponse = 4 << 20

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://latch-admin"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ue interface{ Unwrap() error }
		if errors.As(err, &ue) {
			err = ue.Unwrap()
		}
		return err
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, maxResponse)
	if resp.StatusCode != http.StatusOK {
		var e Error
		if json.NewDecoder(body).Decode(&e) != nil || e.Error == "" {
			e.Error = fmt.Sprintf("unexpected status %d", resp.StatusCode)
		}
		return &APIError{Status: resp.StatusCode, Message: e.Error}
	}
	if err := json.NewDecoder(body).Decode(out); err != nil {
		return errors.New("malformed response from the admin socket")
	}
	return nil
}
