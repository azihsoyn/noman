package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Backend    string `json:"backend"`
	ClaudePath string `json:"claude_path"`
	APIKey     string `json:"api_key"`
	Model      string `json:"model"`
	BaseURL    string `json:"base_url"`
	MaxHistory int    `json:"max_history"`
}

var defaultConfig = Config{
	Backend:    "cli",
	Model:      "claude-sonnet-4-20250514",
	MaxHistory: 500,
}

func loadConfig() Config {
	cfg := defaultConfig
	dir := configDir()

	// Try config.toml first, then config.json
	tomlPath := filepath.Join(dir, "config.toml")
	jsonPath := filepath.Join(dir, "config.json")

	if data, err := os.ReadFile(tomlPath); err == nil {
		parseTOMLInto(string(data), &cfg)
	} else if data, err := os.ReadFile(jsonPath); err == nil {
		_ = json.Unmarshal(data, &cfg)
	}

	// Environment variables override config file
	if v := os.Getenv("NOMAN_BACKEND"); v != "" {
		cfg.Backend = v
	}
	if v := os.Getenv("NOMAN_CLAUDE_PATH"); v != "" {
		cfg.ClaudePath = v
	}
	if v := os.Getenv("NOMAN_API_KEY"); v != "" {
		cfg.APIKey = v
	} else if v := os.Getenv("ANTHROPIC_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("NOMAN_MODEL"); v != "" {
		cfg.Model = v
	}
	if v := os.Getenv("NOMAN_BASE_URL"); v != "" {
		cfg.BaseURL = v
	}
	if v := os.Getenv("NOMAN_MAX_HISTORY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxHistory = n
		}
	}

	return cfg
}

func configDir() string {
	if dir := os.Getenv("NOMAN_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "noman")
}

// parseTOMLInto parses a simple flat TOML (key = value) into Config.
func parseTOMLInto(data string, cfg *Config) {
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		value = unquote(value)

		switch key {
		case "backend":
			cfg.Backend = value
		case "claude_path":
			cfg.ClaudePath = value
		case "api_key":
			cfg.APIKey = value
		case "model":
			cfg.Model = value
		case "base_url":
			cfg.BaseURL = value
		case "max_history":
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				cfg.MaxHistory = n
			}
		}
	}
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
