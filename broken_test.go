package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGroupFolder(t *testing.T) {
	cases := map[string]string{
		"/data/torrents/tv/Some.Release/file.mkv": "/data/torrents/tv",
		"/mnt/remote/file.mkv":                    "/mnt/remote",
		"/file.mkv":                               "/",
		"":                                        "(unreadable link)",
	}
	for in, want := range cases {
		if got := groupFolder(in); got != want {
			t.Errorf("groupFolder(%q) = %q, want %q", in, got, want)
		}
	}
	full := t.TempDir()
	writeFile(t, filepath.Join(full, "x"), time.Hour)
	empty := t.TempDir()
	for folder, want := range map[string]string{full: "gone", empty: "empty", "/no/such/mount": "missing"} {
		if got, _ := groupStatus(folder); got != want {
			t.Errorf("groupStatus(%s) = %s, want %s", folder, got, want)
		}
	}
}

// brokenFixture makes a library with broken links of every kind and a
// checker that has walked it.
func brokenFixture(t *testing.T) (c *Checker, lib, store string) {
	t.Helper()
	lib, store = t.TempDir(), t.TempDir()
	// The targets' folder exists and has other files: these targets are really gone.
	writeFile(t, filepath.Join(store, "keep.mkv"), time.Hour)
	link := func(target, name string) {
		p := filepath.Join(lib, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	link(filepath.Join(store, "gone1.mkv"), "Show/Season 01/E01.mkv")
	link(filepath.Join(store, "gone2.mkv"), "Show/Season 01/E02.mkv")
	link(filepath.Join(store, "later.mkv"), "Show/Season 01/E03.mkv")       // target appears later
	link("/no-such-mount/media/Movie.mkv", "Movie (2020)/Movie (2020).mkv") // not mounted here
	link(filepath.Join(store, "keep.mkv"), "Show/Season 01/E04.mkv")        // works

	st, err := loadStore(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := st.Get()
	cfg.LibraryCheck.Paths = []string{lib}
	if err := st.Set(cfg); err != nil {
		t.Fatal(err)
	}
	c = newChecker(st, newQueue(st))
	lc := st.Get().LibraryCheck
	walked := findMedia(lc, nil, nil)
	if walked.Broken != 4 {
		t.Fatalf("expected 4 broken links, got %d: %v", walked.Broken, walked.BrokenPaths)
	}
	c.setBroken(walked.BrokenPaths)
	return c, lib, store
}

func TestBrokenListAndTest(t *testing.T) {
	c, lib, store := brokenFixture(t)

	all := c.BrokenList("", "", 0, 0)
	if all["total"] != 4 || all["matched"] != 4 {
		t.Fatalf("list: %v", all)
	}
	groups := all["groups"].([]BrokenGroup)
	if len(groups) != 2 || groups[0].Count != 3 || groups[0].Status != "gone" || !groups[0].Deletable ||
		groups[1].Folder != "/no-such-mount/media" || groups[1].Status != "missing" || groups[1].Deletable {
		t.Errorf("groups: %+v", groups)
	}
	if got := c.BrokenList("/no-such-mount/media", "", 0, 0); got["matched"] != 1 {
		t.Errorf("group filter: %v", got)
	}
	if got := c.BrokenList("", "e02", 0, 0); got["matched"] != 1 {
		t.Errorf("search: %v", got)
	}
	if got := c.BrokenList("", "", 3, 2); len(got["items"].([]BrokenLink)) != 1 {
		t.Errorf("paging: %v", got)
	}
	if txt := c.BrokenText(); strings.Count(txt, "\n") != 4 || !strings.Contains(txt, " -> /no-such-mount/media/Movie.mkv") {
		t.Errorf("text: %q", txt)
	}

	// The missing target appears: testing again removes it from the list.
	writeFile(t, filepath.Join(store, "later.mkv"), time.Hour)
	res, err := c.TestBroken(nil, "", true)
	if err != nil || res["tested"] != 4 || res["fixed"] != 1 || res["still_broken"] != 3 {
		t.Errorf("test: %v %v", res, err)
	}
	if got := c.BrokenList("", "", 0, 0); got["total"] != 3 {
		t.Errorf("after test: %v", got)
	}
	_ = lib
}

func TestDeleteBroken(t *testing.T) {
	c, lib, store := brokenFixture(t)
	e01 := filepath.Join(lib, "Show/Season 01/E01.mkv")
	e02 := filepath.Join(lib, "Show/Season 01/E02.mkv")
	e03 := filepath.Join(lib, "Show/Season 01/E03.mkv")
	movie := filepath.Join(lib, "Movie (2020)/Movie (2020).mkv")

	// E02 is replaced by a real file, and E03's target appears, before deleting.
	if err := os.Remove(e02); err != nil {
		t.Fatal(err)
	}
	writeFile(t, e02, time.Hour)
	writeFile(t, filepath.Join(store, "later.mkv"), time.Hour)
	// A link outside the checked folders sneaks into the list.
	outside := filepath.Join(t.TempDir(), "outside.mkv")
	if err := os.Symlink("/nowhere/x.mkv", outside); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.broken = append(c.broken, BrokenLink{Path: outside, Target: "/nowhere/x.mkv", Group: "/nowhere"})
	c.mu.Unlock()

	// While a check runs, nothing is deleted.
	c.mu.Lock()
	c.running = true
	c.mu.Unlock()
	if _, err := c.DeleteBroken(nil, "", true, false); !errors.Is(err, errBusy) {
		t.Errorf("expected busy error, got %v", err)
	}
	c.mu.Lock()
	c.running = false
	c.mu.Unlock()

	res, err := c.DeleteBroken(nil, "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if res["deleted"] != 1 || res["skipped_count"] != 4 || res["failed_count"] != 0 {
		t.Errorf("delete: %+v", res)
	}
	reasons := map[string]string{}
	for _, p := range res["skipped"].([]DeleteProblem) {
		reasons[p.Path] = p.Reason
	}
	if !strings.Contains(reasons[e02], "no longer a symlink") || !strings.Contains(reasons[e03], "works now") ||
		!strings.Contains(reasons[movie], "not mounted") || !strings.Contains(reasons[outside], "not inside a checked folder") {
		t.Errorf("skip reasons: %v", reasons)
	}
	if _, err := os.Lstat(e01); !os.IsNotExist(err) {
		t.Error("E01's broken link should be deleted")
	}
	for _, p := range []string{e02, e03, movie, outside, filepath.Join(store, "keep.mkv"), filepath.Join(store, "later.mkv")} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s should still exist: %v", p, err)
		}
	}
	// Deleted and now-working links leave the list; the rest stay.
	if got := c.BrokenList("", "", 0, 0)["total"]; got != 3 {
		t.Errorf("list after delete: %v", got)
	}

	// Forcing deletes the link whose target folder isn't mounted (and only the link).
	res, err = c.DeleteBroken([]string{movie}, "", false, true)
	if err != nil || res["deleted"] != 1 {
		t.Errorf("forced delete: %+v %v", res, err)
	}
	if _, err := os.Lstat(movie); !os.IsNotExist(err) {
		t.Error("forced delete should remove the link")
	}
}
