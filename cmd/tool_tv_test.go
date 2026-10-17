package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// serveTVFakes starts fake ARD and ZDF servers returning the canned schedules
// and returns a tool wired to them with the clock fixed to fixedTVNow.
func serveTVFakes(t *testing.T, ardBody, zdfBody string, ardStatus, zdfStatus int) Tool {
	t.Helper()
	ardSrv := newTestServer(t, ardStatus, ardBody)
	zdfSrv := newTestServer(t, zdfStatus, zdfBody)

	client, err := newTVClient("", "", "", 5, ardSrv, zdfSrv, "test-key", testLogger())
	if err != nil {
		t.Fatalf("newTVClient: %v", err)
	}
	client.now = fixedTVNow
	return newTVTool(client, testLogger())
}

func newTestServer(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		if body != "" {
			w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestTVToolRunEditionInWindow(t *testing.T) {
	tool := serveTVFakes(t, cannedARDProgram, cannedZDFBroadcasts, 200, 200)

	out, err := tool.Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, want := range []string{
		"Late TV news for Sat 2026-10-17 (program of Das Erste and ZDF, times in Europe/Berlin):",
		"- ZDF heute journal: 21:15–21:45 — INSIDE the 21:15–21:30 window",
		"- ZDF heute journal: 21:45–22:00 — outside the 21:15–21:30 window",
		"- ARD (Das Erste) tagesthemen: 21:14–21:29 — outside the 21:15–21:30 window",
		"- ARD (Das Erste) tagesthemen: 22:15–22:45 — outside the 21:15–21:30 window",
		"Verdict: heute journal (ZDF) at 21:15 starts within the 21:15–21:30 window.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The next-day entries and the other channel must not leak into the output.
	for _, unwanted := range []string{"2026-10-18", "wdr"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("output should not contain %q:\n%s", unwanted, out)
		}
	}
}

func TestTVToolRunNothingInWindow(t *testing.T) {
	// Regular evening: heute journal at 21:45, tagesthemen at 22:15 — nothing
	// in the 21:15–21:30 window, so the verdict is the fallback case.
	ard := `{"channels":[{"id":"daserste","timeSlots":[
		{"broadcastedOn":"2026-10-17T22:15:00+02:00","broadcastEnd":"2026-10-17T22:45:00+02:00","coreTitle":"tagesthemen"}
	]}]}`
	zdf := `{"http://zdf.de/rels/cmdm/broadcasts":[
		{"effectiveAirtimeBegin":"2026-10-17T21:45:00+02:00","effectiveAirtimeEnd":"2026-10-17T22:00:00+02:00","title":"heute journal"}
	]}`
	tool := serveTVFakes(t, ard, zdf, 200, 200)

	out, err := tool.Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "Verdict: no heute-journal or tagesthemen edition starts within the 21:15–21:30 window.") {
		t.Errorf("expected the empty-window verdict:\n%s", out)
	}
}

func TestTVToolRunMissingShows(t *testing.T) {
	// No late news editions at all that day.
	ard := `{"channels":[{"id":"daserste","timeSlots":[
		{"broadcastedOn":"2026-10-17T20:00:00+02:00","broadcastEnd":"2026-10-17T20:15:00+02:00","coreTitle":"tagesschau"}
	]}]}`
	zdf := `{"http://zdf.de/rels/cmdm/broadcasts":[
		{"effectiveAirtimeBegin":"2026-10-17T19:00:00+02:00","effectiveAirtimeEnd":"2026-10-17T19:20:00+02:00","title":"heute"}
	]}`
	tool := serveTVFakes(t, ard, zdf, 200, 200)

	out, err := tool.Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{
		"- tagesthemen (ARD): not in the schedule for this day",
		"- heute journal (ZDF): not in the schedule for this day",
		"Verdict: no heute-journal or tagesthemen edition starts within the 21:15–21:30 window.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestTVToolRunOneSourceFails(t *testing.T) {
	// ARD down, ZDF fine: the tool must still return the ZDF data plus an
	// error note for ARD.
	tool := serveTVFakes(t, `{}`, cannedZDFBroadcasts, 500, 200)

	out, err := tool.Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "- ARD: schedule unavailable (ARD program request failed: HTTP status 500)") {
		t.Errorf("expected an ARD error line:\n%s", out)
	}
	if !strings.Contains(out, "Verdict: heute journal (ZDF) at 21:15 starts within the 21:15–21:30 window.") {
		t.Errorf("expected the ZDF verdict:\n%s", out)
	}
}

func TestTVToolRunBothSourcesFail(t *testing.T) {
	tool := serveTVFakes(t, `{}`, `{}`, 500, 503)

	_, err := tool.Run(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected an error when both sources fail")
	}
	if !strings.Contains(err.Error(), "ARD") || !strings.Contains(err.Error(), "ZDF") {
		t.Errorf("error should mention both sources: %v", err)
	}
}

func TestTVToolRunCustomWindow(t *testing.T) {
	// With a wider window (21:00–22:30) the 21:45 heute journal is inside.
	ardSrv := newTestServer(t, 200, `{"channels":[{"id":"daserste","timeSlots":[
		{"broadcastedOn":"2026-10-17T22:15:00+02:00","broadcastEnd":"2026-10-17T22:45:00+02:00","coreTitle":"tagesthemen"}
	]}]}`)
	zdfSrv := newTestServer(t, 200, `{"http://zdf.de/rels/cmdm/broadcasts":[
		{"effectiveAirtimeBegin":"2026-10-17T21:45:00+02:00","effectiveAirtimeEnd":"2026-10-17T22:00:00+02:00","title":"heute journal"}
	]}`)

	client, err := newTVClient("21:00", "22:30", "", 5, ardSrv, zdfSrv, "test-key", testLogger())
	if err != nil {
		t.Fatalf("newTVClient: %v", err)
	}
	client.now = fixedTVNow
	out, err := newTVTool(client, testLogger()).Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "INSIDE the 21:00–22:30 window") {
		t.Errorf("expected the custom window in the output:\n%s", out)
	}
	if !strings.Contains(out, "Verdict: heute journal (ZDF) at 21:45, tagesthemen (ARD (Das Erste)) at 22:15 start within the 21:00–22:30 window.") {
		t.Errorf("expected both editions in the verdict:\n%s", out)
	}
}

func TestFormatTVNewsScheduleZeroEnd(t *testing.T) {
	// An edition without an end time must render the start time only.
	loc, _ := time.LoadLocation("Europe/Berlin")
	day := time.Date(2026, 10, 17, 0, 0, 0, 0, loc)
	start := time.Date(2026, 10, 17, 21, 30, 0, 0, loc)
	editions := []tvEdition{{Show: "heute journal", Channel: "ZDF", Start: start, InWindow: true}}

	out := formatTVNewsSchedule(day, editions, nil, nil, defaultTVWindowStart, defaultTVWindowEnd, loc)
	if !strings.Contains(out, "- ZDF heute journal: 21:30 — INSIDE the 21:15–21:30 window") {
		t.Errorf("expected start-only rendering:\n%s", out)
	}
}

func TestFormatTVNewsScheduleMultipleInWindow(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Berlin")
	day := time.Date(2026, 10, 17, 0, 0, 0, 0, loc)
	at := func(h, m int) time.Time { return time.Date(2026, 10, 17, h, m, 0, 0, loc) }
	editions := []tvEdition{
		{Show: "heute journal", Channel: "ZDF", Start: at(21, 30), InWindow: true},
		{Show: "tagesthemen", Channel: "ARD (Das Erste)", Start: at(21, 15), InWindow: true},
	}

	out := formatTVNewsSchedule(day, editions, nil, nil, defaultTVWindowStart, defaultTVWindowEnd, loc)
	if !strings.Contains(out, "Verdict: tagesthemen (ARD (Das Erste)) at 21:15, heute journal (ZDF) at 21:30 start within the 21:15–21:30 window.") {
		t.Errorf("expected the plural verdict:\n%s", out)
	}
}

func TestFormatTVNewsScheduleSourceError(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Berlin")
	day := time.Date(2026, 10, 17, 0, 0, 0, 0, loc)
	sourceErrs := map[string]error{"ZDF": http.ErrHijacked}

	out := formatTVNewsSchedule(day, nil, nil, sourceErrs, defaultTVWindowStart, defaultTVWindowEnd, loc)
	if !strings.Contains(out, "- ZDF: schedule unavailable (") {
		t.Errorf("expected the source error line:\n%s", out)
	}
}
