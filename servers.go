package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

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

// KnownFiles returns the (normalised) paths of the media files the server has
// indexed under any of roots. With a non-zero since, only items added or
// changed since then are requested. progress may be nil.
func (t Target) KnownFiles(roots []string, since time.Time, progress pageProgress) (map[string]bool, error) {
	if progress == nil {
		progress = func(string, int, int) {}
	}
	switch t.Type {
	case "plex":
		return t.plexFiles(roots, since, progress)
	case "jellyfin", "emby":
		return t.embyFiles(since, progress)
	}
	return nil, fmt.Errorf("unknown target type %q", t.Type)
}

// overlaps reports whether a and b are the same folder or one contains the other.
func overlaps(a, b string) bool {
	a, b = normPath(a), normPath(b)
	return pathWithin(a, b) || pathWithin(b, a)
}

var pageSize = 1000 // items per request when listing a library (a var so tests can page)

func (t Target) plexFiles(roots []string, since time.Time, progress pageProgress) (map[string]bool, error) {
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
			query := q.Encode()
			if !since.IsZero() {
				// Plex filter syntax for "updated at or after": updatedAt>>=<unix time>.
				query += "&updatedAt>>=" + strconv.FormatInt(since.Unix(), 10)
			}
			req, err := t.newRequest("GET", "/library/sections/"+sec.Key+"/all?"+query, nil)
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

func (t Target) embyFiles(since time.Time, progress pageProgress) (map[string]bool, error) {
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
		if !since.IsZero() {
			q.Set("MinDateLastSaved", since.UTC().Format(time.RFC3339))
		}
		req, err := t.newRequest("GET", "/Items?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		body, err := doRequest(req)
		if err != nil {
			return nil, fmt.Errorf("listing items at offset %d: %w", start, err)
		}
		var res struct {
			Items []struct {
				Path         string
				MediaSources []struct{ Path string }
			}
			TotalRecordCount int
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("unexpected response from %s at offset %d (%d bytes): %w", t.Type, start, len(body), err)
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
		if len(res.Items) == 0 || (res.TotalRecordCount > 0 && start+len(res.Items) >= res.TotalRecordCount) {
			break
		}
		// Some Emby-compatible servers may return fewer than the requested
		// page size while still reporting additional records. Continue using
		// the server's total count rather than treating a short page as EOF.
		if res.TotalRecordCount == 0 && len(res.Items) < pageSize {
			break
		}
	}
	return known, nil
}
