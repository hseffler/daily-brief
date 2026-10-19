package main

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v2"
)

// WebSource is a single web page the daily brief is built from.
type WebSource struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

// Config mirrors daily-brief.yaml. All URLs, credentials and API keys live in
// the config file, never in code.
type Config struct {
	Mistral struct {
		APIKey        string  `yaml:"api_key"`
		Model         string  `yaml:"model"`
		BaseURL       string  `yaml:"base_url"`
		Temperature   float64 `yaml:"temperature"`
		MaxIterations int     `yaml:"max_iterations"`
	} `yaml:"mistral"`

	Brief struct {
		Language string `yaml:"language"`
	} `yaml:"brief"`

	// Prompts are the system and user prompt templates sent to the LLM.
	// Available placeholders: {date}, {language}, {sources}. When a prompt is
	// empty, the built-in default (main.go) is used.
	Prompts struct {
		System string `yaml:"system"`
		User   string `yaml:"user"`
	} `yaml:"prompts"`

	Sources struct {
		Web   []WebSource `yaml:"web"`
		Email struct {
			Enabled     bool   `yaml:"enabled"`
			Host        string `yaml:"host"`
			Port        int    `yaml:"port"`
			Username    string `yaml:"username"`
			Password    string `yaml:"password"`
			Mailbox     string `yaml:"mailbox"`
			SinceHours  int    `yaml:"since_hours"`
			MaxMessages int    `yaml:"max_messages"`
		} `yaml:"email"`
		SMA struct {
			Enabled        bool   `yaml:"enabled"`
			Host           string `yaml:"host"`
			Password       string `yaml:"password"`
			Serial         uint32 `yaml:"serial"`
			Interface      string `yaml:"interface"`
			TimeoutSeconds int    `yaml:"timeout_seconds"`
		} `yaml:"sma"`
		Weather struct {
			Enabled        bool    `yaml:"enabled"`
			Latitude       float64 `yaml:"latitude"`
			Longitude      float64 `yaml:"longitude"`
			Timezone       string  `yaml:"timezone"`
			Days           int     `yaml:"days"`
			URL            string  `yaml:"url"`
			TimeoutSeconds int     `yaml:"timeout_seconds"`
		} `yaml:"weather"`
		EV struct {
			Enabled        bool   `yaml:"enabled"`
			Brand          string `yaml:"brand"`
			User           string `yaml:"user"`
			Password       string `yaml:"password"`
			VIN            string `yaml:"vin"`
			TimeoutSeconds int    `yaml:"timeout_seconds"`
			PortalURL      string `yaml:"portal_url"`   // only override for proxies/tests
			IdentityURL    string `yaml:"identity_url"` // only override for proxies/tests
		} `yaml:"ev"`
		TV struct {
			Enabled        bool   `yaml:"enabled"`
			WindowStart    string `yaml:"window_start"` // "HH:MM", inclusive
			WindowEnd      string `yaml:"window_end"`   // "HH:MM", inclusive
			Timezone       string `yaml:"timezone"`
			TimeoutSeconds int    `yaml:"timeout_seconds"`
			ARDURL         string `yaml:"ard_url"`     // only override for proxies/tests
			ZDFURL         string `yaml:"zdf_url"`     // only override for proxies/tests
			ZDFAPIKey      string `yaml:"zdf_api_key"` // only override; default is ZDF's public web-player token
		} `yaml:"tv"`
		Pets struct {
			Enabled  bool   `yaml:"enabled"`
			Email    string `yaml:"email"`
			Password string `yaml:"password"`
		} `yaml:"pets"`
	} `yaml:"sources"`

	Telegram struct {
		Token  string `yaml:"token"`
		ChatID string `yaml:"chat_id"`
	} `yaml:"telegram"`

	WebFetch struct {
		MaxChars       int `yaml:"max_chars"`
		TimeoutSeconds int `yaml:"timeout_seconds"`
	} `yaml:"web_fetch"`
}

// setDefaults fills unset fields with sane defaults.
func (c *Config) setDefaults() {
	if c.Mistral.Model == "" {
		c.Mistral.Model = "mistral-large-4"
	}
	if c.Mistral.BaseURL == "" {
		c.Mistral.BaseURL = defaultMistralBaseURL
	}
	if c.Mistral.Temperature == 0 {
		c.Mistral.Temperature = 0.3
	}
	if c.Mistral.MaxIterations == 0 {
		c.Mistral.MaxIterations = 12
	}
	if c.Brief.Language == "" {
		c.Brief.Language = "en"
	}
	if c.Sources.Email.Host == "" {
		c.Sources.Email.Host = "posteo.de"
	}
	if c.Sources.Email.Port == 0 {
		c.Sources.Email.Port = 993
	}
	if c.Sources.Email.Mailbox == "" {
		c.Sources.Email.Mailbox = "INBOX"
	}
	if c.Sources.Email.SinceHours == 0 {
		c.Sources.Email.SinceHours = 24
	}
	if c.Sources.Email.MaxMessages == 0 {
		c.Sources.Email.MaxMessages = 25
	}
	if c.Sources.SMA.Password == "" {
		c.Sources.SMA.Password = "0000"
	}
	if c.Sources.SMA.TimeoutSeconds == 0 {
		c.Sources.SMA.TimeoutSeconds = 30
	}
	if c.Sources.Weather.URL == "" {
		c.Sources.Weather.URL = defaultWeatherURL
	}
	if c.Sources.Weather.Days == 0 {
		c.Sources.Weather.Days = 7
	}
	if c.Sources.Weather.Days > 16 {
		c.Sources.Weather.Days = 16 // Open-Meteo forecast limit
	}
	if c.Sources.Weather.TimeoutSeconds == 0 {
		c.Sources.Weather.TimeoutSeconds = 15
	}
	if c.Sources.EV.Brand == "" {
		c.Sources.EV.Brand = "Volkswagen"
	}
	if c.Sources.EV.TimeoutSeconds == 0 {
		c.Sources.EV.TimeoutSeconds = 30
	}
	if c.Sources.TV.WindowStart == "" {
		c.Sources.TV.WindowStart = formatClock(defaultTVWindowStart)
	}
	if c.Sources.TV.WindowEnd == "" {
		c.Sources.TV.WindowEnd = formatClock(defaultTVWindowEnd)
	}
	if c.Sources.TV.Timezone == "" {
		c.Sources.TV.Timezone = defaultTVTimezone
	}
	if c.Sources.TV.TimeoutSeconds == 0 {
		c.Sources.TV.TimeoutSeconds = 15
	}
	if c.WebFetch.MaxChars == 0 {
		c.WebFetch.MaxChars = 8000
	}
	if c.WebFetch.TimeoutSeconds == 0 {
		c.WebFetch.TimeoutSeconds = 15
	}
}

// validate checks required fields and cross-field consistency.
func (c *Config) validate() error {
	if c.Mistral.APIKey == "" {
		return fmt.Errorf("mistral.api_key is required")
	}
	if c.Telegram.Token == "" || c.Telegram.ChatID == "" {
		return fmt.Errorf("telegram.token and telegram.chat_id are required (the brief is delivered via Telegram)")
	}
	if len(c.Sources.Web) == 0 && !c.Sources.Email.Enabled && !c.Sources.SMA.Enabled && !c.Sources.Weather.Enabled && !c.Sources.EV.Enabled && !c.Sources.TV.Enabled && !c.Sources.Pets.Enabled {
		return fmt.Errorf("no sources enabled: configure at least one of sources.web, sources.email, sources.sma, sources.weather, sources.ev, sources.tv or sources.pets")
	}
	for i, w := range c.Sources.Web {
		if w.Name == "" || w.URL == "" {
			return fmt.Errorf("sources.web[%d]: name and url are required", i)
		}
	}
	if c.Sources.Email.Enabled {
		if c.Sources.Email.Username == "" || c.Sources.Email.Password == "" {
			return fmt.Errorf("sources.email: username and password are required")
		}
	}
	if c.Sources.EV.Enabled {
		if c.Sources.EV.User == "" || c.Sources.EV.Password == "" {
			return fmt.Errorf("sources.ev: user and password are required (your Volkswagen ID credentials)")
		}
		if !vwBrandKnown(c.Sources.EV.Brand) {
			return fmt.Errorf("sources.ev: unknown brand %q (supported: Volkswagen, Audi, Skoda, Seat, Cupra)", c.Sources.EV.Brand)
		}
	}
	if c.Sources.Pets.Enabled {
		if c.Sources.Pets.Email == "" || c.Sources.Pets.Password == "" {
			return fmt.Errorf("sources.pets: email and password are required (your Sure Petcare account credentials)")
		}
	}
	// sources.sma needs no required fields: with neither host nor serial the
	// device is discovered on the LAN (a warning is logged at startup). The
	// password length is validated when the client is constructed.
	if c.Sources.Weather.Enabled {
		if c.Sources.Weather.Latitude == 0 && c.Sources.Weather.Longitude == 0 {
			return fmt.Errorf("sources.weather: latitude and longitude are required")
		}
		if c.Sources.Weather.Latitude < -90 || c.Sources.Weather.Latitude > 90 ||
			c.Sources.Weather.Longitude < -180 || c.Sources.Weather.Longitude > 180 {
			return fmt.Errorf("sources.weather: latitude/longitude out of range")
		}
	}
	if c.Sources.TV.Enabled {
		ws, err := parseHHMM(c.Sources.TV.WindowStart)
		if err != nil {
			return fmt.Errorf("sources.tv: window_start: %v", err)
		}
		we, err := parseHHMM(c.Sources.TV.WindowEnd)
		if err != nil {
			return fmt.Errorf("sources.tv: window_end: %v", err)
		}
		if ws > we {
			return fmt.Errorf("sources.tv: window_start (%s) must not be after window_end (%s)",
				c.Sources.TV.WindowStart, c.Sources.TV.WindowEnd)
		}
		if _, err := time.LoadLocation(c.Sources.TV.Timezone); err != nil {
			return fmt.Errorf("sources.tv: unknown timezone %q", c.Sources.TV.Timezone)
		}
	}
	switch c.Brief.Language {
	case "en", "de":
	default:
		return fmt.Errorf("brief.language must be \"en\" or \"de\", got %q", c.Brief.Language)
	}
	return nil
}

// readConfig reads and validates the YAML configuration file.
func readConfig(filename string) (*Config, error) {
	file, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	var config Config
	if err := yaml.Unmarshal(file, &config); err != nil {
		return nil, fmt.Errorf("failed to parse YAML config: %v", err)
	}

	config.setDefaults()
	if err := config.validate(); err != nil {
		return nil, err
	}
	return &config, nil
}
