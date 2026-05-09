package db

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps a *sql.DB and exposes query helpers.
type DB struct {
	sql *sql.DB
}

// AnalysisRow is a single stored analysis result.
type AnalysisRow struct {
	ID                  int64
	PlaceName           string
	Lat                 float64
	Lon                 float64
	AnalyzedAt          time.Time
	ClearSkyNits        float64
	ClearPerceivedNits  float64
	ClearReflectedNits  float64
	ClearContrastRatio  float64
	ClearGlare          bool
	OWMNits             float64
	OWMPerceivedNits    float64
	OWMReflectedNits    float64
	OWMContrastRatio    float64
	OWMGlare            bool
	OWMCloudFraction    float64
	SunZenithDeg        float64
	CalcbrightVersion   string
	OWMObservationID    int64
	OrientationAz       float64
	OrientationTilt     float64
	DisplayNits         float64
	Reflectance         float64
	AltMeters           float64
}

// InsertParams carries the values for a new analysis row.
type InsertParams struct {
	PlaceName          string
	Lat                float64
	Lon                float64
	ClearSkyNits       float64
	ClearPerceivedNits float64
	ClearReflectedNits float64
	ClearContrastRatio float64
	ClearGlare         bool
	OWMNits            float64
	OWMPerceivedNits   float64
	OWMReflectedNits   float64
	OWMContrastRatio   float64
	OWMGlare           bool
	OWMCloudFraction   float64
	SunZenithDeg       float64
	// Recalculation metadata
	CalcbrightVersion string
	OWMObservationID  int64 // 0 when not linked
	OrientationAz     float64
	OrientationTilt   float64
	DisplayNits       float64
	Reflectance       float64
	AltMeters         float64
}

// OWMObservationParams holds raw conditions fetched from OWM (or a fallback).
// Storing these allows replaying the exact weather inputs if calcbright is updated.
type OWMObservationParams struct {
	Lat        float64
	Lon        float64
	ObservedAt time.Time
	Clouds     int     // 0–100 percent
	Uvi        float64
	Visibility int
	SunriseUnix int64
	SunsetUnix  int64
	Fallback   bool // true when OWM was unavailable and we used a default cloud value
}

// Open opens (or creates) the SQLite database at path and runs migrations.
func Open(path string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqlDB.SetMaxOpenConns(1) // SQLite is single-writer

	db := &DB{sql: sqlDB}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

// Close closes the underlying database connection.
func (db *DB) Close() error {
	return db.sql.Close()
}

const schema = `
CREATE TABLE IF NOT EXISTS analyses (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    place_name           TEXT    NOT NULL,
    lat                  REAL    NOT NULL,
    lon                  REAL    NOT NULL,
    analyzed_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    clear_sky_nits       REAL,
    clear_perceived_nits REAL,
    clear_reflected_nits REAL,
    clear_contrast_ratio REAL,
    clear_glare          INTEGER,
    owm_nits             REAL,
    owm_perceived_nits   REAL,
    owm_reflected_nits   REAL,
    owm_contrast_ratio   REAL,
    owm_glare            INTEGER,
    owm_cloud_fraction   REAL,
    sun_zenith_deg       REAL
);

CREATE INDEX IF NOT EXISTS idx_analyses_location
    ON analyses (ROUND(lat, 2), ROUND(lon, 2));

-- owm_observations stores the raw conditions received from OpenWeatherMap
-- (or marked as a fallback when OWM was unavailable).  Linking analyses to
-- this table means the brightness calculation can be replayed with a newer
-- version of calcbright using the exact same weather inputs.
CREATE TABLE IF NOT EXISTS owm_observations (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    lat          REAL    NOT NULL,
    lon          REAL    NOT NULL,
    observed_at  DATETIME NOT NULL,
    clouds       INTEGER NOT NULL,
    uvi          REAL,
    visibility   INTEGER,
    sunrise_unix INTEGER,
    sunset_unix  INTEGER,
    fallback     INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS watched_locations (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    lat_grid   REAL    NOT NULL,
    lon_grid   REAL    NOT NULL,
    place_name TEXT    NOT NULL,
    lat        REAL    NOT NULL,
    lon        REAL    NOT NULL,
    UNIQUE (lat_grid, lon_grid)
);

-- solar_analyses stores panel power estimates (separate from brightness analyses).
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
`

// newColumns are added to existing tables via ALTER TABLE when they don't
// exist yet.  SQLite returns "duplicate column name" if the column is already
// present; we treat that as a no-op so the migration is idempotent.
var newColumns = []string{
	`ALTER TABLE analyses ADD COLUMN calcbright_version TEXT`,
	`ALTER TABLE analyses ADD COLUMN owm_observation_id INTEGER`,
	`ALTER TABLE analyses ADD COLUMN orientation_az     REAL`,
	`ALTER TABLE analyses ADD COLUMN orientation_tilt   REAL`,
	`ALTER TABLE analyses ADD COLUMN display_nits       REAL`,
	`ALTER TABLE analyses ADD COLUMN reflectance        REAL`,
	`ALTER TABLE analyses ADD COLUMN alt_meters         REAL`,
	// Track when a location was last user-requested so the worker can pause
	// inactive locations after the watch window expires.
	`ALTER TABLE watched_locations ADD COLUMN last_requested_at DATETIME`,
}

func (db *DB) migrate() error {
	if _, err := db.sql.Exec(schema); err != nil {
		return err
	}
	for _, stmt := range newColumns {
		if _, err := db.sql.Exec(stmt); err != nil {
			if !strings.Contains(err.Error(), "duplicate column name") {
				return fmt.Errorf("alter table: %w", err)
			}
		}
	}
	return nil
}

// InsertOWMObservation persists raw OWM (or fallback) conditions and returns
// the assigned ID, which should be stored in the linked analysis row.
func (db *DB) InsertOWMObservation(p OWMObservationParams) (int64, error) {
	res, err := db.sql.Exec(`
		INSERT INTO owm_observations
			(lat, lon, observed_at, clouds, uvi, visibility, sunrise_unix, sunset_unix, fallback)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Lat, p.Lon,
		p.ObservedAt.UTC().Format(time.RFC3339),
		p.Clouds, sanitize(p.Uvi), p.Visibility,
		p.SunriseUnix, p.SunsetUnix,
		boolToInt(p.Fallback),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Insert stores one analysis run and returns its assigned ID.
func (db *DB) Insert(p InsertParams) (int64, error) {
	var obsID any
	if p.OWMObservationID != 0 {
		obsID = p.OWMObservationID
	}
	res, err := db.sql.Exec(`
		INSERT INTO analyses (
			place_name, lat, lon, analyzed_at,
			clear_sky_nits, clear_perceived_nits, clear_reflected_nits, clear_contrast_ratio, clear_glare,
			owm_nits, owm_perceived_nits, owm_reflected_nits, owm_contrast_ratio, owm_glare,
			owm_cloud_fraction, sun_zenith_deg,
			calcbright_version, owm_observation_id,
			orientation_az, orientation_tilt, display_nits, reflectance, alt_meters
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.PlaceName, p.Lat, p.Lon, time.Now().UTC().Format(time.RFC3339),
		sanitize(p.ClearSkyNits), sanitize(p.ClearPerceivedNits),
		sanitize(p.ClearReflectedNits), sanitize(p.ClearContrastRatio), boolToInt(p.ClearGlare),
		sanitize(p.OWMNits), sanitize(p.OWMPerceivedNits),
		sanitize(p.OWMReflectedNits), sanitize(p.OWMContrastRatio), boolToInt(p.OWMGlare),
		sanitize(p.OWMCloudFraction), sanitize(p.SunZenithDeg),
		p.CalcbrightVersion, obsID,
		sanitize(p.OrientationAz), sanitize(p.OrientationTilt),
		sanitize(p.DisplayNits), sanitize(p.Reflectance), sanitize(p.AltMeters),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ForLocation returns all analyses for a location rounded to 2 decimal places,
// ordered oldest first (for chart display).
func (db *DB) ForLocation(lat, lon float64) ([]AnalysisRow, error) {
	rows, err := db.sql.Query(`
		SELECT id, place_name, lat, lon, analyzed_at,
		       clear_sky_nits, clear_perceived_nits, clear_reflected_nits, clear_contrast_ratio, clear_glare,
		       owm_nits, owm_perceived_nits, owm_reflected_nits, owm_contrast_ratio, owm_glare,
		       owm_cloud_fraction, sun_zenith_deg
		FROM analyses
		WHERE ROUND(lat, 2) = ROUND(?, 2)
		  AND ROUND(lon, 2) = ROUND(?, 2)
		ORDER BY analyzed_at ASC`,
		lat, lon,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []AnalysisRow
	for rows.Next() {
		var r AnalysisRow
		var analyzedAt string
		var clearGlare, owmGlare int
		err := rows.Scan(
			&r.ID, &r.PlaceName, &r.Lat, &r.Lon, &analyzedAt,
			&r.ClearSkyNits, &r.ClearPerceivedNits, &r.ClearReflectedNits, &r.ClearContrastRatio, &clearGlare,
			&r.OWMNits, &r.OWMPerceivedNits, &r.OWMReflectedNits, &r.OWMContrastRatio, &owmGlare,
			&r.OWMCloudFraction, &r.SunZenithDeg,
		)
		if err != nil {
			return nil, err
		}
		r.ClearGlare = clearGlare != 0
		r.OWMGlare = owmGlare != 0
		if t, err := time.Parse(time.RFC3339, analyzedAt); err == nil {
			r.AnalyzedAt = t
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// UpsertWatchedLocation inserts or replaces the watched location for the
// grid cell containing (lat, lon) rounded to 2 decimal places.
// last_requested_at is always stamped to now so the worker knows this
// location is still actively used and should not be paused.
func (db *DB) UpsertWatchedLocation(placeName string, lat, lon float64) error {
	latGrid := roundGrid(lat)
	lonGrid := roundGrid(lon)
	_, err := db.sql.Exec(`
		INSERT INTO watched_locations (lat_grid, lon_grid, place_name, lat, lon, last_requested_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(lat_grid, lon_grid) DO UPDATE SET
			place_name        = excluded.place_name,
			lat               = excluded.lat,
			lon               = excluded.lon,
			last_requested_at = CURRENT_TIMESTAMP`,
		latGrid, lonGrid, placeName, lat, lon,
	)
	return err
}

// ActiveWatchedLocations returns locations whose last user request was at or
// after since. Rows with a NULL last_requested_at (locations registered before
// this column was added) are always included until they age out naturally.
func (db *DB) ActiveWatchedLocations(since time.Time) ([]WatchedLocation, error) {
	rows, err := db.sql.Query(`
		SELECT place_name, lat, lon FROM watched_locations
		WHERE last_requested_at IS NULL OR last_requested_at >= ?`,
		since.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []WatchedLocation
	for rows.Next() {
		var w WatchedLocation
		if err := rows.Scan(&w.PlaceName, &w.Lat, &w.Lon); err != nil {
			return nil, err
		}
		result = append(result, w)
	}
	return result, rows.Err()
}

// WatchedLocations returns all locations registered for background collection.
func (db *DB) WatchedLocations() ([]WatchedLocation, error) {
	rows, err := db.sql.Query(
		`SELECT place_name, lat, lon FROM watched_locations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []WatchedLocation
	for rows.Next() {
		var w WatchedLocation
		if err := rows.Scan(&w.PlaceName, &w.Lat, &w.Lon); err != nil {
			return nil, err
		}
		result = append(result, w)
	}
	return result, rows.Err()
}

// WatchedLocation is a row from the watched_locations table.
type WatchedLocation struct {
	PlaceName string
	Lat       float64
	Lon       float64
}

// roundGrid rounds a coordinate to 2 decimal places for grid-cell matching.
func roundGrid(v float64) float64 {
	return math.Round(v*100) / 100
}

// sanitize replaces NaN/Inf with 0 so SQLite doesn't choke.
func sanitize(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// SolarInsertParams carries the values for a new solar_analyses row.
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
	OWMObservationID  int64 // 0 when not linked
}

// InsertSolar stores one solar analysis run and returns its assigned ID.
func (db *DB) InsertSolar(p SolarInsertParams) (int64, error) {
	var obsID any
	if p.OWMObservationID != 0 {
		obsID = p.OWMObservationID
	}
	res, err := db.sql.Exec(`
		INSERT INTO solar_analyses (
			place_name, lat, lon, analyzed_at,
			panel_area, panel_efficiency, panel_pr, panel_az, panel_tilt,
			clear_poa_wm2, clear_power_w, clear_daily_kwh,
			owm_poa_wm2, owm_power_w, owm_daily_kwh,
			cloud_fraction, sun_zenith_deg,
			calcbright_version, owm_observation_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.PlaceName, p.Lat, p.Lon, time.Now().UTC().Format(time.RFC3339),
		sanitize(p.PanelArea), sanitize(p.PanelEfficiency), sanitize(p.PanelPR),
		sanitize(p.PanelAz), sanitize(p.PanelTilt),
		sanitize(p.ClearPOAWm2), sanitize(p.ClearPowerW), sanitize(p.ClearDailyKWh),
		sanitize(p.OWMPOAWm2), sanitize(p.OWMPowerW), sanitize(p.OWMDailyKWh),
		sanitize(p.CloudFraction), sanitize(p.SunZenithDeg),
		p.CalcbrightVersion, obsID,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SolarRow is a single stored solar analysis result (for chart/history).
type SolarRow struct {
	ID            int64
	PlaceName     string
	Lat           float64
	Lon           float64
	AnalyzedAt    time.Time
	ClearDailyKWh float64
	OWMDailyKWh   float64
}

// ForLocationSolar returns all solar analyses for a location rounded to 2
// decimal places, ordered oldest first (for chart display).
func (db *DB) ForLocationSolar(lat, lon float64) ([]SolarRow, error) {
	rows, err := db.sql.Query(`
		SELECT id, place_name, lat, lon, analyzed_at, clear_daily_kwh, owm_daily_kwh
		FROM solar_analyses
		WHERE ROUND(lat, 2) = ROUND(?, 2)
		  AND ROUND(lon, 2) = ROUND(?, 2)
		ORDER BY analyzed_at ASC`,
		lat, lon,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []SolarRow
	for rows.Next() {
		var r SolarRow
		var analyzedAt string
		if err := rows.Scan(&r.ID, &r.PlaceName, &r.Lat, &r.Lon, &analyzedAt,
			&r.ClearDailyKWh, &r.OWMDailyKWh); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339, analyzedAt); err == nil {
			r.AnalyzedAt = t
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
