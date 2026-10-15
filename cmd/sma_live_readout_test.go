//go:build live

package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"gitlab.com/bboehmke/sunny"
)

// TestLiveSMAReadout is a manual, opt-in check against the real SMA device:
// it reads the Speedwire values exactly like SMASpeedwireClient.Read does
// (connect -> GetValuesCtx -> smaStatusFromValues), prints the raw value map
// and the formatted get_pv_status output. It never runs in the normal test
// suite — the "live" build tag excludes it:
//
//	go test -tags live -run TestLiveSMAReadout ./cmd -v
//
// DAILY_BRIEF_CONFIG overrides the config path (default ../daily-brief.yaml).
func TestLiveSMAReadout(t *testing.T) {
	configFile := os.Getenv("DAILY_BRIEF_CONFIG")
	if configFile == "" {
		configFile = "../daily-brief.yaml"
	}
	cfg, err := readConfig(configFile)
	if err != nil {
		t.Fatalf("readConfig: %v", err)
	}
	if !cfg.Sources.SMA.Enabled {
		t.Fatal("sources.sma is not enabled in the config")
	}
	sma := cfg.Sources.SMA
	if sma.Host == "" {
		t.Fatal("sources.sma.host is empty — the live readout needs a configured host")
	}

	logger := testLogger()
	initSunnyLog(logger)

	conn, err := sunny.NewConnection(sma.Interface)
	if err != nil {
		t.Fatalf("sunny.NewConnection: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(sma.TimeoutSeconds)*time.Second)
	defer cancel()

	dev, err := conn.NewDevice(sma.Host, sma.Password)
	if err != nil {
		t.Fatalf("no device responding at %s: %v", sma.Host, err)
	}
	defer dev.Close()

	values, err := dev.GetValuesCtx(ctx)
	if err != nil {
		t.Fatalf("GetValuesCtx: %v", err)
	}

	fmt.Printf("\n=== raw Speedwire values (%s, serial %d, energy meter: %v, %d values) ===\n",
		sma.Host, dev.SerialNumber(), dev.IsEnergyMeter(), len(values))
	ids := make([]sunny.ValueID, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	for _, id := range ids {
		fmt.Printf("  %-24s %v\n", id.String(), values[id])
	}

	st := smaStatusFromValues(dev.SerialNumber(), dev.IsEnergyMeter(), values)
	fmt.Printf("\n=== get_pv_status output ===\n%s", formatSMAStatus(st))
}
