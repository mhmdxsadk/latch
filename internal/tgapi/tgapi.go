// Package tgapi is a small client for the Telegram Bot API: long polling for
// updates, sending and editing messages, inline buttons, button answers and
// the typing indicator. It never sets parse_mode: formatting is sent as
// entities this program computed (bold, italic, code and the like, never
// links), so no text can turn into markup by itself.
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
	// Photo lists the sizes of a photo; Caption is the text sent with it.
	Photo   []PhotoSize `json:"photo,omitempty"`
	Caption string      `json:"caption,omitempty"`
	// Document is a file sent as a file. MediaGroupID is shared by the
	// photos or files of one album.
	Document     *Document `json:"document,omitempty"`
	MediaGroupID string    `json:"media_group_id,omitempty"`
	// Entities is the formatting Telegram stored for Text.
	Entities []Entity `json:"entities,omitempty"`
	// ForwardOrigin is set on forwarded messages, which are ignored.
	ForwardOrigin json.RawMessage `json:"forward_origin,omitempty"`
}

// PhotoSize is one size of a photo. FileSize may be 0 when Telegram omits it.
type PhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size,omitempty"`
}

// Document is a file sent as a file.
type Document struct {
	FileID   string `json:"file_id"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
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
	// Stopped is the owner pressing Stop under a streamed draft.
	Stopped *GenerationStopped `json:"stopped_message_generation,omitempty"`
}

// GenerationStopped says which draft the user asked to stop.
type GenerationStopped struct {
	Chat    Chat  `json:"chat"`
	DraftID int64 `json:"draft_id"`
}

// Button styles.
const (
	StyleSuccess = "success" // green
	StyleDanger  = "danger"  // red
	StylePrimary = "primary" // blue
)

// Button is an inline keyboard button. It either sends Data back when
// pressed, or copies Copy to the clipboard.
type Button struct {
	Text  string    `json:"text"`
	Data  string    `json:"callback_data,omitempty"`
	Copy  *CopyText `json:"copy_text,omitempty"`
	Style string    `json:"style,omitempty"`
}

// CopyText is the text a copy button puts on the clipboard (1-256 chars).
type CopyText struct {
	Text string `json:"text"`
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
		"allowed_updates": []string{"message", "callback_query", "stopped_message_generation"},
	}, &out)
	return out, err
}

// Confirm tells Telegram that every update before offset was handled, so
// they are not delivered again after a restart. It does not wait.
func (c *Client) Confirm(ctx context.Context, offset int64) error {
	var out []Update
	return c.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 0, "limit": 1}, &out)
}

// Send sends t to chat, with buttons when kb is not nil. If Telegram
// refuses the formatting, the text goes out again without it.
func (c *Client) Send(ctx context.Context, chat int64, t Text, kb Keyboard) (Message, error) {
	body := map[string]any{
		"chat_id":              chat,
		"text":                 t.S,
		"link_preview_options": map[string]bool{"is_disabled": true},
	}
	if len(t.Entities) > 0 {
		body["entities"] = t.Entities
	}
	if kb != nil {
		body["reply_markup"] = map[string]any{"inline_keyboard": kb}
	}
	var m Message
	err := c.call(ctx, "sendMessage", body, &m)
	if isEntityError(err) {
		delete(body, "entities")
		err = c.call(ctx, "sendMessage", body, &m)
	}
	return m, err
}

// Edit replaces the text and buttons of a message the bot sent. A nil kb
// removes the buttons. An edit that changes nothing is not an error.
func (c *Client) Edit(ctx context.Context, chat, message int64, t Text, kb Keyboard) error {
	if kb == nil {
		kb = Keyboard{}
	}
	body := map[string]any{
		"chat_id":              chat,
		"message_id":           message,
		"text":                 t.S,
		"link_preview_options": map[string]bool{"is_disabled": true},
		"reply_markup":         map[string]any{"inline_keyboard": kb},
	}
	if len(t.Entities) > 0 {
		body["entities"] = t.Entities
	}
	err := c.call(ctx, "editMessageText", body, nil)
	if isEntityError(err) {
		delete(body, "entities")
		err = c.call(ctx, "editMessageText", body, nil)
	}
	if IsNotModified(err) {
		return nil
	}
	return err
}

// isEntityError reports whether Telegram refused a message's formatting.
func isEntityError(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == http.StatusBadRequest && strings.Contains(strings.ToLower(e.Description), "entit")
}

// Reply sends t as a reply to the user's message replyTo (still sent if
// that message is gone).
func (c *Client) Reply(ctx context.Context, chat, replyTo int64, t Text) (Message, error) {
	body := map[string]any{
		"chat_id":              chat,
		"text":                 t.S,
		"link_preview_options": map[string]bool{"is_disabled": true},
		"reply_parameters":     map[string]any{"message_id": replyTo, "allow_sending_without_reply": true},
	}
	if len(t.Entities) > 0 {
		body["entities"] = t.Entities
	}
	var m Message
	err := c.call(ctx, "sendMessage", body, &m)
	if isEntityError(err) {
		delete(body, "entities")
		err = c.call(ctx, "sendMessage", body, &m)
	}
	return m, err
}

// Draft streams a partial message to a private chat: changes to the same
// draftID are animated, an empty text shows "Thinking...", and canStop adds
// a Stop button (pressing it arrives as Update.Stopped). A draft is a
// preview that lasts about 30 seconds; Send the finished text to keep it.
func (c *Client) Draft(ctx context.Context, chat, draftID int64, t Text, canStop bool) error {
	body := map[string]any{"chat_id": chat, "draft_id": draftID, "text": t.S}
	if len(t.Entities) > 0 {
		body["entities"] = t.Entities
	}
	if canStop {
		body["can_stop"] = true
	}
	err := c.call(ctx, "sendMessageDraft", body, nil)
	if isEntityError(err) {
		delete(body, "entities")
		err = c.call(ctx, "sendMessageDraft", body, nil)
	}
	return err
}

// MaxDelete is how many messages one DeleteMessages call takes.
const MaxDelete = 100

// DeleteMessages deletes up to MaxDelete messages of chat, the bot's own and
// the owner's (a private chat allows both). Telegram skips ids it cannot
// find and only deletes messages younger than 48 hours.
func (c *Client) DeleteMessages(ctx context.Context, chat int64, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > MaxDelete {
		return errors.New("telegram deleteMessages: too many ids")
	}
	return c.call(ctx, "deleteMessages", map[string]any{"chat_id": chat, "message_ids": ids}, nil)
}

// SetDescription sets what the bot's profile and empty chat say: the
// description (up to 512 characters) and the short description (120).
func (c *Client) SetDescription(ctx context.Context, description, short string) error {
	if err := c.call(ctx, "setMyDescription", map[string]any{"description": description}, nil); err != nil {
		return err
	}
	return c.call(ctx, "setMyShortDescription", map[string]any{"short_description": short}, nil)
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

// Download fetches the file fileID and returns its bytes. A file over max
// bytes is an error, and nothing past max is read.
func (c *Client) Download(ctx context.Context, fileID string, max int64) ([]byte, error) {
	var f struct {
		FilePath string `json:"file_path"`
		FileSize int64  `json:"file_size"`
	}
	if err := c.call(ctx, "getFile", map[string]any{"file_id": fileID}, &f); err != nil {
		return nil, err
	}
	if f.FileSize > max {
		return nil, fmt.Errorf("telegram download: file is %d bytes, over the %d limit", f.FileSize, max)
	}
	if f.FilePath == "" || strings.ContainsAny(f.FilePath, "?#") || strings.Contains(f.FilePath, "..") {
		return nil, errors.New("telegram download: bad file path")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/file/bot"+c.token+"/"+f.FilePath, nil)
	if err != nil {
		return nil, errors.New("telegram download: bad request")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// Keep only the cause: *url.Error carries the URL and the token.
		var ue interface{ Unwrap() error }
		if errors.As(err, &ue) && ue.Unwrap() != nil {
			err = ue.Unwrap()
		}
		return nil, fmt.Errorf("telegram download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram download: status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("telegram download: %w", err)
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("telegram download: file is over the %d limit", max)
	}
	return b, nil
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
