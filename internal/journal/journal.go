// Package journal implements sfs's append-only operation log.
//
// Every change to a volume is recorded as an Op in a per-device JSONL
// journal. Journals are append-only and each device only ever writes its
// own journal, so syncing is conflict-free at the transport level: a sync
// uploads your journal and downloads everyone else's. The merged view of
// a volume is a deterministic replay of the union of all ops ordered by
// (lamport, time, device, seq) — every device converges to the same state.
package journal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

const (
	KindPut    = "put"
	KindDelete = "delete"
)

// Op is a single journaled file operation.
type Op struct {
	Seq        int64     `json:"seq"`     // per-device sequence number, 1-based
	Lamport    int64     `json:"lamport"` // logical clock for cross-device ordering
	Time       time.Time `json:"time"`
	Device     string    `json:"device"`
	DeviceName string    `json:"device_name,omitempty"`
	Author     string    `json:"author,omitempty"`
	Kind       string    `json:"kind"` // "put" or "delete"
	Path       string    `json:"path"` // slash-separated, relative to volume root
	Blob       string    `json:"blob,omitempty"` // sha256 hex of content (put only)
	Size       int64     `json:"size,omitempty"`
	Mode       uint32    `json:"mode,omitempty"` // permission bits
	Note       string    `json:"note,omitempty"` // e.g. "conflict copy of <path>"
}

// Less defines the total order used to replay ops from many devices.
func Less(a, b Op) bool {
	if a.Lamport != b.Lamport {
		return a.Lamport < b.Lamport
	}
	if !a.Time.Equal(b.Time) {
		return a.Time.Before(b.Time)
	}
	if a.Device != b.Device {
		return a.Device < b.Device
	}
	return a.Seq < b.Seq
}

func Sort(ops []Op) {
	sort.SliceStable(ops, func(i, j int) bool { return Less(ops[i], ops[j]) })
}

// FileState is the resolved state of one path after replay.
type FileState struct {
	Blob string
	Size int64
	Mode uint32
}

// Replay folds a set of ops (from any number of devices) into the
// resulting volume state. Last writer wins per path under the total order.
func Replay(ops []Op) map[string]FileState {
	sorted := append([]Op(nil), ops...)
	Sort(sorted)
	state := make(map[string]FileState)
	for _, op := range sorted {
		switch op.Kind {
		case KindPut:
			state[op.Path] = FileState{Blob: op.Blob, Size: op.Size, Mode: op.Mode}
		case KindDelete:
			delete(state, op.Path)
		}
	}
	return state
}

// Parse decodes a JSONL journal.
func Parse(data []byte) ([]Op, error) {
	var ops []Op
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var op Op
		if err := json.Unmarshal(line, &op); err != nil {
			return nil, fmt.Errorf("parse journal line %d: %w", len(ops)+1, err)
		}
		ops = append(ops, op)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return ops, nil
}

// ReadFile reads a journal file; a missing file is an empty journal.
func ReadFile(path string) ([]Op, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return Parse(data)
}

// Append appends ops to a journal file as JSONL.
func Append(path string, ops []Op) error {
	if len(ops) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for _, op := range ops {
		b, err := json.Marshal(op)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(buf.Bytes())
	return err
}
