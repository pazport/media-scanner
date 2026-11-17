package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TargetCheck is the result of comparing disk with one media server.
type TargetCheck struct {
	Target  string   `json:"target"`
	Type    string   `json:"type"`
	Known   int      `json:"known"`   // media files the server reports
	Missing int      `json:"missing"` // files on disk the server doesn't have
	Queued  []string `json:"queued"`  // folders a scan was requested for
	Waiting []string `json:"waiting"` // folders still missing but in cooldown
	Error   string   `json:"error,omitempty"`
}

// CheckResult describes one library check run.
type CheckResult struct {
	Started  time.Time     `json:"started"`
	Duration string        `json:"duration"`
	Files    int           `json:"files"` // media files found on disk
	Errors   []string      `json:"errors"`
	Targets  []TargetCheck `json:"targets"`
}

// CheckProgress describes a check that is running, for the web UI.
type CheckProgress struct {
	Started time.Time `json:"started"`
	Step    string    `json:"step"`    // "files" while walking folders, "server" while asking a server
	Folders int       `json:"folders"` // folders looked through so far
	Files   int       `json:"files"`   // media files found so far
	Current string    `json:"current"` // folder being walked, or server library being listed
	Server  string    `json:"server"`  // server being asked
	Type    string    `json:"type"`
	Index   int       `json:"index"` // 1-based number of the server being asked
	Servers int       `json:"servers"`
	Items   int       `json:"items"` // items received from the current server (library)
	Total   int       `json:"total"` // total the server reported, 0 if unknown
}

// Checker runs the periodic library check.
type Checker struct {
	store *Store
	queue *Queue

	mu       sync.Mutex
	running  bool
	progress CheckProgress
	last     *CheckResult
	lastRun  time.Time
	cooldown map[string]time.Time // target ID + "\x00" + folder -> last scan request
}

func newChecker(store *Store, queue *Queue) *Checker {
	return &Checker{store: store, queue: queue, cooldown: map[string]time.Time{}}
}

// Status is shown in the web UI.
func (c *Checker) Status() map[string]any {
	cfg := c.store.Get()
	c.mu.Lock()
	defer c.mu.Unlock()
	st := map[string]any{"running": c.running, "last": c.last, "enabled": cfg.LibraryCheck.Enabled}
	if c.running {
		p := c.progress
		st["progress"] = &p
	}
	if cfg.LibraryCheck.Enabled {
		next := time.Now()
		if !c.lastRun.IsZero() {
			next = c.lastRun.Add(time.Duration(cfg.LibraryCheck.IntervalMinutes) * time.Minute)
		}
		st["next"] = next
	}
	return st
}

// Loop starts a check whenever the interval has passed.
func (c *Checker) Loop() {
	time.Sleep(30 * time.Second) // let the media servers settle after a restart
	for {
		lc := c.store.Get().LibraryCheck
		c.mu.Lock()
		due := lc.Enabled && time.Since(c.lastRun) >= time.Duration(lc.IntervalMinutes)*time.Minute
		c.mu.Unlock()
		if due {
			c.Run()
		}
		time.Sleep(15 * time.Second)
	}
}

// Run performs one check. It returns false if a check is already running.
func (c *Checker) Run() bool {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return false
	}
	c.running = true
	c.lastRun = time.Now()
	c.progress = CheckProgress{Started: c.lastRun, Step: "files"}
	c.mu.Unlock()

	res := c.check(c.store.Get())

	c.mu.Lock()
	c.running = false
	c.last = res
	c.mu.Unlock()
	return true
}

func (c *Checker) check(cfg *Config) *CheckResult {
	lc := cfg.LibraryCheck
	start := time.Now()
	res := &CheckResult{Started: start, Errors: []string{}, Targets: []TargetCheck{}}
	defer func() { res.Duration = time.Since(start).Round(time.Millisecond).String() }()

	if len(lc.Paths) == 0 {
		res.Errors = append(res.Errors, "no folders configured")
		return res
	}

	// Files on disk, already rewritten into the shared path form the queue expects
	// (library check rewrites, then global rewrites).
	files, walkErrs := findMedia(lc, time.Now(), func(folders, files int, current string) {
		c.update(func(p *CheckProgress) { p.Folders, p.Files, p.Current = folders, files, current })
	})
	res.Errors = append(res.Errors, walkErrs...)
	res.Files = len(files)
	canon := make([]string, len(files))
	for i, f := range files {
		canon[i] = applyRewrites(applyRewrites(f, lc.Rewrites), cfg.Rewrites)
	}
	roots := make([]string, len(lc.Paths))
	for i, p := range lc.Paths {
		roots[i] = applyRewrites(applyRewrites(p, lc.Rewrites), cfg.Rewrites)
	}

	selected := map[string]bool{}
	for _, id := range lc.Targets {
		selected[id] = true
	}
	cooldown := time.Duration(lc.CooldownMinutes) * time.Minute

	var targets []Target
	for _, t := range cfg.Targets {
		if t.Enabled && (lc.AllTargets || selected[t.ID]) {
			targets = append(targets, t)
		}
	}

	for i, t := range targets {
		c.update(func(p *CheckProgress) {
			p.Step, p.Server, p.Type, p.Index, p.Servers = "server", t.Name, t.Type, i+1, len(targets)
			p.Current, p.Items, p.Total = "", 0, 0
		})
		tc := TargetCheck{Target: t.Name, Type: t.Type, Queued: []string{}, Waiting: []string{}}
		toServer := func(p string) string {
			return applyRewrites(applyRewrites(p, cfg.TypeRewrites[t.Type]), t.Rewrites)
		}
		serverRoots := make([]string, len(roots))
		for i, r := range roots {
			serverRoots[i] = toServer(r)
		}
		known, err := t.KnownFiles(serverRoots, func(library string, items, total int) {
			c.update(func(p *CheckProgress) { p.Current, p.Items, p.Total = library, items, total })
		})
		if err != nil {
			tc.Error = err.Error()
			res.Targets = append(res.Targets, tc)
			c.queue.record(Event{Source: "check", Target: t.Name, Status: "error", Message: "library check: " + err.Error()})
			continue
		}
		tc.Known = len(known)

		missingDirs := map[string]bool{}
		for _, p := range canon {
			if !known[normPath(toServer(p))] {
				tc.Missing++
				missingDirs[path.Dir(p)] = true
			}
		}
		dirs := make([]string, 0, len(missingDirs))
		for d := range missingDirs {
			dirs = append(dirs, d)
		}
		sort.Strings(dirs)
		for _, d := range dirs {
			key := t.ID + "\x00" + d
			c.mu.Lock()
			last, seen := c.cooldown[key]
			ready := !seen || time.Since(last) >= cooldown
			if ready {
				c.cooldown[key] = time.Now()
			}
			c.mu.Unlock()
			if ready {
				c.queue.Add("check", d, false, []string{t.ID})
				tc.Queued = append(tc.Queued, d)
			} else {
				tc.Waiting = append(tc.Waiting, d)
			}
		}
		res.Targets = append(res.Targets, tc)
	}

	// Forget cooldowns for folders that are no longer missing anywhere.
	c.mu.Lock()
	for k, v := range c.cooldown {
		if time.Since(v) >= cooldown {
			delete(c.cooldown, k)
		}
	}
	c.mu.Unlock()

	log.Printf("library check: %d files, %d servers checked in %s", res.Files, len(res.Targets), time.Since(start).Round(time.Millisecond))
	return res
}

// update changes the progress shown while a check runs.
func (c *Checker) update(f func(*CheckProgress)) {
	c.mu.Lock()
	f(&c.progress)
	c.mu.Unlock()
}

// findMedia walks the configured folders and returns media file paths.
// progress, if not nil, is called for every folder with the running totals.
func findMedia(lc LibraryCheck, now time.Time, progress func(folders, files int, current string)) ([]string, []string) {
	exts := map[string]bool{}
	for _, e := range lc.Extensions {
		exts[e] = true
	}
	ignoreDirs := map[string]bool{}
	for _, d := range lc.IgnoreFolders {
		ignoreDirs[strings.ToLower(strings.TrimSpace(d))] = true
	}
	minAge := time.Duration(lc.MinAgeMinutes) * time.Minute
	var files, errs []string
	folders := 0
	for _, root := range lc.Paths {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if p == root {
					return err
				}
				return nil // unreadable subfolder: skip it
			}
			name := d.Name()
			if d.IsDir() {
				if p != root && (strings.HasPrefix(name, ".") || ignoreDirs[strings.ToLower(name)]) {
					return filepath.SkipDir
				}
				folders++
				if progress != nil {
					progress(folders, len(files), filepath.ToSlash(p))
				}
				return nil
			}
			if strings.HasPrefix(name, ".") || !d.Type().IsRegular() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(name))
			if !exts[ext] {
				return nil
			}
			base := strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))
			for _, s := range lc.IgnoreSuffixes {
				if s = strings.ToLower(strings.TrimSpace(s)); s != "" && strings.HasSuffix(base, s) {
					return nil
				}
			}
			if minAge > 0 {
				info, err := d.Info()
				if err != nil || now.Sub(info.ModTime()) < minAge {
					return nil
				}
			}
			files = append(files, filepath.ToSlash(p))
			return nil
		})
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", root, err))
		}
	}
	if progress != nil {
		progress(folders, len(files), "")
	}
	return files, errs
}

// normPath makes paths from different systems comparable: forward slashes,
// and lower case for Windows drive paths (which are case-insensitive).
func normPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if len(p) >= 2 && p[1] == ':' {
		p = strings.ToLower(p)
	}
	return path.Clean(p)
}

// pageProgress reports, after each page, the library being listed, the items
// received from it so far and its total size (0 if the server didn't say).
type pageProgress func(library string, items, total int)

// KnownFiles returns the (normalised) paths of every media file the server
// has indexed under any of roots. progress may be nil.
func (t Target) KnownFiles(roots []string, progress pageProgress) (map[string]bool, error) {
	if progress == nil {
		progress = func(string, int, int) {}
	}
	switch t.Type {
	case "plex":
		return t.plexFiles(roots, progress)
	case "jellyfin", "emby":
		return t.embyFiles(progress)
	}
	return nil, fmt.Errorf("unknown target type %q", t.Type)
}

// overlaps reports whether a and b are the same folder or one contains the other.
func overlaps(a, b string) bool {
	a, b = normPath(a), normPath(b)
	return pathWithin(a, b) || pathWithin(b, a)
}

var pageSize = 1000 // items per request when listing a library (a var so tests can page)

func (t Target) plexFiles(roots []string, progress pageProgress) (map[string]bool, error) {
	secs, err := t.plexSections()
	if err != nil {
		return nil, err
	}
	// Plex item types that carry media files, per library type.
	itemType := map[string]string{"movie": "1", "show": "4", "artist": "10"}
	known := map[string]bool{}
	for _, sec := range secs {
		typ, ok := itemType[sec.Type]
		if !ok {
			continue
		}
		relevant := false
		for _, loc := range sec.Location {
			for _, r := range roots {
				if overlaps(loc.Path, r) {
					relevant = true
				}
			}
		}
		if !relevant {
			continue
		}
		progress(sec.Title, 0, 0)
		received := 0
		for start := 0; ; start += pageSize {
			q := url.Values{
				"type":                   {typ},
				"X-Plex-Container-Start": {strconv.Itoa(start)},
				"X-Plex-Container-Size":  {strconv.Itoa(pageSize)},
			}
			req, err := t.newRequest("GET", "/library/sections/"+sec.Key+"/all?"+q.Encode(), nil)
			if err != nil {
				return nil, err
			}
			body, err := doRequest(req)
			if err != nil {
				return nil, fmt.Errorf("listing library %q: %w", sec.Title, err)
			}
			var res struct {
				MediaContainer struct {
					TotalSize int `json:"totalSize"`
					Metadata  []struct {
						Media []struct {
							Part []struct {
								File string `json:"file"`
							} `json:"Part"`
						} `json:"Media"`
					} `json:"Metadata"`
				} `json:"MediaContainer"`
			}
			if err := json.Unmarshal(body, &res); err != nil {
				return nil, fmt.Errorf("unexpected response from plex: %w", err)
			}
			for _, m := range res.MediaContainer.Metadata {
				for _, media := range m.Media {
					for _, part := range media.Part {
						if part.File != "" {
							known[normPath(part.File)] = true
						}
					}
				}
			}
			received += len(res.MediaContainer.Metadata)
			progress(sec.Title, received, res.MediaContainer.TotalSize)
			if len(res.MediaContainer.Metadata) < pageSize {
				break
			}
		}
	}
	return known, nil
}

func (t Target) embyFiles(progress pageProgress) (map[string]bool, error) {
	known := map[string]bool{}
	progress("", 0, 0)
	for start := 0; ; start += pageSize {
		q := url.Values{
			"Recursive":        {"true"},
			"IsFolder":         {"false"},
			"IncludeItemTypes": {"Movie,Episode,Audio,MusicVideo,Video"},
			"Fields":           {"Path,MediaSources"},
			"EnableImages":     {"false"},
			"EnableUserData":   {"false"},
			"StartIndex":       {strconv.Itoa(start)},
			"Limit":            {strconv.Itoa(pageSize)},
		}
		req, err := t.newRequest("GET", "/Items?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		body, err := doRequest(req)
		if err != nil {
			return nil, fmt.Errorf("listing items: %w", err)
		}
		var res struct {
			Items []struct {
				Path         string
				MediaSources []struct{ Path string }
			}
			TotalRecordCount int
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("unexpected response from %s: %w", t.Type, err)
		}
		for _, it := range res.Items {
			if it.Path != "" {
				known[normPath(it.Path)] = true
			}
			// Extra versions of the same movie/episode are separate files.
			for _, ms := range it.MediaSources {
				if ms.Path != "" {
					known[normPath(ms.Path)] = true
				}
			}
		}
		progress("", start+len(res.Items), res.TotalRecordCount)
		if len(res.Items) < pageSize || (res.TotalRecordCount > 0 && start+len(res.Items) >= res.TotalRecordCount) {
			break
		}
	}
	return known, nil
}
