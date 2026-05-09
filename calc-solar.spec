# calc-solar — Solar Power Estimation Feature Specification

## Purpose

A dedicated `/solar` page that estimates solar panel power output using the
same `calcbright` irradiance primitives used by the brightness feature.
The page provides:
- Instantaneous POA (Plane of Array) irradiance in W/m²
- Estimated panel power output in watts
- Estimated daily energy yield in kWh
- Clear-sky vs. OWM-cloud-adjusted comparison
- Hourly power profile chart for the current day (recomputed on demand)
- Historical daily kWh chart (stored, polled every 120 s)


## Architecture

Solar is a fully separate route (`/solar`). It has its own:
- Page, form, templates
- DB table (`solar_analyses`)
- Handler file (`handlers/solar.go`)
- Package (`solar/solar.go`)

The `analyses` table is **not modified**. Solar results live exclusively in
`solar_analyses`.

The solar package imports `calcbright/brightness` and reuses:
- `SunPosition` — solar azimuth and zenith angles
- `ClearSkyIrradiance` — DNI, DHI, GHI in W/m²
- `ApplyCloudAttenuation` — cloud-adjusted DNI, DHI, GHI
- `IlluminanceOnTilt` — POA irradiance (called with `luminousEfficacy=1.0`
  to return W/m² instead of lux)


## Routes

| Method | Path                       | Description                                      |
|--------|----------------------------|--------------------------------------------------|
| GET    | `/solar`                   | Full solar page with form                        |
| POST   | `/solar/analyze`           | Run estimate, store in DB, return results partial|
| GET    | `/solar-chart?lat=&lon=&place=` | Historical kWh chart partial (HTMX)         |


## Panel model — `solar` package

### Types

```go
// Panel describes a photovoltaic panel installation.
type Panel struct {
    AreaM2     float64 // panel area in m² (e.g. 1.7)
    Efficiency float64 // conversion efficiency 0-1 (e.g. 0.18 = 18 %)
    PR         float64 // performance ratio 0-1 (e.g. 0.80)
    AzDeg      float64 // panel azimuth, degrees clockwise from North
    TiltDeg    float64 // panel tilt, degrees from horizontal
}

// HourPoint is one hour's irradiance and power for the daily profile chart.
type HourPoint struct {
    Hour        int     // 0–23 UTC
    ClearPOA    float64 // W/m² clear-sky POA
    OWMPOA      float64 // W/m² cloud-attenuated POA
    ClearPowerW float64 // W clear-sky output
    OWMPowerW   float64 // W cloud-attenuated output
}

// Report holds the result of a solar estimate call.
type Report struct {
    // Instantaneous (at time t)
    ClearPOA    float64 // W/m²
    OWMPOA      float64 // W/m²
    ClearPowerW float64 // W
    OWMPowerW   float64 // W
    // Daily totals for the calendar day containing t (UTC)
    ClearDailyKWh float64
    OWMDailyKWh   float64
    // 24-point hourly profile (index = hour 0-23 UTC)
    HourlyProfile []HourPoint
}
```

### Estimate function

```go
func Estimate(t time.Time, loc brightness.Location, panel Panel,
    cloudFrac float64, albedo float64) (Report, error)
```

**Algorithm:**

1. Compute sun position and clear-sky irradiance at `t`:
   ```
   sunAz, sunZen = SunPosition(t, loc)
   dni, dhi, ghi = ClearSkyIrradiance(t, loc)
   ```

2. Compute cloud-attenuated irradiance:
   ```
   dniA, dhiA, ghiA = ApplyCloudAttenuation(dni, dhi, ghi, cloudFrac)
   ```

3. Compute POA via `IlluminanceOnTilt` with `luminousEfficacy = 1.0`:
   ```
   _, _, _, clearPOA = IlluminanceOnTilt(dni, dhi, ghi,
       sunAz, sunZen, panel.AzDeg, panel.TiltDeg, albedo, 1.0)

   _, _, _, owmPOA = IlluminanceOnTilt(dniA, dhiA, ghiA,
       sunAz, sunZen, panel.AzDeg, panel.TiltDeg, albedo, 1.0)
   ```

4. Compute instantaneous power:
   ```
   clearPowerW = clearPOA × panel.AreaM2 × panel.Efficiency × panel.PR
   owmPowerW   = owmPOA  × panel.AreaM2 × panel.Efficiency × panel.PR
   ```

5. Compute daily profile (24 iterations, pure math, ~microseconds):
   - For each hour h in 0–23:
     - `tH = start_of_UTC_day(t) + h * time.Hour`
     - Compute `ClearSkyIrradiance(tH, loc)`
     - Compute attenuated values using the same `cloudFrac`
     - Compute both POA values and both power values
     - Append `HourPoint{Hour: h, ClearPOA, OWMPOA, ClearPowerW, OWMPowerW}`
   - Daily kWh = sum(hourly watts) / 1000  (each point = 1 Wh → /1000 → kWh)

**Note:** `cloudFrac` is held constant for the entire daily profile (one OWM
fetch per estimate call, same as the brightness feature).


## SQLite schema

```sql
CREATE TABLE IF NOT EXISTS solar_analyses (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    place_name          TEXT    NOT NULL,
    lat                 REAL    NOT NULL,
    lon                 REAL    NOT NULL,
    analyzed_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    panel_area          REAL,
    panel_efficiency    REAL,
    panel_pr            REAL,
    panel_az            REAL,
    panel_tilt          REAL,
    clear_poa_wm2       REAL,
    clear_power_w       REAL,
    clear_daily_kwh     REAL,
    owm_poa_wm2         REAL,
    owm_power_w         REAL,
    owm_daily_kwh       REAL,
    cloud_fraction      REAL,
    sun_zenith_deg      REAL,
    calcbright_version  TEXT,
    owm_observation_id  INTEGER
);

CREATE INDEX IF NOT EXISTS idx_solar_analyses_location
    ON solar_analyses (ROUND(lat, 2), ROUND(lon, 2));
```

Per-location queries use `ROUND(lat,2)` / `ROUND(lon,2)` — same ~1 km grid
as the `analyses` table.


## DB methods

### InsertSolar

```go
type SolarInsertParams struct {
    PlaceName         string
    Lat               float64
    Lon               float64
    PanelArea         float64
    PanelEfficiency   float64
    PanelPR           float64
    PanelAz           float64
    PanelTilt         float64
    ClearPOAWm2       float64
    ClearPowerW       float64
    ClearDailyKWh     float64
    OWMPOAWm2         float64
    OWMPowerW         float64
    OWMDailyKWh       float64
    CloudFraction     float64
    SunZenithDeg      float64
    CalcbrightVersion string
    OWMObservationID  int64
}

func (db *DB) InsertSolar(p SolarInsertParams) (int64, error)
```

### ForLocationSolar

```go
type SolarRow struct {
    ID              int64
    PlaceName       string
    Lat, Lon        float64
    AnalyzedAt      time.Time
    ClearDailyKWh   float64
    OWMDailyKWh     float64
}

func (db *DB) ForLocationSolar(lat, lon float64) ([]SolarRow, error)
```

Returns rows ordered oldest-first for the historical chart.


## Handler: POST /solar/analyze

1. Parse form: `lat`, `lon`, `place_name`, `area`, `efficiency`, `pr`, `az`, `tilt`
2. Default panel values (if missing/invalid):
   - area = 1.7, efficiency = 0.18, pr = 0.80
   - az = 180 (or 0 if lat < 0), tilt = abs(lat)
3. Fetch OWM cloud fraction (same graceful fallback as `/analyze`)
4. Call `solar.Estimate(now, loc, panel, cloudFrac, 0.2)`
5. Call `db.InsertOWMObservation` then `db.InsertSolar`
6. Call `db.UpsertWatchedLocation` (same watched-location pool as brightness)
7. Render `solar-results` partial


## Handler: GET /solar-chart

Same pattern as `GET /chart` for brightness:
- Parse `lat`, `lon`, `place` query params
- Call `db.ForLocationSolar(lat, lon)`
- Embed as `template.JS` JSON
- Render `solar-chart` partial
- The partial wraps itself in an `hx-trigger="every 120s"` container


## Worker integration

The background worker calls `solar.Estimate` for each watched location
immediately after the brightness analysis, using per-location panel defaults:

| Default | Value |
|---------|-------|
| Area    | 1.7 m² |
| Efficiency | 0.18 |
| PR      | 0.80 |
| Tilt    | `abs(lat)` degrees |
| Azimuth | 180° if lat ≥ 0, else 0° (equator-facing) |

Results are stored in `solar_analyses` alongside the brightness result in
`analyses`. Both share the same `owm_observation_id`.


## UI layout

### `/solar` page form

```
Location
  [ search autocomplete — same as brightness page ]

Panel specification
  Area (m²)        Efficiency (%)    Performance ratio
  [ 1.70 ]         [ 18 ]            [ 0.80 ]

  Azimuth (° from N)    Tilt (° from horizontal)
  [ 180 ]               [ 45.0 ]
  (auto-set on location select)

  [ Estimate ]
```

### Results partial (after POST)

```
┌──────────────────────────┬──────────────────────────┐
│   Clear-sky              │   OWM-adjusted           │
│                          │                          │
│  POA:        423 W/m²    │  POA:        251 W/m²    │
│  Power now:  103 W       │  Power now:   61 W       │
│  Daily est:  1.24 kWh    │  Daily est:  0.74 kWh    │
│                          │  Cloud cover: 34%        │
└──────────────────────────┴──────────────────────────┘

   Daily power profile for <place name>
   [Chart.js bar chart — 24 hourly bars, 2 datasets: Clear / OWM]
   [X-axis: hour 0-23 UTC]
   [Y-axis: Watts]

   [Historical kWh chart loaded via HTMX hx-trigger="load"]
```

### Historical chart partial

```
   Solar history for <place name>
   [Chart.js line chart — 2 lines: Clear-sky kWh / OWM kWh]
   [X-axis: analyzed_at timestamp]
   [Y-axis: kWh/day]
   [Refreshes automatically every 120 s]
```


## Auto-set panel orientation on location select

When the user selects a location from autocomplete, JS auto-populates:
```js
document.getElementById('panel-az').value   = lat >= 0 ? 180 : 0;
document.getElementById('panel-tilt').value = Math.abs(lat).toFixed(1);
```

These values can be overridden manually before submitting.


## Panel defaults

| Field      | Default | Notes                                        |
|------------|---------|----------------------------------------------|
| Area       | 1.7 m²  | Typical single residential panel            |
| Efficiency | 18%     | Mid-range monocrystalline                   |
| PR         | 0.80    | Accounts for wiring, inverter, temperature  |
| Tilt       | abs(lat)| Optimal tilt ≈ latitude for annual yield    |
| Azimuth    | 180°    | South-facing (N hemisphere); 0° S hemisphere|


## Ground albedo

Fixed at `0.20` (grass/dirt) — same as the brightness feature. Not exposed
as a form field to keep the UI simple.


## Graceful OWM fallback

Same policy as the brightness feature:
- If OWM is unavailable, use 50% cloud fraction
- Show a `<mark>` warning in the results partial
- The OWM observation is still stored with `fallback=1`
