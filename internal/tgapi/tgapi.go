// Package tgapi is a small client for the Telegram Bot API: long polling for
// updates, sending and editing plain-text messages, inline buttons, button
// answers and the typing indicator. It never sets parse_mode, so text is shown
// exactly as given and can never turn into links or formatting.
//
// The bot token is part of every request URL. Errors never include the URL,
// so the token cannot leak into logs.
package tgapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// DefaultBaseURL is Telegram's Bot API endpoint.
const DefaultBaseURL = "https://api.telegram.org"

// MaxText is the longest message text Telegram accepts, in UTF-16 code units.
const MaxText = 4096

// MaxCallbackData is the longest callback data Telegram accepts, in bytes.
const MaxCallbackData = 64

const maxResponse = 8 << 20

// User is a Telegram account.
type User struct {
	ID    int64 `json:"id"`
	IsBot bool  `json:"is_bot"`
}

// Chat is a Telegram chat. Type is "private" for a one-to-one chat.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// Message is an incoming or sent message. Only the fields Latch uses.
type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from,omitempty"`
	Chat      Chat   `json:"chat"`
	Date      int64  `json:"date,omitempty"` // Unix time it was sent
	Text      string `json:"text,omitempty"`
	// ForwardOrigin is set on forwarded messages, which are ignored.
	ForwardOrigin json.RawMessage `json:"forward_origin,omitempty"`
}

// CallbackQuery is a press on an inline button.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

// Update is one event from getUpdates.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

// Button is an inline keyboard button that sends Data back when pressed.
type Button struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}

// Keyboard is rows of inline buttons. A nil Keyboard sends no buttons; an
// empty one (Keyboard{}) removes the buttons of an edited message.
type Keyboard [][]Button

// Command is one entry of the bot's command menu.
type Command struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// Error is an error answer from Telegram.
type Error struct {
	Method      string
	Code        int
	Description string
	// RetryAfter is set when Telegram asks to slow down (code 429).
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Description)
}

// IsNotModified reports whether err is Telegram refusing an edit that would
// not change the message. Callers treat it as success.
func IsNotModified(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == http.StatusBadRequest && strings.Contains(e.Description, "message is not modified")
}

// Client calls the Bot API for one bot.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New returns a Client for token. base is DefaultBaseURL, or a fake server's
// URL in tests.
func New(base, token string) *Client {
	return &Client{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http: &http.Client{
			// Long polls wait up to PollTimeout; give them room.
			Timeout:       PollTimeout + 30*time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// PollTimeout is how long one getUpdates call waits for an update.
const PollTimeout = 50 * time.Second

// Updates long-polls for updates after offset (the last update id + 1).
// Only messages and button presses are requested.
func (c *Client) Updates(ctx context.Context, offset int64) ([]Update, error) {
	var out []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         int(PollTimeout / time.Second),
		"allowed_updates": []string{"message", "callback_query"},
	}, &out)
	return out, err
}

// Confirm tells Telegram that every update before offset was handled, so
// they are not delivered again after a restart. It does not wait.
func (c *Client) Confirm(ctx context.Context, offset int64) error {
	var out []Update
	return c.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 0, "limit": 1}, &out)
}

// Send sends text to chat, with buttons when kb is not nil.
func (c *Client) Send(ctx context.Context, chat int64, text string, kb Keyboard) (Message, error) {
	body := map[string]any{
		"chat_id":              chat,
		"text":                 text,
		"link_preview_options": map[string]bool{"is_disabled": true},
	}
	if kb != nil {
		body["reply_markup"] = map[string]any{"inline_keyboard": kb}
	}
	var m Message
	err := c.call(ctx, "sendMessage", body, &m)
	return m, err
}

// Edit replaces the text and buttons of a message the bot sent. A nil kb
// removes the buttons. An edit that changes nothing is not an error.
func (c *Client) Edit(ctx context.Context, chat, message int64, text string, kb Keyboard) error {
	if kb == nil {
		kb = Keyboard{}
	}
	err := c.call(ctx, "editMessageText", map[string]any{
		"chat_id":              chat,
		"message_id":           message,
		"text":                 text,
		"link_preview_options": map[string]bool{"is_disabled": true},
		"reply_markup":         map[string]any{"inline_keyboard": kb},
	}, nil)
	if IsNotModified(err) {
		return nil
	}
	return err
}

// Answer acknowledges a button press, showing text briefly when not empty.
func (c *Client) Answer(ctx context.Context, callbackID, text string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]any{
		"callback_query_id": callbackID,
		"text":              text,
	}, nil)
}

// Typing shows "typing..." in chat for about five seconds.
func (c *Client) Typing(ctx context.Context, chat int64) error {
	return c.call(ctx, "sendChatAction", map[string]any{"chat_id": chat, "action": "typing"}, nil)
}

// SetCommands sets the bot's command menu.
func (c *Client) SetCommands(ctx context.Context, cmds []Command) error {
	return c.call(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil)
}

// Me returns the bot's own account; it checks that the token works.
func (c *Client) Me(ctx context.Context) (User, error) {
	var u User
	err := c.call(ctx, "getMe", map[string]any{}, &u)
	return u, err
}

type response struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

func (c *Client) call(ctx context.Context, method string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/bot"+c.token+"/"+method, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("telegram %s: bad request", method)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// *url.Error carries the URL, and with it the token: keep only the
		// cause.
		var ue interface{ Unwrap() error }
		if errors.As(err, &ue) && ue.Unwrap() != nil {
			err = ue.Unwrap()
		}
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	defer resp.Body.Close()
	var r response
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(&r); err != nil {
		return &Error{Method: method, Code: resp.StatusCode, Description: "malformed response"}
	}
	if !r.OK {
		e := &Error{Method: method, Code: r.ErrorCode, Description: r.Description}
		if r.Parameters.RetryAfter > 0 {
			e.RetryAfter = time.Duration(r.Parameters.RetryAfter) * time.Second
		}
		return e
	}
	if out != nil {
		if err := json.Unmarshal(r.Result, out); err != nil {
			return &Error{Method: method, Code: resp.StatusCode, Description: "malformed result"}
		}
	}
	return nil
}

// TextLen is the length of s the way Telegram counts it: UTF-16 code units.
func TextLen(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// Split cuts s into pieces of at most limit UTF-16 code units, preferring to
// cut after a newline, then after a space. Empty input gives no pieces.
func Split(s string, limit int) []string {
	var out []string
	for s != "" {
		if TextLen(s) <= limit {
			out = append(out, s)
			break
		}
		// end is the byte offset of the longest prefix within limit.
		end, n := 0, 0
		for i, r := range s {
			if n+utf16.RuneLen(r) > limit {
				break
			}
			n += utf16.RuneLen(r)
			end = i + utf8.RuneLen(r)
		}
		if end == 0 { // limit below one character; never loop forever
			_, end = utf8.DecodeRuneInString(s)
		}
		cut := end
		if i := strings.LastIndexByte(s[:end], '\n'); i > end/2 {
			cut = i + 1
		} else if i := strings.LastIndexByte(s[:end], ' '); i > end/2 {
			cut = i + 1
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	return out
}
