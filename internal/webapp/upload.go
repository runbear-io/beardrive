package webapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// Uploads run in two modes, chosen by the server per request so the client
// never needs to know what the storage is:
//
//	direct: POST /api/upload/init returns a presigned, expiring URL; the
//	        client PUTs the content straight to the object store (blob first,
//	        keyed by its sha256), then POST /api/upload/commit journals it.
//	        Storage credentials never leave the server.
//	server: the backend can't presign (file://, plain folders); the client
//	        PUTs the content to /api/upload/content and the server stores it.
//
// The blobs-before-journal invariant holds in both modes: commit refuses to
// journal an op whose blob is not already in the store.

// Uploader is implemented by sources that accept writes through the server.
type Uploader interface {
	Upload(ctx context.Context, path string, r io.Reader, size int64) error
}

// DirectUploader is additionally implemented by sources whose storage can
// accept presigned direct uploads.
type DirectUploader interface {
	Uploader
	SignBlobPut(ctx context.Context, blob string, size int64, ttl time.Duration) (*remote.SignedPut, error)
	HasBlob(ctx context.Context, blob string) (bool, error)
	Commit(ctx context.Context, path, blob string, size int64) error
}

// ---- RemoteSource: writes go to the object store + our own journal ----

// SignBlobPut presigns a direct upload of the blob, if the backend can sign.
func (r *RemoteSource) SignBlobPut(ctx context.Context, blob string, size int64, ttl time.Duration) (*remote.SignedPut, error) {
	signer, ok := r.Backend.(remote.PutSigner)
	if !ok {
		return nil, fmt.Errorf("backend cannot presign uploads")
	}
	return signer.SignPut(ctx, "blobs/"+blob, size, ttl)
}

func (r *RemoteSource) HasBlob(ctx context.Context, blob string) (bool, error) {
	return r.Backend.Exists(ctx, "blobs/"+blob)
}

// Upload stores content through the server: spool to disk while hashing,
// push the blob, then journal the op.
func (r *RemoteSource) Upload(ctx context.Context, p string, src io.Reader, _ int64) error {
	tmp, err := os.CreateTemp("", ".bdrive-tmp-upload-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	h := sha256.New()
	size, err := io.Copy(tmp, io.TeeReader(src, h))
	if err != nil {
		return err
	}
	blob := hex.EncodeToString(h.Sum(nil))
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	// Blob before journal, always.
	if err := r.Backend.Put(ctx, "blobs/"+blob, tmp, size); err != nil {
		return fmt.Errorf("push blob: %w", err)
	}
	return r.Commit(ctx, p, blob, size)
}

// Commit appends a put op for path→blob to this server's own journal. It
// refuses if the blob is not in the store yet (a peer must never see an op
// whose content is missing). Only this server writes this journal key, so
// the read-modify-write below has a single writer; upmu serializes it across
// concurrent requests.
func (r *RemoteSource) Commit(ctx context.Context, p, blob string, size int64) error {
	if r.Device.ID == "" {
		return fmt.Errorf("no device identity configured for uploads")
	}
	ok, err := r.HasBlob(ctx, blob)
	if err != nil {
		return fmt.Errorf("check blob: %w", err)
	}
	if !ok {
		return errBlobMissing
	}

	r.upmu.Lock()
	defer r.upmu.Unlock()

	all, err := r.loadOps(ctx)
	if err != nil {
		return err
	}
	var maxLamport, mySeq int64
	for _, op := range all {
		maxLamport = max(maxLamport, op.Lamport)
		if op.Device == r.Device.ID {
			mySeq = max(mySeq, op.Seq)
		}
	}
	op := journal.Op{
		Seq: mySeq + 1, Lamport: maxLamport + 1, Time: time.Now().UTC(),
		Device: r.Device.ID, DeviceName: r.Device.Name, Author: r.Device.Author,
		Kind: journal.KindPut, Path: p, Blob: blob, Size: size, Mode: 0o644,
	}

	// Read-modify-write of our own journal. A transient read error must fail
	// the commit — treating it as "no journal yet" would rewrite the key
	// without our earlier ops.
	key := "journal/" + r.Device.ID + ".jsonl"
	var existing []byte
	if exists, err := r.Backend.Exists(ctx, key); err != nil {
		return fmt.Errorf("check journal: %w", err)
	} else if exists {
		rc, err := r.Backend.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("fetch journal: %w", err)
		}
		existing, err = io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
	}
	line, err := journal.Marshal([]journal.Op{op})
	if err != nil {
		return err
	}
	data := append(existing, line...)
	return r.Backend.Put(ctx, key, strings.NewReader(string(data)), int64(len(data)))
}

var errBlobMissing = fmt.Errorf("content not uploaded yet")

// ---- DirSource: writes land straight in the folder ----

// Upload writes the file atomically under Root. There is no journal here;
// on a mounted folder the daemon scans, journals, and syncs it like any
// local edit.
func (d *DirSource) Upload(_ context.Context, p string, src io.Reader, _ int64) error {
	dst := filepath.Join(d.Root, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".bdrive-tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// ---- HTTP handlers ----

var blobRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// cleanUploadPath validates a client-supplied destination path and returns
// its normalized form.
func cleanUploadPath(p string) (string, error) {
	if p == "" || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return "", fmt.Errorf("invalid path %q", p)
	}
	cl := path.Clean(p)
	if cl != p || cl == "." || cl == ".." || strings.HasPrefix(cl, "../") {
		return "", fmt.Errorf("invalid path %q", p)
	}
	base := path.Base(cl)
	if base == ".bdrive" || strings.HasPrefix(base, ".bdrive-tmp-") {
		return "", fmt.Errorf("reserved name %q", base)
	}
	return cl, nil
}

// gateUpload enforces the server's upload setting and returns the volume's
// writable source, failing the request if uploads are off or the source is
// read-only.
func (s *Server) gateUpload(v *volume, w http.ResponseWriter) Uploader {
	if !s.Upload.Enabled {
		http.Error(w, "uploads are disabled on this server", http.StatusForbidden)
		return nil
	}
	up := v.uploader()
	if up == nil {
		http.Error(w, "this source is read-only", http.StatusForbidden)
	}
	return up
}

type uploadReq struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func (s *Server) decodeUpload(w http.ResponseWriter, r *http.Request, needBlob bool) (uploadReq, bool) {
	var req uploadReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return req, false
	}
	p, err := cleanUploadPath(req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return req, false
	}
	req.Path = p
	if req.Size < 0 {
		http.Error(w, "invalid size", http.StatusBadRequest)
		return req, false
	}
	if needBlob && !blobRe.MatchString(req.SHA256) {
		http.Error(w, "sha256 must be 64 lowercase hex chars", http.StatusBadRequest)
		return req, false
	}
	return req, true
}

// handleUploadInit tells the client how to upload this content: a presigned
// direct URL when the storage supports it, otherwise through the server.
func (s *Server) handleUploadInit(v *volume, w http.ResponseWriter, r *http.Request) {
	up := s.gateUpload(v, w)
	if up == nil {
		return
	}
	req, ok := s.decodeUpload(w, r, true)
	if !ok {
		return
	}
	if direct, isDirect := up.(DirectUploader); isDirect {
		if exists, err := direct.HasBlob(r.Context(), req.SHA256); err == nil && exists {
			// Content already in the store (same file elsewhere, or a retry):
			// skip the upload, go straight to commit.
			writeJSON(w, map[string]any{"mode": "direct", "exists": true})
			return
		}
		signed, err := direct.SignBlobPut(r.Context(), req.SHA256, req.Size, s.Upload.ttl())
		if err == nil {
			writeJSON(w, map[string]any{
				"mode":    "direct",
				"url":     signed.URL,
				"method":  signed.Method,
				"headers": signed.Headers,
				"expires": signed.Expires.UTC(),
			})
			return
		}
		// Backend can't presign right now (e.g. credentials that can't
		// sign): degrade to uploading through the server.
	}
	writeJSON(w, map[string]any{"mode": "server"})
}

// handleUploadContent receives content through the server (server mode).
func (s *Server) handleUploadContent(v *volume, w http.ResponseWriter, r *http.Request) {
	up := s.gateUpload(v, w)
	if up == nil {
		return
	}
	p, err := cleanUploadPath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := up.Upload(r.Context(), p, r.Body, r.ContentLength); err != nil {
		http.Error(w, fmt.Sprintf("store: %v", err), http.StatusBadGateway)
		return
	}
	v.invalidate()
	writeJSON(w, map[string]any{"ok": true, "path": p})
}

// handleUploadCommit journals a direct upload after the blob is in the store.
func (s *Server) handleUploadCommit(v *volume, w http.ResponseWriter, r *http.Request) {
	up := s.gateUpload(v, w)
	if up == nil {
		return
	}
	direct, isDirect := up.(DirectUploader)
	if !isDirect {
		http.Error(w, "this source has no direct-upload commit", http.StatusBadRequest)
		return
	}
	req, ok := s.decodeUpload(w, r, true)
	if !ok {
		return
	}
	if err := direct.Commit(r.Context(), req.Path, req.SHA256, req.Size); err != nil {
		code := http.StatusBadGateway
		if err == errBlobMissing {
			code = http.StatusConflict
		}
		http.Error(w, fmt.Sprintf("commit: %v", err), code)
		return
	}
	v.invalidate()
	writeJSON(w, map[string]any{"ok": true, "path": req.Path})
}
