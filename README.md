sun-chaser
===========

A small Go + HTMX web app that compares a clear-sky brightness model (calcbright) with OpenWeatherMap-adjusted brightness and provides simple solar panel production estimates. Results are stored in SQLite and a background worker collects history for watched locations.

This README documents how to run and configure the application, the background-worker behaviour (including the 60-day pause rule), and the important runtime defaults.

Features
- Clear-sky vs OWM-adjusted brightness comparisons (UI at `/`)
- Solar production estimates and simple economics (UI at `/solar`)
- Background worker that collects historical analyses for watched locations
- Pause inactive locations after a configurable watch window (default 60 days)
- Request spacing inside the worker and a circuit breaker for OWM failures
- Per-endpoint per-IP sliding-window rate limits (geocode, analyze, solar)
- Minimal security headers and a sensible CSP for CDN-hosted assets

Quick start (development)
- Requirements: `mise` (project shim) or Go toolchain, SQLite, Docker (optional)
- Build and run the dev server (example):

```bash
# build + start with sensible test env
OPENWEATHERMAP_API_KEY=your_real_key_here \
  WATCH_WINDOW=1440h \
  WORKER_REQUEST_DELAY=2s \
  TRUST_PROXY=1 \
  bash dev-server.sh
```

- The server listens on `:8080` by default; set `PORT` to change.
- The dev server compiles to `./.sun-chaser-bin` and writes its PID to `.server.pid`.

Docker (deploy)
- A multi-stage Dockerfile and docker-compose.yml are included for container deployment. The compose file is intended for Portainer-style deployment and the Dockerfile uses the parent directory as build context (so the sibling `calcbright` module is available).
- To build and run with Compose:

```bash
docker compose up -d --build
```

Important environment variables
- OPENWEATHERMAP_API_KEY - required. Your OpenWeatherMap API key.
- OPENWEATHERMAP_BASE_URL - optional override for OWM base URL.
- PHOTON_BASE_URL - Photon (autocomplete) base URL. Default: `https://photon.komoot.io`.
- DB_PATH - SQLite file. Default: `./sunchaser.db`.
- COLLECT_INTERVAL - worker tick interval. Default: `30m`.
- WATCH_WINDOW - how long since last user request before the worker pauses a location. Default: `1440h` (60 days).
- WORKER_REQUEST_DELAY - pause between OWM calls within a worker batch (duration). Default: `2s`.
- TRUST_PROXY - set to `1` or `true` when running behind Traefik/Nginx so X-Real-IP / X-Forwarded-For are trusted for rate-limiting. Default: disabled.
- PORT - HTTP listen port. Default: `8080`.

Notes on environment loading
- `.env.development` is loaded by `main.go` if present. Real environment variables always take precedence. `.env.*` files are gitignored except `!.env.example`.

Background worker behaviour
- The worker collects brightness and solar analyses for all watched locations. It runs immediately on startup and then on each `COLLECT_INTERVAL` tick.
- Watched locations are registered when a user requests `/analyze` or `/solar/analyze`. Each registration updates `watched_locations.last_requested_at`.
- The worker only collects for locations where `last_requested_at` is within `WATCH_WINDOW`. Rows with NULL `last_requested_at` (pre-migration) are treated as active until they naturally age out.
- The worker inserts `WORKER_REQUEST_DELAY` between consecutive OWM calls to avoid bursting the OpenWeatherMap free-tier rate limit.
- If every OWM call in a full batch fails, the worker increments a counter. After 3 consecutive full-batch failures the worker opens a circuit breaker and skips collection cycles using exponential backoff (skip 1, then 2, then 4 ticks...). The breaker resets on any successful OWM call.

Rate limiting
- The server applies simple sliding-window per-IP rate limits:
- `/geocode` — 10 requests/minute per IP
- `/analyze` and `/solar/analyze` — 5 requests/minute per IP

OpenWeatherMap considerations
- The application uses the OWM Current Weather API to adjust cloud fraction. The free tier is limited (60 calls/min, 1000/day), so the worker spacing, per-IP analyze limiter, and OWM client-cache are the primary defenses.
- The OWM cache key was simplified to `lat:lon` (previously included a time slot) to avoid a stampede when slots rolled over. The cache still honors TTL (default 10 minutes).

Database & migrations
- The app uses SQLite (via `modernc.org/sqlite`). The database path is controlled by `DB_PATH`.
- On startup the app runs migrations. A `last_requested_at` DATETIME column was added to `watched_locations` so the worker can pause stale locations.
- Existing rows created before this change will have `NULL` in `last_requested_at` and are treated as active until the watch window expires. To backfill `last_requested_at` for existing rows (make everything active immediately):

```bash
sqlite3 ./sunchaser.db "UPDATE watched_locations SET last_requested_at = CURRENT_TIMESTAMP WHERE last_requested_at IS NULL;"
```

Inspecting the DB
- Example: show watched locations and last-request timestamps:

```bash
sqlite3 ./sunchaser.db "SELECT place_name, last_requested_at FROM watched_locations;"
```

Security
- The app sets several security headers by default (Content-Security-Policy, X-Content-Type-Options, X-Frame-Options, Referrer-Policy). HSTS is intentionally omitted because TLS is expected to be handled by a reverse proxy (Traefik/Nginx).
- CSP permits `unpkg.com` (HTMX) and `cdn.jsdelivr.net` (Pico.css / Chart.js) and allows `'unsafe-inline'` for inline `<script>` blocks used in templates.

Troubleshooting
- Dev server PID: `.server.pid` when using `dev-server.sh`.
- Check if the server is listening: `ss -tlnp | rg :8080`.
- Run the server in foreground to see logs directly (helpful for debugging env/migration errors):

```bash
OPENWEATHERMAP_API_KEY=yourkey mise exec -- go build -o ./.sun-chaser-bin . && ./ ./.sun-chaser-bin
```

- Check SQLite directly with the `sqlite3` client.

Development notes
- The calcbright module is included as a sibling module via a `replace` directive in `go.mod`. The Dockerfile uses the parent directory as build context so both modules are available during the image build.
- Templates are pre-compiled by the server at startup. HTMX partials are served standalone.

