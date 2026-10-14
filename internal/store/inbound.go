package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The inbound spool queues the paths a cycle materialized from peers, until
// the agent hook (`bdrive sync --hook`) drains it into the turn's context —
// "these changed since your last turn, re-read before editing". It is a spool
// rather than a field on Result because the daemon usually materializes the
// peer's change seconds before the turn starts, so the hook's own cycle
// reports nothing: the record has to outlive the cycle that made it.
//
// syncer.Result.Inbound carries the same events and is NOT a duplicate to be
// deleted. The post_sync hook fires from the cycle that materialized, so it
// wants the batch in-process; DrainInbound is destructive and single-consumer,
// so a second drainer would silently empty the agent hook's context about half
// the time. Different lifetimes, different consumers — keep both.

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

func (s *Store) inboundMountPaths(mountID string) (spool, drain string, err error) {
	if mountID == "" {
		return s.inboundSpoolPath(), s.inboundDrainPath(), nil
	}
	if mountID == "." || mountID == ".." || strings.ContainsAny(mountID, `/\`) {
		return "", "", fmt.Errorf("invalid mount id %q", mountID)
	}
	return filepath.Join(s.dir, "inbound-"+mountID+".jsonl"),
		filepath.Join(s.dir, "inbound-draining-"+mountID+".jsonl"), nil
}

func (s *Store) logInboundPath(spool, rel string, deleted bool) error {
	if fi, err := os.Stat(spool); err == nil && fi.Size() > inboundSpoolMax {
		return nil // spool full: drop, never grow unbounded
	}
	line, err := json.Marshal(InboundEvent{Path: rel, Deleted: deleted, Time: time.Now().UTC()})
	if err != nil {
		return err
	}
	// 0600: the spool is a list of this project's file paths.
	f, err := os.OpenFile(spool, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// LogInbound appends one materialized path to the legacy spool for backward
// compatibility with callers and installations that do not have a mount id.
func (s *Store) LogInbound(rel string, deleted bool) error {
	spool, _, err := s.inboundMountPaths("")
	if err != nil {
		return err
	}
	return s.logInboundPath(spool, rel, deleted)
}

// LogInboundForMount appends one materialized path to the mount's spool.
func (s *Store) LogInboundForMount(mountID, rel string, deleted bool) error {
	spool, _, err := s.inboundMountPaths(mountID)
	if err != nil {
		return err
	}
	return s.logInboundPath(spool, rel, deleted)
}

// drainInboundPaths rotates one spool aside, reads the queued batch, and
// clears the drain file. Events logged during the drain land in a fresh spool.
// The drain runs outside the volume flock, and a daemon on the same mount may
// be appending.
func (s *Store) drainInboundPaths(spool, drain string) ([]InboundEvent, error) {
	if _, err := os.Stat(drain); os.IsNotExist(err) {
		if err := os.Rename(spool, drain); err != nil {
			if os.IsNotExist(err) {
				return nil, nil // nothing queued
			}
			return nil, err
		}
	}
	data, err := os.ReadFile(drain)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		// Unreadable: drop it rather than wedging every future drain behind
		// it. Losing a turn's list is the cheaper failure.
		os.Remove(drain)
		return nil, err
	}
	defer os.Remove(drain)

	var events []InboundEvent
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e InboundEvent
		if json.Unmarshal(line, &e) != nil || e.Path == "" {
			continue // torn or corrupt line; drop it
		}
		events = append(events, e)
	}
	return mergeInboundEvents(events), nil
}

func mergeInboundEvents(groups ...[]InboundEvent) []InboundEvent {
	latest := map[string]InboundEvent{}
	var order []string
	for _, events := range groups {
		for _, e := range events {
			if _, ok := latest[e.Path]; !ok {
				order = append(order, e.Path)
			}
			if prev, ok := latest[e.Path]; !ok || !e.Time.Before(prev.Time) {
				latest[e.Path] = e
			}
		}
	}
	if len(order) > inboundDrainMax {
		order = order[len(order)-inboundDrainMax:]
	}
	out := make([]InboundEvent, 0, len(order))
	for _, p := range order {
		out = append(out, latest[p])
	}
	return out
}

// DrainInbound returns the queued batch from the legacy spool.
func (s *Store) DrainInbound() ([]InboundEvent, error) {
	return s.drainInboundPaths(s.inboundSpoolPath(), s.inboundDrainPath())
}

// DrainInboundForMount drains a mount-specific spool. Legacy spool events are
// also drained when present so older callers remain visible to the hook.
func (s *Store) DrainInboundForMount(mountID string) ([]InboundEvent, error) {
	if mountID == "" {
		return s.DrainInbound()
	}
	spool, drain, err := s.inboundMountPaths(mountID)
	if err != nil {
		return nil, err
	}
	mountEvents, err := s.drainInboundPaths(spool, drain)
	if err != nil {
		return nil, err
	}
	var legacyEvents []InboundEvent
	_, spoolErr := os.Stat(s.inboundSpoolPath())
	_, drainErr := os.Stat(s.inboundDrainPath())
	if spoolErr == nil || drainErr == nil {
		legacyEvents, err = s.DrainInbound()
		if err != nil {
			return nil, err
		}
	}
	return mergeInboundEvents(mountEvents, legacyEvents), nil
}
