package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// newTVTool creates the get_tv_news_schedule tool: it fetches the ARD (Das
// Erste) and ZDF program for the coming day and checks whether the late news
// editions heute-journal (ZDF) and tagesthemen (ARD) start within the
// configured window (default 21:15-21:30).
func newTVTool(client *tvClient, logger *logrus.Logger) Tool {
	return Tool{
		Name: "get_tv_news_schedule",
		Description: "Fetch the TV program of ARD (Das Erste) and ZDF for the coming day and check whether the late news " +
			"editions heute-journal (ZDF) and tagesthemen (ARD) start early, within the configured evening window " +
			"(see the source description for the exact times). Returns each edition's channel, start and end time " +
			"and a verdict listing the editions that start within the window. Use this for the TV news section of the brief.",
		Parameters: json.RawMessage(`{"type": "object", "properties": {}}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			now := client.now().In(client.location)
			day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, client.location)

			editions, missing, sourceErrs := client.lateNewsEditions(ctx, day)
			if sourceErrs["ARD"] != nil && sourceErrs["ZDF"] != nil {
				return "", fmt.Errorf("ARD: %v; ZDF: %v", sourceErrs["ARD"], sourceErrs["ZDF"])
			}
			return formatTVNewsSchedule(day, editions, missing, sourceErrs, client.windowStart, client.windowEnd, client.location), nil
		},
	}
}

// formatTVNewsSchedule renders the late news editions and the window verdict
// as compact text for the agent.
func formatTVNewsSchedule(day time.Time, editions []tvEdition, missing []string, sourceErrs map[string]error, windowStart, windowEnd int, loc *time.Location) string {
	window := tvWindowLabel(windowStart, windowEnd)

	var sb strings.Builder
	fmt.Fprintf(&sb, "Late TV news for %s %s (program of Das Erste and ZDF, times in %s):\n",
		day.Format("Mon"), day.Format("2006-01-02"), loc)

	for _, source := range []string{"ARD", "ZDF"} {
		if err := sourceErrs[source]; err != nil {
			fmt.Fprintf(&sb, "- %s: schedule unavailable (%v)\n", source, err)
		}
	}
	for _, e := range editions {
		verdict := "outside"
		if e.InWindow {
			verdict = "INSIDE"
		}
		fmt.Fprintf(&sb, "- %s %s: %s", e.Channel, e.Show, e.Start.Format("15:04"))
		if !e.End.IsZero() && e.End.After(e.Start) {
			fmt.Fprintf(&sb, "–%s", e.End.Format("15:04"))
		}
		fmt.Fprintf(&sb, " — %s the %s window\n", verdict, window)
	}
	for _, m := range missing {
		fmt.Fprintf(&sb, "- %s: not in the schedule for this day\n", m)
	}

	var inWindowEds []tvEdition
	for _, e := range editions {
		if e.InWindow {
			inWindowEds = append(inWindowEds, e)
		}
	}
	sort.Slice(inWindowEds, func(i, j int) bool {
		if !inWindowEds[i].Start.Equal(inWindowEds[j].Start) {
			return inWindowEds[i].Start.Before(inWindowEds[j].Start)
		}
		return inWindowEds[i].Show < inWindowEds[j].Show
	})
	inWindow := make([]string, 0, len(inWindowEds))
	for _, e := range inWindowEds {
		inWindow = append(inWindow, fmt.Sprintf("%s (%s) at %s", e.Show, e.Channel, e.Start.Format("15:04")))
	}
	switch len(inWindow) {
	case 0:
		fmt.Fprintf(&sb, "Verdict: no heute-journal or tagesthemen edition starts within the %s window.\n", window)
	case 1:
		fmt.Fprintf(&sb, "Verdict: %s starts within the %s window.\n", inWindow[0], window)
	default:
		fmt.Fprintf(&sb, "Verdict: %s start within the %s window.\n", strings.Join(inWindow, ", "), window)
	}
	return sb.String()
}
