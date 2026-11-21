package main

import (
	"encoding/json"
	"net/http"
	"path"
	"strings"
)

// arrPayload covers the fields we need from Radarr, Sonarr and Lidarr webhooks.
type arrPayload struct {
	EventType string `json:"eventType"`
	Movie     *struct {
		FolderPath string `json:"folderPath"`
		Path       string `json:"path"`
	} `json:"movie"`
	Series *struct {
		Path string `json:"path"`
	} `json:"series"`
	Artist *struct {
		Path string `json:"path"`
	} `json:"artist"`
	EpisodeFile  *arrFile  `json:"episodeFile"`
	EpisodeFiles []arrFile `json:"episodeFiles"`
	TrackFiles   []arrFile `json:"trackFiles"`
	RenamedFiles []arrFile `json:"renamedEpisodeFiles"`
}

type arrFile struct {
	Path         string `json:"path"`
	RelativePath string `json:"relativePath"`
}

func clean(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "" {
		return ""
	}
	return path.Clean(p)
}

// pathsFor works out which folders to scan for a webhook event.
func pathsFor(source string, p arrPayload) []string {
	set := map[string]bool{}
	add := func(s string) {
		if s = clean(s); s != "" && s != "." && s != "/" {
			set[s] = true
		}
	}
	deleted := strings.HasSuffix(p.EventType, "Delete") && !strings.Contains(p.EventType, "File")

	switch source {
	case "radarr":
		if p.Movie != nil {
			dir := p.Movie.FolderPath
			if dir == "" {
				dir = p.Movie.Path
			}
			if deleted {
				dir = path.Dir(clean(dir))
			}
			add(dir)
		}
	case "sonarr":
		if p.Series == nil {
			break
		}
		root := clean(p.Series.Path)
		if deleted {
			add(path.Dir(root))
			break
		}
		files := append([]arrFile{}, p.EpisodeFiles...)
		if p.EpisodeFile != nil {
			files = append(files, *p.EpisodeFile)
		}
		for _, f := range files {
			if f.RelativePath != "" {
				add(path.Dir(path.Join(root, clean(f.RelativePath))))
			} else if f.Path != "" {
				add(path.Dir(clean(f.Path)))
			}
		}
		if len(set) == 0 {
			add(root)
		}
	case "lidarr":
		if p.Artist == nil {
			break
		}
		root := clean(p.Artist.Path)
		if deleted {
			add(path.Dir(root))
			break
		}
		for _, f := range p.TrackFiles {
			if f.Path != "" {
				add(path.Dir(clean(f.Path)))
			}
		}
		if len(set) == 0 {
			add(root)
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	return out
}

// Events that mean files on disk changed.
var scanEvents = map[string]bool{
	"Download": true, "Rename": true, "MovieDelete": true, "MovieFileDelete": true,
	"SeriesDelete": true, "EpisodeFileDelete": true,
	"AlbumDelete": true, "ArtistDelete": true, "TrackRetag": true, "TrackFileDelete": true,
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *App) handleArr(source string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := a.store.Get()
		trig := cfg.Triggers[source]
		if !trig.Enabled {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": source + " trigger is disabled"})
			return
		}
		var p arrPayload
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&p); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		if p.EventType == "Test" {
			a.queue.record(Event{Source: source, Status: "ok", Message: "test webhook received"})
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		if !scanEvents[p.EventType] {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "event": p.EventType})
			return
		}
		paths := pathsFor(source, p)
		for _, pth := range paths {
			a.enqueue(source, pth, cfg, trig)
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "queued", "paths": paths})
	}
}

func (a *App) enqueue(source, pth string, cfg *Config, trig *Trigger) {
	// Trigger rewrites map the source application's path to the location visible
	// inside this container. Preserve that path for the local filesystem check.
	// Global rewrites then map it to the canonical path expected by the media
	// servers, and must not be used to probe the filesystem.
	checkPath := applyRewrites(pth, trig.Rewrites)
	scanPath := applyRewrites(checkPath, cfg.Rewrites)
	a.queue.AddWithCheckPath(source, scanPath, checkPath, trig.AllTargets, trig.Targets)
}

// handleManual accepts ?path=/a&path=/b or a JSON body {"paths": [...], "targets": [...]}.
func (a *App) handleManual(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()
	trig := *cfg.Triggers["manual"]
	if !trig.Enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "manual trigger is disabled"})
		return
	}
	paths := r.URL.Query()["path"]
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Paths   []string `json:"paths"`
			Targets []string `json:"targets"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		paths = append(paths, body.Paths...)
		if len(body.Targets) > 0 {
			trig.AllTargets, trig.Targets = false, body.Targets
		}
	}
	var queued []string
	for _, p := range paths {
		if p = clean(strings.TrimSpace(p)); p != "" && p != "." {
			a.enqueue("manual", p, cfg, &trig)
			queued = append(queued, p)
		}
	}
	if len(queued) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no paths given"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "queued", "paths": queued})
}
