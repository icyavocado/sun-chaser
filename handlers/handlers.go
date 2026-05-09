package handlers

import (
	"html/template"
	"log"
	"time"

	"github.com/icyavocado/calcbright/brightness"
	"github.com/icyavocado/sun-chaser/db"
)

// Handler holds shared server-wide state for all HTTP handlers.
type Handler struct {
	db                *db.DB
	owmClient         *brightness.OWMClient
	photonBase        string
	geocodeRL         *ipRateLimiter
	tmpls             map[string]*template.Template
	calcbrightVersion string
}

// New constructs a Handler, parses all templates, and wires up shared state.
// owmClient and calcbrightVersion are created in main and shared with the
// background worker so both use the same cache and version label.
func New(database *db.DB, owmClient *brightness.OWMClient, photonBase, calcbrightVersion string) *Handler {
	h := &Handler{
		db:                database,
		owmClient:         owmClient,
		photonBase:        photonBase,
		geocodeRL:         newIPRateLimiter(10, time.Minute),
		tmpls:             make(map[string]*template.Template),
		calcbrightVersion: calcbrightVersion,
	}
	h.parseTemplates()
	return h
}

func (h *Handler) parseTemplates() {
	// Full pages: layout wraps a content block.
	// empty.html must be included here because index.html calls {{template "empty" .}}.
	h.tmpls["index"] = template.Must(
		template.ParseFiles(
			"templates/layout.html",
			"templates/index.html",
			"templates/partials/empty.html",
		),
	)

	// HTMX partials: rendered stand-alone.
	h.tmpls["suggestions"] = template.Must(
		template.ParseFiles("templates/partials/suggestions.html"),
	)
	h.tmpls["results"] = template.Must(
		template.ParseFiles("templates/partials/results.html"),
	)
	h.tmpls["empty"] = template.Must(
		template.ParseFiles("templates/partials/empty.html"),
	)
	h.tmpls["chart"] = template.Must(
		template.ParseFiles("templates/partials/chart.html"),
	)
}

func (h *Handler) render(name string, data any) ([]byte, error) {
	tmpl, ok := h.tmpls[name]
	if !ok {
		return nil, nil
	}
	var buf []byte
	w := &byteWriter{buf: &buf}
	// Full pages execute the "layout" template; partials use their own name.
	entryPoint := name
	if name == "index" {
		entryPoint = "layout"
	}
	if err := tmpl.ExecuteTemplate(w, entryPoint, data); err != nil {
		log.Printf("template %s: %v", name, err)
		return nil, err
	}
	return buf, nil
}

// byteWriter satisfies io.Writer and collects into a *[]byte.
type byteWriter struct{ buf *[]byte }

func (w *byteWriter) Write(p []byte) (int, error) {
	*w.buf = append(*w.buf, p...)
	return len(p), nil
}
