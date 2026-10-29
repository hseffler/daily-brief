package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
	"github.com/sirupsen/logrus"
)

const (
	// emailMaxBodyBytes caps the full-message fetch: mails with large
	// attachments still contribute their subject/sender, but not their body.
	emailMaxBodyBytes = 512 << 10
	// emailSnippetChars is the per-mail body snippet length.
	emailSnippetChars = 500
)

// Email is one fetched mail, reduced to what the brief needs.
type Email struct {
	UID     uint32
	From    string
	Subject string
	Date    time.Time
	Snippet string
}

// EmailFetcher is the seam between the tool and the IMAP wire protocol; tests
// inject a fake instead of talking to Posteo.
type EmailFetcher interface {
	FetchNew(ctx context.Context, since time.Time, max int) ([]Email, error)
}

// imapFetcher fetches new mails from an IMAP mailbox (Posteo) over TLS.
type imapFetcher struct {
	host     string
	port     int
	username string
	password string
	mailbox  string
	logger   *logrus.Logger
	// dial is injectable for tests.
	dial func(ctx context.Context, addr string) (*imapclient.Client, error)
}

// ctxDialer adapts a context to the go-imap Dialer interface so connection
// attempts respect cancellation/timeouts.
type ctxDialer struct {
	ctx context.Context
}

func (d ctxDialer) Dial(network, addr string) (net.Conn, error) {
	var nd net.Dialer
	return nd.DialContext(d.ctx, network, addr)
}

func (f *imapFetcher) dialIMAP(ctx context.Context, addr string) (*imapclient.Client, error) {
	if f.dial != nil {
		return f.dial(ctx, addr)
	}
	return imapclient.DialWithDialerTLS(ctxDialer{ctx: ctx}, addr, nil)
}

// FetchNew returns mails in the mailbox with an internal date on/after since,
// newest last, capped at max messages. The mailbox is opened read-only so no
// \Seen flags are set.
//
// Note: IMAP SEARCH SINCE compares calendar dates only — the time part is
// ignored (RFC 3501) — so a 24h window at the 05:00 run effectively covers
// everything since yesterday 00:00 (up to ~29h), not a true rolling 24h.
//
// go-imap v1's protocol calls (Login/Select/Search/Fetch) are blocking and
// cannot be cancelled, so FetchNew runs the work in a goroutine and abandons
// it when ctx is done: on timeout the goroutine (and its connection) is left
// behind and dies with the process. This binary runs once per invocation, so
// the leak is bounded by the process lifetime.
func (f *imapFetcher) FetchNew(ctx context.Context, since time.Time, max int) ([]Email, error) {
	type fetchResult struct {
		emails []Email
		err    error
	}
	resCh := make(chan fetchResult, 1)
	go func() {
		emails, err := f.fetchNew(ctx, since, max)
		resCh <- fetchResult{emails, err}
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("IMAP fetch aborted: %w", ctx.Err())
	case res := <-resCh:
		return res.emails, res.err
	}
}

// fetchNew is the blocking IMAP implementation behind FetchNew.
func (f *imapFetcher) fetchNew(ctx context.Context, since time.Time, max int) ([]Email, error) {
	addr := fmt.Sprintf("%s:%d", f.host, f.port)

	c, err := f.dialIMAP(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("IMAP connect to %s failed: %w", addr, err)
	}
	defer func() { _ = c.Logout() }()

	if err := c.Login(f.username, f.password); err != nil {
		return nil, fmt.Errorf("IMAP login for %s failed: %w", f.username, err)
	}

	if _, err := c.Select(f.mailbox, true); err != nil {
		return nil, fmt.Errorf("IMAP select %s failed: %w", f.mailbox, err)
	}

	criteria := imap.NewSearchCriteria()
	criteria.Since = since
	uids, err := c.UidSearch(criteria)
	if err != nil {
		return nil, fmt.Errorf("IMAP search failed: %w", err)
	}
	if len(uids) == 0 {
		return nil, nil
	}
	// UidSearch returns UIDs ascending; keep the newest `max`.
	if len(uids) > max {
		uids = uids[len(uids)-max:]
	}

	// Phase 1: metadata (UID, envelope, size) for all matches.
	seq := new(imap.SeqSet)
	seq.AddNum(uids...)
	metaItems := []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope, imap.FetchRFC822Size}
	msgs, err := uidFetchAll(c, seq, metaItems)
	if err != nil {
		return nil, fmt.Errorf("IMAP fetch (metadata) failed: %w", err)
	}

	// Phase 2: full bodies, but only for messages small enough that they
	// cannot carry huge attachments.
	bodies := map[uint32]string{}
	var small []uint32
	for _, m := range msgs {
		if m.Size > 0 && m.Size <= emailMaxBodyBytes {
			small = append(small, m.Uid)
		}
	}
	if len(small) > 0 {
		bodySeq := new(imap.SeqSet)
		bodySeq.AddNum(small...)
		section := &imap.BodySectionName{
			BodyPartName: imap.BodyPartName{Specifier: imap.EntireSpecifier},
			Peek:         true,
		}
		bodyMsgs, err := uidFetchAll(c, bodySeq, []imap.FetchItem{section.FetchItem()})
		if err != nil {
			return nil, fmt.Errorf("IMAP fetch (bodies) failed: %w", err)
		}
		for _, m := range bodyMsgs {
			lit := m.GetBody(section)
			if lit == nil {
				continue
			}
			raw, err := io.ReadAll(io.LimitReader(lit, emailMaxBodyBytes+1))
			if err != nil {
				f.logger.WithError(err).Warn("failed to read mail body")
				continue
			}
			bodies[m.Uid] = string(raw)
		}
	}

	emails := make([]Email, 0, len(msgs))
	for _, m := range msgs {
		e := Email{UID: m.Uid}
		if m.Envelope != nil {
			e.Subject = m.Envelope.Subject
			e.Date = m.Envelope.Date
			if len(m.Envelope.From) > 0 {
				e.From = addressString(m.Envelope.From[0])
			}
		}
		if raw, ok := bodies[m.Uid]; ok {
			e.Snippet = emailSnippet(raw, emailSnippetChars)
		}
		emails = append(emails, e)
	}

	sort.Slice(emails, func(i, j int) bool { return emails[i].Date.Before(emails[j].Date) })
	return emails, nil
}

// uidFetchAll runs a UID FETCH and collects all messages until the channel
// closes, then returns the fetch error (if any).
func uidFetchAll(c *imapclient.Client, seq *imap.SeqSet, items []imap.FetchItem) ([]*imap.Message, error) {
	ch := make(chan *imap.Message, 32)
	done := make(chan error, 1)
	go func() { done <- c.UidFetch(seq, items, ch) }()

	var msgs []*imap.Message
	for m := range ch {
		msgs = append(msgs, m)
	}
	if err := <-done; err != nil {
		return nil, err
	}
	return msgs, nil
}

// addressString renders an IMAP address as "mailbox@host".
func addressString(a *imap.Address) string {
	if a == nil {
		return ""
	}
	if a.MailboxName == "" {
		return a.PersonalName
	}
	if a.HostName == "" {
		return a.MailboxName
	}
	return a.MailboxName + "@" + a.HostName
}

// emailSnippet extracts a short plain-text snippet from a raw RFC 822 message.
// It prefers the first text/plain part of multipart messages and decodes
// quoted-printable/base64 transfer encodings.
func emailSnippet(raw string, maxChars int) string {
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		s, truncated := truncateRunes(collapseSpace(raw), maxChars)
		if truncated {
			return s + "…"
		}
		return s
	}

	body := msg.Body
	if mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type")); err == nil {
		if strings.HasPrefix(mediaType, "multipart/") {
			mr := multipart.NewReader(msg.Body, params["boundary"])
			for {
				part, err := mr.NextPart()
				if err != nil {
					break
				}
				if strings.HasPrefix(part.Header.Get("Content-Type"), "text/plain") {
					body = decodeCTE(part, part.Header.Get("Content-Transfer-Encoding"))
					break
				}
			}
		} else {
			// Non-multipart message: the transfer encoding applies to the
			// whole body.
			body = decodeCTE(msg.Body, msg.Header.Get("Content-Transfer-Encoding"))
		}
	}

	data, err := io.ReadAll(io.LimitReader(body, int64(maxChars*4)))
	if err != nil {
		return ""
	}
	snippet, truncated := truncateRunes(collapseSpace(string(data)), maxChars)
	if truncated {
		return snippet + "…"
	}
	return snippet
}

// decodeCTE wraps r in the decoder matching the given Content-Transfer-
// Encoding value ("" / 7bit / 8bit pass through unchanged).
func decodeCTE(r io.Reader, cte string) io.Reader {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, r)
	default:
		return r
	}
}

// emailFetchLimitMin is the minimum number of mails fetched per run. Fetching
// more than the brief cap (max_messages) keeps the IMAP work bounded while
// still letting the cap below select the newest mails when a burst arrives.
const emailFetchLimitMin = 100

// fetchLimit is the IMAP fetch limit: more than the brief cap, bounded below
// by emailFetchLimitMin.
func fetchLimit(maxMessages int) int {
	limit := maxMessages * 4
	if limit < emailFetchLimitMin {
		limit = emailFetchLimitMin
	}
	return limit
}

// newEmailTool creates the get_new_emails tool. The tool is stateless: each
// call fetches the mails from the rolling since_hours window and keeps the
// newest max_messages of them. No seen-mail state is kept between runs, so a
// delayed or repeated run may report mails from the window again.
func newEmailTool(cfg *Config, fetcher EmailFetcher, logger *logrus.Logger) Tool {
	return Tool{
		Name: "get_new_emails",
		Description: "Fetch emails from the user's Posteo mailbox that arrived within the configured recent time window. " +
			"Returns the sender, subject, date and a short text snippet for each mail.",
		Parameters: json.RawMessage(`{"type": "object", "properties": {}}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			since := time.Now().Add(-time.Duration(cfg.Sources.Email.SinceHours) * time.Hour)

			emails, err := fetcher.FetchNew(ctx, since, fetchLimit(cfg.Sources.Email.MaxMessages))
			if err != nil {
				return "", err
			}

			// Keep the newest max_messages (emails arrive date-ascending).
			// validate() guarantees max_messages > 0; the guard keeps a
			// hand-built Config from panicking on a negative slice bound.
			if max := cfg.Sources.Email.MaxMessages; max > 0 && len(emails) > max {
				emails = emails[len(emails)-max:]
			}

			if len(emails) == 0 {
				return fmt.Sprintf("No emails in the last %d hours.", cfg.Sources.Email.SinceHours), nil
			}

			var sb strings.Builder
			fmt.Fprintf(&sb, "%d email(s) from the last %d hours:\n", len(emails), cfg.Sources.Email.SinceHours)
			for _, e := range emails {
				fmt.Fprintf(&sb, "- %s | From: %s | Subject: %s\n",
					e.Date.Format("2006-01-02 15:04"), e.From, e.Subject)
				if e.Snippet != "" {
					fmt.Fprintf(&sb, "  %s\n", e.Snippet)
				}
			}
			return sb.String(), nil
		},
	}
}
