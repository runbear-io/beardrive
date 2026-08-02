package webapp

// Round 7 — the hub surfaces with zero TestSec_* coverage after six rounds:
// the device-code sign-in pair (/api/auth/device/{start,poll}, recorded as
// "token-only coverage, unchanged since round 5") and the ReadRepo BATCH
// layer (PutBatch/DeleteBatch, named as never reached by any test).
//
// Helpers are prefixed sec7.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// /api/auth/device/start
// ---------------------------------------------------------------------------

// TestSec_DeviceFlow_AnAnonymousStrangerCannotAccumulateHubState
//
// POST /api/auth/device/start is one of the four routes authGate declares open
// to everybody (auth.go:81, "/api/auth/"). It is not a lookup: every call
// ALLOCATES, on the hub, a pending grant that lives for ten minutes —
//
//	code := c.newGrant(cliGrant{
//	    kind: "device", device: req.Device, os: req.OS, ip: requestIP(r),
//	}, 10*time.Minute)                                     // authcli.go:340
//
// — and `req.Device` / `req.OS` are whatever the caller put in a body bounded
// only by io.LimitReader(r.Body, 1<<16). Nothing evicts it: `take` and `peek`
// only ever look at the ONE id they are handed, so an id nobody polls is never
// removed. There is no reaper and no cap on the map.
//
// It is also not in the rate limiter's set. rateLimitAuth (ratelimit.go:136)
// throttles POSTs to /auth/login, /auth/signup and /auth/reset per IP,
// "credential endpoints ... to blunt password brute-force and signup floods".
// The device flow is a credential endpoint by construction — it is the half of
// `bdrive login` that mints a device token — and it was left out of that list,
// along with its /poll sibling, which is an unmetered oracle on the same
// grants.
//
// So an unauthenticated stranger converts each request into retained hub
// memory, at ~64 KiB a request, with no throttle to slow the loop down.
//
// The secure behavior asserted: an anonymous caller either gets throttled the
// way every other credential endpoint throttles it, or the state the hub keeps
// on its behalf is bounded. One or the other — not neither.
func TestSec_DeviceFlow_AnAnonymousStrangerCannotAccumulateHubState(t *testing.T) {
	h, srv, _, _ := permHub(t)
	auth, ok := srv.Auth.(*BuiltinAuth)
	if !ok {
		t.Fatalf("permHub built %T, not *BuiltinAuth", srv.Auth)
	}

	// Control: the endpoints the limiter DOES cover refuse an anonymous flood,
	// so the harness is known to reach the limiter at all.
	refusedLogin := 0
	for i := 0; i < 40; i++ {
		rec := sec7Anon(t, h, "POST", "/auth/login", "email=a%40x.io&password=nope",
			"application/x-www-form-urlencoded")
		if rec.Code == http.StatusTooManyRequests {
			refusedLogin++
		}
	}
	if refusedLogin == 0 {
		t.Fatalf("control: 40 anonymous POSTs to /auth/login were never throttled; " +
			"the limiter is not reachable from this fixture")
	}

	// The attack: the same stranger, on the sign-in route nobody metered.
	const requests = 1000
	const nameSize = 16 << 10 // well inside the handler's own 64 KiB body cap
	big := strings.Repeat("A", nameSize)
	refused, accepted := 0, 0
	for i := 0; i < requests; i++ {
		body, _ := json.Marshal(map[string]string{"device": big, "os": big})
		rec := sec7Anon(t, h, "POST", "/api/auth/device/start", string(body), "application/json")
		switch {
		case rec.Code == http.StatusTooManyRequests, rec.Code == http.StatusServiceUnavailable:
			refused++
		case rec.Code == http.StatusOK:
			accepted++
		}
	}
	if refused > 0 {
		return // throttled: the secure outcome
	}

	auth.cli.mu.Lock()
	kept := len(auth.cli.pending)
	var bytes int
	for _, g := range auth.cli.pending {
		bytes += len(g.device) + len(g.os) + len(g.ip)
	}
	auth.cli.mu.Unlock()

	t.Errorf("%d anonymous POSTs to /api/auth/device/start were all accepted (%d) and "+
		"left %d pending grants holding %d bytes of hub memory — no rate limit "+
		"(rateLimitAuth covers /auth/login, /auth/signup and /auth/reset only), "+
		"no cap, and no reaper: take/peek only ever evict the one id they are handed",
		requests, accepted, kept, bytes)
}

// sec7Anon sends a request carrying no credential at all.
func sec7Anon(t *testing.T, h http.Handler, method, url, body, ctype string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", ctype)
	req.RemoteAddr = "203.0.113.9:34567"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// ReadRepo.DeleteBatch
// ---------------------------------------------------------------------------

// TestSec_Ledger_OneUnstorableDeletionCannotWedgeTheLedger
//
// Round 3 closed a hub-wide telemetry kill from the lowest privilege there is:
// one bucket a backend permanently refuses (Postgres rejects a NUL byte in a
// text column) took every other bucket down with it, forever, because the
// ledger persists them in ONE batch and keeps them all dirty on failure
// (TestSec_Reads_OneUnstorableBucketCannotWedgeTheLedger). The fix is the
// per-key retry in persistLocked — and it was applied to the PUT queue only:
//
//	func (l *ReadLedger) persistLocked() error {
//	    if len(l.pendingDel) > 0 {
//	        if err := l.repo.DeleteBatch(l.pendingDel); err != nil {
//	            return err                      // reads.go:341 — no fallback
//	        }
//	        l.pendingDel = nil
//	    }
//	    ...
//	    err := l.repo.PutBatch(batch)           // reads.go:352 — has the fallback
//
// The delete queue is fed by compactLocked from the very same byKey map, so it
// carries exactly the same keys the put queue does. A key the store will never
// accept therefore parks in pendingDel permanently, and because the deletion is
// attempted FIRST and its failure returns early, PutBatch is never reached
// again on that hub. Every subsequent read — every project, every actor — is
// counted in memory and never persisted, and is gone at the next restart.
// Round 3's own comment on the put path names this outcome precisely; the
// delete path was not given the same treatment.
//
// The secure behavior asserted (round 3's, verbatim): whatever the ledger
// cannot store, what it CAN store must still reach disk.
func TestSec_Ledger_OneUnstorableDeletionCannotWedgeTheLedger(t *testing.T) {
	stale := time.Now().UTC().AddDate(0, 0, -90).Format("2006-01-02")

	// A store that permanently refuses one record — the shape Postgres has for
	// a NUL byte, on both halves of the interface.
	repo := &sec7ReadRepo{stored: map[ReadStatKey]ReadStat{}}
	for _, st := range []ReadStat{
		{Project: "p1", Path: "ok.md", Day: stale, Kind: ReadKindHuman, Actor: "a@x.io", Count: 1, Last: time.Now().UTC()},
		{Project: "p1", Path: "pois\x00on.md", Day: stale, Kind: ReadKindAgent, Actor: "dev-1", Count: 1, Last: time.Now().UTC()},
	} {
		repo.stored[st.key()] = st
	}

	// Control: the same ledger with no poisoned row persists what it is given.
	t.Run("control_clean_store", func(t *testing.T) {
		clean := &sec7ReadRepo{stored: map[ReadStatKey]ReadStat{
			{Project: "p1", Path: "ok.md", Day: stale, Kind: ReadKindHuman, Actor: "a@x.io"}: {
				Project: "p1", Path: "ok.md", Day: stale, Kind: ReadKindHuman, Actor: "a@x.io",
				Count: 1, Last: time.Now().UTC()},
		}}
		l, err := NewReadLedger(clean, 30)
		if err != nil {
			t.Fatal(err)
		}
		l.Record("p1", "after.md", ReadKindHuman, "b@x.io")
		if err := l.Close(); err != nil {
			t.Fatalf("control: clean ledger failed to flush: %v", err)
		}
		if !clean.has("p1", "after.md") {
			t.Fatalf("control: a read recorded on a clean ledger never reached the store")
		}
	})

	// NewReadLedger compacts past the horizon immediately: both stale rows fold
	// into all-time rows and both daily keys are queued for deletion.
	l, err := NewReadLedger(repo, 30)
	if err != nil {
		t.Fatal(err)
	}

	// Ordinary reads, from ordinary members of unrelated projects, after the
	// poisoned row is already in the delete queue.
	l.Record("p1", "after.md", ReadKindHuman, "b@x.io")
	l.Record("other-project", "unrelated.md", ReadKindHuman, "c@x.io")
	closeErr := l.Close()

	for _, want := range [][2]string{{"p1", "after.md"}, {"other-project", "unrelated.md"}} {
		if !repo.has(want[0], want[1]) {
			t.Errorf("one record the store refuses to DELETE wedged the whole hub's read "+
				"ledger: %s/%s was never persisted (close reported %v).\n"+
				"DeleteBatch failing returns before PutBatch is even attempted, and the "+
				"key stays in pendingDel forever — the put path got round 3's per-key "+
				"retry, the delete path did not",
				want[0], want[1], closeErr)
		}
	}
}

// sec7ReadRepo is a ReadRepo whose store permanently refuses any record whose
// path carries a NUL — what a Postgres text column does, and the exact input
// round 3's finding used. Both batch methods refuse, so neither half of the
// interface is special-cased.
type sec7ReadRepo struct {
	mu     sync.Mutex
	stored map[ReadStatKey]ReadStat
}

func sec7Unstorable(path string) bool { return strings.ContainsRune(path, 0) }

func (r *sec7ReadRepo) Load() ([]ReadStat, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ReadStat, 0, len(r.stored))
	for _, st := range r.stored {
		out = append(out, st)
	}
	return out, nil
}

func (r *sec7ReadRepo) PutBatch(stats []ReadStat) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, st := range stats {
		if sec7Unstorable(st.Path) {
			return fmt.Errorf("invalid byte 0x00 in path") // whole transaction rolls back
		}
	}
	for _, st := range stats {
		r.stored[st.key()] = st
	}
	return nil
}

func (r *sec7ReadRepo) DeleteBatch(keys []ReadStatKey) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range keys {
		if sec7Unstorable(k.Path) {
			return fmt.Errorf("invalid byte 0x00 in path") // whole transaction rolls back
		}
	}
	for _, k := range keys {
		delete(r.stored, k)
	}
	return nil
}

func (r *sec7ReadRepo) has(project, path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.stored {
		if k.Project == project && k.Path == path {
			return true
		}
	}
	return false
}
