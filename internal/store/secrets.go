package store

import (
	"bytes"
	"encoding/json"
	"os"

	"github.com/runbear-io/beardrive/internal/secrets"
)

// The secrets record is what a cycle's credential findings survive in. The scan
// flags a file the moment it changes; the two surfaces that report it —
// `bdrive status` and the agent hook — run minutes later, long after that file
// stopped changing and the cheap size+mtime path stopped reading it. So the
// record has to outlive the cycle that made it, for the same reason the inbound
// spool does (see inbound.go).
//
// It is merged PER PATH, never replaced wholesale: nearly every cycle scans
// zero changed files, so a whole-set rewrite would erase the warning three
// seconds after it appeared. A finding stands until the file changes again
// without it — which is what makes fixing the file the only thing needed to
// clear the warning, no command and no flag.
//
// It is advisory telemetry about paths this device already journaled and
// pushed. It must never be able to fail a cycle.

// secretsPath names the per-mount findings file, validated exactly like
// cachePath: the mount id comes from a folder's .bdrive/config.json, so a
// separator in it would put this file wherever its author chose.
func (s *Store) secretsPath(mountID string) (string, error) {
	return s.mountStatePath("secrets-", mountID)
}

// SecretRecord is the findings file: what the rule set named by Rules found
// in each path, as of the blob that path held when it was read.
type SecretRecord struct {
	Rules string                 `json:"rules"`
	Paths map[string]SecretEntry `json:"paths"`
}

// SecretEntry is one path's findings and the content they describe. A path
// whose file now holds another blob, or a record written under other rules,
// is read again rather than trusted.
type SecretEntry struct {
	Blob  string            `json:"blob,omitempty"`
	Found []secrets.Finding `json:"found"`
}

// LoadSecretRecord reads the findings file. A file in the bare per-path shape
// ({"path": [findings]}) loads with no rules and no blobs, so every path in it
// is read again.
func (s *Store) LoadSecretRecord(mountID string) (SecretRecord, error) {
	rec := SecretRecord{Paths: map[string]SecretEntry{}}
	p, err := s.secretsPath(mountID)
	if err != nil {
		return rec, err
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return rec, nil
	}
	if err != nil {
		return rec, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return rec, err
	}
	// Bare-shape values are arrays, so an object under "paths" is the record.
	if raw := bytes.TrimSpace(top["paths"]); len(raw) > 0 && raw[0] == '{' {
		if err := json.Unmarshal(data, &rec); err != nil {
			return SecretRecord{Paths: map[string]SecretEntry{}}, err
		}
		if rec.Paths == nil {
			rec.Paths = map[string]SecretEntry{}
		}
	} else {
		for rel, raw := range top {
			var f []secrets.Finding
			if err := json.Unmarshal(raw, &f); err != nil {
				return SecretRecord{Paths: map[string]SecretEntry{}}, err
			}
			rec.Paths[rel] = SecretEntry{Found: f}
		}
	}
	// Same treatment as the cache's keys: plain JSON in $BDRIVE_HOME that
	// anything running as the user can write, rendered into a terminal and
	// joined against cache keys. An out-of-volume path is dropped, not shown.
	for rel, e := range rec.Paths {
		if !cleanRel(rel) || len(e.Found) == 0 {
			delete(rec.Paths, rel)
		}
	}
	return rec, nil
}

func (s *Store) SaveSecretRecord(mountID string, rec SecretRecord) error {
	p, err := s.secretsPath(mountID)
	if err != nil {
		return err
	}
	return WriteJSONAtomic(p, rec)
}

// LoadSecrets returns the findings recorded for each mount-relative path.
func (s *Store) LoadSecrets(mountID string) (map[string][]secrets.Finding, error) {
	rec, err := s.LoadSecretRecord(mountID)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]secrets.Finding, len(rec.Paths))
	for rel, e := range rec.Paths {
		out[rel] = e.Found
	}
	return out, nil
}

// SaveSecrets records findings with no blob or rules, so a cycle reads each
// path again before trusting them.
func (s *Store) SaveSecrets(mountID string, f map[string][]secrets.Finding) error {
	rec := SecretRecord{Paths: make(map[string]SecretEntry, len(f))}
	for rel, found := range f {
		rec.Paths[rel] = SecretEntry{Found: found}
	}
	return s.SaveSecretRecord(mountID, rec)
}
