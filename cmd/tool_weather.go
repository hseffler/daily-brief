package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// The weather source is the Open-Meteo forecast API (https://open-meteo.com/):
// free and no API key required. The endpoint URL, coordinates, timezone and
// forecast length all come from the config file, never from code.

const defaultWeatherURL = "https://api.open-meteo.com/v1/forecast"

// wmoWeatherCodes maps WMO weather interpretation codes (as returned by
// Open-Meteo) to short English descriptions.
var wmoWeatherCodes = map[int]string{
	0:  "clear sky",
	1:  "mainly clear",
	2:  "partly cloudy",
	3:  "overcast",
	45: "fog",
	48: "rime fog",
	51: "light drizzle",
	53: "drizzle",
	55: "dense drizzle",
	61: "light rain",
	63: "rain",
	65: "heavy rain",
	66: "light freezing rain",
	67: "freezing rain",
	71: "light snow",
	73: "snow",
	75: "heavy snow",
	77: "snow grains",
	80: "light rain showers",
	81: "rain showers",
	82: "heavy rain showers",
	85: "light snow showers",
	86: "snow showers",
	95: "thunderstorm",
	96: "thunderstorm with light hail",
	99: "thunderstorm with hail",
}

// weatherCondition maps a WMO code to a short description.
func weatherCondition(code int) string {
	if c, ok := wmoWeatherCodes[code]; ok {
		return c
	}
	return fmt.Sprintf("unknown (code %d)", code)
}

// weatherDay is one forecast day.
type weatherDay struct {
	Date          time.Time
	Condition     string
	TempMinC      float64
	TempMaxC      float64
	PrecipMM      float64
	PrecipProbMax int // percent; -1 when the API did not return it
}

// weatherClient fetches the Open-Meteo daily forecast.
type weatherClient struct {
	baseURL   string
	latitude  float64
	longitude float64
	timezone  string
	days      int
	client    *http.Client
	logger    *logrus.Logger
}

// newWeatherClient creates a client. baseURL defaults to the public Open-Meteo
// API (overridable for tests/proxies).
func newWeatherClient(latitude, longitude float64, timezone string, days, timeoutSeconds int, baseURL string, logger *logrus.Logger) *weatherClient {
	if baseURL == "" {
		baseURL = defaultWeatherURL
	}
	if days <= 0 {
		days = 7
	}
	if timeoutSeconds <= 0 {
		timeoutSeconds = 15
	}
	if logger == nil {
		logger = logrus.New()
	}
	return &weatherClient{
		baseURL:   strings.TrimRight(baseURL, "/"),
		latitude:  latitude,
		longitude: longitude,
		timezone:  timezone,
		days:      days,
		client:    &http.Client{Timeout: time.Duration(timeoutSeconds) * time.Second},
		logger:    logger,
	}
}

// openMeteoResponse is the subset of the Open-Meteo response used here.
type openMeteoResponse struct {
	Daily struct {
		Time                        []string  `json:"time"`
		WeatherCode                 []int     `json:"weather_code"`
		Temperature2mMax            []float64 `json:"temperature_2m_max"`
		Temperature2mMin            []float64 `json:"temperature_2m_min"`
		PrecipitationSum            []float64 `json:"precipitation_sum"`
		PrecipitationProbabilityMax []int     `json:"precipitation_probability_max"`
	} `json:"daily"`
}

// fetchForecast downloads and parses the daily forecast.
func (c *weatherClient) fetchForecast(ctx context.Context) ([]weatherDay, error) {
	q := url.Values{}
	q.Set("latitude", fmt.Sprintf("%.4f", c.latitude))
	q.Set("longitude", fmt.Sprintf("%.4f", c.longitude))
	q.Set("daily", "weather_code,temperature_2m_max,temperature_2m_min,precipitation_sum,precipitation_probability_max")
	q.Set("forecast_days", fmt.Sprintf("%d", c.days))
	if c.timezone != "" {
		q.Set("timezone", c.timezone)
	} else {
		q.Set("timezone", "auto")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build weather request: %w", err)
	}
	req.Header.Set("User-Agent", "daily-brief/1.0 (weather forecast)")
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("weather request failed: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("weather API returned HTTP status %d", resp.StatusCode)
	}

	var om openMeteoResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&om); err != nil {
		return nil, fmt.Errorf("failed to parse weather response: %w", err)
	}

	d := om.Daily
	if len(d.Time) == 0 {
		return nil, fmt.Errorf("weather API returned no forecast days")
	}

	days := make([]weatherDay, 0, len(d.Time))
	for i, dateStr := range d.Time {
		date, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			c.logger.WithError(err).Warnf("skipping malformed forecast date %q", dateStr)
			continue
		}
		day := weatherDay{Date: date, PrecipProbMax: -1}
		if i < len(d.WeatherCode) {
			day.Condition = weatherCondition(d.WeatherCode[i])
		}
		if i < len(d.Temperature2mMax) {
			day.TempMaxC = d.Temperature2mMax[i]
		}
		if i < len(d.Temperature2mMin) {
			day.TempMinC = d.Temperature2mMin[i]
		}
		if i < len(d.PrecipitationSum) {
			day.PrecipMM = d.PrecipitationSum[i]
		}
		if i < len(d.PrecipitationProbabilityMax) {
			day.PrecipProbMax = d.PrecipitationProbabilityMax[i]
		}
		days = append(days, day)
	}

	c.logger.WithFields(logrus.Fields{
		"days": len(days),
		"lat":  c.latitude,
		"lon":  c.longitude,
	}).Debug("weather forecast fetched")

	return days, nil
}

// formatWeatherForecast renders the forecast as compact text for the agent.
func formatWeatherForecast(days []weatherDay, latitude, longitude float64, timezone string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Weather forecast for the coming %d day(s) (%.4f, %.4f", len(days), latitude, longitude)
	if timezone != "" {
		fmt.Fprintf(&sb, ", timezone %s", timezone)
	}
	sb.WriteString("):\n")
	for _, d := range days {
		fmt.Fprintf(&sb, "- %s %s: %s, %.0f to %.0f °C",
			d.Date.Format("Mon"), d.Date.Format("2006-01-02"), d.Condition, d.TempMinC, d.TempMaxC)
		fmt.Fprintf(&sb, ", precipitation %.1f mm", d.PrecipMM)
		if d.PrecipProbMax >= 0 {
			fmt.Fprintf(&sb, " (max %d %%)", d.PrecipProbMax)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// newWeatherTool creates the get_weather_forecast tool.
func newWeatherTool(client *weatherClient, logger *logrus.Logger) Tool {
	return Tool{
		Name: "get_weather_forecast",
		Description: "Fetch the weather forecast for the user's location for the coming days: per day the " +
			"weather condition, minimum/maximum temperature, and precipitation amount and probability. " +
			"Use this for the weather section of the brief.",
		Parameters: json.RawMessage(`{"type": "object", "properties": {}}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			days, err := client.fetchForecast(ctx)
			if err != nil {
				return "", err
			}
			return formatWeatherForecast(days, client.latitude, client.longitude, client.timezone), nil
		},
	}
}
