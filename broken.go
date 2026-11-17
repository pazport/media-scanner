package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// BrokenLink is a symlink in a checked folder whose target can't be found.
type BrokenLink struct {
	Path   string `json:"path"`   // the link
	Target string `json:"target"` // where it points (absolute)
	Group  string `json:"group"`  // the BrokenGroup it belongs to
}

// BrokenGroup collects broken links by where they point, so you can see
// whether the targets are really gone or just not visible in this container.
type BrokenGroup struct {
	Folder string `json:"folder"` // first levels of the target path, e.g. /data/torrents/tv
	Count  int    `json:"count"`
	// Status: "gone" (the folder exists and has files, so the targets really are
	// gone), "empty" (the folder is empty: maybe a mount that is down) or
	// "missing" (the folder doesn't exist here: probably not mounted).
	Status    string `json:"status"`
	Deletable bool   `json:"deletable"` // only "gone" links can be deleted without forcing
}

// linkTarget returns where a symlink points, as an absolute path.
func linkTarget(p string) string {
	t, err := os.Readlink(p)
	if err != nil {
		return ""
	}
	if !filepath.IsAbs(t) {
		t = filepath.Join(filepath.Dir(p), t)
	}
	return filepath.ToSlash(filepath.Clean(t))
}

// groupFolder is the folder a broken link is grouped under: the first three
// levels of its target's folder, e.g. /data/torrents/tv for
// /data/torrents/tv/Some.Release/file.mkv.
func groupFolder(target string) string {
	if target == "" {
		return "(unreadable link)"
	}
	parts := strings.Split(strings.Trim(filepath.ToSlash(filepath.Dir(target)), "/"), "/")
	if len(parts) == 1 && parts[0] == "" {
		return "/"
	}
	if len(parts) > 3 {
		parts = parts[:3]
	}
	return "/" + strings.Join(parts, "/")
}

// groupStatus checks whether a group's folder is visible in this container.
func groupStatus(folder string) (status string, deletable bool) {
	if !strings.HasPrefix(folder, "/") {
		return "missing", false
	}
	entries, err := os.ReadDir(folder)
	switch {
	case err == nil && len(entries) > 0:
		return "gone", true
	case err == nil:
		return "empty", false
	default:
		return "missing", false
	}
}

// setBroken replaces the broken link list after a folder walk.
func (c *Checker) setBroken(paths []string) {
	links := make([]BrokenLink, 0, len(paths))
	for _, p := range paths {
		t := linkTarget(p)
		links = append(links, BrokenLink{Path: p, Target: t, Group: groupFolder(t)})
	}
	c.mu.Lock()
	c.broken = links
	c.mu.Unlock()
}

// brokenGroups summarises the broken links by group (call with c.mu held).
func (c *Checker) brokenGroups() []BrokenGroup {
	counts := map[string]int{}
	for _, l := range c.broken {
		counts[l.Group]++
	}
	groups := make([]BrokenGroup, 0, len(counts))
	for folder, n := range counts {
		status, deletable := groupStatus(folder)
		groups = append(groups, BrokenGroup{Folder: folder, Count: n, Status: status, Deletable: deletable})
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Count != groups[j].Count {
			return groups[i].Count > groups[j].Count
		}
		return groups[i].Folder < groups[j].Folder
	})
	return groups
}

// BrokenList returns the broken links matching a group and/or search text,
// one page at a time, plus the group summary.
func (c *Checker) BrokenList(group, search string, offset, limit int) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	search = strings.ToLower(search)
	matched := []BrokenLink{}
	for _, l := range c.broken {
		if group != "" && l.Group != group {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(l.Path), search) &&
			!strings.Contains(strings.ToLower(l.Target), search) {
			continue
		}
		matched = append(matched, l)
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	end := offset + limit
	if offset > len(matched) {
		offset = len(matched)
	}
	if end > len(matched) {
		end = len(matched)
	}
	return map[string]any{
		"total":   len(c.broken),
		"matched": len(matched),
		"offset":  offset,
		"items":   matched[offset:end],
		"groups":  c.brokenGroups(),
	}
}

// BrokenText is the whole list as text, one "link -> target" per line.
func (c *Checker) BrokenText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, l := range c.broken {
		fmt.Fprintf(&b, "%s -> %s\n", l.Path, l.Target)
	}
	return b.String()
}

// errBusy is returned while a check is running (it replaces the list).
var errBusy = errors.New("a library check is running; try again when it has finished")

// selectBroken picks links by explicit paths, by group, or all of them.
// Call with c.mu held.
func (c *Checker) selectBroken(paths []string, group string, all bool) []BrokenLink {
	if all {
		return append([]BrokenLink{}, c.broken...)
	}
	want := map[string]bool{}
	for _, p := range paths {
		want[p] = true
	}
	var out []BrokenLink
	for _, l := range c.broken {
		if (group != "" && l.Group == group) || want[l.Path] {
			out = append(out, l)
		}
	}
	return out
}

// removeBroken drops the given links from the list. Call with c.mu held.
func (c *Checker) removeBroken(drop map[string]bool) {
	kept := c.broken[:0:0]
	for _, l := range c.broken {
		if !drop[l.Path] {
			kept = append(kept, l)
		}
	}
	c.broken = kept
	c.walk.Broken = len(kept)
	if c.last != nil {
		last := *c.last
		last.BrokenLinks = len(kept)
		c.last = &last
	}
}

// TestBroken looks up the selected links' targets again. Links that work now
// are removed from the list (the next folder read counts their media files).
func (c *Checker) TestBroken(paths []string, group string, all bool) (map[string]int, error) {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return nil, errBusy
	}
	sel := c.selectBroken(paths, group, all)
	c.mu.Unlock()

	fixed := map[string]bool{}
	for _, l := range sel {
		if _, err := os.Stat(l.Path); err == nil {
			fixed[l.Path] = true
		}
	}
	c.mu.Lock()
	c.removeBroken(fixed)
	c.mu.Unlock()
	return map[string]int{"tested": len(sel), "fixed": len(fixed), "still_broken": len(sel) - len(fixed)}, nil
}

// DeleteProblem explains why a link wasn't deleted.
type DeleteProblem struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// DeleteBroken removes the selected broken symlinks (only the links, never
// what they point to). Each link is checked again first: it must still be a
// symlink, still be broken, and be inside a checked folder. Links whose
// target folder looks unmounted are skipped unless force is set.
func (c *Checker) DeleteBroken(paths []string, group string, all, force bool) (map[string]any, error) {
	cfg := c.store.Get()
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return nil, errBusy
	}
	sel := c.selectBroken(paths, group, all)
	c.mu.Unlock()

	groupOK := map[string]bool{}
	for _, l := range sel {
		if _, seen := groupOK[l.Group]; !seen {
			_, groupOK[l.Group] = groupStatus(l.Group)
		}
	}
	inRoots := func(p string) bool {
		for _, r := range cfg.LibraryCheck.Paths {
			if pathWithin(filepath.ToSlash(p), filepath.ToSlash(r)) {
				return true
			}
		}
		return false
	}

	deleted, gone := 0, map[string]bool{}
	skipped, failed := []DeleteProblem{}, []DeleteProblem{}
	for _, l := range sel {
		info, err := os.Lstat(l.Path)
		switch {
		case err != nil:
			gone[l.Path] = true // already removed
			continue
		case info.Mode()&os.ModeSymlink == 0:
			skipped = append(skipped, DeleteProblem{l.Path, "no longer a symlink"})
			continue
		case !inRoots(l.Path):
			skipped = append(skipped, DeleteProblem{l.Path, "not inside a checked folder"})
			continue
		case !force && !groupOK[l.Group]:
			skipped = append(skipped, DeleteProblem{l.Path, "its target folder isn't visible in this container (not mounted?)"})
			continue
		}
		if _, err := os.Stat(l.Path); err == nil {
			skipped = append(skipped, DeleteProblem{l.Path, "works now (its target exists)"})
			gone[l.Path] = true // no longer broken
			continue
		}
		if err := os.Remove(l.Path); err != nil {
			reason := err.Error()
			if errors.Is(err, syscall.EROFS) {
				reason = "read-only file system: mount this folder into the container without :ro to delete links"
			}
			failed = append(failed, DeleteProblem{l.Path, reason})
			continue
		}
		deleted++
		gone[l.Path] = true
	}

	c.mu.Lock()
	c.removeBroken(gone)
	c.mu.Unlock()
	if deleted > 0 {
		c.queue.record(Event{Source: "check", Status: "ok",
			Message: fmt.Sprintf("deleted %d broken symlink(s)", deleted)})
	}
	// Keep the response small: list the first problems only.
	counts := map[string]int{"skipped": len(skipped), "failed": len(failed)}
	if len(skipped) > 50 {
		skipped = skipped[:50]
	}
	if len(failed) > 50 {
		failed = failed[:50]
	}
	return map[string]any{"selected": len(sel), "deleted": deleted, "skipped": skipped, "failed": failed,
		"skipped_count": counts["skipped"], "failed_count": counts["failed"]}, nil
}
