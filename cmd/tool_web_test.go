package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFetchWebpageExtractsArticles(t *testing.T) {
	html := `<!DOCTYPE html><html><head><title>News Site</title></head><body>
	<header>site header junk</header>
	<nav>menu junk</nav>
	<article><h2>Headline One</h2><p>Teaser text one.</p></article>
	<article><h2>Headline Two</h2><p>Teaser text two.</p></article>
	<script>var junk = "scripttext";</script>
	<footer>footer junk</footer>
	</body></html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(html))
	}))
	defer srv.Close()

	out, err := fetchWebpage(context.Background(), srv.Client(), srv.URL, 8000, testLogger())
	if err != nil {
		t.Fatalf("fetchWebpage: %v", err)
	}

	for _, want := range []string{"URL: " + srv.URL, "Title: News Site", "Headline One", "Teaser text one.", "Headline Two", "Teaser text two."} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"scripttext", "menu junk", "footer junk", "site header junk"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("output should not contain %q:\n%s", unwanted, out)
		}
	}
}

func TestFetchWebpageFallsBackToMainOrBody(t *testing.T) {
	html := `<html><head><title>T</title></head><body><main><p>Main content here.</p></main></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(html))
	}))
	defer srv.Close()

	out, err := fetchWebpage(context.Background(), srv.Client(), srv.URL, 8000, testLogger())
	if err != nil {
		t.Fatalf("fetchWebpage: %v", err)
	}
	if !strings.Contains(out, "Main content here.") {
		t.Errorf("expected main content in output:\n%s", out)
	}
}

func TestFetchWebpageTruncates(t *testing.T) {
	longText := strings.Repeat("word ", 5000) // 25000 chars
	html := `<html><head><title>T</title></head><body><article><h2>H</h2><p>` + longText + `</p></article></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(html))
	}))
	defer srv.Close()

	out, err := fetchWebpage(context.Background(), srv.Client(), srv.URL, 1000, testLogger())
	if err != nil {
		t.Fatalf("fetchWebpage: %v", err)
	}
	if !strings.Contains(out, "... (truncated)") {
		t.Error("expected truncation marker")
	}
	// Header + truncated text must stay well below the full page.
	if len(out) > 1200 {
		t.Errorf("output not truncated: len=%d", len(out))
	}
}

func TestFetchWebpageHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := fetchWebpage(context.Background(), srv.Client(), srv.URL, 8000, testLogger())
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status: %v", err)
	}
}

func TestWebFetchToolValidatesURL(t *testing.T) {
	tool := newWebFetchTool(1000, 5, testLogger())
	if _, err := tool.Run(context.Background(), []byte(`{}`)); err == nil {
		t.Error("expected error for missing url")
	}
	if _, err := tool.Run(context.Background(), []byte(`not json`)); err == nil {
		t.Error("expected error for invalid json")
	}
}

func TestCollapseSpace(t *testing.T) {
	if got := collapseSpace("  a\n\tb  c  "); got != "a b c" {
		t.Errorf("got %q", got)
	}
}

func TestFetchWebpageExtractsRSSFeed(t *testing.T) {
	rss := `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>DIE ZEIT | Nachrichten</title>
    <link>https://www.zeit.de/index</link>
    <description>Aktuelle Nachrichten</description>
    <item>
      <title>Headline One: Something happened</title>
      <link>https://www.zeit.de/news/1</link>
      <description>Teaser one</description>
    </item>
    <item>
      <title>Headline Two: Another event</title>
      <link>https://www.zeit.de/news/2</link>
      <description>Teaser two</description>
    </item>
    <item>
      <title>Headline Three: Third story</title>
      <link>https://www.zeit.de/news/3</link>
    </item>
  </channel>
</rss>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
		w.Write([]byte(rss))
	}))
	defer srv.Close()

	out, err := fetchWebpage(context.Background(), srv.Client(), srv.URL, 8000, testLogger())
	if err != nil {
		t.Fatalf("fetchWebpage: %v", err)
	}

	// RSS item titles should appear as bullet points.
	for _, want := range []string{"- Headline One: Something happened", "- Headline Two: Another event", "- Headline Three: Third story"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// Channel metadata should not appear in the extracted headlines.
	for _, unwanted := range []string{"Aktuelle Nachrichten", "Teaser one", "Teaser two"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("output should not contain %q:\n%s", unwanted, out)
		}
	}
}

func TestFetchWebpageExtractsAtomFeed(t *testing.T) {
	atom := `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Example News</title>
  <entry>
    <title>Atom Headline One</title>
    <link href="https://example.com/1"/>
  </entry>
  <entry>
    <title>Atom Headline Two</title>
    <link href="https://example.com/2"/>
  </entry>
</feed>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
		w.Write([]byte(atom))
	}))
	defer srv.Close()

	out, err := fetchWebpage(context.Background(), srv.Client(), srv.URL, 8000, testLogger())
	if err != nil {
		t.Fatalf("fetchWebpage: %v", err)
	}

	for _, want := range []string{"- Atom Headline One", "- Atom Headline Two"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestFetchWebpageTruncationIsRuneSafe(t *testing.T) {
	// Multi-byte characters at the truncation boundary must not be split.
	// 100 × "ä" (2 bytes each) = 200 bytes; truncating at 150 bytes would
	// split the 75th "ä" if done byte-wise.
	longText := strings.Repeat("ä", 500) // 1000 bytes, 500 runes
	html := `<html><head><title>T</title></head><body><article><h2>H</h2><p>` + longText + `</p></article></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(html))
	}))
	defer srv.Close()

	// maxChars=100 runes: the cut must fall on a rune boundary.
	out, err := fetchWebpage(context.Background(), srv.Client(), srv.URL, 100, testLogger())
	if err != nil {
		t.Fatalf("fetchWebpage: %v", err)
	}
	if !utf8.ValidString(out) {
		t.Error("output is not valid UTF-8 — truncation split a multi-byte character")
	}
	if !strings.Contains(out, "... (truncated)") {
		t.Error("expected truncation marker")
	}
}
