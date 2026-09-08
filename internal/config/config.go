// Package config loads claude-lens configuration from the process environment.
package config

import (
	"os"
	"path/filepath"
	"strings"
	"time"
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

	// LiteLLMSyncInterval is how often model_prices is refreshed from the
	// upstream's LiteLLM /model/info endpoint (see internal/pricesync).
	// Zero disables the background loop entirely — the admin UI's manual
	// "Sync from LiteLLM" button still works either way.
	LiteLLMSyncInterval time.Duration
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

		LiteLLMSyncInterval: getEnvDuration("CLENS_LITELLM_SYNC_INTERVAL", 24*time.Hour),
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

// getEnvDuration parses name as a Go duration string (e.g. "24h", "30m").
// "0" explicitly disables the feature it configures; an unset, empty, or
// unparseable value falls back to fallback.
func getEnvDuration(name string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}
