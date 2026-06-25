// Package server exposes the cached calendar feed over plain HTTP at a secret
// path, with no authentication.
package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"time"

	"caldavproxy/internal/config"
	"caldavproxy/internal/store"
)

// New builds the HTTP server. Only the secret feed path is routed; every other
// request gets a bare 404 so the feed's existence is not advertised.
func New(cfg *config.Config, st *store.Store) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+cfg.FeedPath(), feedHandler(st))

	return &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// feedHandler serves the latest rendered .ics snapshot with caching validators.
func feedHandler(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snap := st.Get()
		if !snap.OK {
			slog.Warn("feed requested before first successful fetch", "status", http.StatusServiceUnavailable)
			http.Error(w, "calendar not ready", http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
		w.Header().Set("Content-Disposition", `inline; filename="calendar.ics"`)
		w.Header().Set("ETag", snap.ETag)
		w.Header().Set("Cache-Control", "no-cache")

		slog.Debug("serving feed",
			"bytes", len(snap.Body),
			"etag", snap.ETag,
			"lastModified", snap.LastModified,
			"ifNoneMatch", r.Header.Get("If-None-Match"))

		// ServeContent handles conditional requests (If-None-Match,
		// If-Modified-Since) and range requests against the in-memory body.
		http.ServeContent(w, r, "calendar.ics", snap.LastModified, bytes.NewReader(snap.Body))
	}
}

// logRequests logs every request. At info level the path is omitted so the
// secret segment never lands in normal logs; at debug level the full path,
// remote address and user agent are included to aid troubleshooting.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		dur := time.Since(start)

		if slog.Default().Enabled(r.Context(), slog.LevelDebug) {
			slog.Debug("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.bytes,
				"duration", dur,
				"remote", r.RemoteAddr,
				"userAgent", r.UserAgent())
		} else {
			slog.Info("http request",
				"method", r.Method,
				"status", rec.status,
				"bytes", rec.bytes,
				"duration", dur)
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}
