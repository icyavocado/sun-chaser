# sunchaser — specification

## Purpose

A Go+HTMX web application that uses the `calcbright` library to compare
clear-sky and OWM-cloud-adjusted display brightness recommendations side by
side, and accumulates historical analysis results in a local SQLite database
to track how the two models diverge over time for any given location.


## Tech stack

| Layer       | Choice                                        |
|-------------|-----------------------------------------------|
| Backend     | Go `net/http` + `chi` router                  |
| Templating  | `html/template` (stdlib)                      |
| Frontend    | HTMX (CDN), Chart.js (CDN), Pico.css v2 (CDN)|
| Dark mode   | `data-theme="auto"` — follows system pref     |
| Database    | SQLite via `modernc.org/sqlite` (pure Go)     |
| Geocoding   | Photon by Komoot (backend-proxied, no key)    |
| Brightness  | `calcbright/brightness` via replace directive |


## Configuration (environment variables)

| Variable                  | Default          | Description                          |
|---------------------------|------------------|--------------------------------------|
| `OPENWEATHERMAP_API_KEY`  | *(required)*     | OWM server-side API key              |
| `PORT`                    | `8080`           | HTTP listen port                     |
| `DB_PATH`                 | `./sunchaser.db` | SQLite file path                     |
| `PHOTON_BASE_URL`         | `https://photon.komoot.io` | Override for testing       |

### .env files

On startup `main.go` calls `loadEnvFile(".env.development")` — a tiny
inline KEY=VALUE parser (no external dependency). Variables already set in
the real environment are never overwritten, so production deployments are
unaffected.

| File                | Committed | Purpose                                    |
|---------------------|-----------|--------------------------------------------|
| `.env.example`      | Yes       | Template showing which variables are needed|
| `.env.development`  | No        | Local dev values (gitignored via `.env.*`) |

`.gitignore` rule: `.env.*` ignores all env files; `!.env.example` carves
out the template so it remains tracked.


## Go module

```
module github.com/icyavocado/sun-chaser

require (
    github.com/go-chi/chi/v5
    modernc.org/sqlite
    github.com/icyavocado/calcbright
)

replace github.com/icyavocado/calcbright => ../calcbright
```


## File structure

```
sun-chaser/
├── go.mod
├── go.sum
├── sunchaser.spec
├── main.go                       # Server init, routes, env config, DB migration
├── db/
│   └── db.go                     # SQLite open, schema, query helpers
├── handlers/
│   ├── handlers.go               # Handler struct + constructor
│   ├── ratelimit.go              # Sliding-window per-IP rate limiter
│   ├── geocode.go                # GET /geocode — Photon proxy (rate limited)
│   ├── analyze.go                # POST /analyze — run calcbright, store in DB
│   └── history.go                # GET /history — JSON historical data
├── templates/
│   ├── layout.html               # Base: <head> with CDN links, body wrapper
│   ├── index.html                # Main page: form + empty #results slot
│   └── partials/
│       ├── suggestions.html      # Autocomplete <ul> dropdown (HTMX partial)
│       ├── results.html          # Comparator cards + chart canvas (HTMX partial)
│       └── empty.html            # Empty state placeholder
└── static/
    └── style.css                 # Pico.css minor overrides
```


## Routes

| Method | Path               | Description                                          |
|--------|--------------------|------------------------------------------------------|
| GET    | `/`                | Full page with input form                            |
| GET    | `/geocode?q=`      | Photon proxy; returns suggestions partial (HTMX)     |
| POST   | `/analyze`         | Runs both models, stores in DB, returns results partial |
| GET    | `/history?lat=&lon=` | JSON array of historical analyses for a location  |
| GET    | `/static/*`        | Serves CSS                                           |


## Location autocomplete

- Input triggers `GET /geocode?q=<text>` via HTMX
- Trigger condition: `keyup[this.value.length > 2] delay:500ms`
  - Only fires after the 3rd character is typed
  - 500ms debounce prevents requests on every keystroke
- Backend proxies to `https://photon.komoot.io/api/?q=<text>&limit=5&lang=en`
- Returns `partials/suggestions.html` — a `<ul>` of up to 5 place buttons
- Clicking a suggestion populates hidden `lat`, `lon`, `place_name` inputs
  and clears the dropdown
- Rate limit on `/geocode`: **10 requests/min per IP** (sliding window)
  — exceeding returns HTTP 429 with a plain-text message


## SQLite schema

```sql
CREATE TABLE IF NOT EXISTS analyses (
    id                         INTEGER PRIMARY KEY AUTOINCREMENT,
    place_name                 TEXT    NOT NULL,
    lat                        REAL    NOT NULL,
    lon                        REAL    NOT NULL,
    analyzed_at                DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    -- Clear-sky model
    clear_sky_nits             REAL,
    clear_perceived_nits       REAL,
    clear_reflected_nits       REAL,
    clear_contrast_ratio       REAL,
    clear_glare                INTEGER,   -- 0/1 boolean
    -- OWM-adjusted model
    owm_nits                   REAL,
    owm_perceived_nits         REAL,
    owm_reflected_nits         REAL,
    owm_contrast_ratio         REAL,
    owm_glare                  INTEGER,   -- 0/1 boolean
    owm_cloud_fraction         REAL,
    -- Shared solar context
    sun_zenith_deg             REAL
);

CREATE INDEX IF NOT EXISTS idx_analyses_location
    ON analyses (ROUND(lat, 2), ROUND(lon, 2));
```

Per-location queries round lat/lon to 2 decimal places (~1 km grid) to
group nearby searches together.


## Results UI layout

```
┌──────────────────────────┬──────────────────────────┐
│   Clear-sky model        │   OWM-adjusted           │
│                          │                          │
│  Recommended: 187 nits   │  Recommended: 142 nits   │
│  Perceived:   208 nits   │  Perceived:   163 nits   │
│  Reflected:    21 nits   │  Reflected:    21 nits   │
│  Contrast:    14.2       │  Contrast:    10.8       │
│  Glare:       No         │  Glare:       No         │
│                          │  Cloud cover: 34%        │
└──────────────────────────┴──────────────────────────┘

   Brightness history for <place name>
   [Chart.js line chart — 2 lines: Clear sky / With clouds]
   [X-axis: analyzed_at timestamp of each stored analysis]
   [Y-axis: recommended nits]
```

Chart shows historical readings for the **currently selected location**,
grouped by ROUND(lat,2) / ROUND(lon,2). Current run is included after save.


## Rate limiter implementation

- Sliding-window counter, in-process, per-IP (reads `X-Forwarded-For` or
  `RemoteAddr`)
- Window: 60 seconds, limit: 10 requests
- A background goroutine cleans up stale IP entries every 60 seconds
- HTTP 429 response body: `rate limit exceeded — try again shortly`


## calcbright integration

Both analyses share the same `time.Now()` snapshot. The clear-sky run calls
`brightness.AnalyzeWithValues` (passing 0,0,0 irradiance so it falls back to
the internal clear-sky model). The OWM run creates a shared `OWMClient` at
server startup and calls `GetCurrent` directly to obtain the cloud fraction,
then calls `ApplyCloudAttenuation` + `AnalyzeWithValues`, so the same cached
OWM response is reused for both paths within the TTL window.


## Chart.js integration

After POST /analyze succeeds, the results partial includes:
- A `<canvas id="brightness-chart">` element
- An inline `<script>` that fetches `GET /history?lat=&lon=` and initialises
  a Chart.js line chart with two datasets:
  - "Clear sky" — `clear_sky_nits` per analysis row
  - "With clouds (OWM)" — `owm_nits` per analysis row
- X-axis labels are `analyzed_at` timestamps
- Y-axis label: "Nits"
- If fewer than 2 historical rows exist the canvas is hidden and a message
  shown instead

## Pico.css

- Version: v2 (CDN)
- Theme attribute: `data-theme="auto"` on `<html>` — dark on dark-themed
  systems, light on light-themed systems
- `static/style.css` provides minor overrides: autocomplete dropdown
  positioning and z-index, card grid gap, chart canvas sizing
