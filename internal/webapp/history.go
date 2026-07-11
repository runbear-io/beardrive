package webapp

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/runbear-io/beardrive/internal/journal"
)

// History is read straight from the journals: every put/delete ever made,
// newest first, with the account that made it and what the server knows
// about the device it came from. Content is content-addressed and retained
// forever, so each entry links to its exact version — the groundwork for
// the revert/rollback phase, where restoring is just writing an old blob
// back as a new op.

// HistoryEntry is one change as the history API reports it.
type HistoryEntry struct {
	Time     string     `json:"time"`
	Kind     string     `json:"kind"` // add | edit | delete
	Path     string     `json:"path"`
	Size     int64      `json:"size,omitempty"`
	Blob     string     `json:"blob,omitempty"` // sha256; fetch via the blob endpoint
	User     string     `json:"user,omitempty"`
	UserName string     `json:"user_name,omitempty"`
	Author   string     `json:"author,omitempty"` // offline/git fallback identity
	Device   DeviceInfo `json:"device"`
	Note     string     `json:"note,omitempty"`
}

// handleHistory serves ?path=<file> (one file's versions) or
// ?prefix=<folder/> (everything underneath, "" = the whole project),
// newest first, at most ?n= entries (default 100).
func (s *Server) handleHistory(v *volume, w http.ResponseWriter, r *http.Request) {
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	q := r.URL.Query()
	path, prefix := q.Get("path"), q.Get("prefix")
	if path != "" && q.Has("prefix") {
		http.Error(w, "use ?path= or ?prefix=, not both", http.StatusBadRequest)
		return
	}
	n := 100
	if raw := q.Get("n"); raw != "" {
		var err error
		if n, err = strconv.Atoi(raw); err != nil || n < 1 {
			http.Error(w, "invalid n", http.StatusBadRequest)
			return
		}
	}
	all, err := rs.loadOps(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	journal.Sort(all)
	// A put is an "add" when the path didn't exist just before it (first
	// version, or first after a delete), an "edit" otherwise. Existence is
	// replayed over ALL ops in journal order, before any path/prefix filter,
	// so a filtered view classifies the same as the full feed.
	kinds := make([]string, len(all))
	exists := make(map[string]bool, len(all))
	for i, op := range all {
		switch {
		case op.Kind == journal.KindDelete:
			kinds[i] = "delete"
			exists[op.Path] = false
		case exists[op.Path]:
			kinds[i] = "edit"
		default:
			kinds[i] = "add"
			exists[op.Path] = true
		}
	}
	entries := make([]HistoryEntry, 0, n)
	for i := len(all) - 1; i >= 0 && len(entries) < n; i-- { // newest first
		op := all[i]
		switch {
		case path != "" && op.Path != path:
			continue
		case path == "" && prefix != "" && !strings.HasPrefix(op.Path, strings.TrimSuffix(prefix, "/")+"/"):
			continue
		}
		dev, _ := s.Devices.Get(op.Device)
		if dev.ID == "" {
			dev = DeviceInfo{ID: op.Device, Name: op.DeviceName}
		}
		entries = append(entries, HistoryEntry{
			Time: op.Time.UTC().Format("2006-01-02T15:04:05Z"), Kind: kinds[i],
			Path: op.Path, Size: op.Size, Blob: op.Blob,
			User: op.User, UserName: op.UserName, Author: op.Author,
			Device: dev, Note: op.Note,
		})
	}
	writeJSON(w, map[string]any{"entries": entries})
}

// handleBlob streams one exact version by content hash — view or download
// any point in a file's history.
func (s *Server) handleBlob(v *volume, w http.ResponseWriter, r *http.Request) {
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	sha := r.URL.Query().Get("sha")
	if !blobRe.MatchString(sha) {
		http.Error(w, "invalid sha", http.StatusBadRequest)
		return
	}
	rc, err := rs.Backend.Get(r.Context(), "blobs/"+sha)
	if err != nil {
		http.Error(w, "no such version", http.StatusNotFound)
		return
	}
	defer rc.Close()
	name := r.URL.Query().Get("name")
	if name != "" {
		w.Header().Set("Content-Type", contentType(name))
		if r.URL.Query().Get("download") == "1" {
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", sanitizeFilename(name)))
		}
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	io.Copy(w, rc)
}

func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "/", "-")
	return strings.Map(func(r rune) rune {
		if r < 32 || r == '"' {
			return '-'
		}
		return r
	}, name)
}
