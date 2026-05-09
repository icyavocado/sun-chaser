package handlers

import (
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/icyavocado/calcbright/brightness"
	"github.com/icyavocado/sun-chaser/db"
	"github.com/icyavocado/sun-chaser/solar"
)

// SolarResultData is passed to the solar-results partial template.
type SolarResultData struct {
	PlaceName      string
	Lat            float64
	Lon            float64
	Panel          solar.Panel
	Report         solar.Report
	CloudPercent   int
	OWMWarning     string
	AnalyzedAt     time.Time
	HourlyDataJSON template.JS

	// Financial / practical estimates
	ElectricityRate     float64
	MonthlyUsage        float64
	InstallationCost    float64
	StorageDays         float64
	BatteryPerKWh       float64
	PanelsNeeded        int
	MonthlyProductionKWh float64
	MonthlySavings      float64
	AnnualSavings       float64
	BreakEvenYears      float64
	StorageCapacityKWh  float64
	StorageCost         float64
	WinterReport        solar.Report
}

// SolarIndex serves the /solar page.
func (h *Handler) SolarIndex(w http.ResponseWriter, r *http.Request) {
	out, err := h.render("solar", struct{}{})
	if err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(out)
}

// SolarAnalyze runs the solar estimate, stores the result, and returns the
// solar-results HTMX partial.
func (h *Handler) SolarAnalyze(w http.ResponseWriter, r *http.Request) {
	if !h.analyzeRL.Allow(h.clientIP(r)) {
		http.Error(w, "rate limit exceeded — try again shortly", http.StatusTooManyRequests)
		return
	}

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

	placeName := r.FormValue("place_name")
	if placeName == "" {
		placeName = fmt.Sprintf("%.4f, %.4f", lat, lon)
	}

	// Parse panel spec with per-field defaults.
	defaultPanel := solar.DefaultPanel(lat)
	area := parseFloatOr(r.FormValue("area"), defaultPanel.AreaM2)
	efficiency := parseFloatOr(r.FormValue("efficiency"), defaultPanel.Efficiency*100) / 100.0
	pr := parseFloatOr(r.FormValue("pr"), defaultPanel.PR)
	az := parseFloatOr(r.FormValue("az"), defaultPanel.AzDeg)
	tilt := parseFloatOr(r.FormValue("tilt"), defaultPanel.TiltDeg)

	panel := solar.Panel{
		AreaM2:     area,
		Efficiency: efficiency,
		PR:         pr,
		AzDeg:      az,
		TiltDeg:    tilt,
	}

	loc := brightness.Location{Lat: lat, Lon: lon}
	now := time.Now()

	// --- OWM cloud fraction with graceful fallback ---
	const fallbackCloudFrac = 0.5
	cloudFrac := fallbackCloudFrac
	cloudPercent := 50
	owmWarning := ""

	obsParams := db.OWMObservationParams{
		Lat:        lat,
		Lon:        lon,
		ObservedAt: now,
		Clouds:     50,
		Fallback:   true,
	}

	owmData, owmErr := h.owmClient.GetCurrent(r.Context(), lat, lon)
	if owmErr != nil {
		owmWarning = "OWM unavailable (" + owmErr.Error() + ") — using 50% cloud fallback"
	} else {
		cloudFrac = float64(owmData.Clouds) / 100.0
		cloudPercent = owmData.Clouds
		obsParams.Clouds = owmData.Clouds
		obsParams.Uvi = owmData.Uvi
		obsParams.Visibility = owmData.Visibility
		obsParams.SunriseUnix = owmData.Sunrise
		obsParams.SunsetUnix = owmData.Sunset
		obsParams.Fallback = false
	}

	// --- Run estimate ---
	report, err := solar.Estimate(now, loc, panel, cloudFrac)
	if err != nil {
		http.Error(w, "solar estimate failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	_, sunZenith, _ := brightness.SunPosition(now, loc)

	// --- Persist OWM observation ---
	obsID, _ := h.db.InsertOWMObservation(obsParams)

	// --- Persist solar analysis ---
	_, _ = h.db.InsertSolar(db.SolarInsertParams{
		PlaceName:         placeName,
		Lat:               lat,
		Lon:               lon,
		PanelArea:         panel.AreaM2,
		PanelEfficiency:   panel.Efficiency,
		PanelPR:           panel.PR,
		PanelAz:           panel.AzDeg,
		PanelTilt:         panel.TiltDeg,
		ClearPOAWm2:       report.ClearPOA,
		ClearPowerW:       report.ClearPowerW,
		ClearDailyKWh:     report.ClearDailyKWh,
		OWMPOAWm2:         report.OWMPOA,
		OWMPowerW:         report.OWMPowerW,
		OWMDailyKWh:       report.OWMDailyKWh,
		CloudFraction:     cloudFrac,
		SunZenithDeg:      sunZenith,
		CalcbrightVersion: h.calcbrightVersion,
		OWMObservationID:  obsID,
	})

	// --- Register for background collection ---
	_ = h.db.UpsertWatchedLocation(placeName, lat, lon)

	// --- Parse economics inputs ---
	electricityRate  := parseFloatOr(r.FormValue("electricity_rate"), 0.13)
	monthlyUsage     := parseFloatOr(r.FormValue("monthly_usage"), 900)
	installationCost := parseFloatOr(r.FormValue("installation_cost"), 15000)
	storageDays      := parseFloatOr(r.FormValue("storage_days"), 1)
	batteryPerKWh    := parseFloatOr(r.FormValue("battery_per_kwh"), 400)

	// --- Winter solstice estimate (same panel, same cloud fraction) ---
	winterMonth := time.December
	if lat < 0 { // southern hemisphere — winter is in June
		winterMonth = time.June
	}
	winterDate := time.Date(now.Year(), winterMonth, 21, 12, 0, 0, 0, time.UTC)
	winterReport, _ := solar.Estimate(winterDate, loc, panel, cloudFrac)

	// --- Financial calculations ---
	// Panels needed to cover monthly usage from this panel's daily yield.
	var panelsNeeded int
	if report.OWMDailyKWh > 0 {
		panelsNeeded = int(math.Ceil((monthlyUsage / 30.0) / report.OWMDailyKWh))
	}
	if panelsNeeded < 1 {
		panelsNeeded = 1
	}

	monthlyProductionKWh := report.OWMDailyKWh * float64(panelsNeeded) * 30.0
	mSavings             := monthlyProductionKWh * electricityRate
	annualSavings        := mSavings * 12.0

	var breakEvenYears float64
	if annualSavings > 0 {
		breakEvenYears = installationCost / annualSavings
	} else {
		breakEvenYears = math.Inf(1)
	}

	storageCapacityKWh := report.OWMDailyKWh * float64(panelsNeeded) * storageDays
	storageCost        := storageCapacityKWh * batteryPerKWh

	// --- Build hourly profile JSON for inline chart ---
	hourlyJSON, _ := json.Marshal(report.HourlyProfile)

	data := SolarResultData{
		PlaceName:      placeName,
		Lat:            lat,
		Lon:            lon,
		Panel:          panel,
		Report:         report,
		CloudPercent:   cloudPercent,
		OWMWarning:     owmWarning,
		AnalyzedAt:     now,
		HourlyDataJSON: template.JS(hourlyJSON),

		ElectricityRate:      electricityRate,
		MonthlyUsage:         monthlyUsage,
		InstallationCost:     installationCost,
		StorageDays:          storageDays,
		BatteryPerKWh:        batteryPerKWh,
		PanelsNeeded:         panelsNeeded,
		MonthlyProductionKWh: monthlyProductionKWh,
		MonthlySavings:       mSavings,
		AnnualSavings:        annualSavings,
		BreakEvenYears:       breakEvenYears,
		StorageCapacityKWh:   storageCapacityKWh,
		StorageCost:          storageCost,
		WinterReport:         winterReport,
	}

	out, err := h.render("solar-results", data)
	if err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(out)
}

// SolarChartData is the JSON shape sent to the solar-chart template.
type SolarChartData struct {
	AnalyzedAt    string  `json:"analyzed_at"`
	ClearDailyKWh float64 `json:"clear_daily_kwh"`
	OWMDailyKWh   float64 `json:"owm_daily_kwh"`
}

// SolarChart renders the historical solar kWh chart partial (HTMX-polled).
func (h *Handler) SolarChart(w http.ResponseWriter, r *http.Request) {
	lat, err := strconv.ParseFloat(r.URL.Query().Get("lat"), 64)
	if err != nil {
		http.Error(w, "invalid lat", http.StatusBadRequest)
		return
	}
	lon, err := strconv.ParseFloat(r.URL.Query().Get("lon"), 64)
	if err != nil {
		http.Error(w, "invalid lon", http.StatusBadRequest)
		return
	}
	placeName := r.URL.Query().Get("place")

	rows, err := h.db.ForLocationSolar(lat, lon)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	var points []SolarChartData
	for _, row := range rows {
		points = append(points, SolarChartData{
			AnalyzedAt:    row.AnalyzedAt.Format("2006-01-02 15:04"),
			ClearDailyKWh: row.ClearDailyKWh,
			OWMDailyKWh:   row.OWMDailyKWh,
		})
	}
	dataJSON, _ := json.Marshal(points)

	type chartTmplData struct {
		Lat       float64
		Lon       float64
		PlaceName string
		DataJSON  template.JS
	}
	data := chartTmplData{
		Lat:       lat,
		Lon:       lon,
		PlaceName: placeName,
		DataJSON:  template.JS(dataJSON),
	}

	out, err := h.render("solar-chart", data)
	if err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(out)
}
