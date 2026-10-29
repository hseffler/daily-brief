package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/html/charset"
)

const (
	// webFetchUserAgent mimics a desktop browser: several news sites block or
	// degrade the default Go user agent.
	webFetchUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36 daily-brief/1.0"
	// webFetchMaxBodyBytes caps how much HTML is read from a page.
	webFetchMaxBodyBytes = 4 << 20
	// webFetchMaxArticles caps how many <article> blocks are extracted from a
	// front page so the tool result stays focused on the top stories.
	webFetchMaxArticles = 20
)

// webFetchArgs are the arguments of the fetch_webpage tool.
type webFetchArgs struct {
	URL string `json:"url"`
}

// newWebFetchTool creates the fetch_webpage tool: it downloads a web page and
// returns its readable text (headlines/article teasers), truncated.
func newWebFetchTool(maxChars, timeoutSeconds int, logger *logrus.Logger) Tool {
	client := &http.Client{Timeout: time.Duration(timeoutSeconds) * time.Second}
	return Tool{
		Name: "fetch_webpage",
		Description: "Fetch a web page and return its readable text content (headlines and article teasers), " +
			"truncated to a manageable length. Use this to read news sites and extract the most important headlines.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"url": {"type": "string", "description": "URL of the web page to fetch, e.g. https://www.zeit.de/"}
			},
			"required": ["url"]
		}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a webFetchArgs
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.URL == "" {
				return "", errors.New("url is required")
			}
			return fetchWebpage(ctx, client, a.URL, maxChars, logger)
		},
	}
}

// fetchWebpage downloads rawurl and returns "URL/Title/text" with the page's
// readable content truncated to maxChars characters.
func fetchWebpage(ctx context.Context, client *http.Client, rawurl string, maxChars int, logger *logrus.Logger) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return "", fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("User-Agent", webFetchUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/rss+xml,application/atom+xml,application/xml;q=0.9,text/xml;q=0.8,*/*;q=0.5")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to fetch %s: %w", rawurl, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching %s returned HTTP status %d", rawurl, resp.StatusCode)
	}

	// A JSON endpoint is not a web page or feed; parsing it as HTML would
	// return garbage text instead of a useful error.
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "json") {
		return "", fmt.Errorf("fetching %s returned %s, not a web page or RSS/Atom feed — point the web source at the site's HTML page or its feed URL", rawurl, ct)
	}

	// charset.NewReader transcodes non-UTF-8 pages to UTF-8 based on the
	// Content-Type header (or an HTML meta tag).
	body := io.LimitReader(resp.Body, webFetchMaxBodyBytes)
	reader, err := charset.NewReader(body, resp.Header.Get("Content-Type"))
	if err != nil {
		return "", fmt.Errorf("failed to decode %s: %w", rawurl, err)
	}

	doc, err := goquery.NewDocumentFromReader(reader)
	if err != nil {
		return "", fmt.Errorf("failed to parse HTML from %s: %w", rawurl, err)
	}

	title := collapseSpace(doc.Find("title").First().Text())
	text := extractReadableText(doc)
	if cut, truncated := truncateRunes(text, maxChars); truncated {
		text = cut + "\n... (truncated)"
	}

	logger.WithFields(logrus.Fields{
		"url":      rawurl,
		"title":    title,
		"text_len": len(text),
	}).Debug("web page fetched")

	return fmt.Sprintf("URL: %s\nTitle: %s\n\n%s", rawurl, title, text), nil
}

// extractReadableText pulls the meaningful text out of a parsed HTML document:
// it drops boilerplate elements and, on front pages with many <article>
// teasers, keeps each article's heading plus text. RSS feeds (detected by
// <item> elements) and Atom feeds (detected by <entry> elements) are
// rendered as a simple headline list.
func extractReadableText(doc *goquery.Document) string {
	doc.Find("script, style, noscript, template, svg, nav, footer, aside, form, button, iframe, header").Remove()

	// RSS feed: each <item> becomes one headline line.
	// Atom feed: each <entry> becomes one headline line.
	feedItems := doc.Find("item, entry")
	if feedItems.Length() > 0 {
		var sb strings.Builder
		feedItems.EachWithBreak(func(i int, s *goquery.Selection) bool {
			if i >= webFetchMaxArticles {
				return false
			}
			title := collapseSpace(s.Find("title").First().Text())
			if title != "" {
				sb.WriteString("- " + title + "\n")
			}
			return true
		})
		return strings.TrimSpace(sb.String())
	}

	var sb strings.Builder
	articles := doc.Find("article")
	if articles.Length() > 0 {
		articles.EachWithBreak(func(i int, s *goquery.Selection) bool {
			if i >= webFetchMaxArticles {
				return false
			}
			if heading := collapseSpace(s.Find("h1, h2, h3").First().Text()); heading != "" {
				sb.WriteString(heading + "\n")
			}
			if body := collapseSpace(s.Text()); body != "" {
				sb.WriteString(body + "\n\n")
			}
			return true
		})
	} else {
		main := doc.Find("main").First()
		if main.Length() == 0 {
			main = doc.Find("body").First()
		}
		sb.WriteString(collapseSpace(main.Text()))
	}
	return collapseSpace(sb.String())
}

// collapseSpace reduces all runs of whitespace (including newlines) to single
// spaces and trims the result.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
