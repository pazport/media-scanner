package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

// writeFile creates a file and sets its modification time.
func writeFile(t *testing.T, p string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-age)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func TestFindMedia(t *testing.T) {
	root := t.TempDir()
	old := time.Hour
	for _, f := range []string{
		"Movie A (2020)/Movie A (2020).mkv",
		"Movie A (2020)/Featurettes/Making of.mkv",  // ignored folder
		"Movie A (2020)/Movie A (2020)-trailer.mp4", // ignored suffix
		"Movie A (2020)/sample.mkv",                 // ignored name
		"Movie A (2020)/Movie A (2020).nfo",         // not media
		"Movie A (2020)/.hidden.mkv",                // hidden
		".partial/Movie B.mkv",                      // hidden folder
		"Show/Season 01/Show - S01E01.MKV",          // upper-case extension
		"Music/Band/Album/01 - Track.flac",
	} {
		writeFile(t, filepath.Join(root, f), old)
	}
	writeFile(t, filepath.Join(root, "Movie C (2024)/Movie C (2024).mkv"), time.Minute) // too new

	lc := LibraryCheck{Paths: []string{root}}
	lc.normalize()
	lc.MinAgeMinutes = 10
	files, errs := findMedia(lc, time.Now(), nil)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	for i := range files {
		files[i], _ = filepath.Rel(root, files[i])
	}
	sort.Strings(files)
	want := []string{
		"Movie A (2020)/Movie A (2020).mkv",
		"Music/Band/Album/01 - Track.flac",
		"Show/Season 01/Show - S01E01.MKV",
	}
	if !reflect.DeepEqual(files, want) {
		t.Errorf("got %v\nwant %v", files, want)
	}

	// A missing folder is reported, not fatal.
	lc.Paths = []string{filepath.Join(root, "nope")}
	if _, errs := findMedia(lc, time.Now(), nil); len(errs) != 1 {
		t.Errorf("expected 1 error for missing folder, got %v", errs)
	}
}

func TestNormPath(t *testing.T) {
	cases := map[string]string{
		`D:\Media\Movies\A.mkv`: "d:/media/movies/a.mkv",
		"/data//movies/A.mkv":   "/data/movies/A.mkv",
	}
	for in, want := range cases {
		if got := normPath(in); got != want {
			t.Errorf("normPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLibraryCheck(t *testing.T) {
	// Local disk (as this container sees it): /…/media/movies and /…/media/tv
	root := t.TempDir()
	media := filepath.Join(root, "media")
	files := []string{
		"movies/Known (2020)/Known (2020).mkv",
		"movies/New (2024)/New (2024).mkv",
		"tv/Show/Season 01/S01E01.mkv",
		"tv/Show/Season 01/S01E02.mkv",
	}
	for _, f := range files {
		writeFile(t, filepath.Join(media, f), time.Hour)
	}

	// Plex sees the media under /plex. It knows Known (2020) and S01E01.
	var mu sync.Mutex
	plexScans := []string{}
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "tok" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/library/sections":
			w.Write([]byte(`{"MediaContainer":{"Directory":[
				{"key":"1","title":"Movies","type":"movie","Location":[{"path":"/plex/movies"}]},
				{"key":"2","title":"TV","type":"show","Location":[{"path":"/plex/tv"}]},
				{"key":"3","title":"Photos","type":"photo","Location":[{"path":"/plex/photos"}]}]}}`))
		case "/library/sections/1/all":
			if r.URL.Query().Get("type") != "1" {
				t.Errorf("movie section queried with type %q", r.URL.Query().Get("type"))
			}
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"file":"/plex/movies/Known (2020)/Known (2020).mkv"}]}]}]}}`))
		case "/library/sections/2/all":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"file":"/plex/tv/Show/Season 01/S01E01.mkv"}]}]}]}}`))
		case "/library/sections/3/all":
			t.Error("photo library should not be listed")
		default:
			mu.Lock()
			plexScans = append(plexScans, r.URL.Query().Get("path"))
			mu.Unlock()
		}
	}))
	defer plex.Close()

	// Jellyfin sees the media under /jf and knows everything except S01E02.
	jfItems := []string{
		"/jf/movies/Known (2020)/Known (2020).mkv",
		"/jf/movies/New (2024)/New (2024).mkv",
		"/jf/tv/Show/Season 01/S01E01.mkv",
	}
	jfScans := []string{}
	jfPages := 0
	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Items":
			mu.Lock()
			jfPages++
			mu.Unlock()
			start, _ := strconv.Atoi(r.URL.Query().Get("StartIndex"))
			limit, _ := strconv.Atoi(r.URL.Query().Get("Limit"))
			type item struct{ Path string }
			out := struct {
				Items            []item
				TotalRecordCount int
			}{Items: []item{}, TotalRecordCount: len(jfItems)}
			for i := start; i < len(jfItems) && i < start+limit; i++ {
				out.Items = append(out.Items, item{jfItems[i]})
			}
			json.NewEncoder(w).Encode(out)
		case "/Library/Media/Updated":
			var body struct{ Updates []struct{ Path string } }
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			for _, u := range body.Updates {
				jfScans = append(jfScans, u.Path)
			}
			mu.Unlock()
			w.WriteHeader(204)
		}
	}))
	defer jf.Close()

	store, err := loadStore(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.Get()
	cfg.ScanDelay = 0
	cfg.Targets = []Target{
		{ID: "p", Name: "Plex", Type: "plex", URL: plex.URL, Token: "tok", Enabled: true,
			Rewrites: []Rewrite{{From: media, To: "/plex"}}},
		{ID: "j", Name: "Jellyfin", Type: "jellyfin", URL: jf.URL, Token: "x", Enabled: true},
	}
	cfg.TypeRewrites["jellyfin"] = []Rewrite{{From: media, To: "/jf"}}
	cfg.LibraryCheck.Enabled = true
	cfg.LibraryCheck.Paths = []string{filepath.Join(media, "movies"), filepath.Join(media, "tv")}
	if err := store.Set(cfg); err != nil {
		t.Fatal(err)
	}

	defer func(n int) { pageSize = n }(pageSize)
	pageSize = 2 // Jellyfin's 3 items arrive as pages of 2 + 1

	queue := newQueue(store)
	checker := newChecker(store, queue)
	if !checker.Run() {
		t.Fatal("check did not run")
	}
	res := checker.Status()["last"].(*CheckResult)
	if res.Files != 4 || len(res.Errors) != 0 {
		t.Fatalf("files=%d errors=%v", res.Files, res.Errors)
	}
	byName := map[string]TargetCheck{}
	for _, tc := range res.Targets {
		byName[tc.Target] = tc
	}
	p, j := byName["Plex"], byName["Jellyfin"]
	if p.Error != "" || j.Error != "" {
		t.Fatalf("errors: plex=%q jellyfin=%q", p.Error, j.Error)
	}
	if p.Known != 2 || p.Missing != 2 || !reflect.DeepEqual(p.Queued, []string{
		filepath.Join(media, "movies/New (2024)"), filepath.Join(media, "tv/Show/Season 01")}) {
		t.Errorf("plex: %+v", p)
	}
	mu.Lock()
	if jfPages != 2 {
		t.Errorf("jellyfin listed in %d pages, want 2", jfPages)
	}
	mu.Unlock()
	if j.Known != 3 || j.Missing != 1 || !reflect.DeepEqual(j.Queued, []string{filepath.Join(media, "tv/Show/Season 01")}) {
		t.Errorf("jellyfin: %+v", j)
	}

	// The queued scans reach each server with its own paths.
	queue.mu.Lock()
	pending := len(queue.items)
	queue.mu.Unlock()
	if pending != 2 {
		t.Errorf("expected 2 queued folders, got %d", pending)
	}
	go queue.Run()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := len(plexScans) >= 2 && len(jfScans) >= 1
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	plexGot, jfGot := append([]string{}, plexScans...), append([]string{}, jfScans...)
	mu.Unlock()
	sort.Strings(plexGot)
	if !reflect.DeepEqual(plexGot, []string{"/plex/movies/New (2024)", "/plex/tv/Show/Season 01"}) {
		t.Errorf("plex scans: %v", plexGot)
	}
	if !reflect.DeepEqual(jfGot, []string{"/jf/tv/Show/Season 01"}) {
		t.Errorf("jellyfin scans: %v", jfGot)
	}

	// A second run inside the cooldown doesn't ask again; it reports the folders as waiting.
	checker.Run()
	res = checker.Status()["last"].(*CheckResult)
	for _, tc := range res.Targets {
		if tc.Error != "" || len(tc.Queued) != 0 || len(tc.Waiting) == 0 {
			t.Errorf("%s second run: queued=%v waiting=%v error=%q", tc.Target, tc.Queued, tc.Waiting, tc.Error)
		}
	}
}

func TestLibraryCheckConfigDefaults(t *testing.T) {
	c := &Config{}
	c.normalize()
	lc := c.LibraryCheck
	if lc.Enabled || lc.IntervalMinutes != 10 || lc.MinAgeMinutes != 10 || lc.CooldownMinutes != 360 || !lc.AllTargets {
		t.Errorf("defaults: %+v", lc)
	}
	// Settings saved from the UI are kept, including a minimum age of 0.
	c.LibraryCheck = LibraryCheck{Paths: []string{"/media/"}, Extensions: []string{"MKV"}, MinAgeMinutes: 0}
	c.normalize()
	if c.LibraryCheck.MinAgeMinutes != 0 || c.LibraryCheck.AllTargets ||
		!reflect.DeepEqual(c.LibraryCheck.Paths, []string{"/media"}) ||
		!reflect.DeepEqual(c.LibraryCheck.Extensions, []string{".mkv"}) {
		t.Errorf("kept settings: %+v", c.LibraryCheck)
	}
	c.LibraryCheck.Paths = []string{"relative/path"}
	if err := c.validate(); err == nil {
		t.Error("relative folder should be rejected")
	}
}

func TestCheckProgress(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"a/1.mkv", "a/2.mkv", "b/3.mkv"} {
		writeFile(t, filepath.Join(root, f), time.Hour)
	}

	// findMedia reports running totals per folder and a final total.
	lc := LibraryCheck{Paths: []string{root}}
	lc.normalize()
	var lastFolders, lastFiles int
	files, _ := findMedia(lc, time.Now(), func(folders, files int, _ string) { lastFolders, lastFiles = folders, files })
	if lastFolders != 3 || lastFiles != 3 || len(files) != 3 {
		t.Errorf("folders=%d files=%d found=%d", lastFolders, lastFiles, len(files))
	}

	// A Jellyfin that pauses on its second page, so the progress can be read mid-check.
	defer func(n int) { pageSize = n }(pageSize)
	pageSize = 2
	paused, release := make(chan struct{}), make(chan struct{})
	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		all := []string{"/x/a/1.mkv", "/x/a/2.mkv", "/x/b/3.mkv"}
		start, _ := strconv.Atoi(r.URL.Query().Get("StartIndex"))
		if start > 0 {
			close(paused)
			<-release
		}
		type item struct{ Path string }
		out := struct {
			Items            []item
			TotalRecordCount int
		}{Items: []item{}, TotalRecordCount: len(all)}
		for i := start; i < len(all) && i < start+2; i++ {
			out.Items = append(out.Items, item{all[i]})
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer jf.Close()

	store, err := loadStore(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.Get()
	cfg.Targets = []Target{{ID: "j", Name: "Jellyfin", Type: "jellyfin", URL: jf.URL, Enabled: true,
		Rewrites: []Rewrite{{From: root, To: "/x"}}}}
	cfg.LibraryCheck.Paths = []string{root}
	cfg.LibraryCheck.MinAgeMinutes = 0
	if err := store.Set(cfg); err != nil {
		t.Fatal(err)
	}
	checker := newChecker(store, newQueue(store))
	done := make(chan bool)
	go func() { done <- checker.Run() }()

	<-paused
	st := checker.Status()
	p, ok := st["progress"].(*CheckProgress)
	if !ok || st["running"] != true {
		t.Fatalf("expected progress while running, got %+v", st)
	}
	if p.Step != "server" || p.Server != "Jellyfin" || p.Index != 1 || p.Servers != 1 ||
		p.Items != 2 || p.Total != 3 || p.Files != 3 || p.Folders != 3 {
		t.Errorf("progress mid-check: %+v", *p)
	}
	close(release)
	<-done
	if _, ok := checker.Status()["progress"]; ok {
		t.Error("progress should not be reported after the check finished")
	}
	if res := checker.Status()["last"].(*CheckResult); res.Targets[0].Known != 3 || res.Targets[0].Missing != 0 {
		t.Errorf("result: %+v", res.Targets[0])
	}
}
