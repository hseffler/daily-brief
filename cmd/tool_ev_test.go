package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFormatEVStatus(t *testing.T) {
	st := &evStatus{
		VehicleName:  "ID.3",
		VIN:          "WVWZZZ1KZ6W123456",
		SocPercent:   67,
		RangeKm:      231,
		OdometerKm:   45231,
		ChargeStatus: evChargeCharging,
		DataTime:     time.Date(2026, 10, 17, 4, 45, 0, 0, time.UTC),
	}
	out := formatEVStatus(st)
	for _, want := range []string{
		"ID.3 (VIN WVWZZZ1KZ6W123456, via the VW EU Data Act portal):",
		"- Battery state of charge: 67 %",
		"- Estimated range: 231 km",
		"- Charging status: charging",
		"- Odometer: 45231 km",
		"- Data as of: 2026-10-17 04:45 UTC",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestFormatEVStatusMissingValues(t *testing.T) {
	st := &evStatus{VIN: "WVWZZZ", SocPercent: -1, RangeKm: -1, OdometerKm: -1}
	out := formatEVStatus(st)
	for _, want := range []string{
		"Electric vehicle (VIN WVWZZZ",
		"- Battery state of charge: n/a",
		"- Estimated range: n/a",
		"- Charging status: unknown",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Odometer") {
		t.Errorf("odometer line should be omitted when absent:\n%s", out)
	}
	if strings.Contains(out, "Data as of") {
		t.Errorf("data-as-of line should be omitted when absent:\n%s", out)
	}
}

func TestFormatEVStatusChargeStates(t *testing.T) {
	cases := map[string]string{
		evChargeCharging:     "charging",
		evChargeConnected:    "plugged in, not charging",
		evChargeDisconnected: "not plugged in",
		"":                   "unknown",
	}
	for status, want := range cases {
		out := formatEVStatus(&evStatus{VIN: "V", ChargeStatus: status})
		if !strings.Contains(out, want) {
			t.Errorf("status %q: output missing %q:\n%s", status, want, out)
		}
	}
}

func TestEVToolEndToEnd(t *testing.T) {
	w := newVWFakeWorld(t)
	w.addDataset(t, "20261017044500_WVWZZZ.zip",
		time.Date(2026, 10, 17, 4, 0, 0, 0, time.UTC), testVWVIN, vwTestID3Points())

	tool := newEVTool(w.newClient(t, ""), testLogger())
	if tool.Name != "get_ev_status" {
		t.Errorf("tool name: got %q", tool.Name)
	}
	if !strings.Contains(tool.Description, "state of charge") {
		t.Errorf("tool description should mention the state of charge:\n%s", tool.Description)
	}

	out, err := tool.Run(context.Background(), json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("tool.Run: %v", err)
	}
	for _, want := range []string{
		"ID.3 (VIN WVWZZZ1KZ6W123456",
		"- Battery state of charge: 67 %",
		"- Estimated range: 231 km",
		"- Charging status: charging",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tool output missing %q:\n%s", want, out)
		}
	}
}

func TestEVToolErrorPropagates(t *testing.T) {
	w := newVWFakeWorld(t)
	w.identifier404.Store(true)
	tool := newEVTool(w.newClient(t, ""), testLogger())
	_, err := tool.Run(context.Background(), json.RawMessage("{}"))
	if err == nil {
		t.Fatal("expected the portal error to propagate")
	}
}
