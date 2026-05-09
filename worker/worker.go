package worker

import (
	"context"
	"log"
	"time"

	"github.com/icyavocado/calcbright/brightness"
	"github.com/icyavocado/sun-chaser/db"
)

// Worker periodically collects brightness data for all watched locations.
type Worker struct {
	db                *db.DB
	owmClient         *brightness.OWMClient
	interval          time.Duration
	calcbrightVersion string
}

// New creates a Worker with the given collection interval.
func New(database *db.DB, owmClient *brightness.OWMClient, interval time.Duration, calcbrightVersion string) *Worker {
	return &Worker{
		db:                database,
		owmClient:         owmClient,
		interval:          interval,
		calcbrightVersion: calcbrightVersion,
	}
}

// Run starts the collection loop, collecting immediately on startup and then
// on each tick. It returns when ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	w.collect(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			w.collect(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (w *Worker) collect(ctx context.Context) {
	locations, err := w.db.WatchedLocations()
	if err != nil {
		log.Printf("worker: list watched locations: %v", err)
		return
	}
	if len(locations) == 0 {
		return
	}
	log.Printf("worker: collecting %d location(s)", len(locations))
	for _, loc := range locations {
		if ctx.Err() != nil {
			return
		}
		if err := w.analyzeLocation(ctx, loc); err != nil {
			log.Printf("worker: analyze %s: %v", loc.PlaceName, err)
		}
	}
}

// defaultOrientation / device / environment used by the background worker.
// These mirror the form defaults in the UI so worker data is comparable.
const (
	defaultAz          = 180.0
	defaultTilt        = 110.0
	defaultDisplayNits = 300.0
	defaultReflectance = 0.05
	defaultAlt         = 0.0
	fallbackCloudFrac  = 0.5
)

func (w *Worker) analyzeLocation(ctx context.Context, wl db.WatchedLocation) error {
	now := time.Now()
	loc := brightness.Location{Lat: wl.Lat, Lon: wl.Lon, AltMeters: defaultAlt}
	orient := brightness.Orientation{AzimuthDeg: defaultAz, TiltDeg: defaultTilt}
	dev := brightness.DeviceSpec{DisplayNits: defaultDisplayNits, Reflectance: defaultReflectance}
	env := brightness.Environment{GroundAlbedo: 0.2}

	// Clear-sky model.
	clearReport, err := brightness.AnalyzeWithValues(now, loc, orient, dev, env, 120, 0, 0, 0)
	if err != nil {
		return err
	}

	// OWM-adjusted model with graceful fallback.
	// Persist raw OWM conditions before running the calculation so the exact
	// weather inputs are always stored regardless of the calcbright version.
	cloudFrac := fallbackCloudFrac
	obsParams := db.OWMObservationParams{
		Lat:        wl.Lat,
		Lon:        wl.Lon,
		ObservedAt: now,
		Clouds:     50, // fallback default
		Fallback:   true,
	}

	owmData, owmErr := w.owmClient.GetCurrent(ctx, wl.Lat, wl.Lon)
	if owmErr != nil {
		log.Printf("worker: OWM unavailable for %s: %v — using %.0f%% cloud fallback",
			wl.PlaceName, owmErr, fallbackCloudFrac*100)
	} else {
		cloudFrac = float64(owmData.Clouds) / 100.0
		obsParams.Clouds = owmData.Clouds
		obsParams.Uvi = owmData.Uvi
		obsParams.Visibility = owmData.Visibility
		obsParams.SunriseUnix = owmData.Sunrise
		obsParams.SunsetUnix = owmData.Sunset
		obsParams.Fallback = false
	}

	// Persist raw OWM conditions.
	obsID, err := w.db.InsertOWMObservation(obsParams)
	if err != nil {
		log.Printf("worker: insert OWM observation for %s: %v", wl.PlaceName, err)
		obsID = 0
	}

	dni, dhi, ghi, err := brightness.ClearSkyIrradiance(now, loc)
	if err != nil {
		return err
	}
	dniA, dhiA, ghiA := brightness.ApplyCloudAttenuation(dni, dhi, ghi, cloudFrac)

	owmReport, err := brightness.AnalyzeWithValues(now, loc, orient, dev, env, 120, dniA, dhiA, ghiA)
	if err != nil {
		return err
	}
	owmReport.DataSource = "owm"

	_, sunZenith, _ := brightness.SunPosition(now, loc)

	_, err = w.db.Insert(db.InsertParams{
		PlaceName:          wl.PlaceName,
		Lat:                wl.Lat,
		Lon:                wl.Lon,
		ClearSkyNits:       clearReport.RecommendedDisplayNits,
		ClearPerceivedNits: clearReport.PerceivedLuminance,
		ClearReflectedNits: clearReport.ReflectedLuminance,
		ClearContrastRatio: clearReport.ContrastRatio,
		ClearGlare:         clearReport.Glare,
		OWMNits:            owmReport.RecommendedDisplayNits,
		OWMPerceivedNits:   owmReport.PerceivedLuminance,
		OWMReflectedNits:   owmReport.ReflectedLuminance,
		OWMContrastRatio:   owmReport.ContrastRatio,
		OWMGlare:           owmReport.Glare,
		OWMCloudFraction:   cloudFrac,
		SunZenithDeg:       sunZenith,
		CalcbrightVersion:  w.calcbrightVersion,
		OWMObservationID:   obsID,
		OrientationAz:      defaultAz,
		OrientationTilt:    defaultTilt,
		DisplayNits:        defaultDisplayNits,
		Reflectance:        defaultReflectance,
		AltMeters:          defaultAlt,
	})
	return err
}
