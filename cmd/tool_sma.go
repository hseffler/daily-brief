package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"
)

// newSMATool creates the get_pv_status tool, which reads the user's SMA PV
// system over SMA Speedwire (see sma.go).
func newSMATool(client *SMASpeedwireClient, logger *logrus.Logger) Tool {
	return Tool{
		Name: "get_pv_status",
		Description: "Read the current state of the user's SMA photovoltaic (PV) system over SMA Speedwire: " +
			"current PV production in watts, battery state of charge in percent, battery voltage/current/power, " +
			"grid feed-in/draw in watts, and today's and total yield.",
		Parameters: json.RawMessage(`{"type": "object", "properties": {}}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			st, err := client.Read(ctx)
			if err != nil {
				return "", err
			}
			return formatSMAStatus(st), nil
		},
	}
}

// formatSMAStatus renders the PV status as compact text for the agent.
func formatSMAStatus(st *SMAStatus) string {
	var sb strings.Builder

	name := st.DeviceName
	if name == "" {
		name = "SMA device"
	}
	fmt.Fprintf(&sb, "SMA PV system (%s, serial %s, via Speedwire):\n", name, st.Serial)

	if st.HasPvPower {
		fmt.Fprintf(&sb, "- Current PV production: %.0f W\n", st.PvPowerW)
	} else if st.IsEnergyMeter {
		sb.WriteString("- Current PV production: n/a (an energy meter does not measure PV production; point sources.sma.host at the inverter)\n")
	} else {
		sb.WriteString("- Current PV production: n/a (device does not report PV power)\n")
	}

	if st.HasBattery {
		fmt.Fprintf(&sb, "- Battery state of charge: %.0f %%", st.BatterySOCPercent)
		if st.BatteryVoltageV != 0 || st.BatteryCurrentA != 0 {
			fmt.Fprintf(&sb, " (%.1f V, %.2f A, %.0f W)", st.BatteryVoltageV, st.BatteryCurrentA, st.BatteryPowerW)
		}
		sb.WriteString("\n")
		fmt.Fprintf(&sb, "- Battery energy: charged %.1f kWh, discharged %.1f kWh",
			st.BatteryChargeKWh, st.BatteryDischargeKWh)
		if st.BatteryTempC != 0 {
			fmt.Fprintf(&sb, ", temperature %.1f °C", st.BatteryTempC)
		}
		sb.WriteString("\n")
	} else {
		sb.WriteString("- Battery: not available on this device\n")
	}

	if st.HasGridPower {
		fmt.Fprintf(&sb, "- Grid feed-in: %.0f W, grid draw: %.0f W\n", st.GridFeedInW, st.GridDrawW)
	} else {
		sb.WriteString("- Grid feed-in/draw: n/a (device does not measure grid exchange; an SMA Energy Meter would be needed)\n")
	}

	if st.HasDailyYield || st.HasYield {
		fmt.Fprintf(&sb, "- PV yield: today %.1f kWh", st.DailyYieldKWh)
		if st.HasYield {
			fmt.Fprintf(&sb, ", total %.1f kWh", st.TotalYieldKWh)
		}
		sb.WriteString("\n")
	}

	if st.GridImportKWh != 0 || st.GridExportKWh != 0 {
		fmt.Fprintf(&sb, "- Grid energy: imported %.1f kWh, exported %.1f kWh\n",
			st.GridImportKWh, st.GridExportKWh)
	}

	return sb.String()
}
