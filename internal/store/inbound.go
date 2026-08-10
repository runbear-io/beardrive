package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// The inbound spool queues the paths a cycle materialized from peers, until
// the agent hook (`bdrive sync --hook`) drains it into the turn's context —
// "these changed since your last turn, re-read before editing". It is a spool
// rather than a field on Result because the daemon usually materializes the
// peer's change seconds before the turn starts, so the hook's own cycle
// reports nothing: the record has to outlive the cycle that made it.

// InboundEvent is one path a cycle wrote or removed on a peer's behalf
// (mount-relative).
type InboundEvent struct {
	Path    string    `json:"path"`
	Deleted bool      `json:"deleted,omitempty"`
	Time    time.Time `json:"time"`
}

// inboundSpoolMax caps the spool: a machine with no agent hooks never drains,
// so past the cap new events are dropped rather than growing without bound.
const inboundSpoolMax = 1 << 20

// inboundDrainMax bounds one drained batch; the hook renders far fewer.
const inboundDrainMax = 4096

func (s *Store) inboundSpoolPath() string { return filepath.Join(s.dir, "inbound.jsonl") }
func (s *Store) inboundDrainPath() string { return filepath.Join(s.dir, "inbound-draining.jsonl") }

// LogInbound appends one materialized path to the spool. Single-line O_APPEND
// writes keep the daemon and a concurrent CLI cycle from interleaving.
func (s *Store) LogInbound(rel string, deleted bool) error {
	if fi, err := os.Stat(s.inboundSpoolPath()); err == nil && fi.Size() > inboundSpoolMax {
		return nil // spool full: drop, never grow unbounded
	}
	line, err := json.Marshal(InboundEvent{Path: rel, Deleted: deleted, Time: time.Now().UTC()})
	if err != nil {
		return err
	}
	// 0600: the spool is a list of this project's file paths.
	f, err := os.OpenFile(s.inboundSpoolPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// DrainInbound returns the queued batch and clears it, deduplicated by path
// (latest event wins, so a path written and then deleted reports as deleted).
// The spool is rotated aside first, so events logged during the drain land in
// a fresh spool — the drain runs outside the volume flock, and a daemon on
// the same mount may be appending.
//
// One call, unlike PendingReads/ClearPendingReads: those are two steps
// because a read report can fail over the network and must be retried, and
// rendering a string onto stdout cannot.
func (s *Store) DrainInbound() ([]InboundEvent, error) {
	if _, err := os.Stat(s.inboundDrainPath()); os.IsNotExist(err) {
		if err := os.Rename(s.inboundSpoolPath(), s.inboundDrainPath()); err != nil {
			if os.IsNotExist(err) {
				return nil, nil // nothing queued
			}
			return nil, err
		}
	}
	data, err := os.ReadFile(s.inboundDrainPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		// Unreadable: drop it rather than wedging every future drain behind
		// it. Losing a turn's list is the cheaper failure.
		os.Remove(s.inboundDrainPath())
		return nil, err
	}
	defer os.Remove(s.inboundDrainPath())

	latest := map[string]InboundEvent{}
	var order []string
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e InboundEvent
		if json.Unmarshal(line, &e) != nil || e.Path == "" {
			continue // torn or corrupt line; drop it
		}
		if _, ok := latest[e.Path]; !ok {
			order = append(order, e.Path)
		}
		if prev, ok := latest[e.Path]; !ok || !e.Time.Before(prev.Time) {
			latest[e.Path] = e
		}
	}
	if len(order) > inboundDrainMax {
		order = order[len(order)-inboundDrainMax:]
	}
	out := make([]InboundEvent, 0, len(order))
	for _, p := range order {
		out = append(out, latest[p])
	}
	return out, nil
}
