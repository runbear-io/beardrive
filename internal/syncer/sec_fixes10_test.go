package syncer

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/remote"
)

// Round 11 attacks round 10's fixes in this package: pullBound/maxPullBytes,
// maxPeerJournals, safeDevice, and the reassertNote marker.
//
// Helper prefix: secfx10.

// ---------------------------------------------------------------------------
// pullBound is not applied on every path that fetches a blob.
// ---------------------------------------------------------------------------

// TestSec_HostileHub_ARestoreCannotBeSizedByTheHub.
//
// Round 10 put an absolute ceiling under every read of a blob a hub serves —
// "the party serving the bytes must not also choose how many of them the daemon
// buffers" — and applied it at the two sites in pull:
//
//	syncer.go:743  io.ReadAll(io.LimitReader(rc, pullBound(o.Size)))   // journal
//	syncer.go:846  s.Store.PutBlobReader(io.LimitReader(rc, pullBound(op.Size))) // blob
//
// restore.go:58 is the third site and it has no bound at all:
//
//	rc, err := s.Backend.Get(ctx, "blobs/"+sha)
//	got, _, err := s.Store.PutBlobReader(rc)
//
// PutBlobReader spools to the volume's tmp dir before anything checks the hash,
// so the hub writes as many bytes to the user's disk as it cares to send. The
// sha comes off the history listing the SAME hub answered, so the hub picks the
// key as well.
//
// `bdrive restore` is the one command a user runs when something has already
// gone wrong, which makes "the hub decides how much of your disk this costs"
// the wrong answer here specifically.
func TestSec_HostileHub_ARestoreCannotBeSizedByTheHub(t *testing.T) {
	const willing = 72 << 20 // what the hub is prepared to write
	const ceiling = 64 << 20 // maxPullBytes is 32<<20; twice that is generous

	_, hostile := sechostPeer(t, map[string]string{"notes/real.md": "small file, honestly"})
	hostile.onBody = func(key string, body []byte, w http.ResponseWriter) bool {
		if !strings.HasPrefix(key, "blobs/") {
			return false
		}
		hostile.served.Add(sechostFill(w, nil, willing))
		return true
	}
	victim := newDevice(t, "deva", hostile.backend(t))

	// The sha is whatever the hub's own history answer named; this device has
	// never held it, which is the whole reason restore reaches for the network.
	err := victim.Restore(context.Background(), "notes/real.md", strings.Repeat("a", 64))
	t.Logf("restore returned: %v", err)

	if n := hostile.served.Load(); n > ceiling {
		t.Fatalf("one `bdrive restore` let the hub write %d bytes into this device's volume tmp dir "+
			"(ceiling %d, maxPullBytes %d): restore.go's fetchBlob calls PutBlobReader straight off "+
			"the wire, with none of the pullBound round 10 put on the other two blob reads",
			n, int64(ceiling), int64(maxPullBytes))
	}
}

// ---------------------------------------------------------------------------
// maxPeerJournals: the boundary, and whether a hub can churn past it.
// ---------------------------------------------------------------------------

// secfx10Journals returns how many journal files this device's volume holds.
func secfx10Journals(t *testing.T, s *Session) int {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(s.Store.Dir(), "journal"))
	if err != nil {
		return 0
	}
	return len(ents)
}

// TestSec_HostileHub_ARenamingListingCannotChurnPastThePeerJournalCap.
//
// maxPeerJournals (512) is decremented by whatever the journal dir already
// holds, and only NEW journals consume the budget — existing ones always keep
// updating, so a real project's peers are never starved. The attack is churn:
// a hub that names 512 fresh device ids on every listing. If the cap were
// per-cycle rather than per-disk, N cycles would mint 512*N files.
func TestSec_HostileHub_ARenamingListingCannotChurnPastThePeerJournalCap(t *testing.T) {
	storage, hostile := sechostPeer(t, map[string]string{"notes/real.md": "hi"})
	round := 0
	// Every cycle the hub offers a completely different set of peer ids, each
	// carrying the honest peer's journal body.
	hostile.onList = func(objs []remote.Object) []remote.Object {
		out := objs
		for i := 0; i < 700; i++ {
			out = append(out, remote.Object{
				Key:  fmt.Sprintf("journal/churn-%d-%04d.jsonl", round, i),
				Size: 4,
			})
		}
		return out
	}
	hostile.onBody = func(key string, body []byte, w http.ResponseWriter) bool {
		if strings.Contains(key, "churn-") {
			w.Write([]byte("{}\n"))
			return true
		}
		return false
	}
	_ = storage

	victim := newDevice(t, "deva", hostile.backend(t))
	for round = 0; round < 3; round++ {
		victim.Cycle(context.Background())
	}
	if n := secfx10Journals(t, victim); n > maxPeerJournals+1 {
		t.Fatalf("the hub minted %d journal files on this device across 3 cycles (cap %d + this "+
			"device's own): the peer-journal cap is per-cycle, not per-disk, so renaming the set "+
			"every cycle walks straight past it", n, maxPeerJournals)
	}
}

// TestSec_HostileHub_AnUnusableDeviceNameIsNotAPeer re-measures safeDevice at
// its boundary: 200 bytes is a name, 201 is not, and neither may abandon the
// rest of the listing.
func TestSec_HostileHub_AnUnusableDeviceNameIsNotAPeer(t *testing.T) {
	_, hostile := sechostPeer(t, map[string]string{"notes/real.md": "hi"})
	hostile.onList = func(objs []remote.Object) []remote.Object {
		return append([]remote.Object{
			{Key: "journal/" + strings.Repeat("n", 201) + ".jsonl", Size: 4},
			{Key: "journal/" + strings.Repeat("n", 200) + ".jsonl", Size: 4},
		}, objs...)
	}
	hostile.onBody = func(key string, body []byte, w http.ResponseWriter) bool {
		if strings.Contains(key, "nnnn") {
			w.Write([]byte("{}\n"))
			return true
		}
		return false
	}
	victim := newDevice(t, "deva", hostile.backend(t))
	victim.Cycle(context.Background())
	if _, err := os.Stat(filepath.Join(victim.Folder, "notes", "real.md")); err != nil {
		t.Fatalf("an over-long device name at the front of the listing hid the honest peer: %v", err)
	}
	// The 201-byte name must not have become a file.
	ents, _ := os.ReadDir(filepath.Join(victim.Store.Dir(), "journal"))
	for _, e := range ents {
		if len(e.Name()) > 200+len(".jsonl") {
			t.Fatalf("a %d-byte device name became a local journal file: %q", len(e.Name()), e.Name())
		}
	}
}
