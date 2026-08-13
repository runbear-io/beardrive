package syncer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// Row 19, driven end to end: a real syncing device pointed at an HTTP server
// that speaks /api/p/<id>/store/* and answers however it likes.
//
// The hub here PROXIES a real file:// store that an honest peer device syncs
// into, so every test has a control: the same cycle against the same data with
// the hostile lever off must converge. The delta is the finding.

// sechostProxy is the hostile hub. Zero value forwards everything honestly.
type sechostProxy struct {
	*httptest.Server
	be remote.Backend

	onList  func([]remote.Object) []remote.Object                     // rewrite the listing
	onBody  func(key string, body []byte, w http.ResponseWriter) bool // serve a key ourselves
	claimed bool                                                      // sign: "direct, already stored"

	putBytes atomic.Int64 // bytes this hub actually received on PUT
	served   atomic.Int64 // bytes this hub wrote for /store/object
}

func sechostNewProxy(t *testing.T, be remote.Backend) *sechostProxy {
	t.Helper()
	t.Setenv("BDRIVE_HOME", t.TempDir())
	h := &sechostProxy{be: be}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		key := r.URL.Query().Get("key")
		switch {
		case strings.HasSuffix(r.URL.Path, "/store/list"):
			objs, err := h.be.List(ctx, r.URL.Query().Get("prefix"))
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			if h.onList != nil {
				objs = h.onList(objs)
			}
			out := make([]map[string]any, 0, len(objs))
			for _, o := range objs {
				out = append(out, map[string]any{"key": o.Key, "size": o.Size})
			}
			json.NewEncoder(w).Encode(map[string]any{"objects": out})
		case strings.HasSuffix(r.URL.Path, "/store/object") && r.Method == http.MethodGet:
			var body []byte
			if rc, err := h.be.Get(ctx, key); err == nil {
				body, _ = io.ReadAll(rc)
				rc.Close()
			}
			if h.onBody != nil && h.onBody(key, body, w) {
				return
			}
			if body == nil {
				http.Error(w, "no such object", http.StatusNotFound)
				return
			}
			n, _ := w.Write(body)
			h.served.Add(int64(n))
		case strings.HasSuffix(r.URL.Path, "/store/object") && r.Method == http.MethodPut:
			var buf bytes.Buffer
			n, _ := io.Copy(&buf, r.Body)
			h.putBytes.Add(n)
			if err := h.be.Put(ctx, key, &buf, n); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
		case strings.HasSuffix(r.URL.Path, "/store/sign"):
			w.Header().Set("Content-Type", "application/json")
			if h.claimed {
				// The one-field lie an honest hub tells truthfully when a
				// content-addressed blob is already in storage.
				w.Write([]byte(`{"mode":"direct","exists":true}`))
				return
			}
			w.Write([]byte(`{"mode":"server","exists":false}`))
		case strings.HasSuffix(r.URL.Path, "/store/exists"):
			ok, _ := h.be.Exists(ctx, key)
			json.NewEncoder(w).Encode(map[string]any{"exists": ok})
		default:
			http.Error(w, "not served", http.StatusNotFound)
		}
	}))
	t.Cleanup(h.Server.Close)
	return h
}

func (h *sechostProxy) backend(t *testing.T) remote.Backend {
	t.Helper()
	be, err := remote.Open(context.Background(), h.Server.URL+"/p/p-0123abcd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { be.Close() })
	return be
}

// sechostPeer seeds a real project through a file:// store and returns that
// store plus the hostile hub fronting it.
func sechostPeer(t *testing.T, files map[string]string) (remote.Backend, *sechostProxy) {
	t.Helper()
	storage := sharedRemote(t)
	peer := newDevice(t, "peer", storage)
	for rel, content := range files {
		write(t, peer.Folder, rel, content)
	}
	cycle(t, peer)
	return storage, sechostNewProxy(t, storage)
}

// sechostJSONL re-encodes ops as a journal body.
func sechostJSONL(ops []journal.Op) []byte {
	var b bytes.Buffer
	for _, op := range ops {
		line, _ := json.Marshal(op)
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// sechostFill writes filler until stop bytes are out or the client stops
// reading, and reports the count.
func sechostFill(w http.ResponseWriter, head []byte, stop int64) int64 {
	var n int64
	if k, err := w.Write(head); err != nil {
		return int64(k)
	} else {
		n = int64(k)
	}
	w.(http.Flusher).Flush()
	chunk := bytes.Repeat([]byte("x"), 512<<10)
	for n < stop {
		k, err := w.Write(chunk)
		n += int64(k)
		if err != nil {
			break
		}
		w.(http.Flusher).Flush()
	}
	return n
}

// ---------------------------------------------------------------------------

// A hub names the objects, and syncer.pull turns each name into a local file
// path through store.JournalPath, which validates nothing. A name the OS cannot
// open (too long, a NUL, any control byte) makes os.ReadFile fail with something
// that is not IsNotExist — and pull RETURNS on that error, abandoning every
// journal it had not reached yet.
//
// So one extra entry at the front of one listing decides which peers a device
// ever sees, forever, and the only thing the user is told is "offline". This is
// the round-7 finding's shape (a peer choosing which ops each device sees) with
// the hub, not a peer, holding the lever — and it needs no write access to
// anything.
func TestSec_HostileHub_OneUnusableListedKeyCannotHideEveryPeer(t *testing.T) {
	poison := "journal/" + strings.Repeat("d", 300) + ".jsonl"

	// Control: the same hub, same data, no poison key.
	_, ctl := sechostPeer(t, map[string]string{"notes/real.md": "the peer's work"})
	good := newDevice(t, "deva", ctl.backend(t))
	if res := cycle(t, good); res.PulledOps == 0 {
		t.Fatalf("harness: the control device pulled nothing: %+v", res)
	}
	if read(t, good.Folder, "notes/real.md") != "the peer's work" {
		t.Fatal("harness: the control device did not converge")
	}

	// Attack: identical hub, one extra listed key in front.
	_, hostile := sechostPeer(t, map[string]string{"notes/real.md": "the peer's work"})
	hostile.onList = func(objs []remote.Object) []remote.Object {
		return append([]remote.Object{{Key: poison, Size: 10}}, objs...)
	}
	// Served, not 404'd: a 404 is an honest race between LIST and GET and heals
	// on the next cycle. This body arrives fine — it is store.JournalPath(dev)
	// that the OS refuses, on a name the hub will keep listing forever.
	hostile.onBody = func(key string, body []byte, w http.ResponseWriter) bool {
		if key == poison {
			w.Write([]byte("{}\n"))
			return true
		}
		return false
	}
	victim := newDevice(t, "deva", hostile.backend(t))
	res, err := victim.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(victim.Folder, "notes", "real.md")); err != nil {
		t.Fatalf("one unusable key in the listing hid every peer's journal from this device "+
			"(pulled %d ops, offline=%v: %v); pull abandons the whole listing on a JournalPath "+
			"the OS refuses, and the hub picks the order", res.PulledOps, res.Offline, res.OfflineErr)
	}
}

// The bound on a journal body is sizeBound(o.Size) — and o.Size is a number the
// same party that serves the bytes wrote in the same response. Round 8's stated
// property ("the party serving the bytes must not also choose how many of them
// the daemon buffers") is not met when the hub is the peer: there is no absolute
// ceiling anywhere on the path, so one listing entry sizes the allocation.
func TestSec_HostileHub_ADeclaredJournalSizeCannotChooseTheDeviceAllocation(t *testing.T) {
	const declared = 512 << 20         // what the hub says the journal is
	const willing = 2*maxPullBytes + (40 << 20) // more than the generous ceiling: proves truncation
	const ceiling = 2 * maxPullBytes // twice the cap is generous: transport buffering, not the bound, owns the slack

	_, hostile := sechostPeer(t, map[string]string{"notes/real.md": "hi"})
	hostile.onList = func(objs []remote.Object) []remote.Object {
		for i := range objs {
			if strings.HasPrefix(objs[i].Key, "journal/") {
				objs[i].Size = declared
			}
		}
		return objs
	}
	hostile.onBody = func(key string, body []byte, w http.ResponseWriter) bool {
		if !strings.HasPrefix(key, "journal/") {
			return false
		}
		hostile.served.Add(sechostFill(w, body, willing))
		return true
	}
	victim := newDevice(t, "deva", hostile.backend(t))
	victim.Cycle(context.Background())
	if n := hostile.served.Load(); n > ceiling {
		t.Fatalf("the hub declared a %d-byte journal and this device read %d bytes of it into memory "+
			"(io.ReadAll under a bound the hub itself chose); ceiling %d", int64(declared), n, int64(ceiling))
	}
}

// Same shape one layer down, and this one lands on disk rather than in memory:
// a pulled blob is read under sizeBound(op.Size), where op.Size is a field in
// the journal line the hub just served. PutBlobReader spools it into the
// volume's tmp dir before anything checks the hash, so the hub writes as many
// bytes to the user's disk as it names — on every cycle, for every op.
func TestSec_HostileHub_ADeclaredBlobSizeCannotFillTheDisk(t *testing.T) {
	const declared = 512 << 20
	const willing = 2*maxPullBytes + (40 << 20)
	const ceiling = 2 * maxPullBytes

	_, hostile := sechostPeer(t, map[string]string{"notes/real.md": "small file, honestly"})
	hostile.onBody = func(key string, body []byte, w http.ResponseWriter) bool {
		if strings.HasPrefix(key, "journal/") {
			ops, err := journal.Parse(body)
			if err != nil {
				return false
			}
			for i := range ops {
				if ops[i].Kind == journal.KindPut {
					ops[i].Size = declared // the only field that bounds the read
				}
			}
			w.Write(sechostJSONL(ops))
			return true
		}
		if strings.HasPrefix(key, "blobs/") {
			hostile.served.Add(sechostFill(w, nil, willing))
			return true
		}
		return false
	}
	victim := newDevice(t, "deva", hostile.backend(t))
	victim.Cycle(context.Background())
	if n := hostile.served.Load(); n > ceiling {
		t.Fatalf("one Size field in one journal line made this device spool %d bytes into its volume "+
			"tmp dir for a 20-byte file (ceiling %d), and the cycle retries it every tick", n, int64(ceiling))
	}
}

// "Blobs are pushed before the journal, so a peer never sees an op whose
// content is missing" is a stated invariant. The hub can break it from the
// outside with one boolean: sign answers {"mode":"direct","exists":true}, Put
// returns nil without sending anything, push advances st.PushedOps, and the
// journal goes up naming content that was never uploaded. The device never
// retries — the ops are behind the cursor forever — so the user's file is
// silently not backed up and every teammate gets an op with no content.
func TestSec_HostileHub_ClaimingItAlreadyHasABlobCannotSwallowAPush(t *testing.T) {
	storage, hostile := sechostPeer(t, map[string]string{"seed.md": "seed"})
	hostile.claimed = true

	victim := newDevice(t, "deva", hostile.backend(t))
	write(t, victim.Folder, "notes/mine.md", "content the hub never received")
	cycle(t, victim)
	cycle(t, victim) // a second chance to notice

	// An honest peer reading the same storage must be able to materialize it.
	peer := newDevice(t, "peer2", storage)
	cycle(t, peer)
	if got, err := os.ReadFile(filepath.Join(peer.Folder, "notes", "mine.md")); err != nil ||
		string(got) != "content the hub never received" {
		t.Fatalf("the hub said \"already stored\" and this device published the op anyway: "+
			"storage received %d bytes of blob body, the peer cannot materialize the file (%v), "+
			"and the pushed-ops cursor has moved past it so it is never retried",
			hostile.putBytes.Load(), err)
	}
}

// pull skips a listed journal only when its key equals this device's own id.
// On a case-insensitive filesystem (APFS and NTFS by default) a differently
// cased spelling of the same id is a different key and the SAME FILE, so the
// hub gets to overwrite the one journal this device is the sole writer of —
// which push then uploads as its own. That is the one-writer invariant, broken
// from the receiving end.
func TestSec_HostileHub_CannotOverwriteThisDevicesOwnJournal(t *testing.T) {
	storage, hostile := sechostPeer(t, map[string]string{"seed.md": "seed"})
	victim := newDevice(t, "deva", hostile.backend(t))
	write(t, victim.Folder, "mine.md", "authored here")
	cycle(t, victim)

	own := victim.Store.JournalPath(victim.Device.ID)
	before, err := os.ReadFile(own)
	if err != nil {
		t.Fatal(err)
	}
	if !sechostCaseInsensitive(t, filepath.Dir(own)) {
		t.Skip("filesystem is case-sensitive; this collision is only expressible on APFS/NTFS")
	}

	// Bigger than what is there, so pull's size gate lets it through.
	forged := sechostJSONL([]journal.Op{{
		Seq: 1, Lamport: 99, Kind: journal.KindDelete, Path: "mine.md",
		Device: "DEVA", DeviceName: "DEVA", Note: strings.Repeat("p", len(before)+64),
	}})
	hostile.onList = func(objs []remote.Object) []remote.Object {
		return append(objs, remote.Object{Key: "journal/DEVA.jsonl", Size: int64(len(forged))})
	}
	hostile.onBody = func(key string, body []byte, w http.ResponseWriter) bool {
		if key == "journal/DEVA.jsonl" {
			w.Write(forged)
			return true
		}
		return false
	}
	victim.Cycle(context.Background())

	after, err := os.ReadFile(own)
	_, mineErr := os.Stat(filepath.Join(victim.Folder, "mine.md"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the hub listed this device's own journal under a differently cased key and pull "+
			"wrote over it (%d bytes → %d, err %v); each device writes only its own journal. "+
			"The forged op replays in the same cycle: mine.md stat = %v",
			len(before), len(after), err, mineErr)
	}
	if mineErr != nil {
		t.Fatalf("the hub deleted a locally authored file: %v", mineErr)
	}
	_ = storage
}

func sechostCaseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	p := filepath.Join(dir, ".sechost-case-probe")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(p)
	_, err := os.Stat(filepath.Join(dir, ".SECHOST-CASE-PROBE"))
	return err == nil
}

// Round 7 capped the listing BODY at 8 MiB. It did not cap the object COUNT,
// and a journal entry costs about 40 bytes of JSON — so one in-bounds listing
// names ~200k journals, each of which pull fetches and writes as a file in the
// volume's journal dir. Every later cycle then re-reads all of them (AllOps
// walks the directory), and the next listing can name 200k different ones. No
// write access to the project is needed: this is the answer to /store/list.
func TestSec_HostileHub_AListingCannotMintUnboundedLocalJournals(t *testing.T) {
	const named = 8000 // well under the 8 MiB body cap
	const ceiling = 1024

	_, hostile := sechostPeer(t, map[string]string{"notes/real.md": "x"})
	hostile.onList = func(objs []remote.Object) []remote.Object {
		for i := 0; i < named; i++ {
			objs = append(objs, remote.Object{Key: fmt.Sprintf("journal/g%06d.jsonl", i), Size: 96})
		}
		return objs
	}
	hostile.onBody = func(key string, body []byte, w http.ResponseWriter) bool {
		if strings.HasPrefix(key, "journal/g") {
			w.Write([]byte(`{"seq":1,"lamport":1,"kind":"delete","path":"nope.md","device":"g"}` + "\n"))
			return true
		}
		return false
	}
	victim := newDevice(t, "deva", hostile.backend(t))
	victim.Cycle(context.Background())

	ents, err := os.ReadDir(filepath.Join(victim.Store.Dir(), "journal"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) > ceiling {
		t.Fatalf("one listing made this device create %d journal files (ceiling %d); "+
			"the 8 MiB body cap bounds the bytes, nothing bounds the object count, and every "+
			"later cycle re-reads all of them", len(ents), ceiling)
	}
}

// --- attacks the device already refuses (regression cover) ---

// A listing may name the same journal twice, or name one under a prefix that
// belongs to a different project. Neither may double-apply or reach outside
// this project's key space.
func TestSec_HostileHub_DuplicateAndForeignKeysInAListingAreInert(t *testing.T) {
	_, hostile := sechostPeer(t, map[string]string{"notes/real.md": "the peer's work"})
	hostile.onList = func(objs []remote.Object) []remote.Object {
		dup := append([]remote.Object{}, objs...)
		for _, o := range objs {
			dup = append(dup, o, o) // same key three times
		}
		return append(dup,
			remote.Object{Key: "../other-project/journal/x.jsonl", Size: 9},
			remote.Object{Key: "/journal/x.jsonl", Size: 9})
	}
	victim := newDevice(t, "deva", hostile.backend(t))
	res := cycle(t, victim)
	if read(t, victim.Folder, "notes/real.md") != "the peer's work" {
		t.Fatal("the device did not converge through a listing with duplicates")
	}
	if res.PulledOps != 1 {
		t.Fatalf("a key listed three times produced %d ops, want 1", res.PulledOps)
	}
	ents, _ := os.ReadDir(filepath.Join(victim.Store.Dir(), "journal"))
	if len(ents) != 1 { // the one peer journal, once
		t.Fatalf("journal dir holds %d files after a listing with duplicate and foreign keys", len(ents))
	}
}

// Content addressing is what makes a blob body unforgeable: bytes that do not
// hash to the key are filed under their real hash, so the path stays unwritten
// and the next cycle retries rather than materializing the hub's choice.
func TestSec_HostileHub_ABlobBodyThatIsNotItsKeyIsNeverMaterialized(t *testing.T) {
	_, hostile := sechostPeer(t, map[string]string{"notes/real.md": "the peer's work"})
	hostile.onBody = func(key string, body []byte, w http.ResponseWriter) bool {
		if strings.HasPrefix(key, "blobs/") {
			w.Write([]byte("attacker-chosen content"))
			return true
		}
		return false
	}
	victim := newDevice(t, "deva", hostile.backend(t))
	victim.Cycle(context.Background())
	got, err := os.ReadFile(filepath.Join(victim.Folder, "notes", "real.md"))
	if err == nil && string(got) == "attacker-chosen content" {
		t.Fatal("the hub served bytes that are not the blob's content address and the device wrote them")
	}
	if err == nil && string(got) != "the peer's work" {
		t.Fatalf("unexpected content %q", got)
	}
}

// A 200 with an empty (or truncated) journal body must not undo ops this device
// already applied — the shrink guard in pull.
func TestSec_HostileHub_AnEmptiedJournalCannotUndoAppliedOps(t *testing.T) {
	_, hostile := sechostPeer(t, map[string]string{"notes/real.md": "the peer's work"})
	victim := newDevice(t, "deva", hostile.backend(t))
	cycle(t, victim)
	if read(t, victim.Folder, "notes/real.md") != "the peer's work" {
		t.Fatal("harness: first cycle did not converge")
	}
	hostile.onBody = func(key string, body []byte, w http.ResponseWriter) bool {
		if strings.HasPrefix(key, "journal/") {
			w.WriteHeader(http.StatusOK) // 200, zero bytes
			return true
		}
		return false
	}
	victim.Cycle(context.Background())
	if _, err := os.Stat(filepath.Join(victim.Folder, "notes", "real.md")); err != nil {
		t.Fatalf("an empty journal body removed a file this device had already applied: %v", err)
	}
}

// The hub cannot make a device delete a file it holds by simply not serving the
// op any more: a withdrawn put is re-asserted under this device's own identity.
func TestSec_HostileHub_WithdrawingAJournalCannotDeleteLocalFiles(t *testing.T) {
	_, hostile := sechostPeer(t, map[string]string{"notes/real.md": "the peer's work"})
	victim := newDevice(t, "deva", hostile.backend(t))
	cycle(t, victim)
	hostile.onList = func(objs []remote.Object) []remote.Object {
		var out []remote.Object
		for _, o := range objs {
			if !strings.HasPrefix(o.Key, "journal/peer") {
				out = append(out, o)
			}
		}
		return out
	}
	victim.Cycle(context.Background())
	if got, err := os.ReadFile(filepath.Join(victim.Folder, "notes", "real.md")); err != nil || string(got) != "the peer's work" {
		t.Fatalf("dropping a journal from the listing removed the file it published: %q %v", got, err)
	}
}

// A path a peer names is still checked against journal.SafePath on the way to
// disk, whoever serves it.
func TestSec_HostileHub_CannotWriteOutsideTheMount(t *testing.T) {
	_, hostile := sechostPeer(t, map[string]string{"notes/real.md": "x"})
	victim := newDevice(t, "deva", hostile.backend(t))
	outside := filepath.Join(t.TempDir(), "escaped")
	blob := fmt.Sprintf("%x", sechostSha("pwned"))
	hostile.onBody = func(key string, body []byte, w http.ResponseWriter) bool {
		if strings.HasPrefix(key, "journal/") {
			w.Write(sechostJSONL([]journal.Op{{
				Seq: 1, Lamport: 1, Kind: journal.KindPut, Device: "peer",
				Path: "../../../../../../../../.." + outside, Blob: blob, Size: 5, Mode: 0o644,
			}}))
			return true
		}
		w.Write([]byte("pwned"))
		return true
	}
	victim.Cycle(context.Background())
	if _, err := os.Stat(outside); err == nil {
		t.Fatalf("the hub wrote %s outside the mount", outside)
	}
}

func sechostSha(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
