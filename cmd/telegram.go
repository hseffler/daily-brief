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
	// telegramMaxMessageLen is the Bot API hard limit per message.
	telegramMaxMessageLen = 4096
	// telegramChunkLen is the target chunk size, kept below the limit.
	telegramChunkLen = 4000
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
		token:      token,
		chatID:     chatID,
		httpClient: httpClient,
		apiURL:     telegramAPIBase + "/bot" + token,
		logger:     logger,
	}
}

// Send posts text to the configured chat, splitting it into chunks of at most
// telegramMaxMessageLen characters (rune-safe, preferring newline boundaries,
// never breaking inside an HTML tag). Chunks are sent with parse_mode=HTML so
// the brief's <b> section headers render bold. If Telegram rejects the markup
// (HTTP 400 "can't parse entities", e.g. because the model emitted a stray
// <, > or &), the chunk is retried once as plain text with all tags stripped,
// so delivery never fails on formatting.
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

// splitTelegram splits text into chunks of at most maxLen runes. It prefers
// to break at a newline; if none is found in the first half of the window it
// breaks at the hard limit. Chunking is rune-based so multi-byte characters
// are never split, and it never breaks inside an HTML tag (<...>), so chunks
// stay well-formed when sent with parse_mode=HTML. A tag straddling the limit
// is kept whole, which can push that chunk a few runes past maxLen.
func splitTelegram(text string, maxLen int) []string {
	runes := []rune(text)
	if len(runes) <= maxLen {
		return []string{text}
	}

	var chunks []string
	start := 0   // first rune of the current chunk
	lastNL := -1 // last newline outside a tag (absolute index)
	inTag := false
	for i := start; i < len(runes); {
		r := runes[i]
		if r == '<' && i+1 < len(runes) && isTagStart(runes[i+1]) {
			inTag = true
		}
		if !inTag && r == '\n' {
			lastNL = i
		}
		i++
		if r == '>' {
			inTag = false
		}
		if i-start < maxLen || inTag {
			continue
		}
		cut := i
		if lastNL >= start+maxLen/2 {
			cut = lastNL
		}
		chunks = append(chunks, string(runes[start:cut]))
		start = cut
		for start < len(runes) && runes[start] == '\n' {
			start++
		}
		i = start
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

// sendChunk posts one message with the given parse mode ("" for plain text),
// retrying once after the server-advised delay when rate limited (HTTP 429).
func (t *TelegramSender) sendChunk(ctx context.Context, text, parseMode string) error {
	form := url.Values{}
	form.Set("chat_id", t.chatID)
	form.Set("text", text)
	if parseMode != "" {
		form.Set("parse_mode", parseMode)
	}

	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
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
			t.logger.WithField("retry_after", retryAfter).Warn("rate limited by Telegram, backing off")
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryAfter):
			}
			continue
		}

		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return &telegramAPIError{statusCode: resp.StatusCode, body: string(snippet)}
	}
	return lastErr
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
