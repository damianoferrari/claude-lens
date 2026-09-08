package config

import (
	"testing"
	"time"
)

func TestGetEnvDuration(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		fallback time.Duration
		want     time.Duration
	}{
		{"unset falls back", "", time.Hour, time.Hour},
		{"parses a valid duration", "30m", time.Hour, 30 * time.Minute},
		{"explicit zero disables", "0", time.Hour, 0},
		{"unparseable falls back", "not-a-duration", time.Hour, time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const key = "CLENS_TEST_DURATION"
			// An env var set to "" is indistinguishable from unset to
			// getEnvDuration (it trims and checks emptiness either way),
			// so this covers the "unset" case too.
			t.Setenv(key, tt.envValue)
			if got := getEnvDuration(key, tt.fallback); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
