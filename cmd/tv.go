package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// The TV source checks the evening program of the German public broadcasters
// ARD (Das Erste) and ZDF for the coming day: do the late news editions
// heute-journal (ZDF) and tagesthemen (ARD) start early, within a configured
// window (default 21:15-21:30)?
//
// Both broadcasters expose the program through public JSON APIs, no account
// needed:
//   - ARD:  GET https://programm-api.ard.de/program/api/program?day=YYYY-MM-DD
//     returns every ARD channel for that day; we use the "daserste" channel.
//     The API serves at most 8 days ahead.
//   - ZDF:  GET https://api.zdf.de/cmdm/epg/broadcasts?tvServices=zdf&from=…&to=…
//     returns the ZDF channel's broadcasts in a time range. The API expects
//     the public bearer token that ZDF's own web player ships (overridable via
//     sources.tv.zdf_api_key).
const (
	defaultARDProgramURL = "https://programm-api.ard.de"
	defaultZDFAPIURL     = "https://api.zdf.de"
	defaultTVTimezone    = "Europe/Berlin"

	// defaultZDFAPIKeyB64 is the public API token used by ZDF's own web
	// player (base64-encoded), the same constant open-source EPG collectors
	// ship. It is not a user secret; sources.tv.zdf_api_key overrides it.
	defaultZDFAPIKeyB64 = "YWhCYWVNZWVrYWl5NW9oc2FpNGJlZTRraTZPb3BvaTVxdWFpbGllYg=="

	ardProgramPath     = "/program/api/program"
	ardChannelDasErste = "daserste"
	zdfBroadcastsPath  = "/cmdm/epg/broadcasts"
	zdfServiceZDF      = "zdf"

	// The default check window: late news editions starting between 21:15
	// and 21:30 (inclusive) are reported; otherwise the brief suggests
	// re-watching heute (19:00) in the ZDF Mediathek.
	defaultTVWindowStart = 21*60 + 15
	defaultTVWindowEnd   = 21*60 + 30

	zdfBroadcastsLimit   = 100
	zdfBroadcastsMaxPage = 10

	// tvScheduleScopeFromHour: only broadcasts starting at or after 06:00
	// local on the target day belong to that day's own program. The small
	// hours (00:00-05:59) carry overnight repeats of the previous evening's
	// editions (e.g. the ~00:20 heute journal repeat), which belong to the
	// previous program day and would only add noise.
	tvScheduleScopeFromHour = 6
)

// tvClient fetches the ARD and ZDF program schedules.
type tvClient struct {
	ardBaseURL  string
	zdfBaseURL  string
	zdfAPIKey   string // bearer token for api.zdf.de
	windowStart int    // minutes since midnight, inclusive
	windowEnd   int    // minutes since midnight, inclusive
	location    *time.Location
	httpClient  *http.Client
	now         func() time.Time // injectable for tests
	logger      *logrus.Logger
}

// newTVClient creates a client. Empty values fall back to the defaults; the
// window is given as "HH:MM" clock strings and the timezone as an IANA name.
func newTVClient(windowStart, windowEnd, timezone string, timeoutSeconds int, ardURL, zdfURL, zdfAPIKey string, logger *logrus.Logger) (*tvClient, error) {
	ws, we := tvWindowBounds(windowStart, windowEnd)
	if timezone == "" {
		timezone = defaultTVTimezone
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("sources.tv: unknown timezone %q: %w", timezone, err)
	}
	if timeoutSeconds <= 0 {
		timeoutSeconds = 15
	}
	if ardURL == "" {
		ardURL = defaultARDProgramURL
	}
	if zdfURL == "" {
		zdfURL = defaultZDFAPIURL
	}
	if zdfAPIKey == "" {
		key, err := base64.StdEncoding.DecodeString(defaultZDFAPIKeyB64)
		if err != nil {
			return nil, fmt.Errorf("sources.tv: built-in ZDF API token is broken: %w", err)
		}
		zdfAPIKey = string(key)
	}
	if logger == nil {
		logger = logrus.New()
	}
	return &tvClient{
		ardBaseURL:  strings.TrimRight(ardURL, "/"),
		zdfBaseURL:  strings.TrimRight(zdfURL, "/"),
		zdfAPIKey:   zdfAPIKey,
		windowStart: ws,
		windowEnd:   we,
		location:    loc,
		httpClient:  &http.Client{Timeout: time.Duration(timeoutSeconds) * time.Second},
		now:         time.Now,
		logger:      logger,
	}, nil
}

// tvEntry is one broadcast from either channel's schedule.
type tvEntry struct {
	Channel  string // "ARD (Das Erste)" or "ZDF"
	Title    string
	Subtitle string
	Start    time.Time // in the client's location
	End      time.Time // zero when the API did not report it
}

// tvEdition is a late news edition (heute-journal or tagesthemen) with the
// verdict whether it starts within the configured window.
type tvEdition struct {
	Show     string // normalized: "heute journal" or "tagesthemen"
	Channel  string
	Start    time.Time
	End      time.Time
	InWindow bool
}

// parseHHMM parses a "HH:MM" clock time into minutes since midnight.
func parseHHMM(s string) (int, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid time %q, want HH:MM", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, fmt.Errorf("invalid time %q, want HH:MM", s)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, fmt.Errorf("invalid time %q, want HH:MM", s)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("invalid time %q, want HH:MM", s)
	}
	return h*60 + m, nil
}

// formatClock renders minutes since midnight as "HH:MM".
func formatClock(min int) string {
	return fmt.Sprintf("%02d:%02d", min/60, min%60)
}

// tvWindowLabel renders a window as "21:15–21:30".
func tvWindowLabel(start, end int) string {
	return formatClock(start) + "–" + formatClock(end)
}

// tvWindowBounds parses the configured window into minutes since midnight.
// Unset or invalid values fall back to the defaults (validate reports them).
func tvWindowBounds(start, end string) (int, int) {
	ws, err := parseHHMM(start)
	if err != nil {
		ws = defaultTVWindowStart
	}
	we, err := parseHHMM(end)
	if err != nil {
		we = defaultTVWindowEnd
	}
	if ws > we {
		ws, we = defaultTVWindowStart, defaultTVWindowEnd
	}
	return ws, we
}

// parseScheduleTime parses an API timestamp (RFC 3339 with offset, or a bare
// local timestamp) and returns it in the client's location.
func parseScheduleTime(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.In(loc), nil
	}
	// Some feeds omit the timezone offset; the program day is local.
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", s, loc); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, loc); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unparseable timestamp %q", s)
}

// normalizeTitle lowercases a title and collapses punctuation and whitespace,
// so "heute-journal", "heute journal" and "Heute  Journal" compare equal.
func normalizeTitle(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "-", " ")
	return strings.Join(strings.Fields(s), " ")
}

// matchLateNews reports whether a program title is one of the late news
// editions we check, and returns its normalized show name.
//
// "heute journal update" (a short late-night news bulletin ZDF airs on some
// days, e.g. Mondays at 23:50) is explicitly excluded — it is a different
// program, not the main heute journal magazine.
func matchLateNews(title string) (string, bool) {
	n := normalizeTitle(title)
	switch {
	case strings.Contains(n, "tagesthemen"):
		return "tagesthemen", true
	case strings.Contains(n, "heute journal") && !strings.Contains(n, "heute journal update"):
		return "heute journal", true
	}
	return "", false
}

// ardTimeSlots unmarshals the ARD "timeSlots" field, which is a list that may
// nest further lists — the entries are flattened.
type ardTimeSlots []ardTimeSlot

func (ts *ardTimeSlots) UnmarshalJSON(data []byte) error {
	var raw interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var out []ardTimeSlot
	var walk func(node interface{}) error
	walk = func(node interface{}) error {
		switch v := node.(type) {
		case []interface{}:
			for _, child := range v {
				if err := walk(child); err != nil {
					return err
				}
			}
		case map[string]interface{}:
			b, err := json.Marshal(v)
			if err != nil {
				return err
			}
			var slot ardTimeSlot
			if err := json.Unmarshal(b, &slot); err != nil {
				return err
			}
			out = append(out, slot)
		}
		return nil
	}
	if err := walk(raw); err != nil {
		return err
	}
	*ts = out
	return nil
}

// ardTimeSlot is one entry of the ARD program API response.
type ardTimeSlot struct {
	BroadcastedOn string `json:"broadcastedOn"`
	BroadcastEnd  string `json:"broadcastEnd"`
	Title         string `json:"title"`
	CoreTitle     string `json:"coreTitle"`
	Subtitle      string `json:"subline"`
	CoreSubtitle  string `json:"coreSubline"`
}

type ardProgramResponse struct {
	Channels []struct {
		ID        string       `json:"id"`
		TimeSlots ardTimeSlots `json:"timeSlots"`
	} `json:"channels"`
}

// zdfBroadcast is one entry of the ZDF EPG broadcasts response.
type zdfBroadcast struct {
	EffectiveAirtimeBegin string `json:"effectiveAirtimeBegin"`
	AirtimeBegin          string `json:"airtimeBegin"`
	EffectiveAirtimeEnd   string `json:"effectiveAirtimeEnd"`
	AirtimeEnd            string `json:"airtimeEnd"`
	Title                 string `json:"title"`
	Subtitle              string `json:"subtitle"`
}

type zdfBroadcastsResponse struct {
	Broadcasts []zdfBroadcast `json:"http://zdf.de/rels/cmdm/broadcasts"`
}

// getJSON fetches a URL and decodes the JSON response.
func (c *tvClient) getJSON(ctx context.Context, rawURL string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("User-Agent", "daily-brief/1.0 (tv schedule)")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// fetchARDDay downloads one day of the ARD program (all channels) and returns
// the entries of Das Erste for the given day.
func (c *tvClient) fetchARDDay(ctx context.Context, day time.Time) ([]tvEntry, error) {
	q := url.Values{}
	q.Set("day", day.Format("2006-01-02"))
	body, err := c.getJSON(ctx, c.ardBaseURL+ardProgramPath+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("ARD program request failed: %w", err)
	}

	var prog ardProgramResponse
	if err := json.Unmarshal(body, &prog); err != nil {
		return nil, fmt.Errorf("failed to parse ARD program response: %w", err)
	}

	var entries []tvEntry
	for _, channel := range prog.Channels {
		if channel.ID != ardChannelDasErste {
			continue
		}
		for _, slot := range channel.TimeSlots {
			start, err := parseScheduleTime(slot.BroadcastedOn, c.location)
			if err != nil {
				c.logger.WithError(err).Warnf("skipping ARD entry with bad start time %q", slot.BroadcastedOn)
				continue
			}
			if !inScheduleScope(start, day) {
				continue
			}
			title := slot.CoreTitle
			if title == "" {
				title = slot.Title
			}
			subtitle := slot.CoreSubtitle
			if subtitle == "" {
				subtitle = slot.Subtitle
			}
			entry := tvEntry{Channel: "ARD (Das Erste)", Title: title, Subtitle: subtitle, Start: start}
			if slot.BroadcastEnd != "" {
				if end, err := parseScheduleTime(slot.BroadcastEnd, c.location); err == nil {
					entry.End = end
				}
			}
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

// fetchZDFDay downloads one day of the ZDF channel's EPG broadcasts.
func (c *tvClient) fetchZDFDay(ctx context.Context, day time.Time) ([]tvEntry, error) {
	from := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, c.location)
	to := from.AddDate(0, 0, 1)

	headers := map[string]string{
		"Accept":   "application/vnd.de.zdf.v1.0+json",
		"Origin":   "https://www.zdf.de",
		"Api-Auth": "Bearer " + c.zdfAPIKey,
	}

	var entries []tvEntry
	for page := 1; page <= zdfBroadcastsMaxPage; page++ {
		q := url.Values{}
		q.Set("limit", strconv.Itoa(zdfBroadcastsLimit))
		q.Set("order", "asc")
		q.Set("tvServices", zdfServiceZDF)
		q.Set("from", from.Format(time.RFC3339))
		q.Set("to", to.Format(time.RFC3339))
		q.Set("page", strconv.Itoa(page))

		body, err := c.getJSON(ctx, c.zdfBaseURL+zdfBroadcastsPath+"?"+q.Encode(), headers)
		if err != nil {
			return nil, fmt.Errorf("ZDF program request failed: %w", err)
		}

		var resp zdfBroadcastsResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("failed to parse ZDF program response: %w", err)
		}

		for _, b := range resp.Broadcasts {
			begin := b.EffectiveAirtimeBegin
			if begin == "" {
				begin = b.AirtimeBegin
			}
			start, err := parseScheduleTime(begin, c.location)
			if err != nil {
				c.logger.WithError(err).Warnf("skipping ZDF entry with bad start time %q", begin)
				continue
			}
			if !inScheduleScope(start, day) {
				continue
			}
			entry := tvEntry{Channel: "ZDF", Title: b.Title, Subtitle: b.Subtitle, Start: start}
			end := b.EffectiveAirtimeEnd
			if end == "" {
				end = b.AirtimeEnd
			}
			if end != "" {
				if t, err := parseScheduleTime(end, c.location); err == nil {
					entry.End = t
				}
			}
			entries = append(entries, entry)
		}

		if len(resp.Broadcasts) < zdfBroadcastsLimit {
			break
		}
	}
	return entries, nil
}

// sameScheduleDay reports whether t falls on the same calendar day as day
// (both in the client's location).
func sameScheduleDay(t, day time.Time) bool {
	return t.Format("2006-01-02") == day.Format("2006-01-02")
}

// inScheduleScope reports whether a broadcast belongs to the target day's own
// program: it must start on the target calendar day at or after
// tvScheduleScopeFromHour (see there).
func inScheduleScope(t, day time.Time) bool {
	return sameScheduleDay(t, day) && t.Hour() >= tvScheduleScopeFromHour
}

// inWindow reports whether t's clock time falls within the configured window
// (inclusive). t must already be in the client's location.
func (c *tvClient) inWindow(t time.Time) bool {
	m := t.Hour()*60 + t.Minute()
	return m >= c.windowStart && m <= c.windowEnd
}

// lateNewsEditions fetches both channels' program for the given day and
// returns the heute-journal / tagesthemen editions (sorted by start time).
// Each source is fetched independently: a failing source is reported in
// sourceErrs and does not lose the other. Shows that do not appear at all on
// a successfully fetched channel are listed in missing.
func (c *tvClient) lateNewsEditions(ctx context.Context, day time.Time) (editions []tvEdition, missing []string, sourceErrs map[string]error) {
	sourceErrs = map[string]error{}
	found := map[string]bool{}

	collect := func(entries []tvEntry, err error, source string) {
		if err != nil {
			sourceErrs[source] = err
			return
		}
		for _, e := range entries {
			show, ok := matchLateNews(e.Title)
			if !ok {
				continue
			}
			editions = append(editions, tvEdition{
				Show:     show,
				Channel:  e.Channel,
				Start:    e.Start,
				End:      e.End,
				InWindow: c.inWindow(e.Start),
			})
			found[show] = true
		}
	}

	ardEntries, err := c.fetchARDDay(ctx, day)
	collect(ardEntries, err, "ARD")

	zdfEntries, err := c.fetchZDFDay(ctx, day)
	collect(zdfEntries, err, "ZDF")

	// A show missing from a successfully fetched channel means the edition
	// does not air that day (e.g. no tagesthemen on some days).
	if sourceErrs["ARD"] == nil && !found["tagesthemen"] {
		missing = append(missing, "tagesthemen (ARD)")
	}
	if sourceErrs["ZDF"] == nil && !found["heute journal"] {
		missing = append(missing, "heute journal (ZDF)")
	}

	sort.Slice(editions, func(i, j int) bool {
		if !editions[i].Start.Equal(editions[j].Start) {
			return editions[i].Start.Before(editions[j].Start)
		}
		return editions[i].Show < editions[j].Show
	})
	return editions, missing, sourceErrs
}
