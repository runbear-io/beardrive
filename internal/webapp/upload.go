package webapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/textmerge"
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
// who is the signed-in account the write should be attributed to (zero when
// auth is off), and note rides along on the journaled op — "" for an ordinary
// browser upload, a stated origin when the HUB itself authored the bytes
// (seedTemplate). Both are on the interface because the journal is the hub's
// only audit surface: a write with no human behind it must not be able to
// present as one by omission.
type Uploader interface {
	Upload(ctx context.Context, path string, r io.Reader, size int64, who User, note string) error
}

// DirectUploader is additionally implemented by sources whose storage can
// accept presigned direct uploads.
type DirectUploader interface {
	Uploader
	SignBlobPut(ctx context.Context, blob string, size int64, ttl time.Duration) (*remote.SignedPut, error)
	// BlobSize reports the stored size of a blob and whether it is there at
	// all. Size comes from storage, never from the caller: in direct mode the
	// server never sees the bytes, so this is the only true byte count it can
	// quota-check and journal.
	BlobSize(ctx context.Context, blob string) (int64, bool, error)
	// note rides along on the journaled op — "" for an ordinary upload,
	// "restore <path>@<sha8>" when the write is a restore.
	Commit(ctx context.Context, path, blob string, size int64, who User, note string) error
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

func (r *RemoteSource) BlobSize(ctx context.Context, blob string) (int64, bool, error) {
	o, ok, err := r.blobStat(ctx, blob)
	return o.Size, ok, err
}

// blobStat is the stored object behind a blob: its size, its last-modified
// time (zero on a backend that does not report one), and whether it is there.
// One metadata call, no egress.
func (r *RemoteSource) blobStat(ctx context.Context, blob string) (remote.Object, bool, error) {
	key := "blobs/" + blob
	objs, err := r.Backend.List(ctx, key) // full key as prefix: at most one hit
	if err != nil {
		return remote.Object{}, false, err
	}
	for _, o := range objs {
		if o.Key == key {
			return o, true, nil
		}
	}
	// Chunked files have no blobs/<sha> object; their manifest carries the
	// logical file size while retaining the blob-shaped address used by ops.
	mkey := "manifests/" + blob
	mobjs, err := r.Backend.List(ctx, mkey)
	if err != nil {
		return remote.Object{}, false, err
	}
	for _, o := range mobjs {
		if o.Key != mkey {
			continue
		}
		rc, err := r.Backend.Get(ctx, mkey)
		if err != nil {
			return remote.Object{}, false, err
		}
		var m struct {
			Size int64 `json:"size"`
		}
		decErr := json.NewDecoder(io.LimitReader(rc, 4<<20)).Decode(&m)
		rc.Close()
		if decErr != nil {
			return remote.Object{}, false, decErr
		}
		return remote.Object{Key: key, Size: m.Size, Modified: o.Modified}, true, nil
	}
	return remote.Object{}, false, nil
}

// presignTTL is how long a presigned upload URL to this source stays valid —
// the window in which a blob is writable by someone other than the hub.
func (r *RemoteSource) presignTTL() time.Duration {
	if r.PresignTTL > 0 {
		return r.PresignTTL
	}
	return DefaultUploadTTL
}

// spool reads src to a temp file, rewound and ready to re-read, and reports
// its real size and sha256. The server needs both before it stores anything:
// a client's declared size and a content address are claims, not facts. The
// caller closes and removes the file.
func spool(src io.Reader) (f *os.File, size int64, sum string, err error) {
	tmp, err := os.CreateTemp("", ".bdrive-tmp-upload-")
	if err != nil {
		return nil, 0, "", err
	}
	h := sha256.New()
	size, err = io.Copy(tmp, io.TeeReader(src, h))
	if err == nil {
		_, err = tmp.Seek(0, io.SeekStart)
	}
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, 0, "", err
	}
	return tmp, size, hex.EncodeToString(h.Sum(nil)), nil
}

// Upload stores content through the server: spool to disk while hashing,
// push the blob, then journal the op.
func (r *RemoteSource) Upload(ctx context.Context, p string, src io.Reader, _ int64, who User, note string) error {
	return r.uploadIf(ctx, p, src, who, note, "")
}

// uploadIf is Upload held to base, the version of p the writer built on (an
// If-Match sha; "" holds it to nothing), all the way into the journal append.
// See commitBase.
func (r *RemoteSource) uploadIf(ctx context.Context, p string, src io.Reader, who User, note, base string) error {
	tmp, size, blob, err := spool(src)
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	// Blob before journal, always.
	if err := r.Backend.Put(ctx, "blobs/"+blob, tmp, size); err != nil {
		return fmt.Errorf("push blob: %w", err)
	}
	return r.commitIf(ctx, p, blob, size, who, note, base)
}

// Commit appends a put op for path→blob to this server's own journal. It
// refuses if the blob is not in the store yet (a peer must never see an op
// whose content is missing). Only this server writes this journal key, so
// the read-modify-write below has a single writer; upmu serializes it across
// concurrent requests.
func (r *RemoteSource) Commit(ctx context.Context, p, blob string, size int64, who User, note string) error {
	return r.commitIf(ctx, p, blob, size, who, note, "")
}

func (r *RemoteSource) commitIf(ctx context.Context, p, blob string, size int64, who User, note, base string) error {
	var cb *commitBase
	if base != "" {
		cb = &commitBase{path: p, sha: base, blob: blob}
	}
	return r.commitIfBase(ctx, p, blob, size, who, note, cb)
}

func (r *RemoteSource) commitIfBase(ctx context.Context, p, blob string, size int64, who User, note string, cb *commitBase) error {
	if r.Device.ID == "" {
		return fmt.Errorf("no device identity configured for uploads")
	}
	_, ok, err := r.BlobSize(ctx, blob)
	if err != nil {
		return fmt.Errorf("check blob: %w", err)
	}
	if !ok {
		return errBlobMissing
	}
	op := journal.Op{
		Kind: journal.KindPut, Path: p, Blob: blob, Size: size, Mode: 0o644,
		User: who.Email, UserName: who.Name, Note: note,
	}
	// The version this put edited from, when the writer named one: a device
	// merging against this op finds its ancestor without guessing.
	if cb != nil && cb.path == p && cb.sha != "" {
		op.Base = cb.sha
	}
	return r.appendOpsIf(ctx, []journal.Op{op}, cb)
}

// commitBase is the version of one path a write was built on: what its
// If-Match named. A write that holds a base must not replace a version it
// never saw.
//
// It is checked against the journals the append is about to land on, inside
// the append's compare-and-swap loop, and again on every retry. A check made
// anywhere earlier — the upload handler's head lookup, an MCP tool's — reads a
// snapshot that another hub process's write can postdate: both pass, both
// append, and the later lamport silently replaces the other's content.
type commitBase struct {
	path         string
	sha          string // the blob version the writer read (empty for symlink)
	link         string // the symlink target the writer read (empty for regular file)
	mustExist    bool   // when true, a missing/deleted path (!ok) is a staleWriteError
	mustBeAbsent bool   // when true, the path was absent when read and must remain absent (or match target)
	blob         string // what it writes: finding that already there is no conflict
	writeLink    string // symlink target it writes: finding that already there is no conflict
	extra        []commitBase
}

// staleWriteError is a write whose base no longer holds. head is the version
// that replaced it, so the caller can say what it missed.
type staleWriteError struct{ head string }

func (e *staleWriteError) Error() string { return "this file changed since you last read it" }

// holds reports whether the path still resolves to the base in the fold of
// all, through the same replay the tree uses.
func (b *commitBase) holds(all []journal.Op) error {
	bases := append([]commitBase{*b}, b.extra...)
	for _, base := range bases {
		var ops []journal.Op
		for _, op := range all {
			if op.Path == base.path {
				ops = append(ops, op)
			}
		}
		journal.Sort(ops)
		fi, ok := foldFiles(ops)[base.path]
		matchesTarget := (base.blob != "" || base.writeLink != "") && fi.Blob == base.blob && fi.Link == base.writeLink
		if !ok {
			if base.mustExist {
				return &staleWriteError{head: ""}
			}
			continue
		}
		if base.mustBeAbsent {
			if matchesTarget {
				continue
			}
			head := fi.Blob
			if head == "" {
				head = fi.Link
			}
			return &staleWriteError{head: head}
		}
		matchesBase := fi.Blob == base.sha && fi.Link == base.link
		if matchesBase || matchesTarget {
			continue
		}
		head := fi.Blob
		if head == "" {
			head = fi.Link
		}
		return &staleWriteError{head: head}
	}
	return nil
}

// nextLamport is maxLamport+1 without the wrap.
func nextLamport(cur int64) int64 {
	if cur == math.MaxInt64 {
		return cur
	}
	return cur + 1
}

// appendOp stamps op with this server's identity and ordering and appends it
// to this server's own journal. Callers fill in Kind/Path and the content
// fields; Seq, Lamport, Time and the device fields belong to us.
func (r *RemoteSource) appendOp(ctx context.Context, op journal.Op) error {
	return r.appendOps(ctx, []journal.Op{op})
}

// appendOps is the same write for N ops at once, in ONE read-modify-write.
// Every journal write in this package goes through it — appendOp is the
// single-op call — so there is exactly one place that stamps the hub's
// identity and exactly one that rewrites the key.
//
// The batch is not an optimization detail: it is what makes a multi-path
// write atomic. One Put of one object either lands or it does not, so a
// run-wide undo (undorun.go) can never leave half a run reverted. A caller
// that loops appendOp instead gets N whole-journal round trips AND a
// partially-applied write with nothing to report it.
func (r *RemoteSource) appendOps(ctx context.Context, ops []journal.Op) error {
	return r.appendOpsIf(ctx, ops, nil)
}

// appendOpsIf is appendOps held to a base (nil holds to nothing).
//
// Every attempt reads the journals again, checks the base against them and
// stamps the ops from them. A retry used to re-append the first attempt's
// bytes: their seq could be one another process had just taken, and nothing
// asked whether the write that beat it had replaced the version this one was
// built on.
func (r *RemoteSource) appendOpsIf(ctx context.Context, ops []journal.Op, base *commitBase) error {
	// Never rewrite an unchanged journal: a Put of identical bytes still
	// bumps Modified, which invalidates every reader's journal cache.
	if len(ops) == 0 {
		return nil
	}
	r.upmu.Lock()
	defer r.upmu.Unlock()

	next := func(existing []byte) ([]byte, error) {
		keep, err := r.listJournals(ctx)
		if err != nil {
			return nil, err
		}
		keepFP := journalFingerprint(keep)
		sourced, err := r.sourcedOpsOf(ctx, keep)
		if err != nil {
			return nil, err
		}
		// A second hub process may have appended after jcache was populated.
		// Include the version we are about to CAS so sequence and lamport
		// allocation never collides with that unseen tail.
		if len(existing) > 0 {
			existingOps, err := journal.Parse(existing)
			if err != nil {
				return nil, fmt.Errorf("parse current journal: %w", err)
			}
			for _, op := range existingOps {
				sourced = append(sourced, sourcedOp{From: r.Device.ID, Op: op})
			}
		}
		if base != nil {
			all := make([]journal.Op, len(sourced))
			for i, so := range sourced {
				all[i] = so.Op
			}
			if err := base.holds(all); err != nil {
				return nil, err
			}
		}
		if err := r.stamp(sourced, ops); err != nil {
			return nil, err
		}
		if base != nil {
			keepAfter, err := r.listJournals(ctx)
			if err != nil {
				return nil, err
			}
			if journalFingerprint(keepAfter) != keepFP {
				sourcedAfter, err := r.sourcedOpsOf(ctx, keepAfter)
				if err != nil {
					return nil, err
				}
				allAfter := make([]journal.Op, len(sourcedAfter))
				for i, so := range sourcedAfter {
					allAfter[i] = so.Op
				}
				if err := base.holds(allAfter); err != nil {
					return nil, err
				}
				return nil, remote.ErrVersionMismatch
			}
		}
		return journal.Marshal(ops)
	}
	key := "journal/" + r.Device.ID + ".jsonl"
	if cp, ok := r.Backend.(remote.ConditionalPutter); ok {
		return r.appendConditionalExisting(ctx, cp, key, next)
	}
	line, err := next(nil)
	if err != nil {
		return err
	}
	return r.appendLastWriterWins(ctx, key, line)
}

// stamp gives ops this server's identity and the next places in its journal
// and in replay order, as the journals stand. The seq counts only ops read
// from this server's own journal: an op's Device field is JSON its pusher
// chose, and a member's op naming the hub's device with seq MaxInt64 would
// otherwise wrap every later hub op onto one (device, seq) slot.
//
// A seq with no room after it is refused rather than wrapped: no request can
// write the hub's journal (ownJournal), so one there came from outside the
// hub, and wrapping would reuse a (device, seq) on every later op.
func (r *RemoteSource) stamp(sourced []sourcedOp, ops []journal.Op) error {
	var maxLamport, mySeq int64
	for _, so := range sourced {
		maxLamport = max(maxLamport, so.Op.Lamport)
		if so.From == r.Device.ID {
			mySeq = max(mySeq, so.Op.Seq)
		}
	}
	if mySeq > math.MaxInt64-int64(len(ops)) {
		return fmt.Errorf("this hub's journal holds seq %d, with no room after it; it was written from outside the hub", mySeq)
	}
	now := time.Now().UTC()
	lam := maxLamport
	for i := range ops {
		// Saturating, like the client's tickLamport. maxLamport is taken over
		// every journal the hub can see, members' included, and int64 addition
		// wraps: one pushed op carrying MaxInt64 made the hub's next lamport
		// MinInt64 — recomputed on every commit, so every later browser upload
		// in the project silently lost last-writer-wins while commit still
		// answered 200. At saturation the batch's lamports stop increasing and
		// journal.Less orders the rest through (time, device, seq), which is
		// still a total order — so replay stays deterministic.
		lam = nextLamport(lam)
		ops[i].Seq, ops[i].Lamport, ops[i].Time = mySeq+int64(i)+1, lam, now
		ops[i].Device, ops[i].DeviceName, ops[i].Author = r.Device.ID, r.Device.Name, r.Device.Author
	}
	return nil
}

var errBlobMissing = fmt.Errorf("content not uploaded yet")

// emptyBlob is sha256(""), the only content whose size is legitimately zero.
const emptyBlob = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// sizeFitsContentAddress is the one lie about a declared upload size that is
// provable before the bytes exist: zero, for content that is not the empty
// blob. In direct mode the bytes never pass through the hub, so the size is
// the caller's word — and it is the number the quota is charged against, which
// makes this exactly the lie that buys an unmetered presigned URL. Shared,
// because both doors take the number: /upload/init (browsers) and /store/sign
// (devices), and for a while only one of them checked.
func sizeFitsContentAddress(sha string, size int64) bool {
	return size > 0 || sha == emptyBlob
}

// ---- DirSource: writes land straight in the folder ----

// Upload writes the file atomically under Root. There is no journal here;
// on a mounted folder the daemon scans, journals, and syncs it like any
// local edit.
func (d *DirSource) Upload(_ context.Context, p string, src io.Reader, _ int64, _ User, _ string) error {
	dst := filepath.Join(d.Root, filepath.FromSlash(p))
	// cleanUploadPath rules out "..", but a path with no ".." in it still
	// leaves the folder by walking through a symlinked directory that was
	// already there. The viewer promises "this folder": resolve where the
	// write would actually land and refuse anything outside Root.
	//
	// Judged BEFORE anything is created. underRoot needs a directory that
	// exists (EvalSymlinks), so asking after MkdirAll meant a refused upload
	// had already built the whole parent chain on the other side of the
	// symlink. The deepest existing ancestor answers the same question: every
	// directory MkdirAll then creates is a real directory beneath it.
	if err := underRoot(d.Root, existingDir(filepath.Dir(dst))); err != nil {
		return err
	}
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

// existingDir walks up from dir to the deepest ancestor that exists on disk.
func existingDir(dir string) string {
	for {
		if _, err := os.Lstat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

// underRoot reports whether dir, with every symlink resolved, is root or
// inside it.
func underRoot(root, dir string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	if realDir != realRoot && !strings.HasPrefix(realDir, realRoot+string(filepath.Separator)) {
		return fmt.Errorf("path leaves the served folder")
	}
	return nil
}

// ---- HTTP handlers ----

var blobRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// isBlobSHA is blobRe without the regexp engine, for the replay, which asks
// once per op of every journal in the project.
func isBlobSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// cleanUploadPath validates a client-supplied destination path and returns
// its normalized form.
func cleanUploadPath(p string) (string, error) {
	// journal.SafePath is the rule — the same one the /store/* journal door
	// and the device's unsafeRel apply. This door used to carry its own copy
	// of it; the copies disagreed, which is how a control character that this
	// door answered 400 to got journaled through the other one.
	if !journal.SafePath(p) {
		return "", fmt.Errorf("invalid path %q", p)
	}
	cl := p
	// The same set the scan walk would never have uploaded, at any depth:
	// .bdrive/ is the mount's own identity (an upload of it repoints every
	// device that pulls) and .git/ materializes hook scripts that run on a
	// teammate's next commit. materialize re-checks this too — an op can also
	// arrive from a peer's journal — but a hub that journals one has already
	// handed it to every device.
	if config.ReservedPath(cl) {
		return "", fmt.Errorf("reserved name %q", cl)
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
	if !s.writablePath(w, r, req.Path) {
		return
	}
	if !sizeFitsContentAddress(req.SHA256, req.Size) {
		http.Error(w, "declared size does not match the content address", http.StatusForbidden)
		return
	}
	project := r.PathValue("project")
	if rs, ok := v.source.(*RemoteSource); ok {
		s.reconcileGrants(r.Context(), project, rs.Backend)
	}
	org := s.orgOf(project)
	if err := s.quota().CheckWrite(org, req.Size+s.reservedBytes(org)); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if direct, isDirect := up.(DirectUploader); isDirect {
		if _, exists, err := direct.BlobSize(r.Context(), req.SHA256); err == nil && exists {
			// Content already in the store (same file elsewhere, or a retry):
			// skip the upload, go straight to commit.
			writeJSON(w, map[string]any{"mode": "direct", "exists": true})
			return
		}
		// Reserved, exactly like the device door: counted against the cap now,
		// charged when the object is confirmed in storage, released for free
		// if the caller never uploads. A browser upload that never comes back
		// to commit is therefore still billed. Check and reservation are one
		// critical section (see reserveIfFits).
		if err := s.reserveIfFits(project, org, "blobs/"+req.SHA256, req.Size, s.Upload.ttl()); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
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
		s.claimGrant(project, "blobs/"+req.SHA256) // nothing was granted
	}
	writeJSON(w, map[string]any{"mode": "server"})
}

// lockPath serializes writes to one path within this process. Returns the
// unlock, so callers read `defer s.lockPath(...)()`.
func (s *Server) lockPath(project, p string) func() {
	v, _ := s.pathLocks.LoadOrStore(project+"\x00"+p, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// headBlob is the sha a path currently resolves to, and whether it resolves at
// all. Read from the cached snapshot rather than the journal, through current:
// a write through the hub invalidates the snapshot and a device push is folded
// in before current answers, so the answer is as fresh as the last change, and
// a stale one can only cost a spurious 409 the client recovers from — never a
// missed conflict that costs somebody their text.
func (s *Server) headBlob(ctx context.Context, v *volume, p string) (string, bool) {
	fi, ok := s.headFile(ctx, v, p)
	return fi.Blob, ok
}

func (s *Server) headFile(ctx context.Context, v *volume, p string) (FileInfo, bool) {
	snap, err := v.current(ctx)
	if err != nil {
		return FileInfo{}, false
	}
	fi, ok := snap.files[p]
	return fi, ok
}

// An editor save that empties a file, or keeps less than a fifth of it, is
// refused unless the client confirms it.
//
// An editor that attaches before its document has content holds an empty
// buffer that looks clean, and its next save writes 0 bytes over the file. No
// conflict check catches that: the base is right, only the text is missing.
// Applied to conditional (If-Match) writes only — editors and the hub's own
// room snapshots — so an upload that replaces a file wholesale is untouched.
// Small files are exempt from the ratio, not from emptying.
const (
	confirmShrinkHeader = "X-Bdrive-Confirm-Shrink"
	// noMergeHeader asks a conditional write to be refused on a stale base
	// rather than merged: a co-editing room settles its own (ycollab.go).
	noMergeHeader = "X-Bdrive-No-Merge"
	// parkOnConflictHeader asks a conditional write that can be neither
	// taken nor merged to be kept beside the file as a conflict copy, in
	// this request: a closing tab's keepalive request is the last code it
	// runs, so it cannot answer a 409 by parking its text itself.
	parkOnConflictHeader = "X-Bdrive-Park-On-Conflict"
	// maxUploadMergeBytes bounds what a stale conditional write reads into
	// memory to merge; the same bound a device merges under.
	maxUploadMergeBytes = 4 << 20
	shrinkRatioFloor    = 256
)

func sharpShrink(was, now int64) bool {
	if was <= 0 || now >= was {
		return false
	}
	if now == 0 {
		return true
	}
	return was >= shrinkRatioFloor && now*5 < was
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
	if !s.writablePath(w, r, p) {
		return
	}
	// Check-then-write, serialized per path: without this both halves of a
	// simultaneous save pass the check before either lands, and the second
	// still erases the first — which is exactly the case this exists for, two
	// people typing at once. Process-local, which is the same assumption the
	// journal's read-modify-write already makes about this hub being the
	// single writer of its own key.
	//
	// ponytail: a sync.Map of mutexes, never pruned — one entry per path ever
	// written through this route, a few dozen bytes each. Swap for a sharded
	// lock table if a hub ever writes enough distinct paths for that to show
	// up in a heap profile.
	defer s.lockPath(projectID(r), p)()

	tmp, size, blob, err := spool(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("store: %v", err), http.StatusBadGateway)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	project := r.PathValue("project")

	org := s.orgOf(project)
	if rs, ok := v.source.(*RemoteSource); ok {
		s.reconcileGrants(r.Context(), project, rs.Backend)
	}
	base := strings.Trim(r.Header.Get("If-Match"), `"`)
	// If-None-Match: * creates p or refuses: a conflict copy named for a
	// second that another one already took must never replace it.
	createOnly := base == "" && r.Header.Get("If-None-Match") == "*"
	if createOnly {
		if head, ok := s.headBlob(r.Context(), v, p); ok && head != blob {
			writeJSONStatus(w, http.StatusConflict, map[string]any{
				"error": "this file already exists", "sha": head, "path": p,
			})
			return
		}
	}
	if base != "" {
		if head, ok := s.headBlob(r.Context(), v, p); ok && head != base && head != blob {
			if handled, now := s.mergeStaleUpload(w, r, v, p, base, head, tmp, size); handled {
				return
			} else if now != "" {
				head = now
			}
			if s.parkStaleUpload(w, r, v, up, p, head, tmp, size, blob, -1) {
				return
			}
			writeJSONStatus(w, http.StatusConflict, map[string]any{
				"error": "this file changed since you last read it",
				"sha":   head,
				"path":  p,
			})
			return
		}
	}

	// billed is what this write actually adds: zero when the store already
	// holds this exact blob, which is every co-editor after the first saving
	// the same converged text. See billableBytes.
	billed := size
	if rs, ok := v.source.(*RemoteSource); ok {
		var billErr error
		billed, billErr = billableBytes(r.Context(), rs.Backend, "blobs/"+blob, size)
		if billErr != nil {
			storageErr(w, http.StatusBadGateway, "could not check the stored object", billErr)
			return
		}
	}
	if err := s.quota().CheckWrite(org, billed+s.reservedBytes(org)); err != nil {
		if base != "" && billed > 0 {
			v.invalidate()
			if head, ok := s.headBlob(r.Context(), v, p); ok && head != base && head != blob {
				if handled, now := s.mergeStaleUpload(w, r, v, p, base, head, tmp, size); handled {
					return
				} else if now != "" {
					head = now
				}
				writeJSONStatus(w, http.StatusConflict, map[string]any{
					"error": "this file changed since you last read it",
					"sha":   head,
					"path":  p,
				})
				return
			}
		}
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	/* Identical content is not a change, and journaling it as one is not free.

	   Every co-editor after the first saves the same converged text — "whoever
	   stops typing last writes the file" means all of them write it — and a
	   client whose PUT is queued behind a slow response re-sends the same
	   bytes on its next idle timer. Each of those used to append an op: a new
	   version in History, a change frame, and a full tree + heat refetch on
	   every other client in the project. One user's two-minute editing session
	   produced fourteen versions this way.

	   The storage layer already knows this write adds nothing (billableBytes
	   charges zero for a blob the store holds), and restore.go already refuses
	   the same thing with a 409 ("already the current content"). The journal
	   was the last place that had not been told.

	   Reported as ok, not as an error: nothing the caller wanted is missing,
	   the file already says what they asked it to say.

	   AFTER the quota gate, not before it. A write that changes nothing still
	   asks — billableBytes has already made it a request for zero bytes, and
	   an org at its ceiling must be able to re-save text that adds no bytes
	   (TestQuotaCoEditingChargesOneBlobOnce pins exactly that). Skipping the
	   gate would quietly remove a door a managed provider is entitled to
	   answer; skipping the JOURNAL is the whole point and costs it nothing. */
	if head, ok := s.headBlob(r.Context(), v, p); ok && head == blob {
		writeJSON(w, map[string]any{
			"ok": true, "path": p, "sha": head, "unchanged": true,
		})
		return
	}

	/* Only now, whether this write was built on the version it replaces.

	   AFTER the no-op check above, and that order is load-bearing: a client
	   whose base is stale but whose CONTENT matches the file has nothing to
	   conflict about. Answering it with a 409 made the editor park a conflict
	   copy of text nobody disputed — three clients sharing one document
	   produced two such copies of identical text, which is how this was
	   found. A conflict is a disagreement, not a late arrival. */
	// The browser says which version its buffer was built on. A mismatch means
	// somebody else wrote this path in the meantime, and taking this body
	// wholesale would erase their work — which is exactly what happened when a
	// client lost the co-editing relay: two browsers holding different
	// documents, each overwriting the other every few seconds, silently.
	//
	// Optional by design. A caller that sends no If-Match gets the old
	// behaviour, so older clients, the MCP tools and every other writer here
	// are untouched.
	if base != "" {
		if fi, ok := s.headFile(r.Context(), v, p); ok && sharpShrink(fi.Size, size) &&
			r.Header.Get(confirmShrinkHeader) == "" {
			writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{
				"error":  "this save would empty or sharply shrink the file",
				"shrink": true,
				"sha":    fi.Blob,
				"size":   fi.Size,
				"path":   p,
			})
			return
		}
	}
	// Spool first, then charge what actually arrived. Content-Length is -1 on
	// any chunked request, so max(r.ContentLength, 0) admitted an upload of any
	// size against a quota of zero bytes and billed it at zero — the hole round
	// 1 closed on the device door, still open on this one.

	// The check above reads this process's snapshot. On a journal-backed
	// source the base also rides into the append, which checks it against the
	// journals the op lands on — the only place a write from another hub
	// process is guaranteed to be seen.
	switch rs, ok := up.(*RemoteSource); {
	case createOnly:
		err = s.createAbsent(r.Context(), v, up, p, tmp, size, blob, s.requestUser(r))
	case ok:
		err = rs.uploadIf(r.Context(), p, tmp, s.requestUser(r), "", base)
	default:
		err = up.Upload(r.Context(), p, tmp, size, s.requestUser(r), "")
	}
	var stale *staleWriteError
	if errors.As(err, &stale) {
		// The write that beat this one landed between the check above and the
		// append (another hub process): merged the same way, when it can be.
		head := stale.head
		if base != "" && head != "" {
			if handled, now := s.mergeStaleUpload(w, r, v, p, base, head, tmp, size); handled {
				return
			} else if now != "" {
				head = now
			}
		}
		// Quota already passed for these bytes above.
		if !createOnly && s.parkStaleUpload(w, r, v, up, p, head, tmp, size, blob, billed) {
			return
		}
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error": stale.Error(), "sha": head, "path": p,
		})
		return
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("store: %v", err), http.StatusBadGateway)
		return
	}
	if billed > 0 {
		s.quota().RecordUsage(org, billed)
	}
	v.invalidate()
	s.dataWrote(r.Context(), projectID(r))
	s.captureChange(r, "browser", 1, 0)
	s.publishChange(r, "browser", []string{p}, 1, 0)
	/* The version to base the NEXT If-Match on — read back, not assumed.

	   It is NOT necessarily the content sha this write produced. A DirSource
	   identifies a file by mtime and size ("dir-<ns>-<bytes>", dir.go), which
	   is all revalidation needs and is never a content hash — so handing the
	   caller the sha it just uploaded would hand it a value the next
	   comparison could never match, and every save after the first on
	   `bdrive serve --dir` would be refused and parked as a conflict copy.
	   Whatever headBlob says now is, by construction, what the next write
	   will be checked against. */
	head, ok := s.headBlob(r.Context(), v, p)
	if !ok {
		head = blob
	}
	s.adoptRoomWrite(r.Context(), projectID(r), p, head, tmp)
	writeJSON(w, map[string]any{"ok": true, "path": p, "sha": head})
}

// parkStaleUpload keeps a conditional write that lost its base, and could not
// be merged, beside the file as a conflict copy — only when the request asks
// for it (parkOnConflictHeader). handled says a response was written: ok
// naming the copy, or the refusal that kept it from being stored. billed is
// what quota already admitted for these bytes, or -1 when nobody asked yet.
//
// A copy is named to the second, and two tabs of one account can close in
// the same one: each name is created only if it is free (createAbsent), and a
// taken one moves the copy a second on, so neither replaces the other.
func (s *Server) parkStaleUpload(w http.ResponseWriter, r *http.Request, v *volume, up Uploader, p, head string, tmp *os.File, size int64, blob string, billed int64) (handled bool) {
	if r.Header.Get(parkOnConflictHeader) == "" || size == 0 {
		return false
	}
	who := s.requestUser(r)
	ctx := r.Context()
	project := projectID(r)
	org := s.orgOf(project)
	if billed < 0 {
		billed = size
		if rs, ok := v.source.(*RemoteSource); ok {
			var err error
			if billed, err = billableBytes(ctx, rs.Backend, "blobs/"+blob, size); err != nil {
				storageErr(w, http.StatusBadGateway, "could not check the stored object", err)
				return true
			}
		}
		if err := s.quota().CheckWrite(org, billed+s.reservedBytes(org)); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return true
		}
	}
	t0 := time.Now()
	parked := ""
	for i := range maxConflictNameTries {
		name := conflictPath(p, who, t0.Add(time.Duration(i)*time.Second))
		if !s.writablePath(w, r, name) {
			return true
		}
		unlock := s.lockPath(project, name)
		err := s.createAbsent(ctx, v, up, name, tmp, size, blob, who)
		unlock()
		var stale *staleWriteError
		if errors.As(err, &stale) {
			continue // that second's copy is somebody else's
		}
		if err != nil {
			http.Error(w, fmt.Sprintf("store: %v", err), http.StatusBadGateway)
			return true
		}
		parked = name
		break
	}
	if parked == "" {
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error": "no free conflict copy name", "sha": head, "path": p,
		})
		return true
	}
	if billed > 0 {
		s.quota().RecordUsage(org, billed)
	}
	v.invalidate()
	s.dataWrote(ctx, project)
	s.captureChange(r, "browser", 1, 0)
	s.publishChange(r, "browser", []string{parked}, 1, 0)
	writeJSON(w, map[string]any{"ok": true, "path": p, "sha": head, "parked": parked})
	return true
}

// maxConflictNameTries bounds how many seconds past now a conflict copy's
// name moves while earlier ones are taken.
const maxConflictNameTries = 64

// createAbsent stores tmp (blob, size) at p only if p does not exist or
// already holds blob; otherwise it is a *staleWriteError naming what p holds.
// On a journal-backed source the absence rides into the append itself, so a
// second hub process creating p at the same moment is refused there too. On
// any other source the caller holds p's path lock.
func (s *Server) createAbsent(ctx context.Context, v *volume, up Uploader, p string, tmp *os.File, size int64, blob string, who User) error {
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if rs, ok := up.(*RemoteSource); ok {
		// Blob before journal, always.
		if err := rs.Backend.Put(ctx, "blobs/"+blob, tmp, size); err != nil {
			return fmt.Errorf("push blob: %w", err)
		}
		return rs.commitIfBase(ctx, p, blob, size, who, "", &commitBase{path: p, mustBeAbsent: true, blob: blob})
	}
	// A directory names a version by mtime and size, never content, so any
	// file already there is taken.
	if head, ok := s.headBlob(ctx, v, p); ok {
		return &staleWriteError{head: head}
	}
	return up.Upload(ctx, p, tmp, size, who, "")
}

// validMergedText reports whether a clean three-way text merge of path is
// still a well-formed file of its kind. Two edits that each keep a .json
// document valid can merge into one that is not (two keys added at one
// point, with no comma between them); a merge the format refuses is a
// conflict, never a commit. Mirrors the device-side check in
// internal/syncer/merge.go.
func validMergedText(path string, merged []byte) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return json.Valid(merged)
	case ".jsonl":
		for _, line := range bytes.Split(merged, []byte{'\n'}) {
			if line = bytes.TrimSpace(line); len(line) > 0 && !json.Valid(line) {
				return false
			}
		}
	}
	return true
}

// mergeStaleUpload settles a conditional write whose base lost a race: the
// base, the head that replaced it and the incoming bytes are merged three
// ways (textmerge), and the merge is written held to that head. handled says
// a response was written; otherwise the caller refuses with a 409 naming now
// (the newest head seen, "" if unchanged).
//
// Only text, only under maxUploadMergeBytes, only when the base can be read
// back, and never for a co-editing room's own write (noMergeHeader). A head
// that moves again while the merge is appended is merged again, up to three
// times. The merged write reaches an open room like any outside write:
// a clean room follows it, a dirty one reconciles at its next snapshot.
func (s *Server) mergeStaleUpload(w http.ResponseWriter, r *http.Request, v *volume, p, base, head string, tmp *os.File, size int64) (handled bool, now string) {
	rs, ok := v.source.(*RemoteSource)
	if !ok || r.Header.Get(noMergeHeader) != "" || size > maxUploadMergeBytes {
		return false, ""
	}
	ctx := r.Context()
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return false, ""
	}
	incoming, err := io.ReadAll(io.LimitReader(tmp, maxUploadMergeBytes+1))
	if err != nil || len(incoming) > maxUploadMergeBytes || !textmerge.IsText(incoming) {
		return false, ""
	}
	read := func(sha string) ([]byte, bool) {
		rc, err := rs.OpenBlob(ctx, sha)
		if err != nil {
			return nil, false
		}
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, maxUploadMergeBytes+1))
		if err != nil || len(b) > maxUploadMergeBytes {
			return nil, false
		}
		return b, true
	}
	baseBytes, ok := read(base)
	if !ok {
		return false, ""
	}
	// A retry of a write that already landed — the response was lost, and
	// somebody else has written on top of it since — carries a body the
	// path's history already holds after its base. Merging it again would
	// insert its lines a second time; it is already in the file's lineage.
	// Only while the head still carries every line that write made, though:
	// once a later edit removed or rewrote them (X -> A -> B), resubmitting
	// A on X is a fresh edit to merge, and answering B unchanged would drop it.
	project := projectID(r)
	org := s.orgOf(project)
	if rs.landedAfter(ctx, p, base, head, sha256Hex(incoming), s.requestUser(r).Email) {
		if headBytes, ok := read(head); ok && textmerge.RetainsChanges(baseBytes, incoming, headBytes) {
			if err := s.quota().CheckWrite(org, s.reservedBytes(org)); err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return true, head
			}
			writeJSON(w, map[string]any{"ok": true, "path": p, "sha": head, "merged": true, "unchanged": true})
			return true, head
		}
	}
	who := s.requestUser(r)
	for range 3 {
		headBytes, ok := read(head)
		if !ok {
			return false, head
		}
		merged, ok := textmerge.Merge(baseBytes, headBytes, incoming)
		if !ok || len(merged) > maxUploadMergeBytes || !validMergedText(p, merged) {
			return false, head
		}
		sum := sha256.Sum256(merged)
		blob := hex.EncodeToString(sum[:])
		if blob == head {
			if err := s.quota().CheckWrite(org, s.reservedBytes(org)); err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return true, head
			}
			// The head already holds everything this write adds.
			writeJSON(w, map[string]any{"ok": true, "path": p, "sha": head, "merged": true, "unchanged": true})
			return true, head
		}
		// The shrink guard holds the merge to the head it lands on.
		if sharpShrink(int64(len(headBytes)), int64(len(merged))) && r.Header.Get(confirmShrinkHeader) == "" {
			return false, head
		}
		n := int64(len(merged))
		billed, err := billableBytes(ctx, rs.Backend, "blobs/"+blob, n)
		if err != nil {
			storageErr(w, http.StatusBadGateway, "could not check the stored object", err)
			return true, head
		}
		if err := s.quota().CheckWrite(org, billed+s.reservedBytes(org)); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return true, head
		}
		// Blob before journal, always.
		if err := rs.Backend.Put(ctx, "blobs/"+blob, bytes.NewReader(merged), n); err != nil {
			http.Error(w, fmt.Sprintf("store: push blob: %v", err), http.StatusBadGateway)
			return true, head
		}
		err = rs.commitIfBase(ctx, p, blob, n, who, "merged concurrent edits", &commitBase{path: p, sha: head, blob: blob})
		var stale *staleWriteError
		if errors.As(err, &stale) {
			if stale.head == "" {
				return false, head // deleted meanwhile: nothing to merge onto
			}
			head = stale.head
			continue
		}
		if err != nil {
			http.Error(w, fmt.Sprintf("store: %v", err), http.StatusBadGateway)
			return true, head
		}
		if billed > 0 {
			s.quota().RecordUsage(org, billed)
		}
		v.invalidate()
		s.dataWrote(ctx, project)
		s.captureChange(r, "browser", 1, 0)
		s.publishChange(r, "browser", []string{p}, 1, 0)
		// Not inline: a dirty room settles by writing through this door,
		// whose path lock the caller still holds.
		go s.notifyRoomExternalWrite(project, []string{p})
		writeJSON(w, map[string]any{"ok": true, "path": p, "sha": blob, "merged": true})
		return true, blob
	}
	return false, head
}

// landedAfter reports whether the write being retried — blob, built on base,
// by user — already landed and what replaced it since was written on top of
// it. It walks the head's ancestry backwards along each put's Base link and
// stops at the NEWEST occurrence of base: only a put of blob made on that
// base by the same user, strictly between it and the head, is the retried
// write. Anything older is a different lineage — X -> A -> X -> B holds a
// put of A after a put of X, but a stale write of A against the second X is
// a fresh edit to merge, not a retry.
func (r *RemoteSource) landedAfter(ctx context.Context, path, base, head, blob, user string) bool {
	if blob == base || blob == head || base == "" || head == "" {
		return false
	}
	idx, err := r.historyIndex(ctx)
	if err != nil {
		return false
	}
	var ops []journal.Op // this path's ops, oldest first (journal.Less)
	for _, so := range idx.all {
		if so.Op.Path == path {
			ops = append(ops, so.Op)
		}
	}
	if len(ops) == 0 {
		return false
	}
	// The winning op is the newest; it must be the head being merged onto.
	at := len(ops) - 1
	cur := ops[at]
	if cur.Kind != journal.KindPut || cur.Blob != head {
		return false
	}
	for range len(ops) {
		if at != len(ops)-1 && cur.Blob == blob && cur.Base == base && cur.User == user {
			return true
		}
		if cur.Base == "" || cur.Base == base {
			return false
		}
		// Step to the newest earlier put that produced cur's base.
		next := -1
		for i := at - 1; i >= 0; i-- {
			if ops[i].Kind == journal.KindPut && ops[i].Blob == cur.Base {
				next = i
				break
			}
		}
		if next < 0 {
			return false
		}
		at, cur = next, ops[next]
	}
	return false
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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
	// Commit is what makes a blob part of the volume — it writes the op — so
	// it is gated even though init already was: a client may call it directly.
	if !s.writablePath(w, r, req.Path) {
		return
	}
	// The blob may already sit in storage (direct upload), but commit is what
	// makes it part of the volume — so it is the accounting point, and the
	// only one in direct mode. Size comes from storage: req.Size is the
	// caller's claim, and it would otherwise be both the quota charge and the
	// journaled Op.Size.
	// Bytes are charged once, where they land. If this commit finalizes a
	// grant nothing has confirmed yet, it is the one charging them, from the
	// size storage reports; if the reconciler (or a relayed put) already did,
	// there is nothing left to claim and commit charges nothing — committing
	// a path is not a second copy of the content.
	claimed := s.claimGrant(r.PathValue("project"), "blobs/"+req.SHA256)
	size, exists, err := direct.BlobSize(r.Context(), req.SHA256)
	if err != nil {
		http.Error(w, fmt.Sprintf("commit: %v", err), http.StatusBadGateway)
		return
	}
	if !exists {
		http.Error(w, fmt.Sprintf("commit: %v", errBlobMissing), http.StatusConflict)
		return
	}
	org := s.orgOf(r.PathValue("project"))
	if err := s.quota().CheckWrite(org, size); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if err := direct.Commit(r.Context(), req.Path, req.SHA256, size, s.requestUser(r), ""); err != nil {
		code := http.StatusBadGateway
		if err == errBlobMissing {
			code = http.StatusConflict
		}
		http.Error(w, fmt.Sprintf("commit: %v", err), code)
		return
	}
	if claimed {
		s.quota().RecordUsage(org, size)
	}
	v.invalidate()
	s.dataWrote(r.Context(), projectID(r))
	s.captureChange(r, "browser", 1, 0)
	s.publishChange(r, "browser", []string{req.Path}, 1, 0)
	writeJSON(w, map[string]any{"ok": true, "path": req.Path})
}

/*
appendConditional is the journal append when the store can compare and swap.

	The read and the write are ONE transaction as far as the store is
	concerned: read the journal at a version, append, write back only if
	nothing moved. If something did, re-read and redo it. next builds the
	bytes to append, and is called again on every attempt AFTER the version is
	read: whatever it checks or stamps is then at least as new as the journal
	the write is conditioned on, so a write that lands in between fails the
	PutIf and gets looked at by the retry.

	upmu is still held, and is not redundant. Within one process it stops N
	goroutines from all taking the slow path; the precondition covers the case
	a mutex structurally cannot, which is a second process. Both, because they
	guard different things.

	A bounded number of attempts rather than looping until it works.
	Contention here means two hub processes committing to the SAME device
	journal within milliseconds, which is rare — a commit that loses five
	times running is more likely a store misbehaving than a busy one, and
	failing it is how the caller learns to retry instead of the hub spinning.
*/
func (r *RemoteSource) appendConditional(ctx context.Context, cp remote.ConditionalPutter, key string, next any) error {
	return r.appendConditionalExisting(ctx, cp, key, func(existing []byte) ([]byte, error) {
		switch fn := next.(type) {
		case func([]byte) ([]byte, error):
			return fn(existing)
		case func() ([]byte, error):
			return fn()
		default:
			return nil, fmt.Errorf("invalid journal append callback")
		}
	})
}

func (r *RemoteSource) appendConditionalExisting(ctx context.Context, cp remote.ConditionalPutter, key string, next func([]byte) ([]byte, error)) error {
	const attempts = 5
	var lastErr error
	for range attempts {
		existing, version, err := readVersioned(ctx, cp, key)
		if err != nil {
			return fmt.Errorf("fetch journal: %w", err)
		}
		line, err := next(existing)
		if err != nil {
			return err
		}
		data := append(append([]byte(nil), existing...), line...)
		_, err = cp.PutIf(ctx, key, bytes.NewReader(data), int64(len(data)), version)
		if err == nil {
			return nil
		}
		if !errors.Is(err, remote.ErrVersionMismatch) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("journal changed under %d successive appends: %w", attempts, lastErr)
}

// readVersioned reads a journal that may not exist yet. A missing object is
// not an error — it is VersionAbsent, which PutIf turns into "create this and
// fail if somebody beat me to it", so two processes creating one journal at
// the same moment still cannot lose an append.
func readVersioned(ctx context.Context, cp remote.ConditionalPutter, key string) ([]byte, remote.Version, error) {
	rc, version, err := cp.GetVersioned(ctx, key)
	if err != nil {
		if remote.IsNotExist(err) {
			return nil, remote.VersionAbsent, nil
		}
		return nil, remote.VersionAbsent, err
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, remote.VersionAbsent, err
	}
	return body, version, nil
}

// appendLastWriterWins is the original path, for a store with no
// compare-and-swap. Correct while the hub runs as ONE process, which is the
// only configuration it is currently allowed to run in.
func (r *RemoteSource) appendLastWriterWins(ctx context.Context, key string, line []byte) error {
	// A transient read error must fail the commit — treating it as "no
	// journal yet" would rewrite the key without our earlier ops.
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
	data := append(existing, line...)
	return r.Backend.Put(ctx, key, strings.NewReader(string(data)), int64(len(data)))
}
