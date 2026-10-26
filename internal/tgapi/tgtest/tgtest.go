// Package tgtest is a fake Telegram Bot API server for tests. It keeps the
// messages the bot sent and edited, and hands out updates the test queues.
package tgtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhmdxsadk/latch/internal/tgapi"
)

// Token is the bot token the fake server accepts.
const Token = "123:test-token"

// Sent is a message the bot sent, with its latest text and buttons.
type Sent struct {
	ID       int64
	Chat     int64
	Text     string
	Entities []tgapi.Entity
	Keyboard tgapi.Keyboard
	Edits    int
}

// Formatted returns the message as a tgapi.Text.
func (m Sent) Formatted() tgapi.Text { return tgapi.Text{S: m.Text, Entities: m.Entities} }

// Server is a fake Bot API.
type Server struct {
	URL string

	mu       sync.Mutex
	nextID   int64
	nextUpd  int64
	nextCB   int
	updates  []tgapi.Update
	wake     chan struct{}
	sent     []*Sent
	answers  map[string]string
	typing   int
	commands []tgapi.Command
}

// New starts a fake server; it stops when the test ends.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{nextID: 100, nextUpd: 1, wake: make(chan struct{}, 1), answers: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// Client returns a tgapi client for the fake server.
func (s *Server) Client() *tgapi.Client { return tgapi.New(s.URL, Token) }

// Message queues a text message from user in their private chat.
func (s *Server) Message(user int64, text string) {
	s.push(tgapi.Update{Message: &tgapi.Message{
		From: &tgapi.User{ID: user}, Chat: tgapi.Chat{ID: user, Type: "private"}, Text: text, Date: time.Now().Unix(),
	}})
}

// Update queues any update; its update id is filled in.
func (s *Server) Update(u tgapi.Update) { s.push(u) }

// Press queues a press on a button of message msg by user, and returns the
// callback id to look up the answer with Answer.
func (s *Server) Press(user int64, msg *Sent, data string) string {
	s.mu.Lock()
	s.nextCB++
	id := "cb" + strconv.Itoa(s.nextCB)
	s.mu.Unlock()
	s.push(tgapi.Update{CallbackQuery: &tgapi.CallbackQuery{
		ID: id, From: tgapi.User{ID: user}, Data: data,
		Message: &tgapi.Message{MessageID: msg.ID, Chat: tgapi.Chat{ID: msg.Chat, Type: "private"}},
	}})
	return id
}

func (s *Server) push(u tgapi.Update) {
	s.mu.Lock()
	u.UpdateID = s.nextUpd
	s.nextUpd++
	s.updates = append(s.updates, u)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Sent returns a copy of every message the bot sent, oldest first.
func (s *Server) Sent() []Sent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Sent, len(s.sent))
	for i, m := range s.sent {
		out[i] = *m
	}
	return out
}

// Answer returns the text the bot answered callback id with, and whether it
// answered at all.
func (s *Server) Answer(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.answers[id]
	return a, ok
}

// Typing returns how many typing indicators the bot sent.
func (s *Server) Typing() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.typing
}

// Commands returns the bot's command menu.
func (s *Server) Commands() []tgapi.Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commands
}

// WaitFor polls until cond holds or fails the test after five seconds.
func WaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	method, ok := strings.CutPrefix(r.URL.Path, "/bot"+Token+"/")
	if !ok {
		reply(w, false, nil, 401, "Unauthorized")
		return
	}
	var body struct {
		Offset    int64          `json:"offset"`
		Timeout   int            `json:"timeout"`
		ChatID    int64          `json:"chat_id"`
		MessageID int64          `json:"message_id"`
		Text      string         `json:"text"`
		Entities  []tgapi.Entity `json:"entities"`
		ParseMode string         `json:"parse_mode"`
		CBID      string         `json:"callback_query_id"`
		Markup    *struct {
			Keyboard tgapi.Keyboard `json:"inline_keyboard"`
		} `json:"reply_markup"`
		Commands []tgapi.Command `json:"commands"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		reply(w, false, nil, 400, "Bad Request: bad json")
		return
	}
	if body.ParseMode != "" {
		reply(w, false, nil, 400, "Bad Request: tgtest refuses parse_mode")
		return
	}
	if method == "sendMessage" || method == "editMessageText" {
		if tgapi.TextLen(body.Text) > tgapi.MaxText {
			reply(w, false, nil, 400, "Bad Request: message is too long")
			return
		}
		if msg := CheckEntities(body.Text, body.Entities); msg != "" {
			reply(w, false, nil, 400, "Bad Request: can't parse entities: "+msg)
			return
		}
	}
	switch method {
	case "getMe":
		reply(w, true, tgapi.User{ID: 123, IsBot: true}, 0, "")
	case "getUpdates":
		s.getUpdates(w, r, body.Offset, time.Duration(body.Timeout)*time.Second)
	case "sendMessage":
		s.mu.Lock()
		m := &Sent{ID: s.nextID, Chat: body.ChatID, Text: body.Text, Entities: body.Entities}
		if body.Markup != nil {
			m.Keyboard = body.Markup.Keyboard
		}
		s.nextID++
		s.sent = append(s.sent, m)
		s.mu.Unlock()
		reply(w, true, tgapi.Message{MessageID: m.ID, Chat: tgapi.Chat{ID: m.Chat, Type: "private"}, Text: m.Text}, 0, "")
	case "editMessageText":
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, m := range s.sent {
			if m.ID == body.MessageID && m.Chat == body.ChatID {
				var kb tgapi.Keyboard
				if body.Markup != nil && len(body.Markup.Keyboard) > 0 {
					kb = body.Markup.Keyboard
				}
				if m.Text == body.Text && len(m.Keyboard) == len(kb) {
					reply(w, false, nil, 400, "Bad Request: message is not modified")
					return
				}
				m.Text, m.Entities, m.Keyboard = body.Text, body.Entities, kb
				m.Edits++
				reply(w, true, true, 0, "")
				return
			}
		}
		reply(w, false, nil, 400, "Bad Request: message to edit not found")
	case "answerCallbackQuery":
		s.mu.Lock()
		s.answers[body.CBID] = body.Text
		s.mu.Unlock()
		reply(w, true, true, 0, "")
	case "sendChatAction":
		s.mu.Lock()
		s.typing++
		s.mu.Unlock()
		reply(w, true, true, 0, "")
	case "setMyCommands":
		s.mu.Lock()
		s.commands = body.Commands
		s.mu.Unlock()
		reply(w, true, true, 0, "")
	default:
		reply(w, false, nil, 404, "Not Found")
	}
}

func (s *Server) getUpdates(w http.ResponseWriter, r *http.Request, offset int64, wait time.Duration) {
	// Tests should not wait the full poll time; cap it.
	deadline := time.After(min(wait, 2*time.Second))
	for {
		s.mu.Lock()
		kept := s.updates[:0]
		for _, u := range s.updates {
			if u.UpdateID >= offset {
				kept = append(kept, u)
			}
		}
		s.updates = kept
		out := append([]tgapi.Update(nil), kept...)
		s.mu.Unlock()
		if len(out) > 0 {
			reply(w, true, out, 0, "")
			return
		}
		select {
		case <-s.wake:
		case <-deadline:
			reply(w, true, []tgapi.Update{}, 0, "")
			return
		case <-r.Context().Done():
			return
		}
	}
}

func reply(w http.ResponseWriter, ok bool, result any, code int, desc string) {
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": code, "description": desc})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

// CheckEntities applies Telegram's rules for entities and returns what is
// wrong, or "" when they are fine: known formatting types only, ranges inside
// the text, ranges that overlap only by one containing the other, nothing
// inside code or pre, and code or pre inside nothing but blockquote.
func CheckEntities(text string, es []tgapi.Entity) string {
	n := tgapi.TextLen(text)
	for i, e := range es {
		switch e.Type {
		case "bold", "italic", "underline", "strikethrough", "code", "pre", "blockquote":
		default:
			return "entity type " + e.Type + " is not allowed"
		}
		if e.Offset < 0 || e.Length <= 0 || e.Offset+e.Length > n {
			return "entity " + strconv.Itoa(i) + " is out of range"
		}
		for _, f := range es[i+1:] {
			aEnd, bEnd := e.Offset+e.Length, f.Offset+f.Length
			if aEnd <= f.Offset || bEnd <= e.Offset {
				continue
			}
			inner, outer := e, f
			if e.Offset <= f.Offset && aEnd >= bEnd {
				inner, outer = f, e
			} else if f.Offset > e.Offset || bEnd < aEnd {
				return "entities overlap without nesting"
			}
			if outer.Type == "code" || outer.Type == "pre" {
				return "nothing may be inside code or pre"
			}
			if (inner.Type == "code" || inner.Type == "pre") && outer.Type != "blockquote" {
				return "code and pre may only be inside blockquote"
			}
			if inner.Type == "blockquote" && outer.Type == "blockquote" {
				return "blockquotes cannot nest"
			}
		}
	}
	return ""
}
