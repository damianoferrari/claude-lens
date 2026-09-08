// Package config loads claude-lens configuration from the process environment.
package config

import (
	"os"
	"path/filepath"
	"strings"
)

// Config holds all runtime configuration for both the proxy and admin servers.
type Config struct {
	ProxyAddr string
	AdminAddr string

	AnthropicBaseURL       string
	AnthropicAuthToken     string
	AnthropicCustomHeaders string

	DataDir string
	LogDir  string
	DBPath  string
}

const defaultAnthropicBaseURL = "https://api.anthropic.com"

// Load reads configuration from the OS environment (plus a dev-only .env file,
// see dotenv_dev.go / dotenv_release.go), applies defaults, and ensures the
// data/log directories exist.
func Load() (Config, error) {
	loadDotEnv()

	cfg := Config{
		ProxyAddr: getEnv("CLENS_PROXY_ADDR", ":7801"),
		AdminAddr: getEnv("CLENS_ADMIN_ADDR", ":7802"),

		AnthropicBaseURL:       strings.TrimRight(getEnv("CLENS_PROXY_BASE_URL", defaultAnthropicBaseURL), "/"),
		AnthropicAuthToken:     getEnv("CLENS_PROXY_AUTH_TOKEN", ""),
		AnthropicCustomHeaders: getEnv("CLENS_PROXY_CUSTOM_HEADERS", ""),

		DataDir: getEnv("CLENS_DATA_DIR", "data"),
		LogDir:  getEnv("CLENS_LOG_DIR", "logs"),
	}
	cfg.DBPath = filepath.Join(cfg.DataDir, "claude-lens.db")

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return Config{}, err
	}
	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func getEnv(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}
