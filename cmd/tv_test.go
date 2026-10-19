package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Fixed "now" for the TV tests: Saturday 2026-10-17, 05:00 in Berlin (CEST,
// +02:00) — the day the canned schedules below are for.
func fixedTVNow() time.Time {
	loc := time.FixedZone("Europe/Berlin", 2*3600)
	return time.Date(2026, 10, 17, 5, 0, 0, 0, loc)
}

// cannedARDProgram covers: a non-matching show, two tagesthemen editions
// (21:14 outside and 22:15 outside the default window), a nested timeSlots
// array, an overnight repeat at 00:30 (must be filtered out), an entry for
// the next day (must be filtered out) and another channel (must be ignored).
const cannedARDProgram = `{"channels":[
	{"id":"daserste","timeSlots":[
		{"broadcastedOn":"2026-10-17T00:30:00+02:00","broadcastEnd":"2026-10-17T00:45:00+02:00","coreTitle":"tagesthemen","coreSubline":"overnight repeat"},
		{"broadcastedOn":"2026-10-17T20:00:00+02:00","broadcastEnd":"2026-10-17T20:15:00+02:00","coreTitle":"tagesschau","coreSubline":""},
		["nested",
			{"broadcastedOn":"2026-10-17T21:14:00+02:00","broadcastEnd":"2026-10-17T21:29:00+02:00","coreTitle":"tagesthemen","coreSubline":"Schwerpunkt"},
			{"broadcastedOn":"2026-10-17T22:15:00+02:00","broadcastEnd":"2026-10-17T22:45:00+02:00","title":"tagesthemen","subline":""}
		],
		{"broadcastedOn":"2026-10-18T22:15:00+02:00","broadcastEnd":"2026-10-18T22:45:00+02:00","coreTitle":"tagesthemen","coreSubline":"next day"}
	]},
	{"id":"wdr","timeSlots":[
		{"broadcastedOn":"2026-10-17T22:15:00+02:00","broadcastEnd":"2026-10-17T22:45:00+02:00","coreTitle":"tagesthemen","coreSubline":"other channel"}
	]}
]}`

// cannedZDFBroadcasts covers: an overnight heute journal repeat at 00:20
// (must be filtered out), the 19:00 heute main edition and heute Xpress
// (neither matches), a heute journal at 21:15 (inside the default window, via
// the airtimeBegin fallback), a "heute-journal" (hyphenated) at 21:45
// (outside), a "heute journal update" at 23:50 (a different program — must
// NOT match), and an entry for the next day (must be filtered out).
const cannedZDFBroadcasts = `{"http://zdf.de/rels/cmdm/broadcasts":[
	{"effectiveAirtimeBegin":"2026-10-17T00:20:00+02:00","effectiveAirtimeEnd":"2026-10-17T00:40:00+02:00","title":"heute journal","subtitle":"overnight repeat"},
	{"effectiveAirtimeBegin":"2026-10-17T19:00:00+02:00","effectiveAirtimeEnd":"2026-10-17T19:20:00+02:00","title":"heute","subtitle":""},
	{"effectiveAirtimeBegin":"2026-10-17T19:30:00+02:00","effectiveAirtimeEnd":"2026-10-17T19:35:00+02:00","title":"heute Xpress","subtitle":""},
	{"airtimeBegin":"2026-10-17T21:15:00+02:00","airtimeEnd":"2026-10-17T21:45:00+02:00","title":"heute journal","subtitle":""},
	{"effectiveAirtimeBegin":"2026-10-17T21:45:00+02:00","effectiveAirtimeEnd":"2026-10-17T22:00:00+02:00","title":"heute-journal","subtitle":""},
	{"effectiveAirtimeBegin":"2026-10-17T23:50:00+02:00","effectiveAirtimeEnd":"2026-10-18T00:05:00+02:00","title":"heute journal update","subtitle":""},
	{"effectiveAirtimeBegin":"2026-10-18T21:30:00+02:00","effectiveAirtimeEnd":"2026-10-18T21:45:00+02:00","title":"heute journal","subtitle":"next day"}
]}`

// newTestTVClient builds a client against the two fake servers, with the
// injectable clock fixed to fixedTVNow.
func newTestTVClient(t *testing.T, ardHandler, zdfHandler http.HandlerFunc) *tvClient {
	t.Helper()
	ardSrv := httptest.NewServer(ardHandler)
	t.Cleanup(ardSrv.Close)
	zdfSrv := httptest.NewServer(zdfHandler)
	t.Cleanup(zdfSrv.Close)

	client, err := newTVClient("", "", "", 5, ardSrv.URL, zdfSrv.URL, "test-key", testLogger())
	if err != nil {
		t.Fatalf("newTVClient: %v", err)
	}
	client.now = fixedTVNow
	return client
}

func TestTVLateNewsEditions(t *testing.T) {
	client := newTestTVClient(t,
		func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("day"); got != "2026-10-17" {
				t.Errorf("ARD day param: got %q, want 2026-10-17", got)
			}
			w.Write([]byte(cannedARDProgram))
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(cannedZDFBroadcasts))
		},
	)

	day := time.Date(2026, 10, 17, 0, 0, 0, 0, client.location)
	editions, missing, sourceErrs := client.lateNewsEditions(context.Background(), day)

	if len(sourceErrs) != 0 {
		t.Fatalf("unexpected source errors: %v", sourceErrs)
	}
	if len(missing) != 0 {
		t.Errorf("unexpected missing shows: %v", missing)
	}
	if len(editions) != 4 {
		t.Fatalf("expected 4 editions, got %d: %+v", len(editions), editions)
	}

	// The "heute journal update" at 23:50 (a different program) must NOT
	// appear — only the two real heute journal editions (21:15, 21:45).
	for _, e := range editions {
		if e.Show == "heute journal" && e.Start.Format("15:04") == "23:50" {
			t.Errorf("heute journal update must not match: got edition at %s", e.Start.Format("15:04"))
		}
	}

	// Sorted by start time: 21:14 tagesthemen (out), 21:15 heute journal (in),
	// 21:45 heute journal (out), 22:15 tagesthemen (out).
	want := []struct {
		show     string
		channel  string
		start    string
		end      string
		inWindow bool
	}{
		{"tagesthemen", "ARD (Das Erste)", "21:14", "21:29", false},
		{"heute journal", "ZDF", "21:15", "21:45", true},
		{"heute journal", "ZDF", "21:45", "22:00", false},
		{"tagesthemen", "ARD (Das Erste)", "22:15", "22:45", false},
	}
	for i, w := range want {
		e := editions[i]
		if e.Show != w.show || e.Channel != w.channel || e.InWindow != w.inWindow {
			t.Errorf("edition %d: got %+v, want %+v", i, e, w)
		}
		if got := e.Start.Format("15:04"); got != w.start {
			t.Errorf("edition %d start: got %s, want %s", i, got, w.start)
		}
		if got := e.End.Format("15:04"); got != w.end {
			t.Errorf("edition %d end: got %s, want %s", i, got, w.end)
		}
	}
}

func TestTVMissingShows(t *testing.T) {
	// A day without any late news editions: both shows must be reported
	// missing, and no source error.
	client := newTestTVClient(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"channels":[{"id":"daserste","timeSlots":[
				{"broadcastedOn":"2026-10-17T20:00:00+02:00","broadcastEnd":"2026-10-17T20:15:00+02:00","coreTitle":"tagesschau"}
			]}]}`))
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"http://zdf.de/rels/cmdm/broadcasts":[
				{"effectiveAirtimeBegin":"2026-10-17T19:00:00+02:00","effectiveAirtimeEnd":"2026-10-17T19:20:00+02:00","title":"heute"}
			]}`))
		},
	)

	day := time.Date(2026, 10, 17, 0, 0, 0, 0, client.location)
	editions, missing, sourceErrs := client.lateNewsEditions(context.Background(), day)

	if len(editions) != 0 {
		t.Errorf("expected no editions, got %+v", editions)
	}
	if len(sourceErrs) != 0 {
		t.Errorf("unexpected source errors: %v", sourceErrs)
	}
	if len(missing) != 2 || missing[0] != "tagesthemen (ARD)" || missing[1] != "heute journal (ZDF)" {
		t.Errorf("missing: got %v, want [tagesthemen (ARD) heute journal (ZDF)]", missing)
	}
}

func TestTVOneSourceFails(t *testing.T) {
	// A failing ARD must not lose the ZDF editions.
	client := newTestTVClient(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(cannedZDFBroadcasts))
		},
	)

	day := time.Date(2026, 10, 17, 0, 0, 0, 0, client.location)
	editions, missing, sourceErrs := client.lateNewsEditions(context.Background(), day)

	if sourceErrs["ARD"] == nil {
		t.Error("expected an ARD source error")
	}
	if sourceErrs["ZDF"] != nil {
		t.Errorf("unexpected ZDF error: %v", sourceErrs["ZDF"])
	}
	if len(editions) != 2 {
		t.Errorf("expected the 2 ZDF editions, got %+v", editions)
	}
	// tagesthemen must not be reported missing when its source failed.
	if len(missing) != 0 {
		t.Errorf("nothing should be missing when a source failed, got %v", missing)
	}
}

func TestTVZDFRequestShape(t *testing.T) {
	client := newTestTVClient(t,
		func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"channels":[]}`)) },
		func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if got := q.Get("tvServices"); got != "zdf" {
				t.Errorf("tvServices: got %q, want zdf", got)
			}
			if got := q.Get("order"); got != "asc" {
				t.Errorf("order: got %q, want asc", got)
			}
			if got := q.Get("from"); got != "2026-10-17T00:00:00+02:00" {
				t.Errorf("from: got %q", got)
			}
			if got := q.Get("to"); got != "2026-10-18T00:00:00+02:00" {
				t.Errorf("to: got %q", got)
			}
			if got := q.Get("page"); got != "1" {
				t.Errorf("page: got %q, want 1", got)
			}
			if got := r.Header.Get("Api-Auth"); got != "Bearer test-key" {
				t.Errorf("Api-Auth: got %q, want Bearer test-key", got)
			}
			if got := r.Header.Get("Accept"); got != "application/vnd.de.zdf.v1.0+json" {
				t.Errorf("Accept: got %q", got)
			}
			if got := r.Header.Get("Origin"); got != "https://www.zdf.de" {
				t.Errorf("Origin: got %q", got)
			}
			w.Write([]byte(`{"http://zdf.de/rels/cmdm/broadcasts":[]}`))
		},
	)

	day := time.Date(2026, 10, 17, 0, 0, 0, 0, client.location)
	if _, err := client.fetchZDFDay(context.Background(), day); err != nil {
		t.Fatalf("fetchZDFDay: %v", err)
	}
}

func TestTVZDFPagination(t *testing.T) {
	// A full first page (100 entries) must trigger a second page request.
	full := make([]map[string]string, 0, zdfBroadcastsLimit)
	for i := 0; i < zdfBroadcastsLimit; i++ {
		full = append(full, map[string]string{
			"effectiveAirtimeBegin": fmt.Sprintf("2026-10-17T%02d:%02d:00+02:00", 5+i/60, i%60),
			"title":                 fmt.Sprintf("Filler %d", i),
		})
	}
	page1, err := json.Marshal(map[string]interface{}{"http://zdf.de/rels/cmdm/broadcasts": full})
	if err != nil {
		t.Fatal(err)
	}
	page2 := `{"http://zdf.de/rels/cmdm/broadcasts":[
		{"effectiveAirtimeBegin":"2026-10-17T21:30:00+02:00","effectiveAirtimeEnd":"2026-10-17T21:45:00+02:00","title":"heute journal"}
	]}`

	pages := 0
	client := newTestTVClient(t,
		func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"channels":[]}`)) },
		func(w http.ResponseWriter, r *http.Request) {
			pages++
			if r.URL.Query().Get("page") == "1" {
				w.Write(page1)
			} else {
				w.Write([]byte(page2))
			}
		},
	)

	day := time.Date(2026, 10, 17, 0, 0, 0, 0, client.location)
	entries, err := client.fetchZDFDay(context.Background(), day)
	if err != nil {
		t.Fatalf("fetchZDFDay: %v", err)
	}
	if pages != 2 {
		t.Errorf("expected 2 page requests, got %d", pages)
	}
	// The 21:30 heute journal from page 2 must be found; the fillers (05:00+
	// starts) fall on the same day and are returned too.
	found := false
	for _, e := range entries {
		if e.Title == "heute journal" && e.Start.Format("15:04") == "21:30" {
			found = true
		}
	}
	if !found {
		t.Errorf("heute journal from page 2 not found in %d entries", len(entries))
	}
}

func TestTVOvernightRepeatsIgnored(t *testing.T) {
	// Overnight repeats of the previous evening (00:20 heute journal, 00:30
	// tagesthemen) must not show up as editions of the target day; the
	// boundary is 06:00 local.
	client := newTestTVClient(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"channels":[{"id":"daserste","timeSlots":[
				{"broadcastedOn":"2026-10-17T00:30:00+02:00","broadcastEnd":"2026-10-17T00:45:00+02:00","coreTitle":"tagesthemen"},
				{"broadcastedOn":"2026-10-17T05:59:00+02:00","broadcastEnd":"2026-10-17T06:14:00+02:00","coreTitle":"tagesthemen"},
				{"broadcastedOn":"2026-10-17T06:00:00+02:00","broadcastEnd":"2026-10-17T06:15:00+02:00","coreTitle":"tagesthemen"},
				{"broadcastedOn":"2026-10-17T22:15:00+02:00","broadcastEnd":"2026-10-17T22:45:00+02:00","coreTitle":"tagesthemen"}
			]}]}`))
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"http://zdf.de/rels/cmdm/broadcasts":[
				{"effectiveAirtimeBegin":"2026-10-17T00:20:00+02:00","effectiveAirtimeEnd":"2026-10-17T00:40:00+02:00","title":"heute journal"},
				{"effectiveAirtimeBegin":"2026-10-17T21:45:00+02:00","effectiveAirtimeEnd":"2026-10-17T22:00:00+02:00","title":"heute journal"}
			]}`))
		},
	)

	day := time.Date(2026, 10, 17, 0, 0, 0, 0, client.location)
	editions, _, sourceErrs := client.lateNewsEditions(context.Background(), day)
	if len(sourceErrs) != 0 {
		t.Fatalf("unexpected source errors: %v", sourceErrs)
	}

	// Only the 06:00 and 22:15 tagesthemen and the 21:45 heute journal count
	// (sorted by start time); the 00:30 and 05:59 overnight repeats must not.
	if len(editions) != 3 {
		t.Fatalf("expected 3 editions, got %d: %+v", len(editions), editions)
	}
	for i, want := range []string{"06:00", "21:45", "22:15"} {
		if got := editions[i].Start.Format("15:04"); got != want {
			t.Errorf("edition %d: got %s, want %s", i, got, want)
		}
	}
}

func TestTVWindowBoundaries(t *testing.T) {
	client, err := newTVClient("21:15", "21:30", "", 5, "", "", "test-key", testLogger())
	if err != nil {
		t.Fatalf("newTVClient: %v", err)
	}
	at := func(h, m int) time.Time {
		return time.Date(2026, 10, 17, h, m, 0, 0, client.location)
	}
	for _, tc := range []struct {
		h, m int
		want bool
	}{
		{21, 14, false},
		{21, 15, true},  // window start, inclusive
		{21, 22, true},  // middle
		{21, 30, true},  // window end, inclusive
		{21, 31, false}, // just after
		{20, 0, false},
		{22, 15, false},
	} {
		if got := client.inWindow(at(tc.h, tc.m)); got != tc.want {
			t.Errorf("inWindow(%02d:%02d): got %v, want %v", tc.h, tc.m, got, tc.want)
		}
	}
}

func TestTVMatchLateNews(t *testing.T) {
	for _, tc := range []struct {
		title string
		show  string
		match bool
	}{
		{"heute journal", "heute journal", true},
		{"heute-journal", "heute journal", true},
		{"Heute  Journal", "heute journal", true},
		{"tagesthemen", "tagesthemen", true},
		{"Tagesthemen", "tagesthemen", true},
		{"heute", "", false},
		{"heute Xpress", "", false},
		{"tagesschau", "", false},
		{"Tatort", "", false},
	} {
		show, match := matchLateNews(tc.title)
		if match != tc.match || show != tc.show {
			t.Errorf("matchLateNews(%q): got (%q, %v), want (%q, %v)", tc.title, show, match, tc.show, tc.match)
		}
	}
}

func TestTVParseHHMM(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"21:15", 21*60 + 15, false},
		{"00:00", 0, false},
		{"23:59", 23*60 + 59, false},
		{"7:05", 7*60 + 5, false},
		{"24:00", 0, true},
		{"21:60", 0, true},
		{"21", 0, true},
		{"21:15:00", 0, true},
		{"abc", 0, true},
		{"", 0, true},
	} {
		got, err := parseHHMM(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseHHMM(%q): expected error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseHHMM(%q): %v", tc.in, err)
		} else if got != tc.want {
			t.Errorf("parseHHMM(%q): got %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestTVWindowBounds(t *testing.T) {
	for _, tc := range []struct {
		start, end string
		wantStart  int
		wantEnd    int
	}{
		{"", "", defaultTVWindowStart, defaultTVWindowEnd},
		{"21:15", "21:30", 21*60 + 15, 21*60 + 30},
		{"20:00", "23:00", 20 * 60, 23 * 60},
		{"22:00", "21:00", defaultTVWindowStart, defaultTVWindowEnd}, // start after end -> defaults
		{"bogus", "21:30", defaultTVWindowStart, 21*60 + 30},         // bad start -> default start
	} {
		ws, we := tvWindowBounds(tc.start, tc.end)
		if ws != tc.wantStart || we != tc.wantEnd {
			t.Errorf("tvWindowBounds(%q, %q): got (%d, %d), want (%d, %d)",
				tc.start, tc.end, ws, we, tc.wantStart, tc.wantEnd)
		}
	}
}

func TestTVParseScheduleTime(t *testing.T) {
	loc := time.FixedZone("Europe/Berlin", 2*3600)
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"2026-10-17T21:45:00+02:00", "21:45"},
		{"2026-10-17T21:45:00.123+02:00", "21:45"}, // fractional seconds
		{"2026-10-17T21:45:00Z", "23:45"},          // UTC -> Berlin (+2)
		{"2026-10-17T21:45:00", "21:45"},           // no offset -> local
	} {
		got, err := parseScheduleTime(tc.in, loc)
		if err != nil {
			t.Errorf("parseScheduleTime(%q): %v", tc.in, err)
			continue
		}
		if got.Format("15:04") != tc.want {
			t.Errorf("parseScheduleTime(%q): got %s, want %s", tc.in, got.Format("15:04"), tc.want)
		}
	}
	if _, err := parseScheduleTime("not a time", loc); err == nil {
		t.Error("expected error for garbage timestamp")
	}
	if _, err := parseScheduleTime("", loc); err == nil {
		t.Error("expected error for empty timestamp")
	}
}

func TestTVClientDefaults(t *testing.T) {
	c, err := newTVClient("", "", "", 0, "", "", "", testLogger())
	if err != nil {
		t.Fatalf("newTVClient: %v", err)
	}
	if c.ardBaseURL != defaultARDProgramURL {
		t.Errorf("ardBaseURL: got %q, want %q", c.ardBaseURL, defaultARDProgramURL)
	}
	if c.zdfBaseURL != defaultZDFAPIURL {
		t.Errorf("zdfBaseURL: got %q, want %q", c.zdfBaseURL, defaultZDFAPIURL)
	}
	if c.location.String() != defaultTVTimezone {
		t.Errorf("location: got %q, want %q", c.location, defaultTVTimezone)
	}
	if c.windowStart != defaultTVWindowStart || c.windowEnd != defaultTVWindowEnd {
		t.Errorf("window: got (%d, %d), want (%d, %d)", c.windowStart, c.windowEnd, defaultTVWindowStart, defaultTVWindowEnd)
	}
	if c.zdfAPIKey == "" {
		t.Error("zdfAPIKey: the built-in public token should be decoded, not empty")
	}
	if c.now == nil {
		t.Error("now must default to time.Now")
	}
}

func TestTVClientBadTimezone(t *testing.T) {
	if _, err := newTVClient("", "", "Mars/Olympus", 5, "", "", "", testLogger()); err == nil {
		t.Fatal("expected error for unknown timezone")
	} else if !strings.Contains(err.Error(), "timezone") {
		t.Errorf("error should mention the timezone: %v", err)
	}
}

func TestTVWindowLabel(t *testing.T) {
	if got := tvWindowLabel(21*60+15, 21*60+30); got != "21:15–21:30" {
		t.Errorf("tvWindowLabel: got %q, want 21:15–21:30", got)
	}
	if got := formatClock(0); got != "00:00" {
		t.Errorf("formatClock(0): got %q", got)
	}
	if got := formatClock(23*60 + 59); got != "23:59" {
		t.Errorf("formatClock(1439): got %q", got)
	}
}

// Example output for the tool, so the format is pinned down.
func ExampleformatTVNewsSchedule() {
	loc, _ := time.LoadLocation("Europe/Berlin")
	day := time.Date(2026, 10, 17, 0, 0, 0, 0, loc)
	at := func(h, m int) time.Time { return time.Date(2026, 10, 17, h, m, 0, 0, loc) }
	editions := []tvEdition{
		{Show: "heute journal", Channel: "ZDF", Start: at(21, 30), End: at(21, 45), InWindow: true},
		{Show: "tagesthemen", Channel: "ARD (Das Erste)", Start: at(22, 15), End: at(22, 45)},
	}
	fmt.Print(formatTVNewsSchedule(day, editions, nil, nil, defaultTVWindowStart, defaultTVWindowEnd, loc))
	// Output:
	// Late TV news for Sat 2026-10-17 (program of Das Erste and ZDF, times in Europe/Berlin):
	// - ZDF heute journal: 21:30–21:45 — INSIDE the 21:15–21:30 window
	// - ARD (Das Erste) tagesthemen: 22:15–22:45 — outside the 21:15–21:30 window
	// Verdict: heute journal (ZDF) at 21:30 starts within the 21:15–21:30 window.
}

func TestTVMatchLateNewsExcludesUpdate(t *testing.T) {
	// "heute journal update" is a short late-night news bulletin (aired by
	// ZDF on some days, e.g. Mondays at 23:50), not the main heute journal
	// magazine. It must NOT match — otherwise the brief shows a phantom
	// second "heute journal" entry.
	for _, tc := range []struct {
		title string
		match bool
	}{
		{"heute journal", true},
		{"heute journal Wetter", true},  // subtitle concatenated
		{"Heute-Journal", true},         // hyphenated, capitalized
		{"heute journal update", false}, // different program — excluded
		{"heute journal Update", false}, // case-insensitive exclusion
		{"heute journal UPDATE", false}, // case-insensitive exclusion
		{"heute Xpress", false},
		{"heute", false},
		{"heute - in Deutschland", false},
		{"heute - in Europa", false},
	} {
		_, match := matchLateNews(tc.title)
		if match != tc.match {
			t.Errorf("matchLateNews(%q): got %v, want %v", tc.title, match, tc.match)
		}
	}
}
