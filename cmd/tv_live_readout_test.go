//go:build live

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// TestLiveTVReadout is a manual, opt-in check against the real ARD and ZDF
// program APIs: it prints the raw evening entries of both channels and the
// formatted get_tv_news_schedule output for the coming day. It never runs in
// the normal test suite — the "live" build tag excludes it:
//
//	go test -tags live -run TestLiveTVReadout ./cmd -v
func TestLiveTVReadout(t *testing.T) {
	client, err := newTVClient("", "", "", 0, "", "", "", testLogger())
	if err != nil {
		t.Fatalf("newTVClient: %v", err)
	}

	now := client.now().In(client.location)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, client.location)
	fmt.Printf("\n=== checking the program for %s (%s) ===\n", day.Format("Mon 2006-01-02"), client.location)

	ctx := context.Background()

	ardEntries, err := client.fetchARDDay(ctx, day)
	if err != nil {
		t.Logf("ARD fetch failed: %v", err)
	} else {
		fmt.Printf("\n=== ARD (Das Erste): %d entries, evening (18:00-24:00) ===\n", len(ardEntries))
		for _, e := range ardEntries {
			if e.Start.Hour() >= 18 {
				fmt.Printf("  %s-%s  %s %s\n", e.Start.Format("15:04"), e.End.Format("15:04"), e.Title, e.Subtitle)
			}
		}
	}

	zdfEntries, err := client.fetchZDFDay(ctx, day)
	if err != nil {
		t.Logf("ZDF fetch failed: %v", err)
	} else {
		fmt.Printf("\n=== ZDF: %d entries, evening (18:00-24:00) ===\n", len(zdfEntries))
		for _, e := range zdfEntries {
			if e.Start.Hour() >= 18 {
				fmt.Printf("  %s-%s  %s %s\n", e.Start.Format("15:04"), e.End.Format("15:04"), e.Title, e.Subtitle)
			}
		}
	}

	out, err := newTVTool(client, testLogger()).Run(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("get_tv_news_schedule: %v", err)
	}
	fmt.Printf("\n=== get_tv_news_schedule output ===\n%s", out)
}
