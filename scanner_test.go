package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestApplyRewrites(t *testing.T) {
	rws := []Rewrite{{From: "/data/movies/", To: "/movies"}}
	cases := map[string]string{
		"/data/movies/A (2020)": "/movies/A (2020)",
		"/data/movies":          "/movies",
		"/data/moviesX/A":       "/data/moviesX/A",
		"/tv/Show":              "/tv/Show",
	}
	for in, want := range cases {
		if got := applyRewrites(in, rws); got != want {
			t.Errorf("applyRewrites(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPathsFor(t *testing.T) {
	parse := func(s string) arrPayload {
		var p arrPayload
		if err := json.Unmarshal([]byte(s), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		source, body string
		want         []string
	}{
		{"radarr", `{"eventType":"Download","movie":{"folderPath":"/movies/A (2020)"}}`, []string{"/movies/A (2020)"}},
		{"radarr", `{"eventType":"MovieDelete","movie":{"folderPath":"/movies/A (2020)"}}`, []string{"/movies"}},
		{"sonarr", `{"eventType":"Download","series":{"path":"/tv/Show"},"episodeFile":{"relativePath":"Season 01/S01E01.mkv"}}`, []string{"/tv/Show/Season 01"}},
		{"sonarr", `{"eventType":"Download","series":{"path":"/tv/Show"},"episodeFiles":[{"relativePath":"Season 01/a.mkv"},{"relativePath":"Season 02/b.mkv"}]}`, []string{"/tv/Show/Season 01", "/tv/Show/Season 02"}},
		{"sonarr", `{"eventType":"Rename","series":{"path":"/tv/Show"}}`, []string{"/tv/Show"}},
		{"sonarr", `{"eventType":"SeriesDelete","series":{"path":"/tv/Show"}}`, []string{"/tv"}},
		{"lidarr", `{"eventType":"Download","artist":{"path":"/music/Band"},"trackFiles":[{"path":"/music/Band/Album/01.flac"}]}`, []string{"/music/Band/Album"}},
	}
	for _, c := range cases {
		got := pathsFor(c.source, parse(c.body))
		sort.Strings(got)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %s: got %v want %v", c.source, c.body, got, c.want)
		}
	}
}

type fakeServer struct {
	mu   sync.Mutex
	hits []string
	*httptest.Server
}

func (f *fakeServer) record(s string) { f.mu.Lock(); f.hits = append(f.hits, s); f.mu.Unlock() }
func (f *fakeServer) get() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.hits...)
}

func newFakePlex() *fakeServer {
	f := &fakeServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "plextoken" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.URL.Path == "/library/sections":
			io.WriteString(w, `{"MediaContainer":{"Directory":[
				{"key":"1","title":"Movies","Location":[{"path":"/plex/movies"}]},
				{"key":"2","title":"TV","Location":[{"path":"/plex/tv"}]}]}}`)
		case strings.HasSuffix(r.URL.Path, "/refresh"):
			f.record(r.URL.Path + "?" + r.URL.Query().Get("path"))
		}
	}))
	return f
}

func newFakeEmby(header, token string) *fakeServer {
	f := &fakeServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get(header), token) {
			w.WriteHeader(401)
			return
		}
		var body struct{ Updates []struct{ Path string } }
		json.NewDecoder(r.Body).Decode(&body)
		for _, u := range body.Updates {
			f.record(u.Path)
		}
		w.WriteHeader(204)
	}))
	return f
}

func TestEndToEnd(t *testing.T) {
	plex, jf, emby := newFakePlex(), newFakeEmby("Authorization", "jftoken"), newFakeEmby("X-Emby-Token", "embytoken")
	defer plex.Close()
	defer jf.Close()
	defer emby.Close()

	store, err := loadStore(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.Get()
	cfg.ScanDelay = 0
	cfg.Auth = Auth{Username: "u", Password: "p"}
	cfg.Rewrites = []Rewrite{{From: "/data", To: "/media"}}
	cfg.Targets = []Target{
		{ID: "p", Name: "Plex", Type: "plex", URL: plex.URL, Token: "plextoken", Enabled: true,
			Rewrites: []Rewrite{{From: "/media", To: "/plex"}}},
		{ID: "j", Name: "Jellyfin", Type: "jellyfin", URL: jf.URL, Token: "jftoken", Enabled: true},
		{ID: "e", Name: "Emby", Type: "emby", URL: emby.URL, Token: "embytoken", Enabled: true},
	}
	// Sonarr only goes to Jellyfin.
	cfg.Triggers["sonarr"].AllTargets = false
	cfg.Triggers["sonarr"].Targets = []string{"j"}
	if err := store.Set(cfg); err != nil {
		t.Fatal(err)
	}

	app := &App{store: store, queue: newQueue(store)}
	go app.queue.Run()
	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	post := func(path, body string, auth bool) int {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if auth {
			req.SetBasicAuth("u", "p")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := post("/triggers/radarr", `{}`, false); code != 401 {
		t.Fatalf("expected 401 without auth, got %d", code)
	}
	if code := post("/triggers/radarr", `{"eventType":"Test"}`, true); code != 200 {
		t.Fatalf("test event: %d", code)
	}
	post("/triggers/radarr", `{"eventType":"Download","movie":{"folderPath":"/data/movies/A (2020)"}}`, true)
	post("/triggers/sonarr", `{"eventType":"Download","series":{"path":"/data/tv/Show"},"episodeFile":{"relativePath":"Season 01/x.mkv"}}`, true)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (len(plex.get()) < 1 || len(jf.get()) < 2 || len(emby.get()) < 1) {
		time.Sleep(100 * time.Millisecond)
	}
	if got := plex.get(); !reflect.DeepEqual(got, []string{"/library/sections/1/refresh?/plex/movies/A (2020)"}) {
		t.Errorf("plex hits: %v", got)
	}
	jfHits := jf.get()
	sort.Strings(jfHits)
	if !reflect.DeepEqual(jfHits, []string{"/media/movies/A (2020)", "/media/tv/Show/Season 01"}) {
		t.Errorf("jellyfin hits: %v", jfHits)
	}
	if got := emby.get(); !reflect.DeepEqual(got, []string{"/media/movies/A (2020)"}) {
		t.Errorf("emby hits: %v", got)
	}

	// Test-connection endpoint
	for _, tg := range store.Get().Targets {
		if msg, err := tg.Test(); err != nil && tg.Type == "plex" {
			t.Errorf("plex test: %v", err)
		} else if tg.Type == "plex" && !strings.Contains(msg, "Movies") {
			t.Errorf("plex test msg: %s", msg)
		}
	}
}
