package webapp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/runbear-io/beardrive/internal/journal"
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
	journalKeyRe = regexp.MustCompile(`^journal/` + deviceIDPattern + `\.jsonl$`)
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
//
// ops is what the body actually carries (empty when the body is not known
// yet, e.g. at signing time). What needs an owner is AUTHORSHIP: ops replay on
// every device and History attributes them to this journal's device.
func (s *Server) ownJournal(w http.ResponseWriter, r *http.Request, key string, ops []journal.Op) bool {
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
	// Matching the key against the header binds nothing on its own: the same
	// request supplies both, so moving them together satisfies the check by
	// construction and any member could replace any peer's journal object —
	// their ops gone, every peer replaying the forged ones, History crediting
	// them to the victim. The device has to belong to the ACCOUNT as well.
	//
	// Ownership is DeviceRegistry.OwnerOf: hub-wide, first claim, ownerless
	// rows claiming nothing. Three things this deliberately does not do, each
	// because doing it was a hole:
	//
	//   - it does not consult the row this request would create. Every /store
	//     handler used to register the caller's header before asking who owns
	//     it, so an unclaimed id authorized whoever named it first. The
	//     callers observe AFTER this returns.
	//   - it does not treat "unclaimed" as permission. An id nothing has ever
	//     synced under is not this caller's to write.
	//   - it does not scope the claim to the project's org, so offboarding a
	//     teammate does not release her journal to the org she left.
	//
	// A hub with no registry cannot resolve ownership at all (single-volume,
	// auth-less, or a fixture): there is nobody to impersonate, and projectPerm
	// answers admin for exactly those configurations.
	if s.Devices != nil {
		me := normEmail(s.requestUser(r).Email)
		owner, known := s.Devices.OwnerOf(dev)
		switch {
		case normEmail(owner) != "" && normEmail(owner) == me:
			// My device, my journal.
		case !known && journalNames(dev, ops):
			// An id nothing on this hub has ever synced under: this write is
			// its first claim. It has to at least BE that device's journal —
			// every op naming it — which is not proof (the field is the
			// writer's too), but it is the difference between a device
			// starting to sync and a member pasting ops under a name they
			// invented. The claim is recorded by observeDevice below, only
			// after this returns, so the request cannot answer its own
			// ownership question.
		case atLeast(s.projectPerm(r, r.PathValue("project")), PermAdmin):
			// Project admin is the recovery path — the answer to "a squatted id
			// is a permanent lockout". The device's own remedy is in the body.
		default:
			http.Error(w, "this device id belongs to another account on this hub; "+
				"delete device.json in your BearDrive home to mint a new one, "+
				"or ask a project admin", http.StatusForbidden)
			return false
		}
	}
	return true
}

// journalNames reports whether every op in a journal write declares the device
// the key names. A journal that attributes its own ops to somebody else is not
// this device's log; readers that trust Op.Device would credit them there.
func journalNames(dev string, ops []journal.Op) bool {
	for _, op := range ops {
		if op.Device != dev {
			return false
		}
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
	// A sync cycle starts here, which makes it the hub's regular opportunity to
	// confirm what its presigned grants actually delivered.
	s.reconcileGrants(r.Context(), r.PathValue("project"), rs.Backend)
	objs, err := rs.Backend.List(r.Context(), prefix)
	if err != nil {
		storageErr(w, http.StatusBadGateway, "storage is temporarily unavailable", err)
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
	// Blobs go through OpenBlob so a presigned write cannot make this route
	// serve content that does not hash to the key it is stored under.
	var rc io.ReadCloser
	var err error
	if blob, isBlob := strings.CutPrefix(key, "blobs/"); isBlob {
		rc, err = rs.OpenBlob(r.Context(), blob)
	} else {
		rc, err = rs.Backend.Get(r.Context(), key)
	}
	if err != nil {
		// Fixed message: os.Open's error names the hub's absolute storage
		// path, and S3's names the bucket and key.
		storageErr(w, http.StatusNotFound, "no such object", err)
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
		storageErr(w, http.StatusBadGateway, "storage is temporarily unavailable", err)
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
	if r.Header.Get("X-Bdrive-Device") != "" && !s.ownJournal(w, r, req.Key, nil) {
		return
	}
	s.observeDevice(r)
	project := r.PathValue("project")
	s.reconcileGrants(r.Context(), project, rs.Backend)
	org := s.orgOf(project)
	// The cap is checked against this write PLUS everything already granted
	// and not yet accounted for, so concurrent grants cannot oversubscribe an
	// allowance that no single one of them exceeds.
	if err := s.quota().CheckWrite(org, req.Size+s.reservedBytes(org)); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	// Only blobs are presigned. They are content-addressed and immutable, so
	// a leaked URL can at worst re-upload identical bytes. Journals are
	// mutable state and always flow through the server.
	if blob, isBlob := strings.CutPrefix(req.Key, "blobs/"); isBlob {
		if !sizeFitsContentAddress(blob, req.Size) {
			http.Error(w, "declared size does not match the content address", http.StatusForbidden)
			return
		}
		if exists, err := rs.Backend.Exists(r.Context(), req.Key); err == nil && exists {
			writeJSON(w, map[string]any{"mode": "direct", "exists": true})
			return
		}
		if signer, ok := rs.Backend.(remote.PutSigner); ok {
			if signed, err := signer.SignPut(r.Context(), req.Key, req.Size, s.Upload.ttl()); err == nil {
				// Reserved, not charged: the bytes go straight to storage, so
				// this grant counts against the cap immediately and is billed
				// when the object is confirmed there (reconcileGrants), or
				// released for free when the URL expires unused. Booking it
				// here outright charged 20 GiB for 20 JSON posts.
				s.reserve(project, org, req.Key, req.Size, s.Upload.ttl())
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

// journalOps reads the operations a spooled journal body carries, exactly the
// way every device reads it (journal.Parse: a line that decodes to no
// operation is no operation). It leaves the file rewound for the store.
// A non-journal key carries no ops by definition.
func journalOps(key string, tmp *os.File) ([]journal.Op, error) {
	if !strings.HasPrefix(key, "journal/") {
		return nil, nil
	}
	data, err := io.ReadAll(tmp)
	if err != nil {
		return nil, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return journal.Parse(data)
}

func (s *Server) handleStorePut(v *volume, w http.ResponseWriter, r *http.Request) {
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	if !s.Upload.Enabled {
		http.Error(w, "uploads are disabled on this server", http.StatusForbidden)
		return
	}
	key, ok := s.storeKey(w, r)
	if !ok {
		return
	}
	// Spool the body before storing any of it. Everything this handler has to
	// be sure of is a property of the bytes, not of the headers the client
	// sent: what a blob key promises (its sha256), what the write costs
	// (Content-Length is -1 on any chunked request, which made every unsized
	// put free), and how many ops a journal write actually authors.
	// Cost: one temp file per put on the hub's busiest write path.
	tmp, size, sum, err := spool(r.Body)
	if err != nil {
		storageErr(w, http.StatusBadGateway, "could not store the object", err)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if blob, isBlob := strings.CutPrefix(key, "blobs/"); isBlob && blob != sum {
		http.Error(w, "content does not hash to its key", http.StatusBadRequest)
		return
	}
	ops, err := journalOps(key, tmp)
	if err != nil {
		storageErr(w, http.StatusBadGateway, "could not store the object", err)
		return
	}
	if !s.ownJournal(w, r, key, ops) {
		return
	}
	// Observed only after the write is authorized: a request that registers
	// the device it claims to be answers its own ownership question.
	s.observeDevice(r)
	project := r.PathValue("project")
	s.reconcileGrants(r.Context(), project, rs.Backend)
	org := s.orgOf(project)
	if err := s.quota().CheckWrite(org, size+s.reservedBytes(org)); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if err := rs.Backend.Put(r.Context(), key, tmp, size); err != nil {
		storageErr(w, http.StatusBadGateway, "could not store the object", err)
		return
	}
	// These bytes came through the hub, so they are charged here — drop any
	// reservation for the same key rather than charging it twice.
	s.claimGrant(project, key)
	s.quota().RecordUsage(org, size)
	if strings.HasPrefix(key, "journal/") {
		v.invalidate() // new ops should show in the viewer immediately
	}
	writeJSON(w, map[string]any{"ok": true})
}
