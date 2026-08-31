// Package admin implements the admin server: a JSON API plus the static
// HTML/JS frontend that consumes it, over the same exchanges/model_prices
// data.
package admin

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/lfsc09/claude-lens/internal/database"
	"github.com/lfsc09/claude-lens/internal/logging"
	"github.com/lfsc09/claude-lens/internal/pricing"
	"github.com/lfsc09/claude-lens/internal/status"
)

//go:embed static
var staticFS embed.FS

// Server runs the admin HTTP listener.
type Server struct {
	handler http.Handler
	logger  *slog.Logger
}

// NewServer builds a Server with the JSON API and static frontend routes
// registered. It only fails if the embedded static filesystem can't be
// rooted at "static" — a build-time invariant, not a runtime condition —
// so that's treated as fatal rather than something the caller can
// meaningfully recover from at startup.
func NewServer(db *database.DB, est *pricing.Estimator, st *status.Flag, fr *status.Fresh, limitersFresh *status.Fresh, version, dbPath, logDir string) (*Server, error) {
	logger := slog.Default().With("component", "admin")
	h := &handlers{db: db, est: est, status: st, fresh: fr, limitersFresh: limitersFresh, logger: logger, version: version, dbPath: dbPath, logPath: logging.FilePath(logDir)}

	staticContent, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, fmt.Errorf("sub static fs: %w", err)
	}
	fileServer := http.FileServerFS(staticContent)

	mux := http.NewServeMux()

	// Static frontend: plain HTML/JS files, embedded above via staticFS.
	// Pages are served under clean paths (no .html extension); only
	// /exchanges/{id} needs an explicit handler since its id is a path
	// variable, not a real file.
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFileFS(w, r, staticContent, "dashboard.html")
	})
	mux.HandleFunc("GET /exchanges", servePage(staticContent, "exchanges.html"))
	mux.HandleFunc("GET /exchanges/{id}", servePage(staticContent, "exchange.html"))
	mux.HandleFunc("GET /prices", servePage(staticContent, "prices.html"))
	mux.HandleFunc("GET /limiters", servePage(staticContent, "limiters.html"))
	mux.HandleFunc("GET /about", servePage(staticContent, "about.html"))
	mux.HandleFunc("GET /favicon.ico", servePage(staticContent, "img/favicon.ico"))
	mux.Handle("GET /img/", fileServer)
	mux.Handle("GET /js/", fileServer)

	// JSON API
	mux.HandleFunc("GET /api/health", h.health)
	mux.HandleFunc("GET /api/exchanges", h.listExchanges)
	mux.HandleFunc("GET /api/exchanges/{id}", h.exchangeDetail)
	mux.HandleFunc("DELETE /api/exchanges", h.deleteExchanges)
	mux.HandleFunc("GET /api/totals", h.totals)
	mux.HandleFunc("GET /api/session-stats", h.sessionStats)
	mux.HandleFunc("GET /api/daily-costs", h.dailyCosts)
	mux.HandleFunc("GET /api/stream", h.sseStream)
	mux.HandleFunc("GET /api/prices", h.listPrices)
	mux.HandleFunc("POST /api/prices", h.createPrice)
	mux.HandleFunc("PUT /api/prices/{id}", h.updatePrice)
	mux.HandleFunc("DELETE /api/prices/{id}", h.deletePrice)
	mux.HandleFunc("GET /api/limiters", h.listLimiters)
	mux.HandleFunc("POST /api/limiters", h.createLimiter)
	mux.HandleFunc("PUT /api/limiters/{id}", h.updateLimiter)
	mux.HandleFunc("PATCH /api/limiters/{id}/active", h.setLimiterActive)
	mux.HandleFunc("DELETE /api/limiters/{id}", h.deleteLimiter)
	mux.HandleFunc("GET /api/about", h.about)

	handler := chain(mux, recoverMiddleware, loggingMiddleware(logger))
	return &Server{handler: handler, logger: logger}, nil
}

// servePage returns a handler that always serves the same static file,
// regardless of any path variables in the route (e.g. /exchanges/{id}).
func servePage(staticContent fs.FS, file string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, staticContent, file)
	}
}

// Run serves on addr until ctx is done, then gracefully shuts down.
func (s *Server) Run(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:    addr,
		Handler: s.handler,
		// No WriteTimeout: the /api/stream SSE route holds its connection
		// open indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("admin server listening", "addr", addr)
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.logger.Info("admin server shutting down")
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}
