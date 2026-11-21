package main

import (
	"log"
	"sort"
	"sync"
	"time"
)

type Event struct {
	Time    time.Time `json:"time"`
	Source  string    `json:"source"`
	Path    string    `json:"path"`
	Target  string    `json:"target"`
	Status  string    `json:"status"` // queued | ok | error | skipped
	Message string    `json:"message"`
}

type pending struct {
	path    string
	source  string
	targets map[string]bool // target IDs
	all     bool
	due     time.Time
}

// Queue batches and de-duplicates scan requests before sending them.
type Queue struct {
	store *Store

	mu             sync.Mutex
	items          map[string]*pending
	history        []Event
	stats          map[string]int
	fileCheckDelay time.Duration
}

const (
	historySize      = 200
	defaultFileCheckDelay = 5 * time.Second
)

func newQueue(store *Store) *Queue {
	return &Queue{store: store, items: map[string]*pending{}, stats: map[string]int{},
		fileCheckDelay: defaultFileCheckDelay}
}

func (q *Queue) record(e Event) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.recordLocked(e)
}

func (q *Queue) recordLocked(e Event) {
	e.Time = time.Now()
	q.history = append(q.history, e)
	if len(q.history) > historySize {
		q.history = q.history[len(q.history)-historySize:]
	}
	q.stats[e.Status]++
	log.Printf("[%s] %s %s %s %s", e.Status, e.Source, e.Target, e.Path, e.Message)
}

// Add queues a path from a trigger. Trigger rewrites are applied by the caller.
func (q *Queue) Add(source, path string, all bool, targetIDs []string) {
	cfg := q.store.Get()
	q.mu.Lock()
	defer q.mu.Unlock()
	p := q.items[path]
	if p == nil {
		p = &pending{
			path:    path,
			source:  source,
			targets: map[string]bool{},
			due:     time.Now().Add(time.Duration(cfg.ScanDelay) * time.Second),
		}
		q.items[path] = p
	}
	if all {
		p.all = true
	}
	for _, id := range targetIDs {
		p.targets[id] = true
	}
	q.recordLocked(Event{Source: source, Path: path, Status: "queued",
		Message: "waiting " + (time.Until(p.due).Round(time.Second)).String()})
}

func (q *Queue) Pending() []Event {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := []Event{}
	for _, p := range q.items {
		out = append(out, Event{Time: p.due, Source: p.source, Path: p.path, Status: "pending"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

func (q *Queue) History() []Event {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Event, len(q.history))
	for i, e := range q.history {
		out[len(q.history)-1-i] = e // newest first
	}
	return out
}

func (q *Queue) Stats() map[string]int {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := map[string]int{}
	for k, v := range q.stats {
		out[k] = v
	}
	return out
}

func (q *Queue) Run() {
	for range time.Tick(time.Second) {
		q.mu.Lock()
		var due []*pending
		now := time.Now()
		for k, p := range q.items {
			if !p.due.After(now) {
				due = append(due, p)
				delete(q.items, k)
			}
		}
		q.mu.Unlock()
		for _, p := range due {
			q.process(p)
		}
	}
}

// process waits briefly for mounts/downloads to settle, then checks the actual
// container filesystem. A server scan is only sent if at least one supported
// media file is present beneath the queued directory.
func (q *Queue) process(p *pending) {
	if q.fileCheckDelay > 0 {
		time.Sleep(q.fileCheckDelay)
	}

	cfg := q.store.Get()
	lc := cfg.LibraryCheck
	lc.Paths = []string{p.path}
	lc.normalize()
	found := findMedia(lc, nil, nil)
	if len(found.Errors) > 0 {
		q.record(Event{Source: p.source, Path: p.path, Status: "error",
			Message: "filesystem check failed: " + found.Errors[0]})
		return
	}
	if len(found.Files) == 0 {
		q.record(Event{Source: p.source, Path: p.path, Status: "skipped",
			Message: "no media files found after filesystem check; no server scan sent"})
		return
	}

	sent := 0
	for _, t := range cfg.Targets {
		if !t.Enabled || (!p.all && !p.targets[t.ID]) {
			continue
		}
		sent++
		path := applyRewrites(applyRewrites(p.path, cfg.TypeRewrites[t.Type]), t.Rewrites)
		msg, err := t.Scan(path)
		e := Event{Source: p.source, Path: path, Target: t.Name, Status: "ok", Message: msg}
		if err != nil {
			e.Status, e.Message = "error", err.Error()
		}
		q.record(e)
	}
	if sent == 0 {
		q.record(Event{Source: p.source, Path: p.path, Status: "skipped", Message: "no enabled media servers selected for this trigger"})
	}
}
