package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}
	return path
}

func TestReadConfigExample(t *testing.T) {
	cfg, err := readConfig("../daily-brief.yaml.example")
	if err != nil {
		t.Fatalf("example config should load and validate: %v", err)
	}
	if len(cfg.Sources.Web) != 4 {
		t.Errorf("expected 4 web sources, got %d", len(cfg.Sources.Web))
	}
	if !cfg.Sources.Email.Enabled || !cfg.Sources.SMA.Enabled {
		t.Errorf("expected email and SMA to be enabled in the example")
	}
}

func TestReadConfigDefaults(t *testing.T) {
	path := writeTempConfig(t, `
mistral:
  api_key: "key"
telegram:
  token: "tok"
  chat_id: "123"
sources:
  web:
    - name: "Zeit"
      url: "https://www.zeit.de/"
`)
	cfg, err := readConfig(path)
	if err != nil {
		t.Fatalf("readConfig: %v", err)
	}

	checks := map[string]string{
		"model":     cfg.Mistral.Model,
		"base_url":  cfg.Mistral.BaseURL,
		"language":  cfg.Brief.Language,
		"imap_host": cfg.Sources.Email.Host,
	}
	want := map[string]string{
		"model":     "mistral-large-4",
		"base_url":  "https://api.mistral.ai/v1",
		"language":  "en",
		"imap_host": "posteo.de",
	}
	for k, w := range want {
		if checks[k] != w {
			t.Errorf("default %s: got %q, want %q", k, checks[k], w)
		}
	}
	if cfg.Sources.Email.Port != 993 {
		t.Errorf("default imap port: got %d, want 993", cfg.Sources.Email.Port)
	}
	if cfg.Sources.Email.SinceHours != 24 {
		t.Errorf("default since_hours: got %d, want 24", cfg.Sources.Email.SinceHours)
	}
	if cfg.Sources.SMA.Password != "0000" {
		t.Errorf("default SMA password: got %q, want 0000", cfg.Sources.SMA.Password)
	}
	if cfg.Sources.SMA.TimeoutSeconds != 30 {
		t.Errorf("default SMA timeout: got %d, want 30", cfg.Sources.SMA.TimeoutSeconds)
	}
	if cfg.Sources.Weather.URL != "https://api.open-meteo.com/v1/forecast" {
		t.Errorf("default weather url: got %q", cfg.Sources.Weather.URL)
	}
	if cfg.Sources.Weather.Days != 7 {
		t.Errorf("default weather days: got %d, want 7", cfg.Sources.Weather.Days)
	}
	if cfg.Sources.Weather.TimeoutSeconds != 15 {
		t.Errorf("default weather timeout: got %d, want 15", cfg.Sources.Weather.TimeoutSeconds)
	}
	if cfg.WebFetch.MaxChars != 8000 {
		t.Errorf("default max_chars: got %d, want 8000", cfg.WebFetch.MaxChars)
	}
}

func TestReadConfigValidation(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name: "missing api key",
			content: `
telegram: {token: "t", chat_id: "c"}
sources:
  web: [{name: "Z", url: "https://www.zeit.de/"}]
`,
			wantErr: "api_key",
		},
		{
			name: "missing telegram",
			content: `
mistral: {api_key: "k"}
sources:
  web: [{name: "Z", url: "https://www.zeit.de/"}]
`,
			wantErr: "telegram",
		},
		{
			name: "no sources",
			content: `
mistral: {api_key: "k"}
telegram: {token: "t", chat_id: "c"}
`,
			wantErr: "no sources",
		},
		{
			name: "bad language",
			content: `
mistral: {api_key: "k"}
telegram: {token: "t", chat_id: "c"}
brief: {language: "fr"}
sources:
  web: [{name: "Z", url: "https://www.zeit.de/"}]
`,
			wantErr: "language",
		},
		{
			name: "email without password",
			content: `
mistral: {api_key: "k"}
telegram: {token: "t", chat_id: "c"}
sources:
  email: {enabled: true, username: "you@posteo.de"}
`,
			wantErr: "password",
		},

		{
			name: "weather without coordinates",
			content: `
mistral: {api_key: "k"}
telegram: {token: "t", chat_id: "c"}
sources:
  weather: {enabled: true}
`,
			wantErr: "latitude and longitude",
		},
		{
			name: "weather coordinates out of range",
			content: `
mistral: {api_key: "k"}
telegram: {token: "t", chat_id: "c"}
sources:
  weather: {enabled: true, latitude: 123.0, longitude: 13.4}
`,
			wantErr: "out of range",
		},
		{
			name: "web source without url",
			content: `
mistral: {api_key: "k"}
telegram: {token: "t", chat_id: "c"}
sources:
  web: [{name: "Z"}]
`,
			wantErr: "url",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readConfig(writeTempConfig(t, tc.content))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected error containing %q, got %q", tc.wantErr, err.Error())
			}
		})
	}
}

func TestReadConfigMissingFile(t *testing.T) {
	if _, err := readConfig(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
		t.Fatal("expected error for missing config file")
	}
}
