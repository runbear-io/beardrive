package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// The session note is transient per-volume context — "why edits are being
// made right now" (e.g. a Claude Code session id set by the plugin's sync
// hook). Whichever scanner commits an op while the note is live (the daemon
// or a one-shot `bdrive sync`) stamps it into Op.Note, so provenance doesn't
// depend on winning the race against the daemon's scan interval. The TTL
// keeps a stale note from mislabeling unrelated edits made hours later.

// Note is the on-disk shape of the session note.
type Note struct {
	Text    string    `json:"text"`
	Expires time.Time `json:"expires,omitzero"` // zero = never expires
}

func (s *Store) notePath() string { return filepath.Join(s.dir, "note.json") }

func (s *Store) noteMountPath(mountID string) (string, error) {
	if mountID == "" {
		return s.notePath(), nil
	}
	return s.mountStatePath("note-", mountID)
}

// SaveNote sets the session note. ttl > 0 bounds its life; empty text clears.
func (s *Store) SaveNote(text string, ttl time.Duration) error {
	if text == "" {
		return s.ClearNote()
	}
	n := Note{Text: text}
	if ttl > 0 {
		n.Expires = time.Now().Add(ttl)
	}
	return WriteJSONAtomic(s.notePath(), n)
}

// ClearNote removes the session note.
func (s *Store) ClearNote() error {
	err := os.Remove(s.notePath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// LoadNote returns the live session note, or "" if none is set or it has
// expired. Never errors: a missing or unreadable note is just no note.
func (s *Store) LoadNote() string {
	var n Note
	if err := readJSON(s.notePath(), &n); err != nil {
		return ""
	}
	if !n.Expires.IsZero() && time.Now().After(n.Expires) {
		return ""
	}
	return n.Text
}

// SaveNoteForMount sets a mount's session note without labeling sibling edits.
// The primary mount also keeps the legacy note visible to older callers.
func (s *Store) SaveNoteForMount(mountID, text string, ttl time.Duration) error {
	if mountID == "" {
		return s.SaveNote(text, ttl)
	}
	if text == "" {
		return s.ClearNoteForMount(mountID)
	}
	p, err := s.noteMountPath(mountID)
	if err != nil {
		return err
	}
	n := Note{Text: text}
	if ttl > 0 {
		n.Expires = time.Now().Add(ttl)
	}
	if err := WriteJSONAtomic(p, n); err != nil {
		return err
	}
	if mountID == filepath.Base(s.dir) {
		return s.SaveNote(text, ttl)
	}
	return nil
}

// ClearNoteForMount removes a mount's note and its primary-mount legacy copy.
func (s *Store) ClearNoteForMount(mountID string) error {
	if mountID == "" {
		return s.ClearNote()
	}
	p, err := s.noteMountPath(mountID)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	if mountID == filepath.Base(s.dir) {
		return s.ClearNote()
	}
	return nil
}

// LoadNoteForMount returns a live mount note. Only the primary mount may fall
// back to a legacy note, and only when its mount-specific file is absent.
func (s *Store) LoadNoteForMount(mountID string) string {
	if mountID == "" {
		return s.LoadNote()
	}
	p, err := s.noteMountPath(mountID)
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) && mountID == filepath.Base(s.dir) {
		return s.LoadNote()
	}
	if err != nil {
		return ""
	}
	var n Note
	if err := json.Unmarshal(data, &n); err != nil {
		return ""
	}
	if !n.Expires.IsZero() && time.Now().After(n.Expires) {
		return ""
	}
	return n.Text
}
