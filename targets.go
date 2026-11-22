package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

// Media server item listings are paginated, but a page can still exceed 4 MiB
// when MediaSources are included. Keep a generous per-response ceiling while
// detecting oversized responses instead of silently truncating their JSON.
const maxAPIResponseBytes int64 = 64 << 20

func doRequest(req *http.Request) ([]byte, error) {
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Read the complete response up to a generous safety ceiling. The old
	// 4 MiB limit silently truncated larger Emby/Jellyfin JSON responses.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	if int64(len(body)) > maxAPIResponseBytes {
		return nil, fmt.Errorf("response body exceeds %d MiB limit", maxAPIResponseBytes>>20)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("authentication failed (HTTP %d): check the token", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
	return body, nil
}

func (t Target) newRequest(method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, t.URL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "media-scanner")
	switch t.Type {
	case "plex":
		req.Header.Set("X-Plex-Token", t.Token)
	case "jellyfin":
		req.Header.Set("Authorization",
			fmt.Sprintf(`MediaBrowser Client="media-scanner", Device="media-scanner", DeviceId="media-scanner", Version="2", Token="%s"`, t.Token))
	case "emby":
		req.Header.Set("X-Emby-Token", t.Token)
	}
	return req, nil
}

// Test checks the server is reachable and the token is valid.
func (t Target) Test() (string, error) {
	switch t.Type {
	case "plex":
		secs, err := t.plexSections()
		if err != nil {
			return "", err
		}
		names := make([]string, 0, len(secs))
		for _, s := range secs {
			names = append(names, s.Title)
		}
		return fmt.Sprintf("Connected. Libraries: %s", strings.Join(names, ", ")), nil
	case "jellyfin", "emby":
		infoPath := "/System/Info"
		if t.Type == "emby" {
			// Match the Emby API compatibility route used by Autoscan.
			infoPath = "/emby/System/Info"
		}
		req, err := t.newRequest("GET", infoPath, nil)
		if err != nil {
			return "", err
		}
		body, err := doRequest(req)
		if err != nil && t.Type == "emby" && strings.Contains(err.Error(), "HTTP 404") {
			// Some reverse proxies expose Emby's System API without the
			// /emby compatibility prefix.
			req, reqErr := t.newRequest("GET", "/System/Info", nil)
			if reqErr != nil {
				return "", reqErr
			}
			body, err = doRequest(req)
		}
		if err != nil {
			return "", err
		}
		var info struct {
			ServerName string
			Version    string
		}
		_ = json.Unmarshal(body, &info)
		return fmt.Sprintf("Connected to %s (version %s)", info.ServerName, info.Version), nil
	}
	return "", fmt.Errorf("unknown target type %q", t.Type)
}

// Scan asks the media server to scan a folder.
func (t Target) Scan(path string) (string, error) {
	switch t.Type {
	case "plex":
		return t.plexScan(path)
	case "jellyfin", "emby":
		return t.embyScan(path)
	}
	return "", fmt.Errorf("unknown target type %q", t.Type)
}

type plexSection struct {
	Key      string `json:"key"`
	Title    string `json:"title"`
	Type     string `json:"type"` // movie, show, artist, photo
	Location []struct {
		Path string `json:"path"`
	} `json:"Location"`
}

func (t Target) plexSections() ([]plexSection, error) {
	req, err := t.newRequest("GET", "/library/sections", nil)
	if err != nil {
		return nil, err
	}
	body, err := doRequest(req)
	if err != nil {
		return nil, err
	}
	var res struct {
		MediaContainer struct {
			Directory []plexSection `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("unexpected response from plex: %w", err)
	}
	return res.MediaContainer.Directory, nil
}

func pathWithin(path, root string) bool {
	root = strings.TrimRight(root, "/")
	return path == root || strings.HasPrefix(path, root+"/")
}

func (t Target) plexScan(path string) (string, error) {
	secs, err := t.plexSections()
	if err != nil {
		return "", err
	}
	// Pick the library whose folder is the longest match for the path.
	var best *plexSection
	bestLen := -1
	for i := range secs {
		for _, loc := range secs[i].Location {
			if pathWithin(path, loc.Path) && len(loc.Path) > bestLen {
				best, bestLen = &secs[i], len(loc.Path)
			}
		}
	}
	if best == nil {
		return "", fmt.Errorf("no Plex library contains %s (add a path rewrite?)", path)
	}
	q := url.Values{"path": {path}}
	req, err := t.newRequest("GET", "/library/sections/"+best.Key+"/refresh?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	if _, err := doRequest(req); err != nil {
		return "", err
	}
	return fmt.Sprintf("scan queued in library %q", best.Title), nil
}

func (t Target) embyScan(path string) (string, error) {
	updateType := "Modified" // Jellyfin's scan notification type.
	if t.Type == "emby" {
		updateType = "Created" // Match Autoscan's Emby notification contract.
	}
	type scanRequest struct {
		Path       string `json:"path"`
		UpdateType string `json:"updateType"`
	}
	payload, err := json.Marshal(struct {
		Updates []scanRequest `json:"Updates"`
	}{
		Updates: []scanRequest{{Path: path, UpdateType: updateType}},
	})
	if err != nil {
		return "", fmt.Errorf("encoding scan request: %w", err)
	}
	req, err := t.newRequest("POST", "/Library/Media/Updated", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if _, err := doRequest(req); err != nil {
		return "", err
	}
	return "scan requested", nil
}
