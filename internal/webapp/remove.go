package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/runbear-io/beardrive/internal/journal"
)

// Remove un-creates a file — the other half of restore. Like restore it is a
// NEW op, never an edit to history: a delete op journaled under this server's
// own device, which every device then materializes like any other change. The
// blob stays in the store forever, so the resulting DELETED row restores the
// file straight back.
//
// Removing the offending ops instead is ruled out for the same reasons
// restore.go gives: it would break one-writer-per-journal, strand peers that
// already replayed them, and corrupt the push cursor.

// Remove appends a delete op for p to this server's own journal. A delete
// references no content, so there is no blob to push first.
func (r *RemoteSource) Remove(ctx context.Context, p string, who User, note string) error {
	return r.removeIf(ctx, p, "", who, note)
}

func (r *RemoteSource) removeIf(ctx context.Context, p, base string, who User, note string) error {
	var cb *commitBase
	if base != "" {
		cb = &commitBase{path: p, sha: base}
	}
	return r.removeIfBase(ctx, p, cb, who, note)
}

func (r *RemoteSource) removeIfBase(ctx context.Context, p string, cb *commitBase, who User, note string) error {
	if r.Device.ID == "" {
		return fmt.Errorf("no device identity configured for uploads")
	}
	return r.appendOpsIf(ctx, []journal.Op{{
		Kind: journal.KindDelete, Path: p,
		User: who.Email, UserName: who.Name, Note: note,
	}}, cb)
}

// handleRemove serves POST /api/p/<id>/remove {path, if_match?}.
func (s *Server) handleRemove(v *volume, w http.ResponseWriter, r *http.Request) {
	r, sink := withReceiptSink(r)
	up := s.gateUpload(v, w) // a read-only hub stays read-only
	if up == nil {
		return
	}
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	var req struct {
		Path    string `json:"path"`
		IfMatch string `json:"if_match"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	ifMatch := req.IfMatch
	if ifMatch == "" {
		ifMatch = r.Header.Get("If-Match")
	}
	if ifMatch != "" {
		ifMatch = strings.Trim(strings.TrimPrefix(ifMatch, "W/"), `"`)
		if !blobRe.MatchString(ifMatch) {
			http.Error(w, "if_match must be 64 lowercase hex chars", http.StatusBadRequest)
			return
		}
	}
	p, err := cleanUploadPath(req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.writablePath(w, r, p) {
		return
	}
	// Match the version the caller viewed, refreshing a sibling's cached fold
	// once before treating a mismatch as a concurrent edit.
	snap, err := v.snapshotHaving(r.Context(), p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	fi, ok := snap.files[p]
	if ifMatch != "" && (!ok || fi.Blob != ifMatch) {
		v.invalidate()
		snap, err = v.snapshotHaving(r.Context(), p)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		fi, ok = snap.files[p]
	}
	if !ok {
		http.Error(w, "no such file", http.StatusNotFound)
		return
	}
	if ifMatch != "" && fi.Blob != ifMatch {
		http.Error(w, "file has changed since you viewed it", http.StatusConflict)
		return
	}
	// A delete stores no bytes — but an org whose plan is blocked must still
	// be blocked from writing.
	org := s.orgOf(r.PathValue("project"))
	if err := s.quota().CheckWrite(org, 0); err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	if err := rs.removeIfBase(r.Context(), p, &commitBase{path: p, sha: fi.Blob, link: fi.Link}, s.requestUser(r), "remove "+p); err != nil {
		var stale *staleWriteError
		code := http.StatusBadGateway
		if errors.As(err, &stale) {
			code = http.StatusConflict
		}
		http.Error(w, fmt.Sprintf("remove: %v", err), code)
		return
	}
	s.quota().RecordUsage(org, 0)
	v.invalidate()
	s.dataWrote(r.Context(), projectID(r))
	s.captureChange(r, "browser", 0, 1)
	s.publishChange(r, "browser", []string{p}, 0, 1)
	resp := map[string]any{"ok": true, "path": p}
	stampReceipt(w, resp, sink.String())
	writeJSON(w, resp)
}
