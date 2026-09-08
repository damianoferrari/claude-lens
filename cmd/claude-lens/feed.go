package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/lfsc09/claude-lens/internal/config"
)

// feedTables maps a --table value to the admin API endpoint that creates a
// row there. Add an entry here to make --feed support another table.
var feedTables = map[string]string{
	"limiters":     "/api/limiters",
	"model_prices": "/api/prices",
}

// runFeed posts row as-is to the admin endpoint for table, so that a running
// claude-lens instance can be seeded from the command line without going
// through its Admin UI. All field validation for row lives in the admin
// handler that receives it; runFeed only checks that table is known and row
// is syntactically valid JSON before making the request.
func runFeed(ctx context.Context, cfg config.Config, table, row string) error {
	endpoint, ok := feedTables[table]
	if !ok {
		names := make([]string, 0, len(feedTables))
		for name := range feedTables {
			names = append(names, name)
		}
		sort.Strings(names)
		return fmt.Errorf("unknown table %q, must be one of: %s", table, strings.Join(names, ", "))
	}
	if !json.Valid([]byte(row)) {
		return fmt.Errorf("--row is not valid JSON")
	}

	url := "http://" + dialAddr(cfg.AdminAddr) + endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(row)))
	if err != nil {
		return fmt.Errorf("build feed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach claude-lens admin API at %s (is the service running?): %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read admin API response: %w", err)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		fmt.Println(string(body))
		return nil
	}
	return fmt.Errorf("admin API rejected the row (status %d): %s", resp.StatusCode, string(body))
}

// dialAddr turns a Config.AdminAddr bind address (e.g. ":7802" or
// "0.0.0.0:7802") into an address a client on the same host can dial.
func dialAddr(bindAddr string) string {
	switch {
	case strings.HasPrefix(bindAddr, ":"):
		return "localhost" + bindAddr
	case strings.HasPrefix(bindAddr, "0.0.0.0:"):
		return "localhost" + strings.TrimPrefix(bindAddr, "0.0.0.0")
	default:
		return bindAddr
	}
}
