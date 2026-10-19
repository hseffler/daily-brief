package main

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"gitlab.com/bboehmke/sunny"
)

// The SMA source speaks SMA Speedwire (UDP, port 9522) only — the same
// approach evcc uses, via the github.com/bboehmke/sunny library
// (gitlab.com/bboehmke/sunny, Apache-2.0). Two device families are covered:
//
//   - Battery-capable inverters (Sunny Boy Storage, Sunny Island, Sunny
//     Tripower Smart Energy, ...): read actively with the inverter's user
//     password (Speedwire login). These report PV power, battery state of
//     charge, battery voltage/current, grid power and energy counters.
//   - SMA Energy Meter / Sunny Home Manager 2.0: broadcast their measurements
//     via Speedwire multicast; no login needed. These report grid power and
//     energy counters (per phase).
//
// Point sources.sma.host at the inverter's IP/hostname. With no host, devices
// are discovered on the LAN (optionally selected by serial).

// SMAStatus is the distilled state of the PV system for the brief.
type SMAStatus struct {
	DeviceName          string
	Serial              string
	IsEnergyMeter       bool
	PvPowerW            float64
	HasPvPower          bool
	BatterySOCPercent   float64
	BatteryPowerW       float64
	BatteryVoltageV     float64
	BatteryCurrentA     float64
	BatteryTempC        float64
	BatteryChargeKWh    float64
	BatteryDischargeKWh float64
	GridFeedInW         float64
	GridDrawW           float64
	HasGridPower        bool
	GridImportKWh       float64
	GridExportKWh       float64
	DailyYieldKWh       float64
	HasDailyYield       bool
	TotalYieldKWh       float64
	HasYield            bool
	HasBattery          bool
}

// SMASpeedwireClient reads the PV system over SMA Speedwire.
type SMASpeedwireClient struct {
	host     string
	password string
	serial   uint32
	iface    string
	timeout  time.Duration
	logger   *logrus.Logger

	// device is the connected SMA device, cached after the first successful
	// connect. The sunny library manages the UDP connection lifecycle
	// internally (NewConnection is a singleton per interface with no Close
	// method), so only the device needs to be held here.
	device *sunny.Device
}

// NewSMASpeedwireClient creates a client. It does not connect yet — the
// Speedwire socket is opened lazily on the first Read. The password defaults
// to "0000" (SMA's factory user password, same default as evcc).
func NewSMASpeedwireClient(host, password string, serial uint32, iface string, timeoutSeconds int, logger *logrus.Logger) (*SMASpeedwireClient, error) {
	if password == "" {
		password = "0000"
	}
	if len(password) > 12 {
		return nil, fmt.Errorf("SMA password must be at most 12 characters (Speedwire login limit)")
	}
	if timeoutSeconds <= 0 {
		timeoutSeconds = 30
	}
	if logger == nil {
		logger = logrus.New()
	}
	initSunnyLog(logger)
	return &SMASpeedwireClient{
		host:     host,
		password: password,
		serial:   serial,
		iface:    iface,
		timeout:  time.Duration(timeoutSeconds) * time.Second,
		logger:   logger,
	}, nil
}

// Read connects to the device (if not yet connected) and reads all values.
func (c *SMASpeedwireClient) Read(ctx context.Context) (*SMAStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	dev, err := c.connect()
	if err != nil {
		return nil, err
	}

	values, err := dev.GetValuesCtx(ctx)
	if err != nil {
		return nil, fmt.Errorf("SMA Speedwire: reading values from %s failed: %w", dev.Address().IP, err)
	}

	c.logger.WithFields(logrus.Fields{
		"serial":       dev.SerialNumber(),
		"energy_meter": dev.IsEnergyMeter(),
		"values":       len(values),
	}).Info("SMA values read via Speedwire")

	return smaStatusFromValues(dev.SerialNumber(), dev.IsEnergyMeter(), values), nil
}

// connect establishes the Speedwire connection: directly to the configured
// host, or via LAN discovery (optionally filtered by serial).
func (c *SMASpeedwireClient) connect() (*sunny.Device, error) {
	if c.device != nil {
		return c.device, nil
	}

	conn, err := sunny.NewConnection(c.iface)
	if err != nil {
		return nil, fmt.Errorf("SMA Speedwire: cannot open connection (interface %q): %w", c.iface, err)
	}

	if c.host != "" {
		dev, err := conn.NewDevice(c.host, c.password)
		if err != nil {
			return nil, fmt.Errorf("SMA Speedwire: no device responding at %s (check the IP): %w", c.host, err)
		}
		c.device = dev
		return dev, nil
	}

	// No host configured: discover devices on the LAN and pick by serial
	// (or the first one found).
	c.logger.Info("SMA Speedwire: no host configured, discovering devices on the LAN")
	devices := conn.SimpleDiscoverDevices(c.password)
	for _, dev := range devices {
		if c.serial == 0 || dev.SerialNumber() == c.serial {
			c.device = dev
			return dev, nil
		}
		dev.Close()
	}
	return nil, fmt.Errorf("SMA Speedwire: no device found (discovered %d device(s), none matching serial %d)", len(devices), c.serial)
}

// smaStatusFromValues distills the sunny value map into an SMAStatus. It is a
// pure function so tests can feed it fixtures without any network.
//
// Value conventions (see the sunny library): inverters report GridPowerImport/
// GridPowerExport; energy meters report ActivePowerPlus (import) /
// ActivePowerMinus (export). Energy counters come in kWh aliases for
// inverters; energy meters report raw watt-seconds (÷ 3600000 = kWh).
func smaStatusFromValues(serial uint32, isMeter bool, values map[sunny.ValueID]any) *SMAStatus {
	st := &SMAStatus{
		Serial:        strconv.FormatUint(uint64(serial), 10),
		IsEnergyMeter: isMeter,
	}
	if isMeter {
		st.DeviceName = "SMA Energy Meter / Home Manager"
	}
	if name, ok := values[sunny.DeviceName].(string); ok && name != "" {
		st.DeviceName = name
	}

	// Current PV production. Hybrid inverters report PvPower directly; plain
	// (non-hybrid) inverters don't have that channel, so their current AC
	// output (ActivePowerPlus) is used as the production. Energy meters don't
	// measure PV production at all (their ActivePowerPlus is grid import).
	if isMeter {
		st.PvPowerW, st.HasPvPower = valueOr(values, sunny.PvPower)
	} else {
		st.PvPowerW, st.HasPvPower = firstValue(values, sunny.PvPower, sunny.ActivePowerPlus)
	}

	// Grid power: import (draw) and export (feed-in). Inverters report
	// GridPowerImport/GridPowerExport (hybrid metering); energy meters report
	// ActivePowerPlus (import) / ActivePowerMinus (export).
	var drawOK, feedOK bool
	if isMeter {
		st.GridDrawW, drawOK = valueOr(values, sunny.ActivePowerPlus)
		st.GridFeedInW, feedOK = valueOr(values, sunny.ActivePowerMinus)
	} else {
		st.GridDrawW, drawOK = valueOr(values, sunny.GridPowerImport)
		st.GridFeedInW, feedOK = valueOr(values, sunny.GridPowerExport)
	}
	st.HasGridPower = drawOK || feedOK

	// Battery state (battery inverters: Sunny Boy Storage, Sunny Island, ...).
	if soc, ok := values[sunny.BatteryCharge]; ok {
		st.HasBattery = true
		st.BatterySOCPercent = asFloat(soc)
	}
	battV, vok := values[sunny.BatteryVoltage]
	battA, aok := values[sunny.BatteryCurrent]
	if vok && aok {
		st.HasBattery = true
		st.BatteryVoltageV = asFloat(battV)
		st.BatteryCurrentA = asFloat(battA)
		// The library has no direct battery power value; evcc derives it
		// the same way: P = U × I.
		st.BatteryPowerW = st.BatteryVoltageV * st.BatteryCurrentA
	}
	if t, ok := values[sunny.BatteryTemperature]; ok {
		st.HasBattery = true
		st.BatteryTempC = asFloat(t)
	}
	st.BatteryChargeKWh, _ = valueOr(values, sunny.BatteryEnergyChargeKWh)
	st.BatteryDischargeKWh, _ = valueOr(values, sunny.BatteryEnergyDischargeKWh)

	// Yields.
	st.DailyYieldKWh, st.HasDailyYield = valueOr(values, sunny.ActiveEnergyPlusTodayKWh)
	st.TotalYieldKWh, st.HasYield = firstValue(values, sunny.PvEnergyTotalKWh, sunny.ActiveEnergyPlusKWh)

	// Grid energy counters.
	st.GridImportKWh, st.GridExportKWh = gridEnergyKWh(values)

	return st
}

// gridEnergyKWh extracts the grid import/export counters in kWh. Inverters
// provide kWh aliases directly; energy meters report raw watt-seconds.
func gridEnergyKWh(values map[sunny.ValueID]any) (importKWh, exportKWh float64) {
	if v, ok := valueOr(values, sunny.GridEnergyImportKWh); ok {
		importKWh = v
	} else if v, ok := values[sunny.ActiveEnergyPlus]; ok {
		importKWh = asFloat(v) / 3600000
	}
	if v, ok := valueOr(values, sunny.GridEnergyExportKWh); ok {
		exportKWh = v
	} else if v, ok := values[sunny.ActiveEnergyMinus]; ok {
		exportKWh = asFloat(v) / 3600000
	}
	return importKWh, exportKWh
}

// valueOr returns the float value for id, or false if absent.
func valueOr(values map[sunny.ValueID]any, id sunny.ValueID) (float64, bool) {
	v, ok := values[id]
	if !ok {
		return 0, false
	}
	return asFloat(v), true
}

// firstValue returns the float value of the first present id.
func firstValue(values map[sunny.ValueID]any, ids ...sunny.ValueID) (float64, bool) {
	for _, id := range ids {
		if v, ok := values[id]; ok {
			return asFloat(v), true
		}
	}
	return 0, false
}

// asFloat converts the numeric types the sunny library returns.
func asFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int32:
		return float64(t)
	case int64:
		return float64(t)
	case uint32:
		return float64(t)
	case uint64:
		return float64(t)
	default:
		return 0
	}
}

// logrusAdapter routes the sunny library's trace logging into logrus.
type logrusAdapter struct {
	logger *logrus.Logger
}

func (a logrusAdapter) Printf(format string, v ...interface{}) {
	a.logger.Debugf("sunny: "+format, v...)
}

var sunnyLogOnce sync.Once

// initSunnyLog installs the logrus adapter as the sunny library logger, once.
func initSunnyLog(logger *logrus.Logger) {
	sunnyLogOnce.Do(func() {
		sunny.Log = logrusAdapter{logger: logger}
	})
}
