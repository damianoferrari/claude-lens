package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSend_PostsTextAsJSON(t *testing.T) {
	var gotMethod, gotContentType string
	var gotBody map[string]string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := NewClient()
	if err := c.Send(context.Background(), server.URL, "hello world"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want %q", gotMethod, http.MethodPost)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotBody["text"] != "hello world" {
		t.Errorf(`body["text"] = %q, want "hello world"`, gotBody["text"])
	}
}

// TestSend_BlankURLIsNoop confirms a blank webhookURL never makes a network
// call, so callers (e.g. accrueLimiterCost) can invoke Send unconditionally
// on a limiter/config with no webhook configured.
func TestSend_BlankURLIsNoop(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()

	c := NewClient()
	if err := c.Send(context.Background(), "", "hello"); err != nil {
		t.Fatalf("Send with blank URL returned error: %v", err)
	}
	if called {
		t.Error("Send with blank URL made an HTTP request, want none")
	}
}

func TestSend_NonSuccessStatusReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	c := NewClient()
	if err := c.Send(context.Background(), server.URL, "hello"); err == nil {
		t.Error("Send: got nil error for a 500 response, want an error")
	}
}
