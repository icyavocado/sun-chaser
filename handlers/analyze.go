package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/icyavocado/calcbright/brightness"
	"github.com/icyavocado/sun-chaser/db"
)

// ResultData is passed to the results partial template.
type ResultData struct {
	PlaceName    string
	Lat          float64
	Lon          float64
	ClearSky     brightness.Report
	OWM          brightness.Report
	CloudPercent int    // 0–100
	OWMWarning   string // non-empty when OWM fell back to default cloud fraction
	AnalyzedAt   time.Time
}

// Index serves the main page.
func (h *Handler) Index(w http.ResponseWriter, r *http.Request) {
	out, err := h.render("index", struct{}{})
	if err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(out)
}

// Analyze runs both brightness models, stores the result, and returns the
// results HTMX partial.
func (h *Handler) Analyze(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	lat, err := strconv.ParseFloat(r.FormValue("lat"), 64)
	if err != nil {
		http.Error(w, "missing or invalid lat", http.StatusBadRequest)
		return
	}
	lon, err := strconv.ParseFloat(r.FormValue("lon"), 64)
	if err != nil {
		http.Error(w, "missing or invalid lon", http.StatusBadRequest)
		return
	}

	alt := parseFloatOr(r.FormValue("alt"), 0)
	az := parseFloatOr(r.FormValue("az"), 180)
	tilt := parseFloatOr(r.FormValue("tilt"), 110)
	displayNits := parseFloatOr(r.FormValue("display_nits"), 300)
	reflectance := parseFloatOr(r.FormValue("reflectance"), 0.05)
	placeName := r.FormValue("place_name")
	if placeName == "" {
		placeName = fmt.Sprintf("%.4f, %.4f", lat, lon)
	}

	loc := brightness.Location{Lat: lat, Lon: lon, AltMeters: alt}
	orient := brightness.Orientation{AzimuthDeg: az, TiltDeg: tilt}
	dev := brightness.DeviceSpec{DisplayNits: displayNits, Reflectance: reflectance}
	env := brightness.Environment{GroundAlbedo: 0.2}
	now := time.Now()

	// --- Clear-sky analysis (no cloud attenuation) ---
	clearReport, err := brightness.AnalyzeWithValues(now, loc, orient, dev, env, 120, 0, 0, 0)
	if err != nil {
		http.Error(w, "clear-sky analysis failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// --- OWM analysis: fetch cloud fraction then attenuate ---
	// On OWM failure, degrade gracefully: use 50% cloud fraction as a
	// neutral fallback so the clear-sky comparison is still meaningful.
	const fallbackCloudFrac = 0.5
	cloudFrac := fallbackCloudFrac
	cloudPercent := 50
	owmWarning := ""
	owmFallback := true

	var owmObs db.OWMObservationParams
	owmObs.Lat = lat
	owmObs.Lon = lon
	owmObs.ObservedAt = now

	owmData, owmErr := h.owmClient.GetCurrent(r.Context(), lat, lon)
	if owmErr != nil {
		owmWarning = "OWM unavailable (" + owmErr.Error() + ") — using 50% cloud fallback"
		owmObs.Clouds = 50
		owmObs.Fallback = true
	} else {
		cloudFrac = float64(owmData.Clouds) / 100.0
		cloudPercent = owmData.Clouds
		owmFallback = false
		owmObs.Clouds = owmData.Clouds
		owmObs.Uvi = owmData.Uvi
		owmObs.Visibility = owmData.Visibility
		owmObs.SunriseUnix = owmData.Sunrise
		owmObs.SunsetUnix = owmData.Sunset
		owmObs.Fallback = false
	}
	_ = owmFallback

	dni, dhi, ghi, err := brightness.ClearSkyIrradiance(now, loc)
	if err != nil {
		http.Error(w, "irradiance calculation failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	dniA, dhiA, ghiA := brightness.ApplyCloudAttenuation(dni, dhi, ghi, cloudFrac)

	owmReport, err := brightness.AnalyzeWithValues(now, loc, orient, dev, env, 120, dniA, dhiA, ghiA)
	if err != nil {
		http.Error(w, "OWM analysis failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	owmReport.DataSource = "owm"

	// Sun zenith for context storage.
	_, sunZenith, _ := brightness.SunPosition(now, loc)

	// --- Persist OWM observation first (raw inputs for recalculation) ---
	obsID, _ := h.db.InsertOWMObservation(owmObs)

	// --- Persist analysis (results + link to OWM observation + input params) ---
	_, _ = h.db.Insert(db.InsertParams{
		PlaceName:          placeName,
		Lat:                lat,
		Lon:                lon,
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
		CalcbrightVersion:  h.calcbrightVersion,
		OWMObservationID:   obsID,
		OrientationAz:      az,
		OrientationTilt:    tilt,
		DisplayNits:        displayNits,
		Reflectance:        reflectance,
		AltMeters:          alt,
	})

	// --- Register location for background collection ---
	_ = h.db.UpsertWatchedLocation(placeName, lat, lon)

	data := ResultData{
		PlaceName:    placeName,
		Lat:          lat,
		Lon:          lon,
		ClearSky:     clearReport,
		OWM:          owmReport,
		CloudPercent: cloudPercent,
		OWMWarning:   owmWarning,
		AnalyzedAt:   now,
	}

	out, err := h.render("results", data)
	if err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(out)
}

func parseFloatOr(s string, fallback float64) float64 {
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return v
	}
	return fallback
}
