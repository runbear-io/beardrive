package webapp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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

// ownJournal binds a journal key to the calling device. "Each device writes
// only its own journal" is why no journal object ever has two writers and why
// the hub needs no locking service — write permission on the project is not
// permission to rewrite a peer's log (or the hub's own), where a forged op
// with a high lamport wins replay on every device and History blames the
// victim. Blob keys are content-addressed and immutable, so they carry no
// owner.
//
// A caller that names no device writes no journal either, but that is a 400:
// the request never said who is writing, which is a malformed sync request
// rather than a refused one. Only a caller claiming to be a device it is not
// gets the 403.
func (s *Server) ownJournal(w http.ResponseWriter, r *http.Request, key string) bool {
	if !strings.HasPrefix(key, "journal/") {
		return true
	}
	dev := r.Header.Get("X-Bdrive-Device")
	if dev == "" {
		http.Error(w, "a journal write must identify its device (X-Bdrive-Device)", http.StatusBadRequest)
		return false
	}
	if key != "journal/"+dev+".jsonl" {
		http.Error(w, "a device may only write its own journal", http.StatusForbidden)
		return false
	}
	return true
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
	// Signing grants nothing for a journal (journals are never presigned; the
	// answer is always "come through the server"), so an unidentified caller
	// is fine here — the write itself is where ownership is enforced. A
	// caller that does name a device is held to it, so a forged sync fails at
	// the first step instead of after uploading.
	if r.Header.Get("X-Bdrive-Device") != "" && !s.ownJournal(w, r, req.Key) {
		return
	}
	if err := s.quota().CheckWrite(s.orgOf(r.PathValue("project")), req.Size); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
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
	if !s.ownJournal(w, r, key) {
		return
	}
	// Spool the body before storing any of it. Both things this handler has to
	// be sure of are properties of the bytes, not of the headers the client
	// sent: what a blob key promises (its sha256) and what the write costs
	// (Content-Length is -1 on any chunked request, which made every unsized
	// put free). Cost: one temp file per put on the hub's busiest write path.
	tmp, size, sum, err := spool(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("store: %v", err), http.StatusBadGateway)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if blob, isBlob := strings.CutPrefix(key, "blobs/"); isBlob && blob != sum {
		http.Error(w, "content does not hash to its key", http.StatusBadRequest)
		return
	}
	org := s.orgOf(r.PathValue("project"))
	if err := s.quota().CheckWrite(org, size); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if err := rs.Backend.Put(r.Context(), key, tmp, size); err != nil {
		http.Error(w, fmt.Sprintf("store: %v", err), http.StatusBadGateway)
		return
	}
	s.quota().RecordUsage(org, size)
	if strings.HasPrefix(key, "journal/") {
		v.invalidate() // new ops should show in the viewer immediately
	}
	writeJSON(w, map[string]any{"ok": true})
}
