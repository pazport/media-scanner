package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Rewrite translates a path prefix, e.g. /data/movies -> /mnt/media/movies.
type Rewrite struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Target is a media server that receives scan requests.
type Target struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Type     string    `json:"type"` // plex | jellyfin | emby
	URL      string    `json:"url"`
	Token    string    `json:"token"`
	Enabled  bool      `json:"enabled"`
	Rewrites []Rewrite `json:"rewrites"`
}

// Trigger configures an inbound source (radarr, sonarr, lidarr, manual).
type Trigger struct {
	Enabled bool `json:"enabled"`
	// AllTargets sends to every enabled media server; otherwise only Targets (IDs).
	AllTargets bool     `json:"all_targets"`
	Targets    []string `json:"targets"`
	// Rewrites are applied to paths from this trigger before target rewrites.
	Rewrites []Rewrite `json:"rewrites"`
}

type Auth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// LibraryCheck periodically compares media files on disk with what each
// media server knows about, and requests scans for anything missing.
type LibraryCheck struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"interval_minutes"`
	// Folders to walk, as seen inside this container.
	Paths []string `json:"paths"`
	// Skip files modified more recently than this (still being copied).
	MinAgeMinutes int `json:"min_age_minutes"`
	// After requesting a scan for a folder, don't request it again for this long.
	CooldownMinutes int       `json:"cooldown_minutes"`
	AllTargets      bool      `json:"all_targets"`
	Targets         []string  `json:"targets"`
	Rewrites        []Rewrite `json:"rewrites"`
	// File extensions counted as media (lower case, with the dot).
	Extensions []string `json:"extensions"`
	// Folder names (any level, case-insensitive) that are skipped, e.g. extras.
	IgnoreFolders []string `json:"ignore_folders"`
	// File name endings (before the extension, case-insensitive) that are skipped.
	IgnoreSuffixes []string `json:"ignore_suffixes"`
	// Don't follow symlinked files and folders (they are followed by default).
	IgnoreSymlinks bool `json:"ignore_symlinks"`
}

var (
	defaultExtensions = []string{
		".mkv", ".mp4", ".m4v", ".avi", ".mov", ".wmv", ".ts", ".m2ts", ".mpg", ".mpeg", ".webm", ".iso",
		".flac", ".mp3", ".m4a", ".aac", ".ogg", ".opus", ".wav", ".alac", ".wma",
	}
	defaultIgnoreFolders = []string{
		"extras", "featurettes", "behind the scenes", "deleted scenes", "interviews",
		"scenes", "shorts", "trailers", "other", "sample", "samples", "@eadir",
	}
	defaultIgnoreSuffixes = []string{
		"-trailer", "-sample", "-featurette", "-behindthescenes", "-deleted",
		"-interview", "-scene", "-short", "-other", ".sample", "sample",
	}
)

type Config struct {
	Port      int       `json:"port"`
	ScanDelay int       `json:"scan_delay"` // seconds to wait/batch before scanning
	Auth      Auth      `json:"auth"`
	Rewrites  []Rewrite `json:"rewrites"` // global, applied to every trigger
	// TypeRewrites apply to every server of a type (plex, jellyfin, emby),
	// after global rewrites and before each server's own rewrites.
	TypeRewrites map[string][]Rewrite `json:"type_rewrites"`
	Triggers     map[string]*Trigger  `json:"triggers"`
	Targets      []Target             `json:"targets"`

	LibraryCheck LibraryCheck `json:"library_check"`
}

var triggerNames = []string{"radarr", "sonarr", "lidarr", "manual"}

var validTypes = map[string]bool{"plex": true, "jellyfin": true, "emby": true}

func defaultConfig() *Config {
	c := &Config{Port: 3030, ScanDelay: 10, Triggers: map[string]*Trigger{},
		LibraryCheck: LibraryCheck{AllTargets: true}}
	c.normalize()
	return c
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// normalize fills defaults so the rest of the program never sees nils.
func (c *Config) normalize() {
	if c.Port == 0 {
		c.Port = 3030
	}
	if c.ScanDelay < 0 {
		c.ScanDelay = 0
	}
	if c.Triggers == nil {
		c.Triggers = map[string]*Trigger{}
	}
	for _, n := range triggerNames {
		if c.Triggers[n] == nil {
			c.Triggers[n] = &Trigger{Enabled: true, AllTargets: true}
		}
	}
	for _, t := range c.Triggers {
		if t.Targets == nil {
			t.Targets = []string{}
		}
		if t.Rewrites == nil {
			t.Rewrites = []Rewrite{}
		}
	}
	if c.Rewrites == nil {
		c.Rewrites = []Rewrite{}
	}
	if c.TypeRewrites == nil {
		c.TypeRewrites = map[string][]Rewrite{}
	}
	for typ := range validTypes {
		if c.TypeRewrites[typ] == nil {
			c.TypeRewrites[typ] = []Rewrite{}
		}
	}
	if c.Targets == nil {
		c.Targets = []Target{}
	}
	c.LibraryCheck.normalize()
	for i := range c.Targets {
		t := &c.Targets[i]
		if t.ID == "" {
			t.ID = newID()
		}
		t.Type = strings.ToLower(strings.TrimSpace(t.Type))
		t.URL = strings.TrimRight(strings.TrimSpace(t.URL), "/")
		if t.Rewrites == nil {
			t.Rewrites = []Rewrite{}
		}
		if t.Name == "" {
			t.Name = t.Type
		}
	}
}

func (c *Config) validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	for typ := range c.TypeRewrites {
		if !validTypes[typ] {
			return fmt.Errorf("type_rewrites: unknown server type %q (use plex, jellyfin or emby)", typ)
		}
	}
	for _, p := range c.LibraryCheck.Paths {
		if !strings.HasPrefix(p, "/") {
			return fmt.Errorf("library check: folder %q must be an absolute path (starting with /)", p)
		}
	}
	ids := map[string]bool{}
	for _, t := range c.Targets {
		if !validTypes[t.Type] {
			return fmt.Errorf("target %q: type must be plex, jellyfin or emby", t.Name)
		}
		if !strings.HasPrefix(t.URL, "http://") && !strings.HasPrefix(t.URL, "https://") {
			return fmt.Errorf("target %q: url must start with http:// or https://", t.Name)
		}
		if ids[t.ID] {
			return fmt.Errorf("duplicate target id %q", t.ID)
		}
		ids[t.ID] = true
	}
	return nil
}

// Store guards the config and persists it to disk.
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  *Config
}

func loadStore(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		s.cfg = defaultConfig()
		if err := s.write(s.cfg); err != nil {
			return nil, fmt.Errorf("creating default config: %w", err)
		}
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	cfg.normalize()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	s.cfg = cfg
	return s, nil
}

// Get returns a deep copy so callers can't race with updates.
func (s *Store) Get() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, _ := json.Marshal(s.cfg)
	c := &Config{}
	_ = json.Unmarshal(data, c)
	return c
}

func (s *Store) Set(c *Config) error {
	c.normalize()
	if err := c.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.write(c); err != nil {
		return err
	}
	s.cfg = c
	return nil
}

// write saves atomically (temp file + rename).
func (s *Store) write(c *Config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func applyRewrites(path string, rws []Rewrite) string {
	for _, r := range rws {
		if r.From == "" {
			continue
		}
		from := strings.TrimRight(r.From, "/")
		if path == from || strings.HasPrefix(path, from+"/") {
			return strings.TrimRight(r.To, "/") + path[len(from):]
		}
	}
	return path
}

func (lc *LibraryCheck) normalize() {
	// A config without a library_check section gets the defaults.
	fresh := lc.Paths == nil && lc.Extensions == nil
	if fresh {
		lc.AllTargets = true
		lc.MinAgeMinutes = 10
	}
	if lc.IntervalMinutes <= 0 {
		lc.IntervalMinutes = 10
	}
	if lc.MinAgeMinutes < 0 {
		lc.MinAgeMinutes = 0
	}
	if lc.CooldownMinutes <= 0 {
		lc.CooldownMinutes = 360
	}
	if lc.Targets == nil {
		lc.Targets = []string{}
	}
	if lc.Rewrites == nil {
		lc.Rewrites = []Rewrite{}
	}
	if lc.Extensions == nil {
		lc.Extensions = append([]string{}, defaultExtensions...)
	}
	if lc.IgnoreFolders == nil {
		lc.IgnoreFolders = append([]string{}, defaultIgnoreFolders...)
	}
	if lc.IgnoreSuffixes == nil {
		lc.IgnoreSuffixes = append([]string{}, defaultIgnoreSuffixes...)
	}
	exts := []string{}
	for _, e := range lc.Extensions {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		exts = append(exts, e)
	}
	lc.Extensions = exts
	paths := []string{}
	for _, p := range lc.Paths {
		if p = strings.TrimSpace(p); p != "" {
			if p != "/" {
				p = strings.TrimRight(p, "/")
			}
			paths = append(paths, p)
		}
	}
	lc.Paths = paths
}
