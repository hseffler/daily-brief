package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// canned Open-Meteo response for three days.
const cannedWeatherResponse = `{"daily":{
	"time":["2025-05-14","2025-05-15","2025-05-16"],
	"weather_code":[1,61,95],
	"temperature_2m_max":[21.4,18.2,17.6],
	"temperature_2m_min":[11.2,12.4,10.8],
	"precipitation_sum":[0.0,4.2,1.1],
	"precipitation_probability_max":[10,85,60],
	"sunshine_duration":[18720,3600,7200]
}}`

func TestWeatherFetchAndFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if got := q.Get("latitude"); got != "52.5200" {
			t.Errorf("latitude param: got %q", got)
		}
		if got := q.Get("longitude"); got != "13.4050" {
			t.Errorf("longitude param: got %q", got)
		}
		if got := q.Get("forecast_days"); got != "7" {
			t.Errorf("forecast_days param: got %q, want 7", got)
		}
		if got := q.Get("timezone"); got != "Europe/Berlin" {
			t.Errorf("timezone param: got %q", got)
		}
		if got := q.Get("daily"); !strings.Contains(got, "weather_code") || !strings.Contains(got, "precipitation_probability_max") || !strings.Contains(got, "sunshine_duration") {
			t.Errorf("daily param: got %q", got)
		}
		w.Write([]byte(cannedWeatherResponse))
	}))
	defer srv.Close()

	client := newWeatherClient(52.52, 13.405, "Europe/Berlin", 7, 5, srv.URL, testLogger())
	tool := newWeatherTool(client, testLogger())

	out, err := tool.Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, want := range []string{
		"Weather forecast for the coming 3 day(s) (52.5200, 13.4050, timezone Europe/Berlin)",
		"2025-05-14: mainly clear, 11 to 21 °C, precipitation 0.0 mm (max 10 %), sunshine 5.2 h",
		"2025-05-15: light rain, 12 to 18 °C, precipitation 4.2 mm (max 85 %), sunshine 1.0 h",
		"2025-05-16: thunderstorm, 11 to 18 °C, precipitation 1.1 mm (max 60 %), sunshine 2.0 h",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestWeatherDefaultTimezoneIsAuto(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("timezone"); got != "auto" {
			t.Errorf("timezone param: got %q, want auto", got)
		}
		w.Write([]byte(cannedWeatherResponse))
	}))
	defer srv.Close()

	client := newWeatherClient(52.52, 13.405, "", 7, 5, srv.URL, testLogger())
	if _, err := client.fetchForecast(context.Background()); err != nil {
		t.Fatalf("fetchForecast: %v", err)
	}
}

func TestWeatherHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := newWeatherClient(52.52, 13.405, "", 7, 5, srv.URL, testLogger())
	_, err := client.fetchForecast(context.Background())
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status: %v", err)
	}
}

func TestWeatherEmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"daily":{"time":[]}}`))
	}))
	defer srv.Close()

	client := newWeatherClient(52.52, 13.405, "", 7, 5, srv.URL, testLogger())
	_, err := client.fetchForecast(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no forecast days") {
		t.Fatalf("expected no-forecast-days error, got %v", err)
	}
}

func TestWeatherMissingPrecipProbability(t *testing.T) {
	// Older API responses may omit precipitation_probability_max and
	// sunshine_duration; the formatter must then omit both.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"daily":{
			"time":["2025-05-14"],
			"weather_code":[3],
			"temperature_2m_max":[15.0],
			"temperature_2m_min":[8.0],
			"precipitation_sum":[0.5]
		}}`))
	}))
	defer srv.Close()

	client := newWeatherClient(52.52, 13.405, "", 7, 5, srv.URL, testLogger())
	out, err := newWeatherTool(client, testLogger()).Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "2025-05-14: overcast, 8 to 15 °C, precipitation 0.5 mm\n") {
		t.Errorf("unexpected output:\n%s", out)
	}
	if strings.Contains(out, "%") {
		t.Errorf("no probability should be shown when absent:\n%s", out)
	}
	if strings.Contains(out, "sunshine") {
		t.Errorf("no sunshine should be shown when absent:\n%s", out)
	}
}

func TestWeatherConditionMapping(t *testing.T) {
	for code, want := range map[int]string{
		0:  "clear sky",
		3:  "overcast",
		61: "light rain",
		95: "thunderstorm",
		77: "snow grains",
	} {
		if got := weatherCondition(code); got != want {
			t.Errorf("weatherCondition(%d): got %q, want %q", code, got, want)
		}
	}
	if got := weatherCondition(12345); !strings.Contains(got, "12345") {
		t.Errorf("unknown code should mention the code: %q", got)
	}
}

func TestWeatherClientDefaults(t *testing.T) {
	c := newWeatherClient(52.52, 13.405, "", 0, 0, "", testLogger())
	if c.baseURL != defaultWeatherURL {
		t.Errorf("baseURL: got %q, want %q", c.baseURL, defaultWeatherURL)
	}
	if c.days != 7 {
		t.Errorf("days: got %d, want 7", c.days)
	}
}

func TestWeatherSunshineConversion(t *testing.T) {
	// sunshine_duration is in seconds; the formatter shows hours.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"daily":{
			"time":["2025-05-14","2025-05-15"],
			"weather_code":[0,0],
			"temperature_2m_max":[20.0,20.0],
			"temperature_2m_min":[10.0,10.0],
			"precipitation_sum":[0.0,0.0],
			"sunshine_duration":[36000,0]
		}}`))
	}))
	defer srv.Close()

	client := newWeatherClient(52.52, 13.405, "", 7, 5, srv.URL, testLogger())
	out, err := newWeatherTool(client, testLogger()).Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 36000 s = 10.0 h
	if !strings.Contains(out, "sunshine 10.0 h") {
		t.Errorf("expected 10.0 h sunshine (36000 s):\n%s", out)
	}
	// 0 s = 0.0 h — shown as "sunshine 0.0 h"
	if !strings.Contains(out, "sunshine 0.0 h") {
		t.Errorf("expected 0.0 h sunshine (0 s):\n%s", out)
	}
}

func ExampleformatWeatherForecast() {
	days := []weatherDay{{
		Date:          time.Date(2025, 5, 14, 0, 0, 0, 0, time.UTC),
		Condition:     "mainly clear",
		TempMinC:      11.2,
		TempMaxC:      21.4,
		PrecipMM:      0,
		PrecipProbMax: 10,
		SunshineHours: 5.2,
	}}
	fmt.Print(formatWeatherForecast(days, 52.52, 13.405, "Europe/Berlin"))
	// Output:
	// Weather forecast for the coming 1 day(s) (52.5200, 13.4050, timezone Europe/Berlin):
	// - Wed 2025-05-14: mainly clear, 11 to 21 °C, precipitation 0.0 mm (max 10 %), sunshine 5.2 h
}
