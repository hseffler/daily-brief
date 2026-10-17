package main

import (
	"strings"
	"testing"
	"time"
)

func testPromptConfig() *Config {
	cfg := &Config{}
	cfg.setDefaults()
	cfg.Sources.Web = []WebSource{{Name: "Zeit", URL: "https://www.zeit.de/"}}
	cfg.Sources.Email.Enabled = true
	cfg.Sources.SMA.Enabled = true
	cfg.Sources.Weather.Enabled = true
	cfg.Sources.Weather.Days = 7
	cfg.Sources.EV.Enabled = true
	cfg.Sources.TV.Enabled = true
	return cfg
}

func TestDefaultSystemPromptRenders(t *testing.T) {
	cfg := testPromptConfig()
	sp := buildSystemPrompt(cfg, time.Date(2025, 5, 14, 7, 0, 0, 0, time.UTC))

	for _, want := range []string{
		"Today's date: 2025-05-14.",
		"Write the brief in English.",
		`- Web page "Zeit": https://www.zeit.de/ — fetch it with the fetch_webpage tool`,
		"- Email: the user's Posteo mailbox — call get_new_emails",
		"- SMA PV system: call get_pv_status",
		"- Weather: call get_weather_forecast for the 7-day forecast",
		"- Electric vehicle (VW group, via the EU Data Act portal): call get_ev_status",
		"- TV news (ARD Das Erste + ZDF): call get_tv_news_schedule",
		`"Car": the user's electric vehicle — battery state of charge, estimated range and charging status`,
		`"Weather": a compact 7-day forecast in a monospace table inside <pre>...</pre> tags`,
		`"TV news": the coming day's late news editions from the get_tv_news_schedule tool`,
		"ready to copy and paste, like the weather table",
		"Mark an edition that starts within the configured window with a trailing ⏰",
		"21:30 · ZDF · heute journal ⏰",
		"suggesting to re-watch heute (19:00) in the ZDF Mediathek",
	} {
		if !strings.Contains(sp, want) {
			t.Errorf("system prompt missing %q:\n%s", want, sp)
		}
	}
}

func TestDefaultUserPromptRenders(t *testing.T) {
	cfg := testPromptConfig()
	up := buildUserPrompt(cfg, time.Date(2025, 5, 14, 7, 0, 0, 0, time.UTC))
	if !strings.Contains(up, "date: 2025-05-14") {
		t.Errorf("user prompt missing date: %q", up)
	}
}

func TestCustomPromptsFromConfig(t *testing.T) {
	cfg := testPromptConfig()
	cfg.Prompts.System = "Custom system. Today is {date}. Language: {language}.\n{sources}"
	cfg.Prompts.User = "Custom user prompt for {date}."

	now := time.Date(2025, 5, 14, 7, 0, 0, 0, time.UTC)
	sp := buildSystemPrompt(cfg, now)
	if !strings.HasPrefix(sp, "Custom system. Today is 2025-05-14. Language: English.") {
		t.Errorf("custom system prompt not rendered: %q", sp)
	}
	if !strings.Contains(sp, "Available sources") {
		t.Errorf("custom system prompt should include the sources block: %q", sp)
	}
	if strings.Contains(sp, "You are the user's personal assistant") {
		t.Errorf("default prompt should be replaced, not appended: %q", sp)
	}

	if up := buildUserPrompt(cfg, now); up != "Custom user prompt for 2025-05-14." {
		t.Errorf("custom user prompt: got %q", up)
	}
}

func TestSourcesDescriptionReflectsEnabledSources(t *testing.T) {
	cfg := testPromptConfig()
	cfg.Sources.Weather.Enabled = false
	cfg.Sources.Web = nil

	desc := buildSourcesDescription(cfg)
	if strings.Contains(desc, "Web page") {
		t.Errorf("no web sources should be listed:\n%s", desc)
	}
	if strings.Contains(desc, "Weather") {
		t.Errorf("disabled weather should not be listed:\n%s", desc)
	}
	if !strings.Contains(desc, "get_new_emails") || !strings.Contains(desc, "get_pv_status") {
		t.Errorf("enabled sources missing:\n%s", desc)
	}
	if !strings.Contains(desc, "get_ev_status") {
		t.Errorf("enabled EV source missing:\n%s", desc)
	}
	if !strings.Contains(desc, "get_tv_news_schedule") {
		t.Errorf("enabled TV source missing:\n%s", desc)
	}

	cfg.Sources.EV.Enabled = false
	if desc := buildSourcesDescription(cfg); strings.Contains(desc, "get_ev_status") {
		t.Errorf("disabled EV should not be listed:\n%s", desc)
	}

	cfg.Sources.TV.Enabled = false
	if desc := buildSourcesDescription(cfg); strings.Contains(desc, "get_tv_news_schedule") {
		t.Errorf("disabled TV should not be listed:\n%s", desc)
	}
}

func TestLanguageName(t *testing.T) {
	if languageName("de") != "German" || languageName("en") != "English" {
		t.Error("languageName mapping broken")
	}
	if languageName("fr") != "fr" {
		t.Error("unknown language should pass through")
	}
}

// TestExampleConfigPromptsMatchDefaults guards against drift between the
// prompt templates in daily-brief.yaml.example and the code defaults: with
// the same enabled sources, both must render to the same prompt.
func TestExampleConfigPromptsMatchDefaults(t *testing.T) {
	cfg, err := readConfig("../daily-brief.yaml.example")
	if err != nil {
		t.Fatalf("load example config: %v", err)
	}

	def := testPromptConfig()
	def.Sources.Web = cfg.Sources.Web // match the example's web sources

	now := time.Date(2025, 5, 14, 7, 0, 0, 0, time.UTC)
	got := strings.TrimSpace(buildSystemPrompt(cfg, now))
	want := strings.TrimSpace(buildSystemPrompt(def, now))
	if got != want {
		t.Errorf("example config system prompt drifted from the code default:\n--- example ---\n%s\n--- default ---\n%s", got, want)
	}

	gotUser := strings.TrimSpace(buildUserPrompt(cfg, now))
	wantUser := strings.TrimSpace(buildUserPrompt(def, now))
	if gotUser != wantUser {
		t.Errorf("example config user prompt drifted from the code default:\n--- example ---\n%s\n--- default ---\n%s", gotUser, wantUser)
	}
}
