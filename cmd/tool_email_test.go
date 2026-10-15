package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeFetcher implements EmailFetcher with scripted mails.
type fakeFetcher struct {
	emails   []Email
	err      error
	gotSince time.Time
	gotMax   int
}

func (f *fakeFetcher) FetchNew(ctx context.Context, since time.Time, max int) ([]Email, error) {
	f.gotSince = since
	f.gotMax = max
	return f.emails, f.err
}

func testEmailConfig() *Config {
	cfg := &Config{}
	cfg.setDefaults()
	return cfg
}

func TestEmailToolReturnsWindowMails(t *testing.T) {
	fetcher := &fakeFetcher{emails: []Email{
		{UID: 1, From: "alice@example.com", Subject: "Hello", Date: time.Date(2025, 5, 14, 8, 0, 0, 0, time.UTC), Snippet: "Hi there"},
		{UID: 2, From: "bob@example.com", Subject: "Newsletter", Date: time.Date(2025, 5, 14, 9, 0, 0, 0, time.UTC)},
	}}
	tool := newEmailTool(testEmailConfig(), fetcher, testLogger())

	out, err := tool.Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{"2 email(s) from the last 24 hours", "alice@example.com", "Hello", "Hi there", "bob@example.com", "Newsletter"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// Stateless: a second call re-reports the same window mails (the brief
	// model deduplicates when writing).
	out2, err := tool.Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if !strings.Contains(out2, "alice@example.com") {
		t.Errorf("expected the same mails again (no dedupe state):\n%s", out2)
	}
}

func TestEmailToolEmpty(t *testing.T) {
	fetcher := &fakeFetcher{}
	tool := newEmailTool(testEmailConfig(), fetcher, testLogger())

	out, err := tool.Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "No emails in the last 24 hours." {
		t.Errorf("got %q", out)
	}
}

func TestEmailToolCapsAtMaxMessages(t *testing.T) {
	cfg := testEmailConfig()
	cfg.Sources.Email.MaxMessages = 2

	mkMail := func(uid uint32, subject string, hour int) Email {
		return Email{UID: uid, From: "x@example.com", Subject: subject,
			Date: time.Date(2025, 5, 14, hour, 0, 0, 0, time.UTC)}
	}
	fetcher := &fakeFetcher{emails: []Email{
		mkMail(1, "m1", 8), mkMail(2, "m2", 9), mkMail(3, "m3", 10), mkMail(4, "m4", 11),
	}}
	tool := newEmailTool(cfg, fetcher, testLogger())

	out, err := tool.Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "2 email(s)") || !strings.Contains(out, "m3") || !strings.Contains(out, "m4") {
		t.Errorf("expected the newest 2 mails:\n%s", out)
	}
	if strings.Contains(out, "m1") || strings.Contains(out, "m2") {
		t.Errorf("oldest mails should be capped out:\n%s", out)
	}
}

func TestEmailToolUsesSinceHoursWindow(t *testing.T) {
	cfg := testEmailConfig()
	cfg.Sources.Email.MaxMessages = 10
	fetcher := &fakeFetcher{}
	tool := newEmailTool(cfg, fetcher, testLogger())

	before := time.Now()
	if _, err := tool.Run(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	after := time.Now()

	// since should be ~24h ago (default since_hours).
	if fetcher.gotSince.Before(before.Add(-25*time.Hour)) || fetcher.gotSince.After(after.Add(-23*time.Hour)) {
		t.Errorf("since = %v, want ~24h before now", fetcher.gotSince)
	}
	// The fetch limit must exceed the brief cap.
	if fetcher.gotMax != 100 {
		t.Errorf("fetch limit = %d, want 100 (max_messages 10 x4, min 100)", fetcher.gotMax)
	}
}

func TestEmailToolFetchLimit(t *testing.T) {
	cases := []struct{ maxMessages, want int }{
		{25, 100}, // 25*4 = 100
		{30, 120}, // 30*4 = 120
		{10, 100}, // 10*4 = 40, raised to the minimum
		{1, 100},
	}
	for _, tc := range cases {
		if got := fetchLimit(tc.maxMessages); got != tc.want {
			t.Errorf("fetchLimit(%d) = %d, want %d", tc.maxMessages, got, tc.want)
		}
	}
}

func TestEmailToolFetcherError(t *testing.T) {
	fetcher := &fakeFetcher{err: errors.New("connection refused")}
	tool := newEmailTool(testEmailConfig(), fetcher, testLogger())

	if _, err := tool.Run(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected fetcher error to propagate")
	}
}

func TestEmailSnippetPlainText(t *testing.T) {
	raw := "From: a@b.c\r\nSubject: s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nHello plain body"
	if got := emailSnippet(raw, 500); got != "Hello plain body" {
		t.Errorf("got %q", got)
	}
}

func TestEmailSnippetMultipartQuotedPrintable(t *testing.T) {
	raw := "From: a@b.c\r\nSubject: s\r\nContent-Type: multipart/alternative; boundary=\"BOUND\"\r\n\r\n" +
		"--BOUND\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"Gr=C3=BC=C3=9Fe aus M=C3=BCnchen\r\n" +
		"--BOUND\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" +
		"<p>Grüße aus München</p>\r\n" +
		"--BOUND--\r\n"
	got := emailSnippet(raw, 500)
	if got != "Grüße aus München" {
		t.Errorf("got %q, want the decoded text/plain part", got)
	}
}

func TestEmailSnippetBase64(t *testing.T) {
	// "Hello base64 body" in base64.
	raw := "From: a@b.c\r\nSubject: s\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\nSGVsbG8gYmFzZTY0IGJvZHk=\r\n"
	if got := emailSnippet(raw, 500); got != "Hello base64 body" {
		t.Errorf("got %q", got)
	}
}

func TestEmailSnippetTruncates(t *testing.T) {
	raw := "From: a@b.c\r\nSubject: s\r\nContent-Type: text/plain\r\n\r\n" + strings.Repeat("x", 2000)
	got := emailSnippet(raw, 100)
	if n := utf8.RuneCountInString(got); n != 101 { // 100 chars + ellipsis rune
		t.Errorf("rune count = %d, want 101", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis suffix, got %q", got)
	}
}
