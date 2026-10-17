package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakePetFetcher is a petFetcher fake for tool tests.
type fakePetFetcher struct {
	pets []petStatus
	err  error
}

func (f *fakePetFetcher) Pets(ctx context.Context) ([]petStatus, error) {
	return f.pets, f.err
}

func TestPetsToolRun(t *testing.T) {
	since := time.Date(2026, 10, 16, 19, 4, 0, 0, time.UTC)
	fetcher := &fakePetFetcher{pets: []petStatus{
		{Name: "Minka", Location: petLocationOutside, Since: since},
		{Name: "Felix", Location: petLocationInside, Since: since},
		{Name: "Garfield"},
	}}

	out, err := newPetsTool(fetcher, testLogger()).Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{
		"Cat locations (Sure Petcare, via the SureFlap cat flap, as of",
		"- Minka: outside (since ",
		"- Felix: inside (since ",
		"- Garfield: location unknown\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestPetsToolRunError(t *testing.T) {
	fetcher := &fakePetFetcher{err: errors.New("Sure Petcare request failed: boom")}

	_, err := newPetsTool(fetcher, testLogger()).Run(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected an error when the fetcher fails")
	}
}

func TestFormatPetStatus(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Berlin")
	now := time.Date(2026, 10, 17, 5, 0, 0, 0, loc)
	at := func(y int, m time.Month, d, h, min int) time.Time {
		return time.Date(y, m, d, h, min, 0, 0, loc)
	}

	pets := []petStatus{
		{Name: "Minka", Location: petLocationInside, Since: at(2026, 10, 17, 4, 12)},
		{Name: "Felix", Location: petLocationOutside, Since: at(2026, 10, 16, 21, 4)},
		{Name: "Garfield", Location: petLocationInside, Since: at(2026, 10, 10, 8, 0)},
		{Name: "", Location: petLocationUnknown},
	}

	out := formatPetStatus(pets, now, loc)
	for _, want := range []string{
		"Cat locations (Sure Petcare, via the SureFlap cat flap, as of Sat 2026-10-17 05:00):",
		"- Minka: inside (since 04:12)",
		"- Felix: outside (since yesterday 21:04)",
		"- Garfield: inside (since 2026-10-10 08:00)",
		"- Unnamed cat: location unknown\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestFormatPetStatusNoPets(t *testing.T) {
	out := formatPetStatus(nil, time.Now(), time.Local)
	if !strings.Contains(out, "- No pets registered to the Sure Petcare account.") {
		t.Errorf("expected the no-pets line:\n%s", out)
	}
}

func TestFormatSince(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Berlin")
	now := time.Date(2026, 10, 17, 5, 0, 0, 0, loc)
	cases := []struct {
		t    time.Time
		want string
	}{
		{time.Date(2026, 10, 17, 4, 12, 0, 0, loc), "04:12"},
		{time.Date(2026, 10, 16, 23, 59, 0, 0, loc), "yesterday 23:59"},
		{time.Date(2026, 10, 15, 8, 0, 0, 0, loc), "2026-10-15 08:00"},
	}
	for _, tc := range cases {
		if got := formatSince(tc.t, now, loc); got != tc.want {
			t.Errorf("formatSince(%v): got %q, want %q", tc.t, got, tc.want)
		}
	}
}
