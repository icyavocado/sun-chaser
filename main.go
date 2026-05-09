package main

import (
	"bufio"
	"context"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/icyavocado/calcbright/brightness"
	"github.com/icyavocado/sun-chaser/db"
	"github.com/icyavocado/sun-chaser/handlers"
	"github.com/icyavocado/sun-chaser/worker"
)

// loadEnvFile reads KEY=VALUE pairs from path and sets them as environment
// variables. Lines beginning with # and blank lines are ignored. Variables
// already present in the environment are never overwritten, so real env vars
// always take precedence over the file.
func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // file absent — silently skip
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			continue
		}
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}
}

// calcbrightVersion returns the git short-hash of the calcbright module
// directory, falling back to the module version recorded in the binary's
// build info, and finally to "unknown".
func calcbrightVersion() string {
	// Try to read the git hash of the local replace-path first.
	// This works when the binary is run from the sun-chaser directory.
	if out, err := exec.Command("git", "-C", "../calcbright", "rev-parse", "--short", "HEAD").Output(); err == nil {
		if hash := strings.TrimSpace(string(out)); hash != "" {
			return hash
		}
	}
	// Fall back to what the Go toolchain embedded in the binary.
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/icyavocado/calcbright" {
				if dep.Replace != nil {
					return dep.Replace.Version
				}
				return dep.Version
			}
		}
	}
	return "unknown"
}

func main() {
	loadEnvFile(".env.development")

	owmKey := os.Getenv("OPENWEATHERMAP_API_KEY")
	if owmKey == "" {
		log.Fatal("OPENWEATHERMAP_API_KEY environment variable is required")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "./sunchaser.db"
	}

	photonBase := os.Getenv("PHOTON_BASE_URL")
	if photonBase == "" {
		photonBase = "https://photon.komoot.io"
	}

	collectInterval := 30 * time.Minute
	if v := os.Getenv("COLLECT_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			collectInterval = d
		} else {
			log.Printf("invalid COLLECT_INTERVAL %q, using 30m", v)
		}
	}

	database, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer database.Close()

	cbVersion := calcbrightVersion()
	log.Printf("calcbright version: %s", cbVersion)

	// Shared OWM client — used by both the HTTP handlers and the background
	// worker so they hit the same in-memory cache.
	owmClient := brightness.NewOWMClient(owmKey, 10*time.Minute, 5*time.Second)

	h := handlers.New(database, owmClient, photonBase, cbVersion)

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/", h.Index)
	r.Get("/geocode", h.Geocode)
	r.Post("/analyze", h.Analyze)
	r.Get("/history", h.History)
	r.Get("/chart", h.Chart)
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	// Background collection worker — runs immediately on startup then on each tick.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	w := worker.New(database, owmClient, collectInterval, cbVersion)
	go w.Run(ctx)

	srv := &http.Server{Addr: ":" + port, Handler: r}
	go func() {
		<-ctx.Done()
		log.Println("shutting down…")
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()

	log.Printf("sun-chaser listening on :%s (collect every %s)", port, collectInterval)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
