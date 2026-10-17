//go:build live

package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestLiveVWReadout is a manual, opt-in check against the real VW EU Data Act
// portal: it reads the vehicle status exactly like the get_ev_status tool does
// and prints the formatted output. It never runs in the normal test suite —
// the "live" build tag excludes it:
//
//	go test -tags live -run TestLiveVWReadout ./cmd -v
//
// DAILY_BRIEF_CONFIG overrides the config path (default ../daily-brief.yaml).
// Prerequisites: sources.ev enabled in the config, and the one-time portal
// setup done in a browser (log in at https://eu-data-act.drivesomethinggreater.com,
// confirm the consent screen, link the vehicle, enable the continuous data
// request).
func TestLiveVWReadout(t *testing.T) {
	configFile := os.Getenv("DAILY_BRIEF_CONFIG")
	if configFile == "" {
		configFile = "../daily-brief.yaml"
	}
	cfg, err := readConfig(configFile)
	if err != nil {
		t.Fatalf("readConfig: %v", err)
	}
	if !cfg.Sources.EV.Enabled {
		t.Fatal("sources.ev is not enabled in the config")
	}
	ev := cfg.Sources.EV

	client, err := newVWPortalClient(ev.Brand, ev.User, ev.Password, ev.VIN,
		ev.PortalURL, ev.IdentityURL, ev.TimeoutSeconds, nil, testLogger())
	if err != nil {
		t.Fatalf("newVWPortalClient: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(ev.TimeoutSeconds)*time.Second)
	defer cancel()

	st, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	fmt.Printf("\n=== get_ev_status output ===\n%s", formatEVStatus(st))
}
