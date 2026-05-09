package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// photonFeatureCollection is the GeoJSON envelope Photon returns.
type photonFeatureCollection struct {
	Features []photonFeature `json:"features"`
}

type photonFeature struct {
	Geometry struct {
		Coordinates [2]float64 `json:"coordinates"` // [lon, lat]
	} `json:"geometry"`
	Properties photonProps `json:"properties"`
}

type photonProps struct {
	Name    string `json:"name"`
	Country string `json:"country"`
	State   string `json:"state"`
	County  string `json:"county"`
	Type    string `json:"type"`
}

// Suggestion is passed to the suggestions partial template.
type Suggestion struct {
	DisplayName string
	Lat         float64
	Lon         float64
}

// Geocode proxies a query to Photon and returns the suggestions partial.
func (h *Handler) Geocode(w http.ResponseWriter, r *http.Request) {
	if !h.geocodeRL.Allow(h.clientIP(r)) {
		http.Error(w, "rate limit exceeded — try again shortly", http.StatusTooManyRequests)
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(q)) <= 2 {
		// Return empty partial — clears the suggestions dropdown.
		w.Header().Set("Content-Type", "text/html")
		return
	}

	apiURL := fmt.Sprintf("%s/api/?%s",
		h.photonBase,
		url.Values{
			"q":     {q},
			"limit": {"5"},
			"lang":  {"en"},
		}.Encode(),
	)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, apiURL, nil)
	if err != nil {
		http.Error(w, "geocode error", http.StatusInternalServerError)
		return
	}
	req.Header.Set("User-Agent", "sun-chaser/1.0 (github.com/icyavocado/sun-chaser)")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "geocode unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		http.Error(w, "geocode read error", http.StatusBadGateway)
		return
	}

	var fc photonFeatureCollection
	if err := json.Unmarshal(body, &fc); err != nil {
		http.Error(w, "geocode parse error", http.StatusBadGateway)
		return
	}

	suggestions := make([]Suggestion, 0, len(fc.Features))
	for _, f := range fc.Features {
		suggestions = append(suggestions, Suggestion{
			DisplayName: formatDisplayName(f.Properties),
			Lat:         f.Geometry.Coordinates[1],
			Lon:         f.Geometry.Coordinates[0],
		})
	}

	out, err := h.render("suggestions", suggestions)
	if err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(out)
}

func formatDisplayName(p photonProps) string {
	parts := []string{p.Name}
	if p.State != "" && p.State != p.Name {
		parts = append(parts, p.State)
	} else if p.County != "" && p.County != p.Name {
		parts = append(parts, p.County)
	}
	if p.Country != "" {
		parts = append(parts, p.Country)
	}
	return strings.Join(parts, ", ")
}
