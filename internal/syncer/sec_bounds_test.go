package syncer

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// Scoreboard rows 15 and 19, round 8: the two unbounded reads left on the
// device side, both with the size in hand at the call site.
//
// Round 7 bounded httpBackend.List at 8 MiB and deliberately left these:
//
//   - syncer.pull does io.ReadAll(rc) on a peer's journal object while
//     remote.Object.Size for that same object is in scope on the same loop
//     iteration, unused.
//   - syncer.pull hands a blob body to store.PutBlobReader, an unbounded
//     io.Copy into a temp file in the volume dir, while op.Size — the size the
//     op itself declares — is the loop variable.
//
// The attacker is whoever serves the bytes: a hostile hub (row 19) or, on a
// shared bucket, any peer that owns its own journal object (row 15). Neither
// needs a credential the threat model does not already grant them. The victim
// is every teammate's laptop, and the failure is a disk the sync daemon fills
// on a 3-second retry loop, because the "never break sync, retry next cycle"
// posture means the same oversized object is fetched again forever.
//
// The secure behaviour these tests assert is the one round 7 chose for List:
// read at most what was declared (plus slack), treat more than that as a
// decode/transfer failure, degrade and retry. Nothing here asserts a specific
// cap — only that the number of bytes the device is willing to pull off the
// wire is not chosen by the party sending them.

// secbndSlack is the headroom a bound may legitimately have over the declared
// size (a trailing newline, an off-by-one, a small fixed floor). Anything the
// device reads beyond this is not "the object we were told about".
const secbndSlack = 1 << 20 // 1 MiB

// secbndFlood is how much a hostile server offers. Small enough that the test
// stays fast, far enough above the declared size that no honest slack reaches
// it.
const secbndFlood = 64 << 20 // 64 MiB

// secbndEndless is an io.ReadCloser that produces an unlimited stream of a
// single byte and counts what was taken from it. It never returns io.EOF
// before the cap, so a reader with no bound of its own runs until the cap.
type secbndEndless struct {
	served *int64
	cap    int64
	fill   byte
}

func (e *secbndEndless) Read(p []byte) (int, error) {
	n := atomic.LoadInt64(e.served)
	if n >= e.cap {
		// The real attacker keeps going; the test stops so it can report.
		return 0, io.EOF
	}
	if int64(len(p)) > e.cap-n {
		p = p[:e.cap-n]
	}
	for i := range p {
		p[i] = e.fill
	}
	atomic.AddInt64(e.served, int64(len(p)))
	return len(p), nil
}

func (e *secbndEndless) Close() error { return nil }

// secbndBackend wraps a real backend and lies about one key: List reports the
// honest declared size, and Get serves an unbounded body for it. This is
// exactly what a hostile hub or a peer rewriting its own object does — the
// listing and the object are two separate answers and nothing binds them.
type secbndBackend struct {
	remote.Backend
	sizes    map[string]int64 // key → the size LIST reports (a lie)
	key      string           // the key GET floods (empty: flood nothing)
	declared int64            // the size LIST reports for key
	served   *int64           // bytes actually handed to the client
	fill     byte
}

func (b *secbndBackend) List(ctx context.Context, prefix string) ([]remote.Object, error) {
	objs, err := b.Backend.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	lies := map[string]int64{}
	for k, v := range b.sizes {
		lies[k] = v
	}
	if b.key != "" {
		lies[b.key] = b.declared
	}
	found := map[string]bool{}
	for i := range objs {
		if sz, ok := lies[objs[i].Key]; ok {
			objs[i].Size = sz
			found[objs[i].Key] = true
		}
	}
	for k, sz := range lies {
		if !found[k] && strings.HasPrefix(k, prefix) {
			objs = append(objs, remote.Object{Key: k, Size: sz})
		}
	}
	return objs, nil
}

func (b *secbndBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if key == b.key {
		return &secbndEndless{served: b.served, cap: secbndFlood, fill: b.fill}, nil
	}
	return b.Backend.Get(ctx, key)
}

// --- 1. the journal body ---

// pull lists journal objects, gets each one whose remote size exceeds the
// local copy, and io.ReadAll's the body. o.Size is right there in the loop.
//
// A hub (or a peer) answers "journal/attacker.jsonl is 120 bytes" and then
// serves gigabytes for it. The device buffers all of it in RAM before a single
// byte is parsed, and does it again on the next cycle, and the next.
//
// Secure behaviour: the body a device accepts for an object is bounded by the
// size that object was listed at.
func TestSec_Bounds_APeersJournalBodyIsBoundedByItsDeclaredSize(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)

	// One honest op, so the journal object genuinely exists and the pull loop
	// has a real reason to fetch it.
	content := "the honest file"
	blob := secjrnBlob(t, be, content)
	secjrnPush(t, be, "attacker", []journal.Op{secjrnOp(1, "notes.md", blob, len(content))})

	var served int64
	// The listing says the journal is a couple of hundred bytes. Whatever the
	// real object is, that is what the device was told it was fetching.
	hostile := &secbndBackend{Backend: be, key: "journal/attacker.jsonl", declared: 200, served: &served, fill: 'x'}
	victim.Backend = hostile

	if _, err := secpeerCycle(t, victim); err != nil {
		t.Logf("cycle degraded (fine): %v", err)
	}

	got := atomic.LoadInt64(&served)
	if got > hostile.declared+secbndSlack {
		t.Fatalf("pull read %d bytes for a journal object the hub listed at %d bytes\n"+
			"(the body a device accepts must be bounded by the size it was told; io.ReadAll on the\n"+
			"journal in syncer.pull has no limit, and o.Size is in scope on the same loop iteration)",
			got, hostile.declared)
	}
}

// --- 2. the blob body ---

// The blob loop in pull is the second half. op.Size is the loop variable; the
// body goes to store.PutBlobReader, which io.Copy's into a temp file under the
// volume dir with no cap at all. The hash check that would notice runs AFTER
// the copy, so the disk is already full when it fires.
//
// One JSONL line — {"kind":"put","path":"a.md","blob":<64 hex>,"size":3} —
// plus an object of the attacker's choosing at blobs/<that sha>, and every
// device that pulls the project writes as much as the server cares to send
// into ~/.bdrive/volumes/<id>/tmp/. It is retried every cycle forever.
//
// Secure behaviour: a blob body is bounded by the size the op declares for it.
func TestSec_Bounds_APeersBlobBodyIsBoundedByTheSizeItsOpDeclares(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)

	// A sha that is well-formed but that the attacker never actually stored:
	// the hostile backend answers for it.
	sha := strings.Repeat("ab", 32)
	const declared = 3
	op := journal.Op{
		Kind: journal.KindPut, Path: "a.md", Blob: sha, Size: declared,
		Device: "attacker", Seq: 1, Lamport: 1,
	}
	secjrnPush(t, be, "attacker", []journal.Op{op})

	var served int64
	hostile := &secbndBackend{Backend: be, key: "blobs/" + sha, declared: declared, served: &served, fill: 'z'}
	victim.Backend = hostile

	if _, err := secpeerCycle(t, victim); err != nil {
		t.Logf("cycle degraded (fine): %v", err)
	}

	got := atomic.LoadInt64(&served)
	if got > declared+secbndSlack {
		t.Fatalf("pull spooled %d bytes for a blob whose own op declares size %d\n"+
			"(store.PutBlobReader is an unbounded io.Copy into the volume's temp dir and op.Size is\n"+
			"the loop variable at the call site; the hash check that would notice runs after the copy)",
			got, declared)
	}
}

// --- 3. the same bound must not break the honest case ---

// A bound that refuses an object slightly larger than its listing would break
// every ordinary sync, because a journal grows between the LIST and the GET.
// This is the control: an honest peer that appends between the two calls must
// still converge. It passes today and must keep passing after the bound lands.
func TestSec_Bounds_AJournalThatGrewBetweenListAndGetStillConverges(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)

	content := "content that arrives late"
	blob := secjrnBlob(t, be, content)
	ops := []journal.Op{
		secjrnOp(1, "one.md", blob, len(content)),
		secjrnOp(2, "two.md", blob, len(content)),
	}
	secjrnPush(t, be, "grower", ops)

	// The listing under-reports: the object on the wire is bigger than what
	// LIST said, which is what "it grew since we listed" looks like. Get is
	// not intercepted, so the real object comes back whole.
	var served int64
	under := &secbndBackend{
		Backend: be,
		sizes:   map[string]int64{"journal/grower.jsonl": 1}, // LIST under-reports
		served:  &served,                                     // GET is not intercepted
	}
	victim.Backend = under

	if _, err := secpeerCycle(t, victim); err != nil {
		t.Fatalf("honest peer did not converge: %v", err)
	}
	if got := read(t, victim.Folder, "two.md"); got != content {
		t.Fatalf("op past the declared size was dropped: %q", got)
	}
}
