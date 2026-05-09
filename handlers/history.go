package handlers

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
)

// historyPoint is one data point returned to Chart.js.
type historyPoint struct {
	AnalyzedAt   string  `json:"analyzed_at"`
	ClearSkyNits float64 `json:"clear_sky_nits"`
	OWMNits      float64 `json:"owm_nits"`
}

// History returns JSON historical analysis data for a location.
func (h *Handler) History(w http.ResponseWriter, r *http.Request) {
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

	rows, err := h.db.ForLocation(lat, lon)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	points := make([]historyPoint, 0, len(rows))
	for _, row := range rows {
		points = append(points, historyPoint{
			AnalyzedAt:   row.AnalyzedAt.Format("Jan 2 15:04"),
			ClearSkyNits: roundTo(row.ClearSkyNits, 1),
			OWMNits:      roundTo(row.OWMNits, 1),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(points)
}

func roundTo(f float64, decimals float64) float64 {
	p := math.Pow(10, decimals)
	return math.Round(f*p) / p
}
