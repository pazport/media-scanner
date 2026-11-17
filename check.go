package main

import (
	"log"
	"os"
	"path"
	"sort"
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
	// Full is true when the server's whole list was fetched; otherwise only
	// Fetched changed items were requested and merged into the stored list.
	Full    bool      `json:"full"`
	Fetched int       `json:"fetched"`
	FullAt  time.Time `json:"full_at"` // when the whole list was last fetched
}

// CheckResult describes one library check run.
type CheckResult struct {
	Started  time.Time `json:"started"`
	Duration string    `json:"duration"`
	// Walked is true when the folders were read in this run. Otherwise the
	// files from the walk at WalkedAt were compared again.
	Walked   bool      `json:"walked"`
	WalkedAt time.Time `json:"walked_at"`
	Files    int       `json:"files"`   // media files found on disk
	Folders  int       `json:"folders"` // folders looked through
	Cached   int       `json:"cached"`  // of those, unchanged folders taken from the cache
	Errors   []string  `json:"errors"`
	// Symlinks whose target couldn't be found, usually because it isn't
	// mounted into this container.
	BrokenLinks  int      `json:"broken_links"`
	BrokenSample []string `json:"broken_sample"`
	// Missing files skipped because they changed within the minimum age.
	Recent  int           `json:"recent"`
	Targets []TargetCheck `json:"targets"`
}

// CheckProgress describes a check that is running, for the web UI.
type CheckProgress struct {
	Started time.Time `json:"started"`
	Walk    bool      `json:"walk"`    // whether this run reads the folders
	Step    string    `json:"step"`    // "files" while walking folders, "server" while asking a server
	Folders int       `json:"folders"` // folders looked through so far
	Cached  int       `json:"cached"`  // of those, taken from the cache
	Files   int       `json:"files"`   // media files found so far
	Current string    `json:"current"` // folder being walked, or server library being listed
	Server  string    `json:"server"`  // server being asked
	Type    string    `json:"type"`
	Index   int       `json:"index"` // 1-based number of the server being asked
	Servers int       `json:"servers"`
	Changes bool      `json:"changes"` // only changes since the last check are being fetched
	Items   int       `json:"items"`   // items received from the current server (library)
	Total   int       `json:"total"`   // total the server reported, 0 if unknown
}

// knownCache is a media server's file list, kept between checks.
type knownCache struct {
	files  map[string]bool
	fullAt time.Time // last time the whole list was fetched
	syncAt time.Time // last time it was brought up to date
	sig    string    // server and folders it was fetched for
}

// When asking a server for changes, go back this much further than the last
// check, to allow for clock differences and items saved during that check.
const syncOverlap = 10 * time.Minute

// Checker runs the periodic library check.
type Checker struct {
	store *Store
	queue *Queue

	mu       sync.Mutex
	running  bool
	progress CheckProgress
	last     *CheckResult
	cooldown map[string]time.Time // target ID + "\x00" + folder -> last scan request

	// Kept between checks (written only by the running check).
	files      []string               // media files from the last folder walk
	walk       walkResult             // that walk's totals (Files and Cache aren't used)
	walkedAt   time.Time              // when the folders were last read
	walkSig    string                 // settings the walk and folder cache belong to
	dirs       dirCache               // folder cache from the last walk
	dirsFullAt time.Time              // last walk that read every folder
	comparedAt time.Time              // when the servers were last compared
	known      map[string]*knownCache // per target ID
}

func newChecker(store *Store, queue *Queue) *Checker {
	return &Checker{store: store, queue: queue, cooldown: map[string]time.Time{},
		known: map[string]*knownCache{}}
}

// Status is shown in the web UI.
func (c *Checker) Status() map[string]any {
	cfg := c.store.Get()
	lc := cfg.LibraryCheck
	c.mu.Lock()
	defer c.mu.Unlock()
	st := map[string]any{"running": c.running, "last": c.last, "enabled": lc.Enabled,
		"cached_folders": len(c.dirs)}
	if c.running {
		p := c.progress
		st["progress"] = &p
	}
	if !c.dirsFullAt.IsZero() {
		st["full_at"] = c.dirsFullAt
	}
	if lc.Enabled {
		now := time.Now()
		nextWalk, nextCompare := now, now
		if !c.walkedAt.IsZero() {
			nextWalk = c.walkedAt.Add(time.Duration(lc.IntervalMinutes) * time.Minute)
		}
		if !c.comparedAt.IsZero() {
			nextCompare = c.comparedAt.Add(time.Duration(lc.ServerIntervalMinutes) * time.Minute)
		}
		st["next_walk"], st["next_compare"] = nextWalk, nextCompare
	}
	return st
}

// Loop runs checks on schedule: the servers are compared every
// ServerIntervalMinutes, and the folders are re-read every IntervalMinutes.
func (c *Checker) Loop() {
	time.Sleep(30 * time.Second) // let the media servers settle after a restart
	for {
		lc := c.store.Get().LibraryCheck
		if lc.Enabled && len(lc.Paths) > 0 {
			c.mu.Lock()
			walkDue := c.files == nil || time.Since(c.walkedAt) >= time.Duration(lc.IntervalMinutes)*time.Minute
			compareDue := time.Since(c.comparedAt) >= time.Duration(lc.ServerIntervalMinutes)*time.Minute
			c.mu.Unlock()
			if walkDue || compareDue {
				c.run(walkDue, false)
			}
		}
		time.Sleep(15 * time.Second)
	}
}

// Run performs a check now: the folders are read (using the folder cache) and
// every server is compared. With full, every cache is ignored and rebuilt.
// It returns false if a check is already running.
func (c *Checker) Run(full bool) bool {
	return c.run(true, full)
}

func (c *Checker) run(walk, full bool) bool {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return false
	}
	c.running = true
	c.progress = CheckProgress{Started: time.Now(), Step: "files", Walk: walk}
	c.mu.Unlock()

	res := c.check(c.store.Get(), walk, full)

	c.mu.Lock()
	c.running = false
	c.last = res
	c.mu.Unlock()
	return true
}

// walkSignature describes the settings that decide what a walk finds, so a
// change to them invalidates the stored files and folder cache.
func walkSignature(lc LibraryCheck) string {
	return strings.Join([]string{
		strings.Join(lc.Paths, "\x01"), strings.Join(lc.Extensions, "\x01"),
		strings.Join(lc.IgnoreFolders, "\x01"), strings.Join(lc.IgnoreSuffixes, "\x01"),
		map[bool]string{true: "nolinks", false: "links"}[lc.IgnoreSymlinks],
	}, "\x00")
}

func (c *Checker) check(cfg *Config, walk, full bool) *CheckResult {
	lc := cfg.LibraryCheck
	start := time.Now()
	res := &CheckResult{Started: start, Errors: []string{}, Targets: []TargetCheck{}, BrokenSample: []string{}}
	defer func() { res.Duration = time.Since(start).Round(time.Millisecond).String() }()

	if len(lc.Paths) == 0 {
		res.Errors = append(res.Errors, "no folders configured")
		return res
	}
	fullEvery := time.Duration(lc.FullRefreshHours) * time.Hour

	// Step 1: the media files on disk. Read the folders when due (or when the
	// settings changed), otherwise compare the files from the last walk again.
	sig := walkSignature(lc)
	if walk || c.files == nil || sig != c.walkSig {
		prev := c.dirs
		if full || sig != c.walkSig || time.Since(c.dirsFullAt) >= fullEvery {
			prev = nil // read every folder, rebuilding the cache
		}
		c.update(func(p *CheckProgress) { p.Walk = true })
		walked := findMedia(lc, prev, func(folders, cached, files int, current string) {
			c.update(func(p *CheckProgress) { p.Folders, p.Cached, p.Files, p.Current = folders, cached, files, current })
		})
		c.mu.Lock()
		c.files, c.dirs, c.walkSig, c.walkedAt = walked.Files, walked.Cache, sig, start
		walked.Files, walked.Cache = nil, nil
		c.walk = walked
		if prev == nil {
			c.dirsFullAt = start
		}
		c.mu.Unlock()
		res.Walked = true
	} else {
		c.update(func(p *CheckProgress) {
			p.Walk, p.Folders, p.Cached, p.Files = false, c.walk.Folders, c.walk.Cached, len(c.files)
		})
	}
	files := c.files
	res.WalkedAt = c.walkedAt
	res.Errors = append(res.Errors, c.walk.Errors...)
	res.Files, res.Folders, res.Cached = len(files), c.walk.Folders, c.walk.Cached
	res.BrokenLinks, res.BrokenSample = c.walk.Broken, c.walk.BrokenSample

	// Files on disk, rewritten into the shared path form the queue expects
	// (library check rewrites, then global rewrites).
	canon := make([]string, len(files))
	for i, f := range files {
		canon[i] = applyRewrites(applyRewrites(f, lc.Rewrites), cfg.Rewrites)
	}
	// Files modified recently may still be copying. Rather than looking up every
	// file's age while walking (slow on large or network filesystems), only the
	// files a server is missing are checked, once each.
	minAge := time.Duration(lc.MinAgeMinutes) * time.Minute
	recent := map[int]bool{}
	checkedAge := map[int]bool{}
	tooNew := func(i int) bool {
		if minAge <= 0 {
			return false
		}
		if !checkedAge[i] {
			checkedAge[i] = true
			info, err := os.Stat(files[i]) // follows symlinks: the real file's age
			if err != nil || time.Since(info.ModTime()) < minAge {
				recent[i] = true
			}
		}
		return recent[i]
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

	// Step 2: compare with each media server.
	for i, t := range targets {
		c.update(func(p *CheckProgress) {
			p.Step, p.Server, p.Type, p.Index, p.Servers = "server", t.Name, t.Type, i+1, len(targets)
			p.Current, p.Items, p.Total, p.Changes = "", 0, 0, false
		})
		tc := TargetCheck{Target: t.Name, Type: t.Type, Queued: []string{}, Waiting: []string{}}
		toServer := func(p string) string {
			return applyRewrites(applyRewrites(p, cfg.TypeRewrites[t.Type]), t.Rewrites)
		}
		serverRoots := make([]string, len(roots))
		for i, r := range roots {
			serverRoots[i] = toServer(r)
		}
		known, err := c.serverFiles(t, serverRoots, full, fullEvery, &tc)
		if err != nil {
			tc.Error = err.Error()
			res.Targets = append(res.Targets, tc)
			c.queue.record(Event{Source: "check", Target: t.Name, Status: "error", Message: "library check: " + err.Error()})
			continue
		}
		tc.Known = len(known)

		missingDirs := map[string]bool{}
		for i, p := range canon {
			if !known[normPath(toServer(p))] {
				if tooNew(i) {
					continue
				}
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

	c.mu.Lock()
	c.comparedAt = start
	// Forget cooldowns that have passed, and stored lists of removed servers.
	for k, v := range c.cooldown {
		if time.Since(v) >= cooldown {
			delete(c.cooldown, k)
		}
	}
	ids := map[string]bool{}
	for _, t := range cfg.Targets {
		ids[t.ID] = true
	}
	for id := range c.known {
		if !ids[id] {
			delete(c.known, id)
		}
	}
	c.mu.Unlock()

	res.Recent = len(recent)
	log.Printf("library check: %d files (walked: %v, %d of %d folders from cache), %d servers checked in %s",
		res.Files, res.Walked, res.Cached, res.Folders, len(res.Targets), time.Since(start).Round(time.Millisecond))
	return res
}

// serverFiles returns a server's media files. The list is kept between checks:
// normally only items changed since the last check are requested and merged
// in, and the whole list is fetched again every fullEvery (or when forced, or
// when the server or folders changed). Asking for changes falls back to the
// whole list if the server refuses.
func (c *Checker) serverFiles(t Target, roots []string, full bool, fullEvery time.Duration, tc *TargetCheck) (map[string]bool, error) {
	progress := func(library string, items, total int) {
		c.update(func(p *CheckProgress) { p.Current, p.Items, p.Total = library, items, total })
	}
	sig := t.Type + "\x00" + t.URL + "\x00" + strings.Join(roots, "\x00")
	now := time.Now()
	kc := c.known[t.ID]
	if kc != nil && !full && kc.sig == sig && now.Sub(kc.fullAt) < fullEvery {
		c.update(func(p *CheckProgress) { p.Changes = true })
		changed, err := t.KnownFiles(roots, kc.syncAt.Add(-syncOverlap), progress)
		if err == nil {
			for f := range changed {
				kc.files[f] = true
			}
			kc.syncAt = now
			tc.Fetched, tc.FullAt = len(changed), kc.fullAt
			return kc.files, nil
		}
		log.Printf("library check: %s: asking for changes failed (%v), fetching the whole list", t.Name, err)
		c.update(func(p *CheckProgress) { p.Changes = false })
	}
	files, err := t.KnownFiles(roots, time.Time{}, progress)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.known[t.ID] = &knownCache{files: files, fullAt: now, syncAt: now, sig: sig}
	c.mu.Unlock()
	tc.Full, tc.Fetched, tc.FullAt = true, len(files), now
	return files, nil
}

// update changes the progress shown while a check runs.
func (c *Checker) update(f func(*CheckProgress)) {
	c.mu.Lock()
	f(&c.progress)
	c.mu.Unlock()
}
