package handlers

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"
	"time"
)

// chartData is passed to the chart partial template.
type chartData struct {
	PlaceName string
	Lat       float64
	Lon       float64
	DataJSON  template.JS // pre-marshalled JSON, safe to embed in <script>
}

// Chart renders the chart partial with history data embedded as JSON.
// Called by HTMX polling on the results page.
func (h *Handler) Chart(w http.ResponseWriter, r *http.Request) {
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
	place := r.URL.Query().Get("place")

	rows, err := h.db.ForLocation(lat, lon)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	points := make([]historyPoint, 0, len(rows))
	for _, row := range rows {
		points = append(points, historyPoint{
			AnalyzedAt:   row.AnalyzedAt.UTC().Format(time.RFC3339),
			ClearSkyNits: roundTo(row.ClearSkyNits, 1),
			OWMNits:      roundTo(row.OWMNits, 1),
		})
	}

	jsonBytes, _ := json.Marshal(points)

	data := chartData{
		PlaceName: place,
		Lat:       lat,
		Lon:       lon,
		DataJSON:  template.JS(jsonBytes),
	}

	out, err := h.render("chart", data)
	if err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(out)
}
