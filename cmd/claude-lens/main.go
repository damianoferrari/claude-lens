// Command claude-lens runs the proxy and admin servers as goroutines in a
// single process, sharing one *database.DB, one status.Flag, and one
// status.Fresh.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/lfsc09/claude-lens/admin"
	"github.com/lfsc09/claude-lens/internal/config"
	"github.com/lfsc09/claude-lens/internal/database"
	"github.com/lfsc09/claude-lens/internal/logging"
	"github.com/lfsc09/claude-lens/internal/notify"
	"github.com/lfsc09/claude-lens/internal/pricing"
	"github.com/lfsc09/claude-lens/internal/status"
	"github.com/lfsc09/claude-lens/proxy"
)

func main() {
	feed := flag.Bool("feed", false, "seed a single row into a table via the running admin API, instead of starting the service")
	table := flag.String("table", "", "table to seed with --feed (limiters, model_prices)")
	row := flag.String("row", "", "row to insert with --feed, as a JSON object")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *feed {
		if err := runFeed(ctx, cfg, *table, *row); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	logging.Setup(cfg)

	db, err := database.Open(ctx, cfg.DBPath)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	db.SetNotifications(notify.NewClient())

	est := pricing.New(db)
	if err := est.Refresh(ctx); err != nil {
		slog.Error("failed to load model prices", "error", err)
		os.Exit(1)
	}

	st := status.New()
	fresh := status.NewFresh()
	limitersFresh := status.NewFresh()

	proxySrv, err := proxy.NewServer(cfg, db, est, st, fresh, limitersFresh)
	if err != nil {
		slog.Error("failed to build proxy server", "error", err)
		os.Exit(1)
	}

	adminSrv, err := admin.NewServer(db, est, st, fresh, limitersFresh, Version, cfg.DBPath, cfg.LogDir)
	if err != nil {
		slog.Error("failed to build admin server", "error", err)
		os.Exit(1)
	}

	slog.Info("starting", "version", Version, "proxy_addr", cfg.ProxyAddr, "admin_addr", cfg.AdminAddr)

	var proxyErr, adminErr error
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		// stop() also cancels ctx (see signal.NotifyContext), so if this
		// server exits for its own reason (e.g. a startup failure) before
		// a shutdown signal arrives, the other server unblocks and shuts
		// down too instead of running on its own indefinitely.
		defer stop()
		proxyErr = proxySrv.Run(ctx, cfg.ProxyAddr)
	}()
	go func() {
		defer wg.Done()
		defer stop()
		adminErr = adminSrv.Run(ctx, cfg.AdminAddr)
	}()
	go func() {
		defer wg.Done()
		db.RunLimiterRefreshLoop(ctx, limitersFresh)
	}()

	<-ctx.Done()
	slog.Info("shutdown signal received, shutting down")
	wg.Wait()

	exitCode := 0
	if proxyErr != nil {
		slog.Error("proxy server exited with error", "error", proxyErr)
		exitCode = 1
	}
	if adminErr != nil {
		slog.Error("admin server exited with error", "error", adminErr)
		exitCode = 1
	}
	os.Exit(exitCode)
}
