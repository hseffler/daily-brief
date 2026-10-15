package main

import (
	"strings"
	"testing"

	"gitlab.com/bboehmke/sunny"
)

// TestSMAStatusFromInverterValues feeds a Sunny Boy Storage-style value map
// (the shape the sunny library returns for battery inverters) and checks the
// mapping to SMAStatus.
func TestSMAStatusFromInverterValues(t *testing.T) {
	values := map[sunny.ValueID]any{
		sunny.DeviceName:                "Sunny Boy Storage 5.0",
		sunny.PvPower:                   3450.0,
		sunny.GridPowerExport:           2100.0,
		sunny.GridPowerImport:           0.0,
		sunny.BatteryCharge:             87.0,
		sunny.BatteryVoltage:            48.2,
		sunny.BatteryCurrent:            5.4,
		sunny.BatteryTemperature:        28.5,
		sunny.BatteryEnergyChargeKWh:    120.5,
		sunny.BatteryEnergyDischargeKWh: 98.3,
		sunny.ActiveEnergyPlusTodayKWh:  8.7,
		sunny.PvEnergyTotalKWh:          12345.6,
		sunny.GridEnergyImportKWh:       500.1,
		sunny.GridEnergyExportKWh:       8900.2,
	}

	st := smaStatusFromValues(1902000001, false, values)

	if st.DeviceName != "Sunny Boy Storage 5.0" {
		t.Errorf("DeviceName: got %q", st.DeviceName)
	}
	if st.Serial != "1902000001" {
		t.Errorf("Serial: got %q", st.Serial)
	}
	if st.IsEnergyMeter {
		t.Error("IsEnergyMeter should be false")
	}
	if !st.HasPvPower || st.PvPowerW != 3450 {
		t.Errorf("PvPowerW: got %v (has=%v)", st.PvPowerW, st.HasPvPower)
	}
	if !st.HasBattery {
		t.Error("HasBattery should be true")
	}
	if st.BatterySOCPercent != 87 {
		t.Errorf("BatterySOCPercent: got %v, want 87", st.BatterySOCPercent)
	}
	if st.BatteryVoltageV != 48.2 || st.BatteryCurrentA != 5.4 {
		t.Errorf("battery V/A: got %v/%v", st.BatteryVoltageV, st.BatteryCurrentA)
	}
	// Battery power is derived as U × I (evcc's approach). Compare with a
	// tolerance: the runtime product of two float64s differs from the
	// compile-time constant 48.2*5.4 in the last bits.
	if diff := st.BatteryPowerW - 48.2*5.4; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("BatteryPowerW: got %v, want %v", st.BatteryPowerW, 48.2*5.4)
	}
	if st.BatteryTempC != 28.5 {
		t.Errorf("BatteryTempC: got %v", st.BatteryTempC)
	}
	if st.BatteryChargeKWh != 120.5 || st.BatteryDischargeKWh != 98.3 {
		t.Errorf("battery energy: got %v/%v", st.BatteryChargeKWh, st.BatteryDischargeKWh)
	}
	if st.GridFeedInW != 2100 || st.GridDrawW != 0 {
		t.Errorf("grid power: got feed %v / draw %v", st.GridFeedInW, st.GridDrawW)
	}
	if !st.HasGridPower {
		t.Error("HasGridPower should be true (hybrid reports GridPowerExport/Import)")
	}
	if st.GridImportKWh != 500.1 || st.GridExportKWh != 8900.2 {
		t.Errorf("grid energy: got %v/%v", st.GridImportKWh, st.GridExportKWh)
	}
	if !st.HasDailyYield || st.DailyYieldKWh != 8.7 {
		t.Errorf("daily yield: got %v (has=%v)", st.DailyYieldKWh, st.HasDailyYield)
	}
	if !st.HasYield || st.TotalYieldKWh != 12345.6 {
		t.Errorf("total yield: got %v (has=%v)", st.TotalYieldKWh, st.HasYield)
	}
}

// TestSMAStatusFromEnergyMeterValues feeds an SMA Energy Meter / Home Manager
// style value map (OBIS-derived: ActivePowerPlus = import, ActivePowerMinus =
// export, energy counters in raw watt-seconds).
func TestSMAStatusFromEnergyMeterValues(t *testing.T) {
	values := map[sunny.ValueID]any{
		sunny.ActivePowerPlus:   250.0,         // import, W
		sunny.ActivePowerMinus:  1800.0,        // export, W
		sunny.ActiveEnergyPlus:  1800360000.0,  // Ws = 500.1 kWh
		sunny.ActiveEnergyMinus: 32040720000.0, // Ws = 8900.2 kWh
	}

	st := smaStatusFromValues(3001234567, true, values)

	if !st.IsEnergyMeter {
		t.Error("IsEnergyMeter should be true")
	}
	if st.DeviceName == "" {
		t.Error("energy meter should get a fallback device name")
	}
	if st.HasBattery {
		t.Error("energy meter values must not mark the device as having a battery")
	}
	if st.HasPvPower {
		t.Error("energy meter values must not mark PV power as available")
	}
	if st.GridDrawW != 250 || st.GridFeedInW != 1800 {
		t.Errorf("grid power: got draw %v / feed %v, want 250/1800", st.GridDrawW, st.GridFeedInW)
	}
	if !st.HasGridPower {
		t.Error("HasGridPower should be true (meter reports ActivePowerPlus/Minus)")
	}
	if st.GridImportKWh != 500.1 {
		t.Errorf("GridImportKWh: got %v, want 500.1 (Ws/3600000)", st.GridImportKWh)
	}
	if st.GridExportKWh != 8900.2 {
		t.Errorf("GridExportKWh: got %v, want 8900.2 (Ws/3600000)", st.GridExportKWh)
	}
}

// TestSMAStatusPvInverterWithoutBattery: a plain PV inverter has yield and
// grid values but no battery values.
func TestSMAStatusPvInverterWithoutBattery(t *testing.T) {
	values := map[sunny.ValueID]any{
		sunny.DeviceName:               "Sunny Tripower 10.0",
		sunny.ActivePowerPlus:          5000.0, // AC power (energy meter style register on PV inverters)
		sunny.ActiveEnergyPlusTodayKWh: 21.4,
		sunny.ActiveEnergyPlusKWh:      45000.0, // total yield (0x2601 counter)
		sunny.GridEnergyExportKWh:      44000.0,
	}

	st := smaStatusFromValues(1902000002, false, values)

	// A plain inverter doesn't report PvPower (hybrid-only channel); its
	// current AC output (ActivePowerPlus) is the production.
	if !st.HasPvPower || st.PvPowerW != 5000 {
		t.Errorf("PvPowerW should fall back to ActivePowerPlus: got %v (has=%v)", st.PvPowerW, st.HasPvPower)
	}
	// A plain inverter has no grid measurement (no hybrid metering range).
	if st.HasGridPower {
		t.Error("HasGridPower should be false (no GridPowerImport/Export on a plain inverter)")
	}
	if st.HasBattery {
		t.Error("HasBattery should be false")
	}
	if !st.HasYield || st.TotalYieldKWh != 45000 {
		t.Errorf("total yield should fall back to ActiveEnergyPlusKWh: got %v (has=%v)", st.TotalYieldKWh, st.HasYield)
	}
	if !st.HasDailyYield || st.DailyYieldKWh != 21.4 {
		t.Errorf("daily yield: got %v", st.DailyYieldKWh)
	}
}

func TestSMAStatusNumericTypes(t *testing.T) {
	// The library may return int32/uint32 depending on the register; asFloat
	// must handle all of them.
	values := map[sunny.ValueID]any{
		sunny.PvPower:        int32(1234),
		sunny.BatteryCharge:  uint32(55),
		sunny.BatteryVoltage: int64(48),
		sunny.BatteryCurrent: uint64(2),
	}
	st := smaStatusFromValues(1, false, values)
	if st.PvPowerW != 1234 {
		t.Errorf("PvPowerW: got %v", st.PvPowerW)
	}
	if st.BatterySOCPercent != 55 {
		t.Errorf("BatterySOCPercent: got %v", st.BatterySOCPercent)
	}
	if st.BatteryPowerW != 96 {
		t.Errorf("BatteryPowerW: got %v, want 96", st.BatteryPowerW)
	}
}

func TestFormatSMAStatus(t *testing.T) {
	st := &SMAStatus{
		DeviceName:          "Sunny Boy Storage 5.0",
		Serial:              "1902000001",
		PvPowerW:            3450,
		HasPvPower:          true,
		BatterySOCPercent:   87,
		BatteryVoltageV:     48.2,
		BatteryCurrentA:     5.4,
		BatteryPowerW:       260.08,
		BatteryChargeKWh:    120.5,
		BatteryDischargeKWh: 98.3,
		BatteryTempC:        28.5,
		GridFeedInW:         2100,
		GridDrawW:           0,
		HasGridPower:        true,
		GridImportKWh:       500.1,
		GridExportKWh:       8900.2,
		DailyYieldKWh:       8.7,
		HasDailyYield:       true,
		TotalYieldKWh:       12345.6,
		HasYield:            true,
		HasBattery:          true,
	}
	out := formatSMAStatus(st)
	for _, want := range []string{
		"SMA PV system (Sunny Boy Storage 5.0, serial 1902000001, via Speedwire)",
		"Current PV production: 3450 W",
		"Battery state of charge: 87 % (48.2 V, 5.40 A, 260 W)",
		"charged 120.5 kWh, discharged 98.3 kWh, temperature 28.5 °C",
		"Grid feed-in: 2100 W, grid draw: 0 W",
		"PV yield: today 8.7 kWh, total 12345.6 kWh",
		"Grid energy: imported 500.1 kWh, exported 8900.2 kWh",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// Without battery data the battery line must say so.
	st2 := &SMAStatus{DeviceName: "Sunny Tripower 10.0", Serial: "2", PvPowerW: 100, HasPvPower: true}
	out2 := formatSMAStatus(st2)
	if !strings.Contains(out2, "Battery: not available") {
		t.Errorf("expected battery-not-available note:\n%s", out2)
	}
	if !strings.Contains(out2, "Grid feed-in/draw: n/a") {
		t.Errorf("expected grid n/a note for an inverter without grid measurement:\n%s", out2)
	}

	// Energy meter without PV power: the n/a note must explain that a meter
	// cannot measure PV production.
	st3 := &SMAStatus{DeviceName: "SMA Energy Meter / Home Manager", Serial: "3", IsEnergyMeter: true, GridFeedInW: 1800, GridDrawW: 250, HasGridPower: true}
	out3 := formatSMAStatus(st3)
	if !strings.Contains(out3, "PV production: n/a") || !strings.Contains(out3, "energy meter does not measure PV production") {
		t.Errorf("expected meter-specific PV n/a note:\n%s", out3)
	}
	if !strings.Contains(out3, "Grid feed-in: 1800 W, grid draw: 250 W") {
		t.Errorf("expected meter grid power:\n%s", out3)
	}

	// Inverter without PV power: generic n/a note.
	st4 := &SMAStatus{DeviceName: "Sunny Boy", Serial: "4"}
	out4 := formatSMAStatus(st4)
	if !strings.Contains(out4, "device does not report PV power") {
		t.Errorf("expected generic PV n/a note:\n%s", out4)
	}
}

func TestNewSMASpeedwireClient(t *testing.T) {
	// Defaults.
	c, err := NewSMASpeedwireClient("192.168.1.50", "", 0, "", 0, testLogger())
	if err != nil {
		t.Fatalf("NewSMASpeedwireClient: %v", err)
	}
	if c.password != "0000" {
		t.Errorf("default password: got %q, want 0000", c.password)
	}
	if c.timeout.String() != "30s" {
		t.Errorf("default timeout: got %v, want 30s", c.timeout)
	}
	if c.host != "192.168.1.50" {
		t.Errorf("host: got %q", c.host)
	}

	// Password too long.
	if _, err := NewSMASpeedwireClient("h", "1234567890123", 0, "", 0, testLogger()); err == nil {
		t.Error("expected error for password > 12 chars")
	}
}

func TestAsFloat(t *testing.T) {
	cases := []struct {
		in   any
		want float64
	}{
		{float64(1.5), 1.5},
		{int32(-3), -3},
		{int64(7), 7},
		{uint32(9), 9},
		{uint64(11), 11},
		{"nope", 0},
		{nil, 0},
	}
	for _, tc := range cases {
		if got := asFloat(tc.in); got != tc.want {
			t.Errorf("asFloat(%v): got %v, want %v", tc.in, got, tc.want)
		}
	}
}
