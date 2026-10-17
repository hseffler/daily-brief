package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// newPetsTool creates the get_pet_status tool: it reads the user's cats'
// inside/outside location from their SureFlap cat flap via the Sure Petcare
// cloud API (see sureflap.go).
func newPetsTool(fetcher petFetcher, logger *logrus.Logger) Tool {
	return Tool{
		Name: "get_pet_status",
		Description: "Read the user's cats from their SureFlap cat flap (Sure Petcare cloud API): for each pet, whether it is currently inside or outside, and since when the location last changed. " +
			"The location is tracked by the cat flap (a cat that went through a window or door is not accounted for) and updates near real-time. Use this for the Cats section of the brief.",
		Parameters: json.RawMessage(`{"type": "object", "properties": {}}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			pets, err := fetcher.Pets(ctx)
			if err != nil {
				return "", err
			}
			return formatPetStatus(pets, time.Now(), time.Local), nil
		},
	}
}

// formatPetStatus renders the pets' locations as compact text for the agent.
func formatPetStatus(pets []petStatus, now time.Time, loc *time.Location) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Cat locations (Sure Petcare, via the SureFlap cat flap, as of %s):\n", now.In(loc).Format("Mon 2006-01-02 15:04"))

	if len(pets) == 0 {
		sb.WriteString("- No pets registered to the Sure Petcare account.\n")
		return sb.String()
	}

	for _, p := range pets {
		name := p.Name
		if name == "" {
			name = "Unnamed cat"
		}
		switch p.Location {
		case petLocationInside:
			fmt.Fprintf(&sb, "- %s: inside", name)
		case petLocationOutside:
			fmt.Fprintf(&sb, "- %s: outside", name)
		default:
			fmt.Fprintf(&sb, "- %s: location unknown", name)
		}
		if !p.Since.IsZero() {
			fmt.Fprintf(&sb, " (since %s)", formatSince(p.Since, now, loc))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// formatSince renders a timestamp compactly: "06:12" when it is from today,
// "yesterday 21:04" when it is from yesterday, and the full date otherwise.
func formatSince(t, now time.Time, loc *time.Location) string {
	t = t.In(loc)
	now = now.In(loc)
	if t.Format("2006-01-02") == now.Format("2006-01-02") {
		return t.Format("15:04")
	}
	if t.Format("2006-01-02") == now.AddDate(0, 0, -1).Format("2006-01-02") {
		return "yesterday " + t.Format("15:04")
	}
	return t.Format("2006-01-02 15:04")
}
