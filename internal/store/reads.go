package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// The read spool queues agent tool reads observed by `bdrive read-log` (the
// agent read hook) until a sync cycle drains it to the hub, where they count
// as agent traffic in the read heatmap. Hooks only append locally — no
// network on the hook path — and the flush is best-effort: offline, the
// spool just waits.

// ReadEvent is one observed read of a synced file (mount-relative path).
type ReadEvent struct {
	Path string    `json:"path"`
	Time time.Time `json:"time"`
}

// readSpoolMax caps the spool: past it new events are dropped rather than
// letting an unreachable hub grow telemetry without bound.
const readSpoolMax = 1 << 20

// readReportMax bounds one drained batch to what the hub accepts per report.
const readReportMax = 4096

func (s *Store) readSpoolPath() string { return filepath.Join(s.dir, "reads.jsonl") }
func (s *Store) readFlushPath() string { return filepath.Join(s.dir, "reads-flushing.jsonl") }

// LogRead appends one read event to the spool. Single-line O_APPEND writes
// keep concurrent hook invocations from interleaving.
func (s *Store) LogRead(rel string) error {
	if fi, err := os.Stat(s.readSpoolPath()); err == nil && fi.Size() > readSpoolMax {
		return nil // spool full: drop, never grow unbounded
	}
	line, err := json.Marshal(ReadEvent{Path: rel, Time: time.Now().UTC()})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.readSpoolPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// PendingReads returns the queued batch awaiting report, deduplicated by path
// (latest time wins). The spool is rotated aside first, so events logged
// after this call land in a fresh spool; the batch survives until
// ClearPendingReads — a failed report is simply retried next cycle.
func (s *Store) PendingReads() ([]ReadEvent, error) {
	if _, err := os.Stat(s.readFlushPath()); os.IsNotExist(err) {
		if err := os.Rename(s.readSpoolPath(), s.readFlushPath()); err != nil {
			if os.IsNotExist(err) {
				return nil, nil // nothing queued
			}
			return nil, err
		}
	}
	data, err := os.ReadFile(s.readFlushPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	latest := map[string]time.Time{}
	var order []string
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e ReadEvent
		if json.Unmarshal(line, &e) != nil || e.Path == "" {
			continue // torn or corrupt line; drop it
		}
		if _, ok := latest[e.Path]; !ok {
			order = append(order, e.Path)
		}
		if e.Time.After(latest[e.Path]) {
			latest[e.Path] = e.Time
		}
	}
	if len(order) > readReportMax {
		order = order[len(order)-readReportMax:]
	}
	out := make([]ReadEvent, 0, len(order))
	for _, p := range order {
		out = append(out, ReadEvent{Path: p, Time: latest[p]})
	}
	return out, nil
}

// ClearPendingReads drops the batch PendingReads returned, after a
// successful report.
func (s *Store) ClearPendingReads() error {
	err := os.Remove(s.readFlushPath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
