package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	// telegramAPIBase is the Bot API endpoint. Only the token is sensitive and
	// comes from the config.
	telegramAPIBase = "https://api.telegram.org"
	// telegramMaxMessageLen is the Bot API hard limit per message, counted in
	// UTF-16 code units: astral characters (most emojis) count as two, so a
	// rune-based count would underestimate the real message length.
	telegramMaxMessageLen = 4096
	// telegramChunkLen is the target chunk size in UTF-16 code units, kept
	// below the limit.
	telegramChunkLen = 4000
	// tagSlackUnits is how far past maxLen a chunk may grow (in UTF-16 units)
	// to keep an HTML tag whole. It bounds the damage of an unclosed tag
	// (e.g. a stray "<b" with no ">"): without it the chunk could grow past
	// telegramMaxMessageLen and Telegram would reject it as too long. Any
	// legitimate tag (<pre>, </pre>, <b>, ...) is far shorter than this, and
	// the worst-case chunk stays at maxLen+tagSlackUnits = 4090 < 4096.
	tagSlackUnits = 90
	// telegramMaxAttempts is the total number of send attempts per chunk for
	// retryable failures (HTTP 429 and 5xx).
	telegramMaxAttempts = 3
)

// TelegramSender delivers the brief to a Telegram chat, in the same style as
// hostmonitor's notification sender.
type TelegramSender struct {
	token      string
	chatID     string
	httpClient *http.Client
	// apiURL is the base ("https://api.telegram.org/bot<token>") and is a
	// field so tests can point it at an httptest server.
	apiURL string
	logger *logrus.Logger
	// backoffBase is the base delay for retries on 5xx responses
	// (attempt n waits n*backoffBase); 429 responses use the server-advised
	// retry_after instead. A field so tests can shrink it.
	backoffBase time.Duration
}

// NewTelegramSender creates a sender. A nil httpClient gets a default.
func NewTelegramSender(token, chatID string, httpClient *http.Client, logger *logrus.Logger) *TelegramSender {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if logger == nil {
		logger = logrus.New()
	}
	return &TelegramSender{
		token:       token,
		chatID:      chatID,
		httpClient:  httpClient,
		apiURL:      telegramAPIBase + "/bot" + token,
		logger:      logger,
		backoffBase: 2 * time.Second,
	}
}

// Send posts text to the configured chat, splitting it into chunks of at most
// telegramMaxMessageLen UTF-16 code units (Telegram counts the limit in UTF-16
// units, so astral characters like emojis count double; chunking is
// rune-safe so multi-byte characters are never split, prefers newline
// boundaries, and never breaks inside an HTML tag). Chunks are sent with
// parse_mode=HTML so the brief's <b> section headers render bold. If Telegram
// rejects the markup (HTTP 400 "can't parse entities", e.g. because the model
// emitted a stray <, > or &), the chunk is retried once as plain text with
// all tags stripped, so delivery never fails on formatting.
func (t *TelegramSender) Send(ctx context.Context, text string) error {
	chunks := splitTelegram(text, telegramChunkLen)
	for i, chunk := range chunks {
		err := t.sendChunk(ctx, chunk, "HTML")
		if isEntityParseError(err) {
			t.logger.WithError(err).Warn("Telegram rejected the HTML formatting, resending chunk as plain text")
			err = t.sendChunk(ctx, stripHTMLTags(chunk), "")
		}
		if err != nil {
			return fmt.Errorf("sending Telegram message %d/%d failed: %w", i+1, len(chunks), err)
		}
	}
	t.logger.WithFields(logrus.Fields{
		"chat_id": t.chatID,
		"chunks":  len(chunks),
	}).Info("brief delivered via Telegram")
	return nil
}

// utf16Units returns how many UTF-16 code units r encodes to (astral
// characters, e.g. most emojis, count as two).
func utf16Units(r rune) int {
	if r >= 0x10000 {
		return 2
	}
	return 1
}

// utf16Len returns the length of s in UTF-16 code units — the unit Telegram's
// message length limit (4096) is counted in.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16Units(r)
	}
	return n
}

// splitTelegram splits text into chunks of at most maxLen UTF-16 code units
// (plus a small slack for HTML tags, see tagSlackUnits). It prefers to break
// at a newline; if none is found in the first half of the window it breaks at
// the hard limit. Chunking is rune-based so multi-byte characters are never
// split, and it never breaks inside an HTML tag (<...>), so chunks stay
// well-formed when sent with parse_mode=HTML. A tag straddling the limit is
// kept whole, which can push that chunk a few units past maxLen — but never
// past maxLen+tagSlackUnits, so even an unclosed tag cannot produce a chunk
// exceeding Telegram's hard limit.
func splitTelegram(text string, maxLen int) []string {
	runes := []rune(text)
	if utf16Len(text) <= maxLen {
		return []string{text}
	}

	var chunks []string
	start := 0       // first rune of the current chunk
	units := 0       // UTF-16 units in runes[start:i]
	lastNL := -1     // last newline outside a tag (absolute rune index)
	lastNLUnits := 0 // UTF-16 units before that newline, relative to start
	inTag := false
	for i := start; i < len(runes); {
		r := runes[i]
		if r == '<' && i+1 < len(runes) && isTagStart(runes[i+1]) {
			inTag = true
		}
		if !inTag && r == '\n' {
			lastNL = i
			lastNLUnits = units
		}
		units += utf16Units(r)
		i++
		if r == '>' {
			inTag = false
		}
		// Cut when the chunk reaches maxLen units. Inside a tag, allow up to
		// maxLen+tagSlackUnits so legitimate tags stay whole; past that, cut
		// anyway so an unclosed tag cannot grow the chunk unboundedly.
		if units < maxLen || (inTag && units < maxLen+tagSlackUnits) {
			continue
		}
		cut := i
		if lastNL > start && lastNLUnits >= maxLen/2 {
			cut = lastNL
		}
		chunks = append(chunks, string(runes[start:cut]))
		start = cut
		for start < len(runes) && runes[start] == '\n' {
			start++
		}
		i = start
		units = 0
		lastNL = -1
	}
	if start < len(runes) {
		chunks = append(chunks, string(runes[start:]))
	}
	return chunks
}

// isTagStart reports whether r can follow '<' in an HTML tag: a letter for
// <b>, '/' for </b>. Digits are excluded so a heart like "<3" is not mistaken
// for a tag.
func isTagStart(r rune) bool {
	return r == '/' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// sendChunk posts one message with the given parse mode ("" for plain text).
// Transient failures are retried up to telegramMaxAttempts times: HTTP 429
// after the server-advised retry_after delay, HTTP 5xx after a linear backoff
// (n*backoffBase). All other errors are returned immediately.
func (t *TelegramSender) sendChunk(ctx context.Context, text, parseMode string) error {
	form := url.Values{}
	form.Set("chat_id", t.chatID)
	form.Set("text", text)
	if parseMode != "" {
		form.Set("parse_mode", parseMode)
	}

	var lastErr error
	for attempt := 1; attempt <= telegramMaxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.apiURL+"/sendMessage", strings.NewReader(form.Encode()))
		if err != nil {
			return fmt.Errorf("failed to build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, err := t.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("Telegram API request failed: %w", err)
		}

		if resp.StatusCode == http.StatusOK {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return nil
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfter := telegramRetryAfter(resp)
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("Telegram rate limited (429)")
			t.logger.WithFields(logrus.Fields{
				"attempt":     attempt,
				"retry_after": retryAfter,
			}).Warn("rate limited by Telegram, backing off")
			if !sleepCtx(ctx, retryAfter) {
				return ctx.Err()
			}
			continue
		}

		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		lastErr = &telegramAPIError{statusCode: resp.StatusCode, body: string(snippet)}

		// Retry server-side errors (5xx); client errors (400/403/404/...) are
		// permanent and returned immediately.
		if resp.StatusCode < 500 || attempt == telegramMaxAttempts {
			return lastErr
		}
		delay := time.Duration(attempt) * t.backoffBase
		t.logger.WithFields(logrus.Fields{
			"attempt": attempt,
			"status":  resp.StatusCode,
			"delay":   delay,
		}).Warn("Telegram server error, backing off")
		if !sleepCtx(ctx, delay) {
			return ctx.Err()
		}
	}
	return lastErr
}

// sleepCtx waits for d or until ctx is done; it reports whether the full
// delay elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// telegramAPIError is a non-200 response from the Bot API.
type telegramAPIError struct {
	statusCode int
	body       string
}

func (e *telegramAPIError) Error() string {
	return fmt.Sprintf("Telegram API returned status %d: %s", e.statusCode, strings.TrimSpace(e.body))
}

// isEntityParseError reports whether err is a 400 "can't parse entities"
// response, i.e. Telegram rejected the message's HTML markup.
func isEntityParseError(err error) bool {
	var apiErr *telegramAPIError
	return errors.As(err, &apiErr) &&
		apiErr.statusCode == http.StatusBadRequest &&
		strings.Contains(apiErr.body, "can't parse entities")
}

// htmlTagPattern matches HTML tags for the plain-text fallback.
var htmlTagPattern = regexp.MustCompile(`<[^>]*>`)

// stripHTMLTags removes HTML tags; used when Telegram rejects the markup.
func stripHTMLTags(s string) string {
	return htmlTagPattern.ReplaceAllString(s, "")
}

// telegramRetryAfter parses the retry_after parameter (seconds) from a 429
// response, falling back to a small fixed delay.
func telegramRetryAfter(resp *http.Response) time.Duration {
	const fallback = 2 * time.Second
	var body struct {
		Parameters struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err == nil && body.Parameters.RetryAfter > 0 {
		return time.Duration(body.Parameters.RetryAfter) * time.Second
	}
	return fallback
}
