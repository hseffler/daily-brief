package main

import (
	"context"

	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestSplitTelegram(t *testing.T) {
	t.Run("short text stays one chunk", func(t *testing.T) {
		chunks := splitTelegram("hello", 4000)
		if len(chunks) != 1 || chunks[0] != "hello" {
			t.Errorf("got %v", chunks)
		}
	})

	t.Run("long text without newlines is split at the limit", func(t *testing.T) {
		text := strings.Repeat("a", 9000)
		chunks := splitTelegram(text, 4000)
		if len(chunks) != 3 {
			t.Fatalf("expected 3 chunks, got %d", len(chunks))
		}
		for i, c := range chunks {
			if len([]rune(c)) > 4000 {
				t.Errorf("chunk %d too long: %d runes", i, len([]rune(c)))
			}
		}
		if strings.Join(chunks, "") != text {
			t.Error("chunks should reassemble the original text")
		}
	})

	t.Run("prefers newline boundaries", func(t *testing.T) {
		text := strings.Repeat("word ", 2000) // 10000 chars, spaces only
		chunks := splitTelegram(text, 4000)
		if len(chunks) < 3 {
			t.Fatalf("expected >= 3 chunks, got %d", len(chunks))
		}
		for _, c := range chunks {
			if len([]rune(c)) > 4000 {
				t.Errorf("chunk too long: %d runes", len([]rune(c)))
			}
		}
	})

	t.Run("unicode is not split mid-rune", func(t *testing.T) {
		text := strings.Repeat("ä", 5000) // 2 bytes per rune
		chunks := splitTelegram(text, 4000)
		for i, c := range chunks {
			if !utf8.ValidString(c) {
				t.Errorf("chunk %d is not valid UTF-8", i)
			}
			if len([]rune(c)) > 4000 {
				t.Errorf("chunk %d too long: %d runes", i, len([]rune(c)))
			}
		}
		if strings.Join(chunks, "") != text {
			t.Error("chunks should reassemble the original text")
		}
	})
}

// telegramRecorder records sendMessage calls.
type telegramRecorder struct {
	mu         sync.Mutex
	texts      []string
	parseModes []string
	chatID     string
	// failFirst429 makes the first request return 429 with retry_after=1.
	failFirst429 bool
	requests     int
}

func (r *telegramRecorder) handler(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.requests++
	fail := r.failFirst429 && r.requests == 1
	r.mu.Unlock()

	if fail {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"ok":false,"parameters":{"retry_after":1}}`))
		return
	}
	if err := req.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	r.texts = append(r.texts, req.Form.Get("text"))
	r.parseModes = append(r.parseModes, req.Form.Get("parse_mode"))
	r.chatID = req.Form.Get("chat_id")
	r.mu.Unlock()
	w.Write([]byte(`{"ok":true}`))
}

func TestTelegramSendChunks(t *testing.T) {
	rec := &telegramRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	sender := NewTelegramSender("tok", "chat42", srv.Client(), testLogger())
	sender.apiURL = srv.URL

	text := strings.Repeat("x", 9000)
	if err := sender.Send(context.Background(), text); err != nil {
		t.Fatalf("Send: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.texts) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(rec.texts))
	}
	for i, txt := range rec.texts {
		if len([]rune(txt)) > telegramMaxMessageLen {
			t.Errorf("message %d exceeds Telegram limit: %d runes", i, len([]rune(txt)))
		}
	}
	if rec.chatID != "chat42" {
		t.Errorf("chat_id: got %q, want chat42", rec.chatID)
	}
	if strings.Join(rec.texts, "") != text {
		t.Error("messages should reassemble the original text")
	}
}

func TestTelegramSendRetriesOn429(t *testing.T) {
	rec := &telegramRecorder{failFirst429: true}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	sender := NewTelegramSender("tok", "chat42", srv.Client(), testLogger())
	sender.apiURL = srv.URL

	if err := sender.Send(context.Background(), "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.requests != 2 {
		t.Errorf("expected 2 requests (1 retry), got %d", rec.requests)
	}
	if len(rec.texts) != 1 || rec.texts[0] != "hello" {
		t.Errorf("unexpected texts: %v", rec.texts)
	}
}

func TestTelegramSendError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	sender := NewTelegramSender("tok", "chat42", srv.Client(), testLogger())
	sender.apiURL = srv.URL

	err := sender.Send(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status: %v", err)
	}
}

func TestSplitTelegramExampleSizedInput(t *testing.T) {
	chunks := splitTelegram(strings.Repeat("ab", 3000), 4000)
	if len(chunks) != 2 || len([]rune(chunks[0])) != 4000 {
		t.Errorf("got %d chunks of %d runes", len(chunks), len([]rune(chunks[0])))
	}
}

func TestSplitTelegramKeepsTagsWhole(t *testing.T) {
	t.Run("tag near the limit is not split", func(t *testing.T) {
		text := strings.Repeat("a", 3995) + "<b>bold</b>" + strings.Repeat("b", 100)
		chunks := splitTelegram(text, 4000)
		if len(chunks) < 2 {
			t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
		}
		for i, c := range chunks {
			if strings.Count(c, "<") != strings.Count(c, ">") {
				t.Errorf("chunk %d contains a partial HTML tag", i)
			}
		}
		if strings.Join(chunks, "") != text {
			t.Error("chunks should reassemble the original text")
		}
	})

	t.Run("tag straddling the limit stays whole", func(t *testing.T) {
		text := strings.Repeat("a", 3999) + "<b>x</b>"
		chunks := splitTelegram(text, 4000)
		if len(chunks) != 2 {
			t.Fatalf("expected 2 chunks, got %d", len(chunks))
		}
		if !strings.HasSuffix(chunks[0], "<b>") {
			t.Errorf("chunk 0 should end with the whole <b> tag, got tail %q", chunks[0][len(chunks[0])-10:])
		}
		if chunks[1] != "x</b>" {
			t.Errorf("chunk 1: got %q, want %q", chunks[1], "x</b>")
		}
	})

	t.Run("heart is not mistaken for a tag", func(t *testing.T) {
		text := strings.Repeat("a", 3999) + "<3" + strings.Repeat("b", 100)
		chunks := splitTelegram(text, 4000)
		if strings.Join(chunks, "") != text {
			t.Error("chunks should reassemble the original text")
		}
		for i, c := range chunks {
			if len([]rune(c)) > 4000 {
				t.Errorf("chunk %d too long: %d runes", i, len([]rune(c)))
			}
		}
	})
}

func TestTelegramSendUsesHTMLParseMode(t *testing.T) {
	rec := &telegramRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	sender := NewTelegramSender("tok", "chat42", srv.Client(), testLogger())
	sender.apiURL = srv.URL

	text := "📰 <b>Top news</b>\n▪ story"
	if err := sender.Send(context.Background(), text); err != nil {
		t.Fatalf("Send: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.parseModes) != 1 || rec.parseModes[0] != "HTML" {
		t.Errorf("parse_mode: got %v, want [HTML]", rec.parseModes)
	}
	if len(rec.texts) != 1 || rec.texts[0] != text {
		t.Errorf("texts: got %v, want [%q]", rec.texts, text)
	}
}

func TestTelegramSendFallsBackToPlainTextOnParseError(t *testing.T) {
	var mu sync.Mutex
	var got []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		got = append(got, r.Form)
		html := r.Form.Get("parse_mode") == "HTML"
		mu.Unlock()
		if html {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities: unsupported start tag \"del\" at byte offset 12"}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	sender := NewTelegramSender("tok", "chat42", srv.Client(), testLogger())
	sender.apiURL = srv.URL

	err := sender.Send(context.Background(), "📧 <b>Emails</b>\n▪ a < b & c")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("expected 2 requests (HTML + plain retry), got %d", len(got))
	}
	if got[0].Get("parse_mode") != "HTML" {
		t.Errorf("first request parse_mode: got %q, want HTML", got[0].Get("parse_mode"))
	}
	if got[1].Get("parse_mode") != "" {
		t.Errorf("fallback request should have no parse_mode, got %q", got[1].Get("parse_mode"))
	}
	want := "📧 Emails\n▪ a < b & c"
	if got[1].Get("text") != want {
		t.Errorf("fallback text: got %q, want %q", got[1].Get("text"), want)
	}
}
