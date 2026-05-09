package worker

import (
	"context"
	"log"
	"time"

	"github.com/icyavocado/calcbright/brightness"
	"github.com/icyavocado/sun-chaser/db"
	"github.com/icyavocado/sun-chaser/solar"
)

// Worker periodically collects brightness data for all active watched locations.
type Worker struct {
	db                *db.DB
	owmClient         *brightness.OWMClient
	interval          time.Duration
	calcbrightVersion string

	// watchWindow is how far back we look when deciding which locations are
	// still "active". Locations whose last user request is older than this
	// are skipped until the user requests them again.
	watchWindow time.Duration

	// requestDelay is the pause inserted between consecutive OWM calls within
	// a single collection batch, to avoid hitting the rate limit in a burst.
	requestDelay time.Duration

	// Circuit-breaker state — all fields are accessed only from the single
	// goroutine that calls collect(), so no mutex is needed.
	owmConsecFailBatches int // consecutive batches where every OWM call failed
	skipCycles           int // remaining collect() ticks to skip
	backoffStep          int // next skip length (doubles on each open: 1, 2, 4…)
}

// New creates a Worker with the given collection interval and OWM spacing.
func New(
	database *db.DB,
	owmClient *brightness.OWMClient,
	interval time.Duration,
	calcbrightVersion string,
	watchWindow time.Duration,
	requestDelay time.Duration,
) *Worker {
	return &Worker{
		db:                database,
		owmClient:         owmClient,
		interval:          interval,
		calcbrightVersion: calcbrightVersion,
		watchWindow:       watchWindow,
		requestDelay:      requestDelay,
		backoffStep:       1,
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
	// Circuit breaker: skip this cycle if we're in a backoff window.
	if w.skipCycles > 0 {
		w.skipCycles--
		log.Printf("worker: OWM circuit breaker open — skipping collection (%d cycle(s) remaining)", w.skipCycles)
		return
	}

	since := time.Now().Add(-w.watchWindow)
	locations, err := w.db.ActiveWatchedLocations(since)
	if err != nil {
		log.Printf("worker: list active watched locations: %v", err)
		return
	}
	if len(locations) == 0 {
		return
	}
	log.Printf("worker: collecting %d active location(s)", len(locations))

	owmSuccesses := 0

	for i, loc := range locations {
		if ctx.Err() != nil {
			return
		}

		owmOK, err := w.analyzeLocation(ctx, loc)
		if err != nil {
			log.Printf("worker: analyze %s: %v", loc.PlaceName, err)
		}
		if owmOK {
			owmSuccesses++
		}

		// Space out OWM calls — skip the delay after the last location.
		if i < len(locations)-1 && w.requestDelay > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(w.requestDelay):
			}
		}
	}

	// Update circuit-breaker state.
	if owmSuccesses > 0 {
		// At least one OWM call succeeded — reset the breaker.
		if w.owmConsecFailBatches > 0 {
			log.Printf("worker: OWM recovered — resetting circuit breaker")
		}
		w.owmConsecFailBatches = 0
		w.backoffStep = 1
	} else {
		// Every OWM call in this batch failed.
		w.owmConsecFailBatches++
		log.Printf("worker: OWM failed for all locations (%d consecutive batch(es))", w.owmConsecFailBatches)
		if w.owmConsecFailBatches >= 3 {
			w.skipCycles = w.backoffStep
			log.Printf("worker: OWM circuit breaker tripped — pausing for %d cycle(s)", w.skipCycles)
			w.backoffStep *= 2
			w.owmConsecFailBatches = 0
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

// analyzeLocation runs the full brightness + solar estimate for one location.
// It returns owmOK=true when OWM data was successfully fetched (not a fallback).
func (w *Worker) analyzeLocation(ctx context.Context, wl db.WatchedLocation) (owmOK bool, err error) {
	now := time.Now()
	loc := brightness.Location{Lat: wl.Lat, Lon: wl.Lon, AltMeters: defaultAlt}
	orient := brightness.Orientation{AzimuthDeg: defaultAz, TiltDeg: defaultTilt}
	dev := brightness.DeviceSpec{DisplayNits: defaultDisplayNits, Reflectance: defaultReflectance}
	env := brightness.Environment{GroundAlbedo: 0.2}

	// Clear-sky model.
	clearReport, err := brightness.AnalyzeWithValues(now, loc, orient, dev, env, 120, 0, 0, 0)
	if err != nil {
		return false, err
	}

	// OWM-adjusted model with graceful fallback.
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
		owmOK = true
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
		return owmOK, err
	}
	dniA, dhiA, ghiA := brightness.ApplyCloudAttenuation(dni, dhi, ghi, cloudFrac)

	owmReport, err := brightness.AnalyzeWithValues(now, loc, orient, dev, env, 120, dniA, dhiA, ghiA)
	if err != nil {
		return owmOK, err
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
	if err != nil {
		return owmOK, err
	}

	// --- Solar estimate for this location ---
	panel := solar.DefaultPanel(wl.Lat)
	solarReport, solarErr := solar.Estimate(now, loc, panel, cloudFrac)
	if solarErr != nil {
		// Non-fatal: log and continue so brightness data is still stored.
		log.Printf("worker: solar estimate for %s: %v", wl.PlaceName, solarErr)
		return owmOK, nil
	}

	_, err = w.db.InsertSolar(db.SolarInsertParams{
		PlaceName:         wl.PlaceName,
		Lat:               wl.Lat,
		Lon:               wl.Lon,
		PanelArea:         panel.AreaM2,
		PanelEfficiency:   panel.Efficiency,
		PanelPR:           panel.PR,
		PanelAz:           panel.AzDeg,
		PanelTilt:         panel.TiltDeg,
		ClearPOAWm2:       solarReport.ClearPOA,
		ClearPowerW:       solarReport.ClearPowerW,
		ClearDailyKWh:     solarReport.ClearDailyKWh,
		OWMPOAWm2:         solarReport.OWMPOA,
		OWMPowerW:         solarReport.OWMPowerW,
		OWMDailyKWh:       solarReport.OWMDailyKWh,
		CloudFraction:     cloudFrac,
		SunZenithDeg:      sunZenith,
		CalcbrightVersion: w.calcbrightVersion,
		OWMObservationID:  obsID,
	})
	return owmOK, err
}
