package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"
)

// newEVTool creates the get_ev_status tool, which reads the user's electric
// vehicle (VW group) via the VW EU Data Act portal (see vw.go).
func newEVTool(client *vwPortalClient, logger *logrus.Logger) Tool {
	return Tool{
		Name: "get_ev_status",
		Description: "Read the current state of the user's electric vehicle (VW group: VW ID models, Audi, Skoda, Seat, Cupra) via the VW EU Data Act portal: " +
			"battery state of charge in percent, estimated range in km, charging status (not plugged in, plugged in, charging) and odometer. " +
			"The portal delivers a new dataset roughly every 15 minutes and only while the vehicle is active, so the data may be hours old — the result states how old it is.",
		Parameters: json.RawMessage(`{"type": "object", "properties": {}}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			st, err := client.Status(ctx)
			if err != nil {
				return "", err
			}
			return formatEVStatus(st), nil
		},
	}
}

// formatEVStatus renders the EV status as compact text for the agent.
func formatEVStatus(st *evStatus) string {
	var sb strings.Builder

	name := st.VehicleName
	if name == "" {
		name = "Electric vehicle"
	}
	fmt.Fprintf(&sb, "%s (VIN %s, via the VW EU Data Act portal):\n", name, st.VIN)

	if st.SocPercent >= 0 {
		fmt.Fprintf(&sb, "- Battery state of charge: %.0f %%\n", st.SocPercent)
	} else {
		sb.WriteString("- Battery state of charge: n/a (not in the latest dataset)\n")
	}

	if st.RangeKm >= 0 {
		fmt.Fprintf(&sb, "- Estimated range: %d km\n", st.RangeKm)
	} else {
		sb.WriteString("- Estimated range: n/a (not in the latest dataset)\n")
	}

	switch st.ChargeStatus {
	case evChargeCharging:
		sb.WriteString("- Charging status: charging\n")
	case evChargeConnected:
		sb.WriteString("- Charging status: plugged in, not charging\n")
	case evChargeDisconnected:
		sb.WriteString("- Charging status: not plugged in\n")
	default:
		sb.WriteString("- Charging status: unknown (not in the latest dataset)\n")
	}

	if st.OdometerKm >= 0 {
		fmt.Fprintf(&sb, "- Odometer: %.0f km\n", st.OdometerKm)
	}

	if !st.DataTime.IsZero() {
		fmt.Fprintf(&sb, "- Data as of: %s — the portal delivers a new dataset roughly every 15 minutes and only while the car is active, so it may be hours old\n",
			st.DataTime.UTC().Format("2006-01-02 15:04 UTC"))
	}

	return sb.String()
}
