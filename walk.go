package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

// Kinds of folder entries kept in the folder cache.
const (
	kindFile           uint8 = iota // media file
	kindDir                         // folder
	kindLinkFile                    // symlink to a file
	kindLinkDir                     // symlink to a folder
	kindLinkBroken                  // symlink whose target can't be found
	kindLinkUnresolved              // symlink seen while symlinks were ignored
)

type cachedEntry struct {
	Name string
	Kind uint8
}

// cachedDir is a folder's relevant contents (media files, subfolders and
// symlinks) as they were when its modification time was ModTime.
type cachedDir struct {
	ModTime int64
	Entries []cachedEntry
}

// dirCache maps folder paths to their contents when last read. A folder whose
// modification time hasn't changed since is not read again.
type dirCache map[string]*cachedDir

// walkResult is what findMedia found on disk.
type walkResult struct {
	Files        []string // sorted
	Errors       []string
	Folders      int      // folders looked through
	Cached       int      // of those, folders whose contents came from the cache
	Broken       int      // symlinks whose target doesn't exist (inside this container)
	BrokenSample []string // a few of them, to show in the UI
	Cache        dirCache // every folder read, for the next walk
}

const brokenSampleSize = 10

// readDir is os.ReadDir; tests replace it to count reads or simulate a slow filesystem.
var readDir = os.ReadDir

// walkProgress is called as folders are read, with running totals.
type walkProgress func(folders, cached, files int, current string)

// findMedia walks the configured folders and returns media file paths.
//
// Several folders are read at once (lc.Workers): on union, network and FUSE
// filesystems most of the time goes on waiting for each folder listing, so
// overlapping them is much faster than reading one at a time.
//
// prev is the cache from the last walk, or nil to read every folder. A folder
// whose modification time matches the cache is not listed again, and the
// symlinks in it aren't looked up again (except broken ones, whose target may
// have appeared).
//
// Symlinked files and folders are followed unless lc.IgnoreSymlinks is set;
// paths are reported as the link path, which is what the media servers see.
func findMedia(lc LibraryCheck, prev dirCache, progress walkProgress) walkResult {
	w := &walker{
		lc:         lc,
		prev:       prev,
		exts:       map[string]bool{},
		ignoreDirs: map[string]bool{},
		visited:    map[string]bool{},
		progress:   progress,
		res: walkResult{Files: []string{}, Errors: []string{}, BrokenSample: []string{},
			Cache: dirCache{}},
	}
	w.cond = sync.NewCond(&w.mu)
	for _, e := range lc.Extensions {
		w.exts[e] = true
	}
	for _, d := range lc.IgnoreFolders {
		w.ignoreDirs[strings.ToLower(strings.TrimSpace(d))] = true
	}
	for _, root := range lc.Paths {
		w.push(dirJob{path: root, root: true})
	}

	workers := lc.Workers
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				job, ok := w.pop()
				if !ok {
					return
				}
				w.readFolder(job)
				w.done()
			}
		}()
	}
	wg.Wait()

	sort.Strings(w.res.Files)
	if progress != nil {
		progress(w.res.Folders, w.res.Cached, len(w.res.Files), "")
	}
	return w.res
}

type dirJob struct {
	path string
	root bool
}

type walker struct {
	lc         LibraryCheck
	prev       dirCache // read only
	exts       map[string]bool
	ignoreDirs map[string]bool
	progress   walkProgress

	mu      sync.Mutex
	cond    *sync.Cond
	queue   []dirJob
	pending int             // folders queued or being read; 0 means the walk is done
	visited map[string]bool // real folders already read, so symlink loops end
	res     walkResult
}

func (w *walker) push(job dirJob) {
	w.mu.Lock()
	w.queue = append(w.queue, job)
	w.pending++
	w.cond.Signal()
	w.mu.Unlock()
}

// pop waits for a folder to read. It returns false once every folder is done.
func (w *walker) pop() (dirJob, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for len(w.queue) == 0 && w.pending > 0 {
		w.cond.Wait()
	}
	if len(w.queue) == 0 {
		return dirJob{}, false
	}
	// Last in, first out: finishes one show before starting the next, which
	// keeps the queue short.
	job := w.queue[len(w.queue)-1]
	w.queue = w.queue[:len(w.queue)-1]
	return job, true
}

// done marks a folder as read and wakes the other workers when all are done.
func (w *walker) done() {
	w.mu.Lock()
	w.pending--
	if w.pending == 0 {
		w.cond.Broadcast()
	}
	w.mu.Unlock()
}

func (w *walker) rootError(job dirJob, msg string) {
	if job.root {
		w.mu.Lock()
		w.res.Errors = append(w.res.Errors, fmt.Sprintf("%s: %s", job.path, msg))
		w.mu.Unlock()
	}
}

// dirKey identifies a folder on disk regardless of the path used to reach it.
func dirKey(p string, info fs.FileInfo) string {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

// resolveLink looks up what a symlink points to.
func resolveLink(p string) uint8 {
	st, err := os.Stat(p)
	switch {
	case err != nil:
		return kindLinkBroken
	case st.IsDir():
		return kindLinkDir
	default:
		return kindLinkFile
	}
}

// list reads a folder and keeps the entries that matter: media files,
// subfolders and symlinks (resolved, unless symlinks are ignored).
func (w *walker) list(dir string) ([]cachedEntry, error) {
	entries, err := readDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]cachedEntry, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		typ := e.Type()
		switch {
		case typ&fs.ModeSymlink != 0:
			kind := uint8(kindLinkUnresolved)
			if !w.lc.IgnoreSymlinks {
				kind = resolveLink(filepath.Join(dir, name))
			}
			out = append(out, cachedEntry{name, kind})
		case typ.IsDir():
			out = append(out, cachedEntry{name, kindDir})
		case typ.IsRegular() && w.exts[strings.ToLower(filepath.Ext(name))]:
			out = append(out, cachedEntry{name, kindFile})
		}
	}
	return out, nil
}

// readFolder reads one folder (or takes it from the cache), records its media
// files and queues its subfolders.
func (w *walker) readFolder(job dirJob) {
	info, err := os.Stat(job.path) // follows a symlinked folder
	if err != nil {
		w.rootError(job, err.Error())
		return
	}
	if !info.IsDir() {
		w.rootError(job, "not a folder")
		return
	}
	key := dirKey(job.path, info)
	w.mu.Lock()
	seen := w.visited[key]
	w.visited[key] = true
	w.mu.Unlock()
	if seen {
		return
	}

	modTime := info.ModTime().UnixNano()
	var entries []cachedEntry
	fromCache := false
	if c, ok := w.prev[job.path]; ok && c.ModTime == modTime {
		entries, fromCache = c.Entries, true
	} else if entries, err = w.list(job.path); err != nil {
		w.rootError(job, err.Error())
		return // unreadable subfolder: skip it
	}

	follow := !w.lc.IgnoreSymlinks
	var files, broken []string
	var subdirs []dirJob
	copied := false
	for i, e := range entries {
		p := filepath.Join(job.path, e.Name)
		kind := e.Kind
		// A broken link's target may have appeared; a link seen while symlinks
		// were ignored hasn't been looked up yet.
		if follow && (kind == kindLinkBroken || kind == kindLinkUnresolved) {
			if kind = resolveLink(p); kind != e.Kind {
				if fromCache && !copied { // don't change the previous walk's cache
					entries = append([]cachedEntry(nil), entries...)
					copied = true
				}
				entries[i].Kind = kind
			}
		}
		switch kind {
		case kindFile, kindLinkFile:
			if (kind == kindFile || follow) && w.isMedia(e.Name) {
				files = append(files, filepath.ToSlash(p))
			}
		case kindDir, kindLinkDir:
			if (kind == kindDir || follow) && !w.ignoreDirs[strings.ToLower(e.Name)] {
				subdirs = append(subdirs, dirJob{path: p})
			}
		case kindLinkBroken:
			if follow {
				broken = append(broken, filepath.ToSlash(p))
			}
		}
	}

	w.mu.Lock()
	w.res.Cache[job.path] = &cachedDir{ModTime: modTime, Entries: entries}
	w.res.Folders++
	if fromCache {
		w.res.Cached++
	}
	w.res.Files = append(w.res.Files, files...)
	w.res.Broken += len(broken)
	for _, b := range broken {
		if len(w.res.BrokenSample) < brokenSampleSize {
			w.res.BrokenSample = append(w.res.BrokenSample, b)
		}
	}
	folders, cached, found := w.res.Folders, w.res.Cached, len(w.res.Files)
	w.mu.Unlock()
	if w.progress != nil {
		w.progress(folders, cached, found, filepath.ToSlash(job.path))
	}
	for _, d := range subdirs {
		w.push(d)
	}
}

// isMedia reports whether a file name has a media extension and isn't an
// extra or sample.
func (w *walker) isMedia(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	if !w.exts[ext] {
		return false
	}
	base := strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))
	for _, s := range w.lc.IgnoreSuffixes {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" && strings.HasSuffix(base, s) {
			return false
		}
	}
	return true
}
