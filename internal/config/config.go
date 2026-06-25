// Package config loads and validates the application configuration from the
// environment.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// Config holds all runtime settings, sourced entirely from environment
// variables so the app stays 12-factor friendly.
type Config struct {
	// RemoteURL is the base endpoint of the upstream (authenticated) CalDAV server.
	RemoteURL string
	// Username and Password are the basic-auth credentials for the upstream server.
	Username string
	Password string
	// CalendarPath, if set, points directly at a calendar collection and skips
	// discovery. When empty, the client discovers the calendar automatically.
	CalendarPath string

	// SecretPath is the opaque path segment that hides the public feed,
	// e.g. "7f3a9c". The feed is served at /<SecretPath>/calendar.ics.
	SecretPath string

	// ListenAddr is the address the public HTTP server binds to.
	ListenAddr string
	// PollInterval is how often the upstream calendar is re-read into memory.
	PollInterval time.Duration

	// QueryWindowPast / QueryWindowFuture bound the time range of the CalDAV
	// query (many servers reject open-ended queries).
	QueryWindowPast   time.Duration
	QueryWindowFuture time.Duration

	// LogLevel is the minimum slog level emitted (debug|info|warn|error).
	LogLevel slog.Level
}

// Load reads the configuration from the environment, applying defaults and
// validating required values. It returns an error listing every problem found
// so misconfiguration can be fixed in one pass.
func Load() (*Config, error) {
	cfg := &Config{
		RemoteURL:         strings.TrimSpace(os.Getenv("CALDAV_REMOTE_URL")),
		Username:          os.Getenv("CALDAV_USERNAME"),
		Password:          os.Getenv("CALDAV_PASSWORD"),
		CalendarPath:      strings.TrimSpace(os.Getenv("CALDAV_CALENDAR_PATH")),
		SecretPath:        strings.Trim(strings.TrimSpace(os.Getenv("CALDAV_SECRET_PATH")), "/"),
		ListenAddr:        envOr("LISTEN_ADDR", ":8080"),
		PollInterval:      15 * time.Minute,
		QueryWindowPast:   720 * time.Hour,  // ~30 days
		QueryWindowFuture: 8760 * time.Hour, // ~365 days
		LogLevel:          slog.LevelInfo,
	}

	var problems []string

	if cfg.RemoteURL == "" {
		problems = append(problems, "CALDAV_REMOTE_URL is required")
	}
	if cfg.Username == "" {
		problems = append(problems, "CALDAV_USERNAME is required")
	}
	if cfg.Password == "" {
		problems = append(problems, "CALDAV_PASSWORD is required")
	}
	if cfg.SecretPath == "" {
		problems = append(problems, "CALDAV_SECRET_PATH is required")
	} else if strings.ContainsAny(cfg.SecretPath, "/?#") {
		problems = append(problems, "CALDAV_SECRET_PATH must be a single URL-safe path segment (no / ? #)")
	}

	if v := strings.TrimSpace(os.Getenv("POLL_INTERVAL")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			problems = append(problems, fmt.Sprintf("POLL_INTERVAL is not a valid duration: %v", err))
		} else if d <= 0 {
			problems = append(problems, "POLL_INTERVAL must be positive")
		} else {
			cfg.PollInterval = d
		}
	}
	if v := strings.TrimSpace(os.Getenv("QUERY_WINDOW_PAST")); v != "" {
		if d, err := time.ParseDuration(v); err != nil {
			problems = append(problems, fmt.Sprintf("QUERY_WINDOW_PAST is not a valid duration: %v", err))
		} else {
			cfg.QueryWindowPast = d
		}
	}
	if v := strings.TrimSpace(os.Getenv("QUERY_WINDOW_FUTURE")); v != "" {
		if d, err := time.ParseDuration(v); err != nil {
			problems = append(problems, fmt.Sprintf("QUERY_WINDOW_FUTURE is not a valid duration: %v", err))
		} else {
			cfg.QueryWindowFuture = d
		}
	}
	if v := strings.TrimSpace(os.Getenv("LOG_LEVEL")); v != "" {
		lvl, err := parseLevel(v)
		if err != nil {
			problems = append(problems, fmt.Sprintf("LOG_LEVEL %v", err))
		} else {
			cfg.LogLevel = lvl
		}
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}

// FeedPath returns the public path the calendar feed is served at.
func (c *Config) FeedPath() string {
	return "/" + c.SecretPath + "/calendar.ics"
}

// parseLevel maps a human-readable level name to a slog.Level.
func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("is invalid: %q (use debug|info|warn|error)", s)
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
