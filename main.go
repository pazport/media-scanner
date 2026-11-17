package main

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

//go:embed web
var webFS embed.FS

var version = "dev"

type App struct {
	store   *Store
	queue   *Queue
	checker *Checker
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

	queue := newQueue(store)
	app := &App{store: store, queue: queue, checker: newChecker(store, queue)}
	go app.queue.Run()
	go app.checker.Loop()

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

	mux.HandleFunc("GET /api/check", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, a.checker.Status())
	})
	mux.HandleFunc("POST /api/check/run", func(w http.ResponseWriter, r *http.Request) {
		if len(a.store.Get().LibraryCheck.Paths) == 0 {
			writeJSON(w, 400, map[string]string{"error": "add at least one folder to check first"})
			return
		}
		go a.checker.Run(r.URL.Query().Get("full") == "1")
		writeJSON(w, 200, map[string]string{"status": "started"})
	})

	// Broken symlinks found by the library check.
	mux.HandleFunc("GET /api/broken", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		offset, _ := strconv.Atoi(q.Get("offset"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		writeJSON(w, 200, a.checker.BrokenList(q.Get("group"), q.Get("q"), offset, limit))
	})
	mux.HandleFunc("GET /api/broken.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="broken-symlinks.txt"`)
		io.WriteString(w, a.checker.BrokenText())
	})
	type brokenReq struct {
		Paths []string `json:"paths"`
		Group string   `json:"group"`
		All   bool     `json:"all"`
		Force bool     `json:"force"`
	}
	readBroken := func(w http.ResponseWriter, r *http.Request) (brokenReq, bool) {
		var req brokenReq
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
			return req, false
		}
		if !req.All && req.Group == "" && len(req.Paths) == 0 {
			writeJSON(w, 400, map[string]string{"error": "nothing selected"})
			return req, false
		}
		return req, true
	}
	mux.HandleFunc("POST /api/broken/test", func(w http.ResponseWriter, r *http.Request) {
		req, ok := readBroken(w, r)
		if !ok {
			return
		}
		res, err := a.checker.TestBroken(req.Paths, req.Group, req.All)
		if err != nil {
			writeJSON(w, 409, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, res)
	})
	mux.HandleFunc("POST /api/broken/delete", func(w http.ResponseWriter, r *http.Request) {
		req, ok := readBroken(w, r)
		if !ok {
			return
		}
		res, err := a.checker.DeleteBroken(req.Paths, req.Group, req.All, req.Force)
		if err != nil {
			writeJSON(w, 409, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, res)
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
