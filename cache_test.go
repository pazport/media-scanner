package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countReads wraps readDir to count folder listings.
func countReads(t *testing.T) *int64 {
	t.Helper()
	var n int64
	orig := readDir
	readDir = func(p string) ([]os.DirEntry, error) {
		atomic.AddInt64(&n, 1)
		return orig(p)
	}
	t.Cleanup(func() { readDir = orig })
	return &n
}

func TestFolderCache(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"Show A/Season 01/E01.mkv", "Show A/Season 01/E02.mkv", "Show B/Season 01/E01.mkv"} {
		writeFile(t, filepath.Join(root, f), time.Hour)
	}
	// A link whose target doesn't exist yet (e.g. a remote mount that isn't up).
	target := filepath.Join(t.TempDir(), "later.mkv")
	if err := os.Symlink(target, filepath.Join(root, "Show B/Season 01/E02.mkv")); err != nil {
		t.Fatal(err)
	}
	lc := LibraryCheck{Paths: []string{root}}
	lc.normalize()
	reads := countReads(t)

	first := findMedia(lc, nil, nil)
	if first.Folders != 5 || first.Cached != 0 || *reads != 5 || len(first.Files) != 3 || first.Broken != 1 {
		t.Fatalf("first walk: folders=%d cached=%d reads=%d files=%v broken=%d",
			first.Folders, first.Cached, *reads, first.Files, first.Broken)
	}

	// Nothing changed: every folder comes from the cache, none is listed again.
	*reads = 0
	second := findMedia(lc, first.Cache, nil)
	if *reads != 0 || second.Cached != 5 || !reflect.DeepEqual(second.Files, first.Files) {
		t.Errorf("second walk: reads=%d cached=%d files=%v", *reads, second.Cached, second.Files)
	}

	// A new episode changes its folder's modified time: only that folder is listed again.
	writeFile(t, filepath.Join(root, "Show A/Season 01/E03.mkv"), time.Hour)
	season := filepath.Join(root, "Show A/Season 01")
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(season, later, later); err != nil {
		t.Fatal(err)
	}
	// The broken link's target appears; its folder is unchanged, so it comes from the cache.
	writeFile(t, target, time.Hour)
	*reads = 0
	third := findMedia(lc, second.Cache, nil)
	if *reads != 1 || third.Cached != 4 || len(third.Files) != 5 || third.Broken != 0 {
		t.Errorf("third walk: reads=%d cached=%d files=%v broken=%d", *reads, third.Cached, third.Files, third.Broken)
	}
	// The first walk's cache wasn't changed by later walks.
	for _, e := range first.Cache[filepath.Join(root, "Show B/Season 01")].Entries {
		if e.Name == "E02.mkv" && e.Kind != kindLinkBroken {
			t.Errorf("previous cache was modified: %+v", e)
		}
	}
}

// fakeJellyfin serves a list of items; requests with MinDateLastSaved get
// only the "changed" items (or fail, if failChanges is set).
type fakeJellyfin struct {
	*httptest.Server
	mu          sync.Mutex
	all         []string
	changed     []string
	failChanges bool
	requests    []string // MinDateLastSaved of each /Items request ("" for a full list)
}

func newFakeJellyfin(all, changed []string) *fakeJellyfin {
	f := &fakeJellyfin{all: all, changed: changed}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Items" {
			w.WriteHeader(204)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		since := r.URL.Query().Get("MinDateLastSaved")
		f.requests = append(f.requests, since)
		items := f.all
		if since != "" {
			if f.failChanges {
				w.WriteHeader(400)
				return
			}
			items = f.changed
		}
		type item struct{ Path string }
		out := struct {
			Items            []item
			TotalRecordCount int
		}{Items: []item{}, TotalRecordCount: len(items)}
		for _, p := range items {
			out.Items = append(out.Items, item{p})
		}
		json.NewEncoder(w).Encode(out)
	}))
	return f
}

func (f *fakeJellyfin) reqs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.requests...)
}

func cacheTestChecker(t *testing.T, root string, jf *fakeJellyfin) (*Checker, *Store) {
	t.Helper()
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
	return newChecker(store, newQueue(store)), store
}

func lastResult(c *Checker) *CheckResult { return c.Status()["last"].(*CheckResult) }

func TestServerCache(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"A/1.mkv", "A/2.mkv", "B/3.mkv"} {
		writeFile(t, filepath.Join(root, f), time.Hour)
	}
	// The server has 1 and 2; 3 is added later and reported as a change.
	jf := newFakeJellyfin([]string{"/x/A/1.mkv", "/x/A/2.mkv"}, []string{"/x/B/3.mkv"})
	defer jf.Close()
	c, _ := cacheTestChecker(t, root, jf)

	c.Run(false)
	r := lastResult(c).Targets[0]
	if !r.Full || r.Known != 2 || r.Missing != 1 || len(jf.reqs()) != 1 || jf.reqs()[0] != "" {
		t.Fatalf("first check: %+v requests=%q", r, jf.reqs())
	}

	// Next check: only changes are asked for, and merged into the stored list.
	c.run(false, false)
	r = lastResult(c).Targets[0]
	reqs := jf.reqs()
	if r.Full || r.Fetched != 1 || r.Known != 3 || r.Missing != 0 || len(reqs) != 2 || reqs[1] == "" {
		t.Errorf("second check: %+v requests=%q", r, reqs)
	}
	if since, err := time.Parse(time.RFC3339, reqs[1]); err != nil || time.Since(since) < syncOverlap {
		t.Errorf("changes requested since %q (%v): want at least %s ago", reqs[1], err, syncOverlap)
	}

	// A full check fetches the whole list again.
	c.Run(true)
	if r = lastResult(c).Targets[0]; !r.Full || jf.reqs()[2] != "" {
		t.Errorf("full check: %+v requests=%q", r, jf.reqs())
	}

	// A server that refuses the changes request gets a full list instead.
	jf.mu.Lock()
	jf.failChanges = true
	jf.mu.Unlock()
	c.run(false, false)
	r = lastResult(c).Targets[0]
	reqs = jf.reqs()
	if r.Error != "" || !r.Full || len(reqs) != 5 || reqs[3] == "" || reqs[4] != "" {
		t.Errorf("fallback: %+v requests=%q", r, reqs)
	}
}

func TestCheckReusesWalk(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "A/1.mkv"), time.Hour)
	jf := newFakeJellyfin([]string{"/x/A/1.mkv"}, nil)
	defer jf.Close()
	c, store := cacheTestChecker(t, root, jf)
	reads := countReads(t)

	c.Run(false)
	if res := lastResult(c); !res.Walked || res.Files != 1 || *reads == 0 {
		t.Fatalf("first check: walked=%v files=%d reads=%d", res.Walked, res.Files, *reads)
	}
	// A server-only check reuses the last walk without reading any folder.
	*reads = 0
	c.run(false, false)
	if res := lastResult(c); res.Walked || res.Files != 1 || *reads != 0 || res.WalkedAt.IsZero() {
		t.Errorf("server-only check: walked=%v files=%d reads=%d", res.Walked, res.Files, *reads)
	}
	// Changing what counts as media forces a fresh walk, and doesn't use the old cache.
	cfg := store.Get()
	cfg.LibraryCheck.Extensions = []string{".mp4"}
	if err := store.Set(cfg); err != nil {
		t.Fatal(err)
	}
	*reads = 0
	c.run(false, false)
	if res := lastResult(c); !res.Walked || res.Files != 0 || res.Cached != 0 || *reads == 0 {
		t.Errorf("after settings change: walked=%v files=%d cached=%d reads=%d", res.Walked, res.Files, res.Cached, *reads)
	}
}

func TestPlexChangesQuery(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/sections":
			w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","title":"TV","type":"show","Location":[{"path":"/tv"}]}]}}`))
		default:
			mu.Lock()
			queries = append(queries, r.URL.RawQuery)
			mu.Unlock()
			w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
		}
	}))
	defer plex.Close()
	tg := Target{Type: "plex", URL: plex.URL, Token: "x"}
	if _, err := tg.KnownFiles([]string{"/tv"}, time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	since := time.Unix(1700000000, 0)
	if _, err := tg.KnownFiles([]string{"/tv"}, since, nil); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 2 || strings.Contains(queries[0], "updatedAt") ||
		!strings.Contains(queries[1], "updatedAt>>=1700000000") || !strings.Contains(queries[1], "type=4") {
		t.Errorf("queries: %q", queries)
	}
}
