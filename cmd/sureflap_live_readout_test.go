//go:build live

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestLivePetsReadout is a manual, opt-in check against the real Sure Petcare
// cloud API: it reads the pets' inside/outside locations exactly like the
// get_pet_status tool does and prints the raw and the formatted output. It
// never runs in the normal test suite — the "live" build tag excludes it:
//
//	go test -tags live -run TestLivePetsReadout ./cmd -v
//
// DAILY_BRIEF_CONFIG overrides the config path (default ../daily-brief.yaml).
// Prerequisites: sources.pets enabled in the config with your Sure Petcare
// account credentials, and the cat flap registered to that account.
func TestLivePetsReadout(t *testing.T) {
	configFile := os.Getenv("DAILY_BRIEF_CONFIG")
	if configFile == "" {
		configFile = "../daily-brief.yaml"
	}
	cfg, err := readConfig(configFile)
	if err != nil {
		t.Fatalf("readConfig: %v", err)
	}
	if !cfg.Sources.Pets.Enabled {
		t.Fatal("sources.pets is not enabled in the config")
	}
	p := cfg.Sources.Pets

	client := newSureFlapClient(p.Email, p.Password)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pets, err := client.Pets(ctx)
	if err != nil {
		t.Fatalf("Pets: %v", err)
	}
	for _, pet := range pets {
		fmt.Printf("  %s: location=%d since=%s\n", pet.Name, pet.Location, pet.Since.Format(time.RFC3339))
	}

	out, err := newPetsTool(client, testLogger()).Run(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("get_pet_status: %v", err)
	}
	fmt.Printf("\n=== get_pet_status output ===\n%s", out)
}
