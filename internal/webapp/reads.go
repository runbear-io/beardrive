package webapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Read telemetry: who consumes what, aggregated. Together with the write
// history the journals already carry, this completes the read×write matrix —
// heavily-read + long-unwritten is the danger zone an admin should fix first
// (see docs/design/read-heatmap.md).
//
// What counts as a read: viewer file/render/download hits (human), share-link
// hits (share), and agent tool reads reported by syncing devices (agent).
// /store/* sync traffic is replication, not reading, and is never counted;
// history /blob views are spelunking, not consumption, and aren't either.
//
// Privacy: rows are daily aggregation buckets, never an event log. The actor
// column (account email / device id / share token) exists only to count
// distinct readers and never appears in an API response.

// Read kinds.
const (
	ReadKindHuman = "human"
	ReadKindAgent = "agent"
	ReadKindShare = "share"
)

// ReadStat is one aggregation bucket: reads of one path by one actor on one
// day. Day == "" is the all-time fold that survives retention.
type ReadStat struct {
	Project string    `json:"project"`
	Path    string    `json:"path"`
	Day     string    `json:"day"` // "2006-01-02" UTC, or "" for all-time
	Kind    string    `json:"kind"`
	Actor   string    `json:"actor"`
	Count   int64     `json:"count"`
	Last    time.Time `json:"last"`
}

// ReadStatKey identifies one bucket.
type ReadStatKey struct {
	Project, Path, Day, Kind, Actor string
}

func (s ReadStat) key() ReadStatKey {
	return ReadStatKey{s.Project, s.Path, s.Day, s.Kind, s.Actor}
}

// HeatEntry is the per-path aggregate the heat API returns. Counts only —
// never identities.
type HeatEntry struct {
	Human    int64     `json:"human,omitempty"`
	Agent    int64     `json:"agent,omitempty"`
	Share    int64     `json:"share,omitempty"`
	Readers  int       `json:"readers,omitempty"` // distinct human readers
	LastRead time.Time `json:"last_read,omitzero"`
}

const (
	// readDebounce collapses request storms (reloads, render-then-raw double
	// fetches) into visits: repeat reads of a path by the same actor within
	// the window don't count again.
	readDebounce = 10 * time.Minute
	// readFlushEvery throttles persistence; dirty buckets ride in memory
	// between flushes, so a crash loses at most this much telemetry.
	readFlushEvery = 30 * time.Second
	// DefaultReadRetentionDays is how long daily buckets keep per-day
	// resolution before folding into the all-time row.
	DefaultReadRetentionDays = 400
)

// ReadLedger is the in-memory read-telemetry service over a ReadRepo, in the
// mold of DeviceRegistry: reads stay in memory, writes are throttled. There is
// no background goroutine — flushes piggyback on Record calls, and telemetry
// failures never surface to the request that triggered them.
type ReadLedger struct {
	repo      ReadRepo
	retention time.Duration

	mu         sync.Mutex
	byKey      map[ReadStatKey]ReadStat
	dirty      map[ReadStatKey]bool
	pendingDel []ReadStatKey             // retention deletions awaiting a successful flush
	seen       map[ReadStatKey]time.Time // debounce; Day field unused ("")
	lastFlush  time.Time
	warned     bool
}

// NewReadLedger loads the ledger and immediately folds buckets older than the
// retention horizon into their all-time rows. retentionDays <= 0 means the
// default.
func NewReadLedger(repo ReadRepo, retentionDays int) (*ReadLedger, error) {
	if retentionDays <= 0 {
		retentionDays = DefaultReadRetentionDays
	}
	l := &ReadLedger{
		repo:      repo,
		retention: time.Duration(retentionDays) * 24 * time.Hour,
		byKey:     map[ReadStatKey]ReadStat{},
		dirty:     map[ReadStatKey]bool{},
		seen:      map[ReadStatKey]time.Time{},
		lastFlush: time.Now(),
	}
	stats, err := repo.Load()
	if err != nil {
		return nil, err
	}
	for _, st := range stats {
		l.byKey[st.key()] = st
	}
	// Fold anything past the retention horizon right away. A failed persist
	// is not a boot failure — the fold stays dirty and later flushes retry.
	l.mu.Lock()
	l.compactLocked()
	if err := l.persistLocked(); err != nil {
		log.Printf("beardrive: read telemetry compact failed (will retry): %v", err)
	}
	l.mu.Unlock()
	return l, nil
}

// OpenReadLedger loads the file-backed ledger at path.
func OpenReadLedger(path string, retentionDays int) (*ReadLedger, error) {
	return NewReadLedger(newFileReadRepo(path), retentionDays)
}

// Record counts one read. Nil-safe and never fails: telemetry must not break
// the page view (or sync cycle) that triggered it.
func (l *ReadLedger) Record(project, path, kind, actor string) {
	if l == nil || project == "" || path == "" {
		return
	}
	now := time.Now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()
	visit := ReadStatKey{Project: project, Path: path, Kind: kind, Actor: actor}
	if t, ok := l.seen[visit]; ok && now.Sub(t) < readDebounce {
		return
	}
	l.seen[visit] = now
	key := visit
	key.Day = now.Format("2006-01-02")
	st := l.byKey[key]
	st.Project, st.Path, st.Day, st.Kind, st.Actor = project, path, key.Day, kind, actor
	st.Count++
	st.Last = now
	l.byKey[key] = st
	l.dirty[key] = true
	if now.Sub(l.lastFlush) >= readFlushEvery {
		l.flushLocked()
	}
}

// Heat aggregates reads per path for one project. since bounds the window
// (zero = all time, including retention folds); prefix "" means the whole
// project, otherwise paths under "<prefix>/".
func (l *ReadLedger) Heat(project, prefix string, since time.Time) map[string]HeatEntry {
	if l == nil {
		return nil
	}
	sinceDay := ""
	if !since.IsZero() {
		sinceDay = since.UTC().Format("2006-01-02")
	}
	prefix = strings.TrimSuffix(prefix, "/")
	out := map[string]HeatEntry{}
	humans := map[string]map[string]bool{} // path → distinct human actors
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, st := range l.byKey {
		if key.Project != project {
			continue
		}
		if prefix != "" && !strings.HasPrefix(key.Path, prefix+"/") {
			continue
		}
		if key.Day == "" {
			if sinceDay != "" {
				continue // all-time fold is older than any windowed query
			}
		} else if key.Day < sinceDay {
			continue
		}
		e := out[key.Path]
		switch key.Kind {
		case ReadKindHuman:
			e.Human += st.Count
			set := humans[key.Path]
			if set == nil {
				set = map[string]bool{}
				humans[key.Path] = set
			}
			set[key.Actor] = true
		case ReadKindAgent:
			e.Agent += st.Count
		case ReadKindShare:
			e.Share += st.Count
		}
		if st.Last.After(e.LastRead) {
			e.LastRead = st.Last
		}
		out[key.Path] = e
	}
	for p, set := range humans {
		e := out[p]
		e.Readers = len(set)
		out[p] = e
	}
	return out
}

// AgentHeat aggregates agent reads per device per top-level folder ("" for
// root files) — the coverage-matrix data. Agent buckets only, by design:
// agent actors are device ids, which history already exposes; human actors
// (emails) must never leave the server, so human/share buckets are not
// consulted at all.
func (l *ReadLedger) AgentHeat(project string, since time.Time) map[string]map[string]int64 {
	if l == nil {
		return nil
	}
	sinceDay := ""
	if !since.IsZero() {
		sinceDay = since.UTC().Format("2006-01-02")
	}
	out := map[string]map[string]int64{}
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, st := range l.byKey {
		if key.Project != project || key.Kind != ReadKindAgent {
			continue
		}
		if key.Day == "" {
			if sinceDay != "" {
				continue
			}
		} else if key.Day < sinceDay {
			continue
		}
		folder := ""
		if i := strings.IndexByte(key.Path, '/'); i >= 0 {
			folder = key.Path[:i]
		}
		m := out[key.Actor]
		if m == nil {
			m = map[string]int64{}
			out[key.Actor] = m
		}
		m[folder] += st.Count
	}
	return out
}

// Close flushes any pending buckets.
func (l *ReadLedger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flushLocked()
	if n := len(l.dirty); n > 0 {
		return fmt.Errorf("read ledger: flush failed, %d buckets pending", n)
	}
	return nil
}

// flushLocked persists dirty buckets (and, once a day, retention folds),
// pruning the debounce map along the way. Failures keep the buckets dirty for
// the next attempt and log once — telemetry never breaks a request.
func (l *ReadLedger) flushLocked() {
	now := time.Now()
	l.lastFlush = now
	for k, t := range l.seen {
		if now.Sub(t) >= readDebounce {
			delete(l.seen, k)
		}
	}
	l.compactLocked()
	if err := l.persistLocked(); err != nil {
		if !l.warned {
			l.warned = true
			log.Printf("beardrive: read telemetry flush failed (will retry): %v", err)
		}
	} else {
		l.warned = false
	}
}

// compactLocked folds daily buckets older than the retention horizon into
// their all-time rows, queueing the daily rows for deletion. Callers hold mu.
func (l *ReadLedger) compactLocked() {
	horizon := time.Now().UTC().Add(-l.retention).Format("2006-01-02")
	for key, st := range l.byKey {
		if key.Day == "" || key.Day >= horizon {
			continue
		}
		fold := key
		fold.Day = ""
		agg := l.byKey[fold]
		agg.Project, agg.Path, agg.Kind, agg.Actor = st.Project, st.Path, st.Kind, st.Actor
		agg.Day = ""
		agg.Count += st.Count
		if st.Last.After(agg.Last) {
			agg.Last = st.Last
		}
		l.byKey[fold] = agg
		l.dirty[fold] = true
		delete(l.byKey, key)
		delete(l.dirty, key)
		l.pendingDel = append(l.pendingDel, key)
	}
}

// persistLocked writes queued deletions and dirty buckets through the repo.
// Callers hold mu. Both queues survive a failure so the next flush retries —
// dropping a deletion would resurrect folded rows on the next load and
// double-count them.
func (l *ReadLedger) persistLocked() error {
	if len(l.pendingDel) > 0 {
		if err := l.repo.DeleteBatch(l.pendingDel); err != nil {
			return err
		}
		l.pendingDel = nil
	}
	if len(l.dirty) == 0 {
		return nil
	}
	batch := make([]ReadStat, 0, len(l.dirty))
	for key := range l.dirty {
		batch = append(batch, l.byKey[key])
	}
	if err := l.repo.PutBatch(batch); err != nil {
		return err
	}
	l.dirty = map[ReadStatKey]bool{}
	return nil
}

// ---- server integration ----

// ctxProjectKey carries the resolved project id from the proj() route
// resolver to handlers that record reads.
type ctxProjectKey struct{}

func withProjectID(r *http.Request, id string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxProjectKey{}, id))
}

func projectID(r *http.Request) string {
	id, _ := r.Context().Value(ctxProjectKey{}).(string)
	return id
}

// recordRead counts a human read of path for the request's project. No-op
// outside hub mode (no project id) or when read tracking is off.
func (s *Server) recordRead(r *http.Request, path string) {
	if s.Reads == nil {
		return
	}
	project := projectID(r)
	if project == "" {
		return
	}
	actor := s.requestUser(r).Email
	if actor == "" {
		actor = "anonymous"
	}
	s.Reads.Record(project, path, ReadKindHuman, actor)
}

// handleHeat serves per-path read aggregates: ?prefix= bounds to a folder,
// ?days= bounds the window (default 30, 0 = all time). With ?by=device it
// returns the agent-kind breakdown instead: per device (registry-joined),
// reads per top-level folder. In both shapes, counts only — human actor
// identities never leave the server (agent devices are already public via
// history, so naming them here is consistent).
func (s *Server) handleHeat(v *volume, w http.ResponseWriter, r *http.Request) {
	if s.Reads == nil {
		http.Error(w, "read tracking is not enabled on this server", http.StatusNotFound)
		return
	}
	_ = v
	q := r.URL.Query()
	days := 30
	if raw := q.Get("days"); raw != "" {
		var err error
		if days, err = strconv.Atoi(raw); err != nil || days < 0 {
			http.Error(w, "invalid days", http.StatusBadRequest)
			return
		}
	}
	var since time.Time
	if days > 0 {
		since = time.Now().UTC().AddDate(0, 0, -days)
	}
	switch q.Get("by") {
	case "":
	case "device":
		s.heatByDevice(w, projectID(r), since)
		return
	default:
		http.Error(w, "invalid by (use device)", http.StatusBadRequest)
		return
	}
	entries := s.Reads.Heat(projectID(r), q.Get("prefix"), since)
	out := map[string]any{"entries": entries}
	if !since.IsZero() {
		out["since"] = since.Format("2006-01-02")
	}
	writeJSON(w, out)
}

// deviceHeat is one row of the ?by=device response.
type deviceHeat struct {
	ID      string           `json:"id"`
	Name    string           `json:"name,omitempty"`
	OS      string           `json:"os,omitempty"`
	Folders map[string]int64 `json:"folders"`
	Total   int64            `json:"total"`
}

func (s *Server) heatByDevice(w http.ResponseWriter, project string, since time.Time) {
	byDevice := s.Reads.AgentHeat(project, since)
	devices := make([]deviceHeat, 0, len(byDevice))
	for id, folders := range byDevice {
		d := deviceHeat{ID: id, Folders: folders}
		if info, ok := s.Devices.Get(id); ok {
			d.Name, d.OS = info.Name, info.OS
		}
		for _, n := range folders {
			d.Total += n
		}
		devices = append(devices, d)
	}
	sort.Slice(devices, func(i, j int) bool {
		if devices[i].Total != devices[j].Total {
			return devices[i].Total > devices[j].Total
		}
		return devices[i].ID < devices[j].ID
	})
	out := map[string]any{"devices": devices}
	if !since.IsZero() {
		out["since"] = since.Format("2006-01-02")
	}
	writeJSON(w, out)
}

// handleReadReport ingests agent reads from a syncing device: the client's
// read spool, drained best-effort at sync time. Requires a device identity —
// the device id is the actor, so reads count as agent traffic.
func (s *Server) handleReadReport(v *volume, w http.ResponseWriter, r *http.Request) {
	if s.Reads == nil {
		http.Error(w, "read tracking is not enabled on this server", http.StatusNotFound)
		return
	}
	_ = v
	device := r.Header.Get("X-Bdrive-Device")
	if device == "" {
		http.Error(w, "agent read reports need a device identity", http.StatusBadRequest)
		return
	}
	var req struct {
		Reads []struct {
			Path string `json:"path"`
			// Time is accepted for forward compatibility but buckets use
			// server time: client clocks are unreliable and late flushes are
			// telemetry noise, not data loss.
			Time time.Time `json:"time,omitzero"`
		} `json:"reads"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Reads) > 4096 {
		http.Error(w, "too many reads in one report", http.StatusBadRequest)
		return
	}
	s.observeDevice(r)
	project := projectID(r)
	n := 0
	for _, e := range req.Reads {
		if e.Path == "" || strings.Contains(e.Path, "..") {
			continue
		}
		s.Reads.Record(project, e.Path, ReadKindAgent, device)
		n++
	}
	writeJSON(w, map[string]any{"accepted": n})
}
