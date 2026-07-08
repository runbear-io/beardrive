package webapp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/runbear-io/beardrive/internal/remote"
)

// The store API (/api/store/*) lets other devices sync through this server
// instead of talking to the object store themselves: the server is the only
// machine that knows where the storage is or holds credentials. It exposes
// the same key space every backend uses (blobs/<sha256>, journal/<dev>.jsonl)
// so the regular sync machinery works unchanged over it.
//
// Reads are always allowed (this is the same data the viewer serves). Writes
// follow the server's upload setting and go direct-to-storage via presigned
// URLs when the backend can sign, exactly like browser uploads.

var (
	blobKeyRe    = regexp.MustCompile(`^blobs/[0-9a-f]{64}$`)
	journalKeyRe = regexp.MustCompile(`^journal/[A-Za-z0-9._-]+\.jsonl$`)
)

func validStoreKey(key string) bool {
	return blobKeyRe.MatchString(key) || journalKeyRe.MatchString(key)
}

// storeSource returns the volume's RemoteSource; only real beardrive
// remotes have a store to expose.
func storeSource(v *volume, w http.ResponseWriter) *RemoteSource {
	rs, ok := v.source.(*RemoteSource)
	if !ok {
		http.Error(w, "this server does not front a beardrive remote", http.StatusNotFound)
		return nil
	}
	return rs
}

func (s *Server) storeKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.URL.Query().Get("key")
	if !validStoreKey(key) {
		http.Error(w, fmt.Sprintf("invalid store key %q", key), http.StatusBadRequest)
		return "", false
	}
	return key, true
}

func (s *Server) handleStoreList(v *volume, w http.ResponseWriter, r *http.Request) {
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	s.observeDevice(r)
	prefix := r.URL.Query().Get("prefix")
	if prefix != "" && prefix != "journal/" && prefix != "blobs/" &&
		!strings.HasPrefix(prefix, "journal/") && !strings.HasPrefix(prefix, "blobs/") {
		http.Error(w, fmt.Sprintf("invalid prefix %q", prefix), http.StatusBadRequest)
		return
	}
	objs, err := rs.Backend.List(r.Context(), prefix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"objects": objs})
}

func (s *Server) handleStoreGet(v *volume, w http.ResponseWriter, r *http.Request) {
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	s.observeDevice(r)
	key, ok := s.storeKey(w, r)
	if !ok {
		return
	}
	rc, err := rs.Backend.Get(r.Context(), key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	io.Copy(w, rc)
}

func (s *Server) handleStoreExists(v *volume, w http.ResponseWriter, r *http.Request) {
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	s.observeDevice(r)
	key, ok := s.storeKey(w, r)
	if !ok {
		return
	}
	exists, err := rs.Backend.Exists(r.Context(), key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"exists": exists})
}

// handleStoreSign answers how a client should upload a key: a presigned
// direct-to-storage URL when the backend can sign, through the server
// otherwise — same contract as browser uploads.
func (s *Server) handleStoreSign(v *volume, w http.ResponseWriter, r *http.Request) {
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	s.observeDevice(r)
	if !s.Upload.Enabled {
		http.Error(w, "uploads are disabled on this server", http.StatusForbidden)
		return
	}
	var req struct {
		Key  string `json:"key"`
		Size int64  `json:"size"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !validStoreKey(req.Key) || req.Size < 0 {
		http.Error(w, fmt.Sprintf("invalid store key %q", req.Key), http.StatusBadRequest)
		return
	}
	// Only blobs are presigned. They are content-addressed and immutable, so
	// a leaked URL can at worst re-upload identical bytes. Journals are
	// mutable state and always flow through the server.
	if strings.HasPrefix(req.Key, "blobs/") {
		if exists, err := rs.Backend.Exists(r.Context(), req.Key); err == nil && exists {
			writeJSON(w, map[string]any{"mode": "direct", "exists": true})
			return
		}
		if signer, ok := rs.Backend.(remote.PutSigner); ok {
			if signed, err := signer.SignPut(r.Context(), req.Key, req.Size, s.Upload.ttl()); err == nil {
				writeJSON(w, map[string]any{
					"mode": "direct", "url": signed.URL, "method": signed.Method,
					"headers": signed.Headers, "expires": signed.Expires.UTC(),
				})
				return
			}
		}
	}
	writeJSON(w, map[string]any{"mode": "server"})
}

func (s *Server) handleStorePut(v *volume, w http.ResponseWriter, r *http.Request) {
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	s.observeDevice(r)
	if !s.Upload.Enabled {
		http.Error(w, "uploads are disabled on this server", http.StatusForbidden)
		return
	}
	key, ok := s.storeKey(w, r)
	if !ok {
		return
	}
	if err := rs.Backend.Put(r.Context(), key, r.Body, r.ContentLength); err != nil {
		http.Error(w, fmt.Sprintf("store: %v", err), http.StatusBadGateway)
		return
	}
	if strings.HasPrefix(key, "journal/") {
		v.invalidate() // new ops should show in the viewer immediately
	}
	writeJSON(w, map[string]any{"ok": true})
}
