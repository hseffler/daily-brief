package main

import (
	"fmt"
	"os"

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
		c.Mistral.BaseURL = "https://api.mistral.ai/v1"
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
		c.Sources.Weather.URL = "https://api.open-meteo.com/v1/forecast"
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
	if len(c.Sources.Web) == 0 && !c.Sources.Email.Enabled && !c.Sources.SMA.Enabled && !c.Sources.Weather.Enabled {
		return fmt.Errorf("no sources enabled: configure at least one of sources.web, sources.email, sources.sma or sources.weather")
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
