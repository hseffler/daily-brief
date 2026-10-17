package main

import (
	"context"
	"testing"
	"time"

	gosure "github.com/perbu/go-sure"
)

func wherePtr(w gosure.PetPositionWhere) *gosure.PetPositionWhere { return &w }
func timePtr(t time.Time) *time.Time                              { return &t }

func TestPetStatusesFromGosure(t *testing.T) {
	since := time.Date(2026, 10, 16, 19, 4, 0, 0, time.UTC)
	pets := []gosure.Pet{
		{ID: 1, Name: "Minka", Position: &gosure.PetPosition{Where: wherePtr(gosure.PetPositionOutside), Since: timePtr(since)}},
		{ID: 2, Name: "Felix", Position: &gosure.PetPosition{Where: wherePtr(gosure.PetPositionInside), Since: timePtr(since)}},
		{ID: 3, Name: "NoPosition"},
		{ID: 4, Name: "NoWhere", Position: &gosure.PetPosition{Since: timePtr(since)}},
	}

	out := petStatusesFromGosure(pets)
	if len(out) != 4 {
		t.Fatalf("expected 4 pets, got %d", len(out))
	}
	if out[0].Name != "Minka" || out[0].Location != petLocationOutside || !out[0].Since.Equal(since) {
		t.Errorf("pet 0: got %+v, want Minka outside since %v", out[0], since)
	}
	if out[1].Name != "Felix" || out[1].Location != petLocationInside || !out[1].Since.Equal(since) {
		t.Errorf("pet 1: got %+v, want Felix inside since %v", out[1], since)
	}
	if out[2].Location != petLocationUnknown || !out[2].Since.IsZero() {
		t.Errorf("pet 2: got %+v, want unknown location and no since", out[2])
	}
	if out[3].Location != petLocationUnknown || !out[3].Since.Equal(since) {
		t.Errorf("pet 3: got %+v, want unknown location with since %v", out[3], since)
	}
}

func TestSureFlapClientPetsContextCancelled(t *testing.T) {
	// A cancelled context must fail before any network call.
	client := newSureFlapClient("you@example.com", "secret")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Pets(ctx); err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
}
