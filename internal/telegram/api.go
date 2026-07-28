// Package telegram exposes container control through a Telegram bot.
//
// It talks to the Bot API over plain HTTPS with long polling, so no library and
// no inbound port are needed — which matters for a home server behind NAT.
package telegram

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
	"unicode/utf8"
)

// pollTimeout is how long the Bot API holds a getUpdates request open when there
// is nothing to report. Long polling like this gives near-instant delivery
// without hammering the API.
const pollTimeout = 30 * time.Second

// Bot API limits worth respecting rather than discovering at runtime.
const (
	maxMessageRunes  = 4096
	maxCallbackRunes = 190
)

// api is a thin wrapper around the Bot API methods Dockontroler uses.
type api struct {
	http  *http.Client
	token string
}

func newAPI(token string) *api {
	return &api{
		// Comfortably longer than pollTimeout, so a held-open poll is never cut
		// off by the client itself.
		http:  &http.Client{Timeout: pollTimeout + 30*time.Second},
		token: token,
	}
}

// call invokes one Bot API method. out may be nil when the result is not needed.
func (a *api) call(ctx context.Context, method string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode %s request: %w", method, err)
		}
		body = bytes.NewReader(encoded)
	}

	endpoint := "https://api.telegram.org/bot" + a.token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return fmt.Errorf("build %s request: %w", method, err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.http.Do(req)
	if err != nil {
		// Failures come back as *url.Error, which quotes the request URL — and
		// the URL contains the bot token. Never propagate it unredacted.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("telegram %s: %s", method, a.redact(err.Error()))
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("telegram %s: read response: %s", method, a.redact(err.Error()))
	}

	var envelope struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("telegram %s: malformed response (HTTP %d)", method, resp.StatusCode)
	}
	if !envelope.OK {
		return &apiError{Method: method, Code: envelope.ErrorCode, Description: envelope.Description}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, out); err != nil {
		return fmt.Errorf("telegram %s: decode result: %w", method, err)
	}
	return nil
}

// redact removes the bot token from text so it cannot reach a log file.
func (a *api) redact(text string) string {
	if a.token == "" {
		return text
	}
	return strings.ReplaceAll(text, a.token, "<token>")
}

// apiError is an error reported by the Bot API itself.
type apiError struct {
	Method      string
	Code        int
	Description string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("telegram %s: %s (error_code %d)", e.Method, e.Description, e.Code)
}

// getMe verifies the token and returns the bot's own account.
func (a *api) getMe(ctx context.Context) (User, error) {
	var me User
	err := a.call(ctx, "getMe", nil, &me)
	return me, err
}

// getUpdates fetches pending updates, blocking until one arrives or pollTimeout
// expires. offset is the id of the first update not yet confirmed.
func (a *api) getUpdates(ctx context.Context, offset int64) ([]Update, error) {
	payload := map[string]any{
		"offset":  offset,
		"timeout": int(pollTimeout.Seconds()),
		// Everything else — edited messages, channel posts, polls — would only
		// have to be filtered out again.
		"allowed_updates": []string{"message", "callback_query"},
	}
	var updates []Update
	err := a.call(ctx, "getUpdates", payload, &updates)
	return updates, err
}

// sendMessage posts a new message. markup may be nil.
func (a *api) sendMessage(ctx context.Context, chatID int64, text string, markup *InlineKeyboardMarkup) error {
	payload := map[string]any{
		"chat_id":    chatID,
		"text":       truncate(text, maxMessageRunes),
		"parse_mode": "HTML",
		// Image references are not links, and a preview would push the buttons
		// off screen.
		"link_preview_options": map[string]any{"is_disabled": true},
	}
	if markup != nil {
		payload["reply_markup"] = markup
	}
	return a.call(ctx, "sendMessage", payload, nil)
}

// editMessage rewrites an existing message in place, which is how the bot shows
// new state without filling the chat with near-identical messages.
func (a *api) editMessage(ctx context.Context, chatID, messageID int64, text string, markup *InlineKeyboardMarkup) error {
	payload := map[string]any{
		"chat_id":              chatID,
		"message_id":           messageID,
		"text":                 truncate(text, maxMessageRunes),
		"parse_mode":           "HTML",
		"link_preview_options": map[string]any{"is_disabled": true},
	}
	if markup != nil {
		payload["reply_markup"] = markup
	}

	err := a.call(ctx, "editMessageText", payload, nil)

	// Telegram rejects an edit that would change nothing. That happens routinely
	// — tapping the policy a container already has — and is not a problem.
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusBadRequest &&
		strings.Contains(strings.ToLower(apiErr.Description), "message is not modified") {
		return nil
	}
	return err
}

// answerCallback dismisses the loading spinner on an inline button. Telegram
// expects this within a few seconds, so it is always sent before any slow work
// begins.
func (a *api) answerCallback(ctx context.Context, callbackID, text string, alert bool) error {
	payload := map[string]any{
		"callback_query_id": callbackID,
		"show_alert":        alert,
	}
	if text != "" {
		payload["text"] = truncate(text, maxCallbackRunes)
	}
	return a.call(ctx, "answerCallbackQuery", payload, nil)
}

// truncate shortens text to at most limit runes, counting runes rather than
// bytes because Telegram's limits are expressed in UTF-16 code units and
// container names may well contain non-ASCII characters.
func truncate(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit-1]) + "…"
}
