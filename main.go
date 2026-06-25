// Command caldavproxy reads an authenticated upstream CalDAV calendar and
// re-publishes it as a plain, unauthenticated iCalendar (.ics) feed at a secret
// path, refreshing periodically.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"caldavproxy/internal/caldav"
	"caldavproxy/internal/config"
	"caldavproxy/internal/server"
	"caldavproxy/internal/store"
)

func main() {
	// Bootstrap logger (info level) so configuration errors are reported
	// before the configured level is known.
	slog.SetDefault(newLogger(slog.LevelInfo))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	// Re-install the logger at the configured level.
	slog.SetDefault(newLogger(cfg.LogLevel))

	slog.Info("starting caldavproxy",
		"listenAddr", cfg.ListenAddr,
		"feedPath", cfg.FeedPath(),
		"pollInterval", cfg.PollInterval,
		"logLevel", cfg.LogLevel,
	)
	slog.Debug("effective configuration",
		"remoteURL", cfg.RemoteURL,
		"username", cfg.Username,
		"calendarPath", orAuto(cfg.CalendarPath),
		"queryWindowPast", cfg.QueryWindowPast,
		"queryWindowFuture", cfg.QueryWindowFuture,
	)

	client, err := caldav.New(cfg)
	if err != nil {
		slog.Error("failed to create caldav client", "error", err)
		os.Exit(1)
	}

	st := store.New()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Refresh the feed in the background; the HTTP server keeps serving the
	// last good snapshot across transient upstream failures.
	go poll(ctx, client, st, cfg.PollInterval)

	srv := server.New(cfg, st)
	go func() {
		slog.Info("http server listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server failed", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutdown signal received, stopping")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("graceful shutdown failed", "error", err)
	}
	slog.Info("stopped")
}

// newLogger builds a text slog logger writing to stdout at the given level.
// Source locations are included at debug level to aid troubleshooting.
func newLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level:     level,
		AddSource: level <= slog.LevelDebug,
	}))
}

func orAuto(s string) string {
	if s == "" {
		return "(auto-discover)"
	}
	return s
}

// poll refreshes the store immediately and then on every tick until ctx is done.
func poll(ctx context.Context, client *caldav.Client, st *store.Store, interval time.Duration) {
	refresh := func(trigger string) {
		start := time.Now()
		slog.Debug("refresh starting", "trigger", trigger)

		fetchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()

		body, err := client.Fetch(fetchCtx)
		if err != nil {
			slog.Error("refresh failed, keeping previous snapshot",
				"trigger", trigger, "error", err, "duration", time.Since(start))
			return
		}
		st.Set(body, time.Now().UTC())
		slog.Info("refresh ok", "bytes", len(body), "duration", time.Since(start))
	}

	refresh("startup")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Debug("poller stopping")
			return
		case <-ticker.C:
			refresh("tick")
		}
	}
}
