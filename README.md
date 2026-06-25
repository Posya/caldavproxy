# CalDav

Reads an **authenticated** upstream CalDAV calendar and re-publishes it as a
plain, **unauthenticated** iCalendar (`.ics`) feed at a secret URL path — for
services that can subscribe to a calendar only by an open link.

The feed is held in memory and refreshed periodically; on restart it is simply
re-read from upstream.

## How it works

```
upstream CalDAV (basic auth) ──poll──▶ in-memory .ics ──HTTP (no auth)──▶ /<secret>/calendar.ics
```

## Configuration

All settings come from environment variables (see [.env.example](.env.example)):

| Variable | Required | Default | Description |
|---|---|---|---|
| `CALDAV_REMOTE_URL` | yes | — | Upstream CalDAV endpoint |
| `CALDAV_USERNAME` | yes | — | Basic-auth user |
| `CALDAV_PASSWORD` | yes | — | Basic-auth password |
| `CALDAV_SECRET_PATH` | yes | — | Secret path segment that hides the feed |
| `CALDAV_CALENDAR_PATH` | no | (auto-discover) | Specific calendar collection path |
| `LISTEN_ADDR` | no | `:8080` | Public bind address |
| `POLL_INTERVAL` | no | `15m` | Upstream refresh interval |
| `QUERY_WINDOW_PAST` | no | `720h` | How far back to fetch events |
| `QUERY_WINDOW_FUTURE` | no | `8760h` | How far ahead to fetch events |
| `LOG_LEVEL` | no | `info` | Log verbosity: `debug` / `info` / `warn` / `error` |

The feed is served at `http://<host>/<CALDAV_SECRET_PATH>/calendar.ics`.
Generate a secret with e.g. `openssl rand -hex 16`. To "rotate" the URL, change
`CALDAV_SECRET_PATH` and restart.

### Logging

Structured logs (`slog`, text format) are written to **stdout**, so they are
captured by `docker logs` / your log collector. Set `LOG_LEVEL`:

- `info` (default) — lifecycle events, each refresh, one line per HTTP request
  (method, status, duration — **the request path is omitted** so the secret
  never appears in normal logs).
- `debug` — adds CalDAV discovery/query/merge details, source locations, and the
  full request path/remote address (use only when troubleshooting).
- `warn` / `error` — quieter.

## Make targets

```sh
make help               # list targets
make build              # compile binary into ./bin
make test               # unit tests (no Docker)
make test-integration   # integration tests (requires Docker)
make lint               # golangci-lint (falls back to go vet)
make docker-build       # build the Docker image
make image              # build + package image into dist/caldavproxy-latest.tar.gz
make clean              # remove build artifacts
```

Override the image name/tag, e.g. `make image TAG=v1.0.0`.

## Run with Docker

```sh
docker build -t caldavproxy .
docker run -d --name caldavproxy -p 8080:8080 --env-file .env caldavproxy
```

### Run with Docker Compose

Create `.env` from [.env.example](.env.example), then:

```sh
docker compose up -d        # builds the image and starts the service
docker compose logs -f      # follow logs
docker compose down         # stop and remove
```

## Deploy on another machine (no registry)

Build and package the image into a portable tarball on the build machine:

```sh
make image                  # produces dist/caldavproxy-latest.tar.gz
```

Copy it to the target machine and load it:

```sh
scp dist/caldavproxy-latest.tar.gz user@target:/tmp/
ssh user@target

# on the target machine:
docker load -i /tmp/caldavproxy-latest.tar.gz   # imports caldavproxy:latest
```

Then run it. With a prepared `.env` file:

```sh
docker run -d --name caldavproxy -p 8080:8080 --env-file .env caldavproxy:latest
```

Or via Compose — copy `docker-compose.yml` and `.env` to the target, comment out
the `build:` line in `docker-compose.yml` (so it uses the loaded image), and run
`docker compose up -d`.

The target machine needs only Docker installed — no Go toolchain or source.

## Run locally

```sh
go build ./...
CALDAV_REMOTE_URL=... CALDAV_USERNAME=... CALDAV_PASSWORD=... \
CALDAV_SECRET_PATH=$(openssl rand -hex 16) go run .
```

## Test

Unit tests (fast, no Docker):

```sh
go test ./...
```

Integration test — spins up a real [Radicale](https://radicale.org/) CalDAV
server in Docker via testcontainers, seeds events, and verifies the full
fetch → merge → serve path. Requires Docker:

```sh
go test -tags integration ./...
```
