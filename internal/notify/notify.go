// Package notify sends Slack incoming-webhook notifications for cost alerts.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Client posts messages to Slack incoming webhooks with a bounded timeout.
type Client struct {
	http *http.Client
}

// NewClient builds a Client with a 5s request timeout — generous for
// Slack's webhook endpoint without risking a stuck request piling up behind
// proxied traffic, since callers always dispatch Send in its own goroutine.
func NewClient() *Client {
	return &Client{http: &http.Client{Timeout: 5 * time.Second}}
}

// Send posts text as a Slack message to webhookURL. A blank webhookURL is a
// no-op so callers can pass an unconfigured target unconditionally.
func (c *Client) Send(ctx context.Context, webhookURL, text string) error {
	if webhookURL == "" {
		return nil
	}

	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return fmt.Errorf("marshal slack payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build slack request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("post slack webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("slack webhook returned status %d", resp.StatusCode)
	}
	return nil
}
