//go:build integration

package caldav_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"caldavproxy/internal/caldav"
	"caldavproxy/internal/config"
	"caldavproxy/internal/server"
	"caldavproxy/internal/store"
)

const (
	radicaleUser = "testuser"
	radicalePass = "testpass"
	calendarPath = "/testuser/test-calendar/"
)

// radicaleConfig is a minimal Radicale 3 config: htpasswd basic auth, owner-only
// rights, filesystem storage.
const radicaleConfig = `[server]
hosts = 0.0.0.0:5232

[auth]
type = htpasswd
htpasswd_filename = /config/users
htpasswd_encryption = plain

[rights]
type = owner_only

[storage]
filesystem_folder = /data/collections
`

// startRadicale boots a Radicale container with one authenticated user and
// returns its base URL. The container is terminated via t.Cleanup.
func startRadicale(t *testing.T, ctx context.Context) string {
	t.Helper()

	// The Ryuk resource reaper hangs in some Docker setups; we terminate the
	// container ourselves via t.Cleanup, so disabling it is safe.
	t.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")

	req := testcontainers.ContainerRequest{
		Image:        "tomsquest/docker-radicale:latest",
		ExposedPorts: []string{"5232/tcp"},
		Files: []testcontainers.ContainerFile{
			{Reader: strings.NewReader(radicaleConfig), ContainerFilePath: "/config/config", FileMode: 0o644},
			{Reader: strings.NewReader(radicaleUser + ":" + radicalePass + "\n"), ContainerFilePath: "/config/users", FileMode: 0o644},
		},
		WaitingFor: wait.ForAll(
			wait.ForListeningPort("5232/tcp"),
			wait.ForLog("Radicale server ready"),
		).WithStartupTimeout(90 * time.Second),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start radicale: %v", err)
	}
	t.Cleanup(func() {
		_ = container.Terminate(context.Background())
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := container.MappedPort(ctx, "5232")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}
	return fmt.Sprintf("http://%s:%s", host, port.Port())
}

// seedEvents creates the calendar collection and PUTs the given events, with
// retries to absorb the brief gap between "port open" and "app ready".
func seedEvents(t *testing.T, baseURL string, events map[string]string) {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}

	// MKCALENDAR the collection (idempotent: 405 if it already exists).
	deadline := time.Now().Add(45 * time.Second)
	for {
		req, _ := http.NewRequest("MKCALENDAR", baseURL+calendarPath, nil)
		req.SetBasicAuth(radicaleUser, radicalePass)
		resp, err := client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusConflict {
				break
			}
			t.Logf("MKCALENDAR -> %d: %s", resp.StatusCode, body)
		}
		if time.Now().After(deadline) {
			t.Fatalf("MKCALENDAR did not succeed in time (last err: %v)", err)
		}
		time.Sleep(time.Second)
	}

	for name, body := range events {
		req, _ := http.NewRequest(http.MethodPut, baseURL+calendarPath+name, strings.NewReader(body))
		req.SetBasicAuth(radicaleUser, radicalePass)
		req.Header.Set("Content-Type", "text/calendar")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("PUT %s: %v", name, err)
		}
		rb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
			t.Fatalf("PUT %s -> %d: %s", name, resp.StatusCode, rb)
		}
	}
}

// eventICS builds a one-event VCALENDAR object.
func eventICS(uid, summary string, start time.Time) string {
	stamp := func(tm time.Time) string { return tm.UTC().Format("20060102T150405Z") }
	return strings.Join([]string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//integration-test//EN",
		"BEGIN:VEVENT",
		"UID:" + uid,
		"DTSTAMP:" + stamp(time.Now()),
		"DTSTART:" + stamp(start),
		"DTEND:" + stamp(start.Add(time.Hour)),
		"SUMMARY:" + summary,
		"END:VEVENT",
		"END:VCALENDAR",
	}, "\r\n") + "\r\n"
}

func TestIntegrationFetchAndServe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	baseURL := startRadicale(t, ctx)

	seedEvents(t, baseURL, map[string]string{
		"ev1.ics": eventICS("event-one@test", "Standup", time.Now().Add(24*time.Hour)),
		"ev2.ics": eventICS("event-two@test", "Review", time.Now().Add(48*time.Hour)),
	})

	cfg := &config.Config{
		RemoteURL:         baseURL,
		Username:          radicaleUser,
		Password:          radicalePass,
		CalendarPath:      calendarPath,
		SecretPath:        "sekret",
		QueryWindowPast:   720 * time.Hour,
		QueryWindowFuture: 8760 * time.Hour,
	}

	client, err := caldav.New(cfg)
	if err != nil {
		t.Fatalf("caldav.New: %v", err)
	}

	body, err := client.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	t.Logf("fetched %d bytes:\n%s", len(body), body)

	for _, want := range []string{"Standup", "Review", "event-one@test", "event-two@test"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("feed is missing %q", want)
		}
	}

	// --- exercise the public HTTP layer with the fetched feed ---
	st := store.New()
	st.Set(body, time.Now().UTC())
	srv := server.New(cfg, st)
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	feedURL := ts.URL + cfg.FeedPath()

	t.Run("feed 200", func(t *testing.T) {
		resp, err := http.Get(feedURL)
		if err != nil {
			t.Fatalf("GET feed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/calendar") {
			t.Errorf("Content-Type = %q, want text/calendar", ct)
		}
		got, _ := io.ReadAll(resp.Body)
		if !bytes.Equal(got, body) {
			t.Errorf("served body differs from fetched feed")
		}
	})

	t.Run("wrong path 404", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/")
		if err != nil {
			t.Fatalf("GET /: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("conditional 304", func(t *testing.T) {
		etag := st.Get().ETag
		req, _ := http.NewRequest(http.MethodGet, feedURL, nil)
		req.Header.Set("If-None-Match", etag)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("conditional GET: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotModified {
			t.Errorf("status = %d, want 304", resp.StatusCode)
		}
	})
}
