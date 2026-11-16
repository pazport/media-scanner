package main

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"
)

//go:embed web
var webFS embed.FS

var version = "dev"

type App struct {
	store *Store
	queue *Queue
}

func main() {
	defPath := os.Getenv("MEDIA_SCANNER_CONFIG")
	if defPath == "" {
		defPath = "/config/config.json"
	}
	cfgPath := flag.String("config", defPath, "path to config file")
	flag.Parse()

	store, err := loadStore(*cfgPath)
	if flag.Arg(0) == "healthcheck" {
		os.Exit(healthcheck(store, err))
	}
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	app := &App{store: store, queue: newQueue(store)}
	go app.queue.Run()

	cfg := store.Get()
	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("media-scanner %s listening on %s (config: %s)", version, addr, *cfgPath)
	srv := &http.Server{Addr: addr, Handler: app.routes(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// healthcheck is used by the Docker HEALTHCHECK (the scratch image has no curl).
func healthcheck(store *Store, err error) int {
	if err != nil {
		return 1
	}
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", store.Get().Port))
	if err != nil || resp.StatusCode != 200 {
		return 1
	}
	return 0
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})

	// Webhook triggers
	mux.HandleFunc("POST /triggers/radarr", a.handleArr("radarr"))
	mux.HandleFunc("POST /triggers/sonarr", a.handleArr("sonarr"))
	mux.HandleFunc("POST /triggers/lidarr", a.handleArr("lidarr"))
	mux.HandleFunc("POST /triggers/manual", a.handleManual)

	// Web UI API
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, a.store.Get())
	})
	mux.HandleFunc("PUT /api/config", func(w http.ResponseWriter, r *http.Request) {
		old := a.store.Get()
		cfg := &Config{}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(cfg); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		if err := a.store.Set(cfg); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		resp := map[string]any{"status": "saved", "config": a.store.Get()}
		if old.Port != cfg.Port {
			resp["notice"] = "Port change takes effect after restarting the container."
		}
		writeJSON(w, 200, resp)
	})
	mux.HandleFunc("POST /api/test", func(w http.ResponseWriter, r *http.Request) {
		var t Target
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&t); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
			return
		}
		c := Config{Targets: []Target{t}}
		c.normalize()
		msg, err := c.Targets[0].Test()
		if err != nil {
			writeJSON(w, 200, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "message": msg})
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"version": version,
			"pending": a.queue.Pending(),
			"history": a.queue.History(),
			"stats":   a.queue.Stats(),
		})
	})

	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(sub))

	return a.auth(mux)
}

// auth applies HTTP basic auth to everything except /health when a username is set.
// Radarr/Sonarr/Lidarr webhooks support basic auth via their Username/Password fields.
func (a *App) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := a.store.Get()
		if cfg.Auth.Username == "" || r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		u, p, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(u), []byte(cfg.Auth.Username)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(cfg.Auth.Password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="media-scanner"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
