package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestQueueProcessSkipsScanWhenNoMediaFileExists(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "readme.nfo"), []byte("not media"), 0644); err != nil { t.Fatal(err) }
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer server.Close()
	store, err := loadStore(filepath.Join(t.TempDir(), "config.json"))
	if err != nil { t.Fatal(err) }
	cfg := store.Get()
	cfg.Targets = []Target{{ID:"j", Name:"Jellyfin", Type:"jellyfin", URL:server.URL, Enabled:true}}
	if err := store.Set(cfg); err != nil { t.Fatal(err) }
	q := newQueue(store)
	q.fileCheckDelay = 0
	q.process(&pending{path:root, source:"manual", all:true, targets:map[string]bool{}})
	if hits != 0 { t.Fatalf("expected no server request, got %d", hits) }
	h := q.History()
	if len(h) != 1 || h[0].Status != "skipped" { t.Fatalf("expected skipped event, got %#v", h) }
}

func TestQueueProcessScansSelectedServersWhenMediaExists(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Movie.mkv"), []byte("media"), 0644); err != nil { t.Fatal(err) }
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Library/Media/Updated" { t.Errorf("unexpected path %s", r.URL.Path) }
		var body struct { Updates []struct { Path string } }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil { t.Errorf("decode request: %v", err) }
		if len(body.Updates) != 1 || body.Updates[0].Path != root { t.Errorf("unexpected scan payload: %#v", body) }
		mu.Lock(); requests++; mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	store, err := loadStore(filepath.Join(t.TempDir(), "config.json"))
	if err != nil { t.Fatal(err) }
	cfg := store.Get()
	cfg.Targets = []Target{
		{ID:"j", Name:"Jellyfin", Type:"jellyfin", URL:server.URL, Enabled:true},
		{ID:"disabled", Name:"Disabled", Type:"emby", URL:server.URL, Enabled:false},
	}
	if err := store.Set(cfg); err != nil { t.Fatal(err) }
	q := newQueue(store)
	q.fileCheckDelay = 0
	q.process(&pending{path:root, source:"manual", all:true, targets:map[string]bool{}})
	mu.Lock(); got := requests; mu.Unlock()
	if got != 1 { t.Fatalf("expected one scan request to enabled server, got %d", got) }
	h := q.History()
	if len(h) != 1 || h[0].Status != "ok" { t.Fatalf("expected successful event, got %#v", h) }
}

func TestQueueProcessWaitsBeforeFilesystemCheck(t *testing.T) {
	root := t.TempDir()
	q := &Queue{store:&Store{cfg:defaultConfig()}, items:map[string]*pending{}, stats:map[string]int{}, fileCheckDelay:20*time.Millisecond}
	start := time.Now()
	q.process(&pending{path:root, source:"manual", targets:map[string]bool{}})
	if time.Since(start) < 20*time.Millisecond { t.Fatal("filesystem check ran before delay") }
}


func TestQueueProcessChecksMountedPathButScansCanonicalPath(t *testing.T) {
	mounted := t.TempDir()
	if err := os.WriteFile(filepath.Join(mounted, "Episode.mkv"), []byte("media"), 0644); err != nil {
		t.Fatal(err)
	}
	const canonical = "/content-vault/content/TV-Kids/Example/Season 02"
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct { Updates []struct { Path string } }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil { t.Errorf("decode request: %v", err) }
		if len(body.Updates) == 1 { gotPath = body.Updates[0].Path }
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	store, err := loadStore(filepath.Join(t.TempDir(), "config.json"))
	if err != nil { t.Fatal(err) }
	cfg := store.Get()
	cfg.Targets = []Target{{ID:"j", Name:"Jellyfin", Type:"jellyfin", URL:server.URL, Enabled:true}}
	if err := store.Set(cfg); err != nil { t.Fatal(err) }
	q := newQueue(store)
	q.fileCheckDelay = 0
	q.process(&pending{path:canonical, checkPath:mounted, source:"check", all:true, targets:map[string]bool{}})
	if gotPath != canonical { t.Fatalf("server scan path = %q, want %q", gotPath, canonical) }
	h := q.History()
	if len(h) != 1 || h[0].Status != "ok" { t.Fatalf("expected successful scan, got %#v", h) }
}
