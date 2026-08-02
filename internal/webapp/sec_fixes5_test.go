package webapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Round 6 — attacking round 5's own fixes on the hub: DeviceRegistry.OwnerOf
// (row 5) and the brand-new quota reservation ledger, webapp/reserve.go
// (rows 5 and 6), which has never been attacked.
//
// All helpers here are prefixed secfx5.

// ---------------------------------------------------------------------------
// OwnerOf went hub-wide. Round 3 closed a device-existence oracle; check it
// did not reopen through the write gate.
// ---------------------------------------------------------------------------

// secfx5Sign posts one /store/sign request, optionally naming a device.
func secfx5Sign(t *testing.T, h http.Handler, project, key string, size int64, dev string, c *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"key": key, "size": size})
	req := httptest.NewRequest("POST", "/api/p/"+project+"/store/sign", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if dev != "" {
		req.Header.Set("X-Bdrive-Device", dev)
	}
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestSec_Journal_OwnershipIsNotAHubWideDeviceExistenceOracle.
//
// Round 4 bound a journal key to its owning account through the project's ORG.
// Round 5 replaced that with DeviceRegistry.OwnerOf, which is deliberately
// HUB-WIDE — "so offboarding a teammate does not release her journal to the
// org she left". That is the right ownership answer, but ownJournal turns the
// answer into a response code on a route any org member may call, and it is
// the only lookup on the hub that is not scoped to the caller's org.
//
// So a plain member of org A asks about a device id belonging to an account in
// org B — a completely separate tenant, whose projects, members and devices he
// cannot see through any other surface — and the status code answers.
//
// This is the class round 3 closed for History
// (TestSec_Journal_IsNotAnExistenceOracle) and round 4 re-closed for the
// registry join (TestSec_Devices_HistoryFallbackDoesNotDistinguishUnknownFromDenied).
// /store/sign reopened it: a journal key is never presigned, so the answer for
// a journal is always "come through the server" — the ownership question does
// not have to be asked here at all, and asking it is what leaks.
func TestSec_Journal_OwnershipIsNotAHubWideDeviceExistenceOracle(t *testing.T) {
	h, srv, c, p := permHub(t)
	secfx4Registry(t, srv)

	// dave is in a different org. He makes his own project and syncs a device
	// through it — the ordinary way any device becomes known to the hub.
	rec := doAs(t, h, "POST", "/api/projects", map[string]string{"name": "daves-wiki"}, c["dave"])
	if rec.Code != 200 {
		t.Fatalf("dave create project: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Project Project `json:"project"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Project.Org == p.Org {
		t.Fatalf("fixture wrong: dave's project is in alice's org")
	}
	const daveDev = "dave-laptop-9f21"
	if rec := secfx4Store(t, h, "GET", "/api/p/"+out.Project.ID+"/store/list", "", c["dave"], daveDev); rec.Code != 200 {
		t.Fatalf("dave's device sync: %d %s", rec.Code, rec.Body)
	}
	if owner, known := srv.Devices.OwnerOf(daveDev); !known || owner != "dave@x.io" {
		t.Fatalf("fixture wrong: OwnerOf(%s) = %q,%v, want dave@x.io", daveDev, owner, known)
	}

	// bob is a plain member of alice's org with write on her project. He has
	// no way to learn anything about dave's org through any other route.
	const neverSeen = "no-such-device-0001"
	probeKnown := secfx5Sign(t, h, p.ID, "journal/"+daveDev+".jsonl", 0, daveDev, c["bob"])
	probeUnknown := secfx5Sign(t, h, p.ID, "journal/"+neverSeen+".jsonl", 0, neverSeen, c["bob"])

	// Control: the probe is a request bob is allowed to make at all.
	if probeUnknown.Code == http.StatusForbidden {
		t.Fatalf("control failed: bob may not sign in this project at all: %d %s",
			probeUnknown.Code, probeUnknown.Body)
	}
	if probeKnown.Code != probeUnknown.Code {
		t.Errorf("a device id owned by another ORG answers %d %q while an id nothing has ever "+
			"synced under answers %d %q — one /store/sign from a plain member is a hub-wide "+
			"device-existence oracle across the org wall",
			probeKnown.Code, strings.TrimSpace(probeKnown.Body.String()),
			probeUnknown.Code, strings.TrimSpace(probeUnknown.Body.String()))
	}
}

// ---------------------------------------------------------------------------
// reserve.go — the reservation ledger, new in round 5, never attacked.
// ---------------------------------------------------------------------------

// secfx5Cap is a QuotaProvider with a real byte cap, so CheckWrite can say no.
type secfx5Cap struct {
	mu       sync.Mutex
	limit    int64
	used     int64
	recorded int64
}

func (q *secfx5Cap) CheckWrite(_ string, n int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.used+n > q.limit {
		return fmt.Errorf("storage limit reached")
	}
	return nil
}
func (q *secfx5Cap) CheckSeat(string, int) error { return nil }
func (q *secfx5Cap) RecordUsage(_ string, n int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.used += n
	q.recorded += n
}
func (q *secfx5Cap) totals() (int64, int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.used, q.recorded
}

func secfx5Sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestSec_Reserve_ConcurrentGrantsCannotOversubscribeTheCap is reserve.go's
// own stated property, quoted from handleStoreSign:
//
//	"The cap is checked against this write PLUS everything already granted and
//	 not yet accounted for, so concurrent grants cannot oversubscribe an
//	 allowance that no single one of them exceeds."
//
// It is a check-then-act: reservedBytes() takes the lock, returns, releases
// it, CheckWrite runs, the backend is asked and the URL is minted, and only
// THEN reserve() takes the lock again. Nothing holds the ledger across the
// decision, so N simultaneous grants all read the same "already granted"
// total — zero — and all pass. This is round 2's
// TestSec_Invite_SeatCheckIsAtomic on the new ledger.
func TestSec_Reserve_ConcurrentGrantsCannotOversubscribeTheCap(t *testing.T) {
	h, srv, p, _ := secsignHub(t)
	const cap0 = 1000
	const each = 600
	const n = 16
	q := &secfx5Cap{limit: cap0}
	srv.Quota = q

	// Sequentially, the second grant is correctly refused: 600 + 600 > 1000.
	seq := secfx5Sign(t, h, p.ID, "blobs/"+secfx5Sha("seq-a"), each, "", nil)
	if seq.Code != 200 || secsignPlan(t, seq)["url"] == nil {
		t.Fatalf("control failed: the first grant was not signed: %d %s", seq.Code, seq.Body)
	}
	seq2 := secfx5Sign(t, h, p.ID, "blobs/"+secfx5Sha("seq-b"), each, "", nil)
	if seq2.Code != http.StatusForbidden {
		t.Fatalf("control failed: a second sequential grant over the cap answered %d %s — "+
			"the reservation is not being counted at all", seq2.Code, seq2.Body)
	}

	// Same hub, same cap, same sizes — issued at the same moment instead.
	// A few trials: the window is short, and a correctly serialized ledger
	// grants exactly one in every trial however many run.
	for trial := 0; trial < 6; trial++ {
		h2, srv2, p2, _ := secsignHub(t)
		q2 := &secfx5Cap{limit: cap0}
		srv2.Quota = q2
		var wg sync.WaitGroup
		start := make(chan struct{})
		granted := make([]bool, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				rec := secfx5Sign(t, h2, p2.ID,
					"blobs/"+secfx5Sha(fmt.Sprintf("par-%d-%d", trial, i)), each, "", nil)
				if rec.Code == 200 && strings.Contains(rec.Body.String(), `"url"`) {
					granted[i] = true
				}
			}(i)
		}
		close(start)
		wg.Wait()

		var ok int
		for _, g := range granted {
			if g {
				ok++
			}
		}
		if int64(ok)*each > cap0 {
			t.Fatalf("trial %d: %d of %d simultaneous grants were signed (%d bytes) against a %d "+
				"byte cap; sequentially only %d bytes fit. reservedBytes() and reserve() are two "+
				"separate lock acquisitions with CheckWrite, Exists and SignPut in between, so "+
				"every concurrent caller reads the same zero reservation",
				trial, ok, n, int64(ok)*each, int64(cap0), int64(cap0/each*each))
		}
		if u, _ := q2.totals(); u > cap0 {
			t.Fatalf("trial %d: recorded usage %d already exceeds the cap %d", trial, u, cap0)
		}
	}
}

// TestSec_Reserve_ReconcileReadsTheLedgerUnderItsLock.
//
// reconcileGrants decides whether to charge a grant like this:
//
//	s.resMu.Lock()
//	before := len(s.grants)
//	s.dropLocked(...)
//	s.resMu.Unlock()
//	if before != len(s.grants) { ... RecordUsage ... }
//
// The second len(s.grants) is read AFTER the mutex is released. That is an
// unsynchronised read of a slice another goroutine appends to (reserve) and
// reslices (dropLocked) — a data race on the hub's billing ledger, on the code
// path that runs on GET /store/list, i.e. the first call of every sync cycle
// of every device. Its functional consequence is a silent under-charge: a
// concurrent reserve() that restores the length makes the comparison equal, so
// the arrived bytes this call just dropped are never billed to anyone.
//
// Run with -race.
func TestSec_Reserve_ReconcileReadsTheLedgerUnderItsLock(t *testing.T) {
	// Control: with no concurrency at all, an arrived grant is charged exactly
	// once, so the fixture (keys, prefix, storage) is right.
	{
		_, ctrl, cp, cbe := secsignHub(t)
		cq := &secsignQuota{}
		ctrl.Quota = cq
		key := "blobs/" + secfx5Sha("solo")
		if err := cbe.Put(context.Background(), key, strings.NewReader("x"), 1); err != nil {
			t.Fatal(err)
		}
		ctrl.reserve(cp.ID, "", key, 1, time.Hour)
		ctrl.reconcileGrants(context.Background(), cp.ID, cbe)
		ctrl.reconcileGrants(context.Background(), cp.ID, cbe)
		if _, rec := cq.totals(); rec != 1 {
			t.Fatalf("control failed: a single arrived grant booked %d bytes, want 1", rec)
		}
	}

	// The race is a window of a few instructions, so the observation is
	// repeated. Under -race the detector reports it on the first trial and
	// fails the binary outright; without -race this loop is what makes the
	// mis-billing visible. A correctly locked reconciler bills exactly n in
	// every trial, however many trials run.
	const trials = 6
	const n = 60
	for trial := 0; trial < trials; trial++ {
		_, srv, p, signer := secsignHub(t)
		q := &secsignQuota{}
		srv.Quota = q
		for i := 0; i < n; i++ {
			key := "blobs/" + secfx5Sha(fmt.Sprintf("arrived-%d", i))
			if err := signer.Put(context.Background(), key, strings.NewReader("x"), 1); err != nil {
				t.Fatal(err)
			}
			srv.reserve(p.ID, "", key, 1, time.Hour)
		}

		var wg sync.WaitGroup
		stop := make(chan struct{})
		// Devices syncing: every cycle starts with a list, which reconciles.
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 40; j++ {
					srv.reconcileGrants(context.Background(), p.ID, signer)
				}
			}()
		}
		// Another member minting grants at the same time — each one taken back
		// straight away, so the ledger stays small and only its LENGTH churns.
		var minter sync.WaitGroup
		minter.Add(1)
		go func() {
			defer minter.Done()
			key := "blobs/" + secfx5Sha("in-flight-elsewhere")
			for {
				select {
				case <-stop:
					return
				default:
				}
				srv.reserve(p.ID, "", key, 0, time.Hour)
				srv.claimGrant(p.ID, key)
			}
		}()
		wg.Wait()
		close(stop)
		minter.Wait()

		if _, recorded := q.totals(); recorded != n {
			t.Fatalf("trial %d: %d bytes charged for %d one-byte grants that all arrived. "+
				"reconcileGrants compares the ledger's length AFTER releasing the lock, so a "+
				"concurrent reserve() or claimGrant() in that window makes the same arrival "+
				"bill twice or not at all — and the read itself is a data race (-race)",
				trial, recorded, n)
		}
	}
}

// TestSec_Reserve_ArrivedBytesAreChargedEvenIfNothingAsksBeforeExpiry.
//
// reserve.go states the contract: a grant "is CHARGED only when the bytes are
// actually in storage" and "RELEASED for free when it expires with nothing
// there". dropExpiredLocked implements only the second half — it drops on
// expiry without ever asking storage whether the object arrived. A member
// uploads through the presigned URL and then simply makes no further request
// to the hub until the TTL passes; the grant is forgotten, and because
// reconciliation is driven off the grant list and nothing else, those bytes
// are never charged to the org by anything, ever.
//
// This is round 4's TestSec_Sign_DirectDeviceUploadIsBookedAgainstTheQuota
// with one added wait: the bytes are in storage, the hub is asked, and no
// usage is booked.
func TestSec_Reserve_ArrivedBytesAreChargedEvenIfNothingAsksBeforeExpiry(t *testing.T) {
	h, srv, p, signer := secsignHub(t)
	srv.Upload.TTL = 150 * time.Millisecond
	q := &secsignQuota{}
	srv.Quota = q

	// Control: the identical sequence with no wait bills the bytes, so the
	// fixture (prefix, key, storage) is right and only the delay differs.
	{
		ch, csrv, cp, cbe := secsignHub(t)
		cq := &secsignQuota{}
		csrv.Quota = cq
		body := strings.Repeat("Y", 4096)
		sha := secfx5Sha(body)
		if rec := secfx5Sign(t, ch, cp.ID, "blobs/"+sha, int64(len(body)), "", nil); rec.Code != 200 {
			t.Fatalf("control sign: %d %s", rec.Code, rec.Body)
		}
		if err := cbe.Put(context.Background(), cp.ID+"/blobs/"+sha, strings.NewReader(body), int64(len(body))); err != nil {
			t.Fatal(err)
		}
		if rec := doAs(t, ch, "GET", "/api/p/"+cp.ID+"/store/list", nil, nil); rec.Code != 200 {
			t.Fatalf("control list: %d %s", rec.Code, rec.Body)
		}
		if _, recorded := cq.totals(); recorded != int64(len(body)) {
			t.Fatalf("control failed: a promptly reconciled grant booked %d bytes, want %d",
				recorded, len(body))
		}
	}

	content := strings.Repeat("Z", 4096)
	sha := secfx5Sha(content)
	rec := secfx5Sign(t, h, p.ID, "blobs/"+sha, int64(len(content)), "", nil)
	if rec.Code != 200 || secsignPlan(t, rec)["url"] == nil {
		t.Fatalf("control failed: no signed URL: %d %s", rec.Code, rec.Body)
	}
	// The direct upload: bytes go straight to the object store, past the hub.
	if err := signer.Put(context.Background(), p.ID+"/blobs/"+sha, strings.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	// The uploader goes quiet for longer than the URL's lifetime, which is all
	// it takes: nothing else on the hub remembers the grant.
	time.Sleep(300 * time.Millisecond)

	// Now an ordinary sync cycle starts — the hub's stated reconciliation
	// point, with the project's storage in hand.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/store/list", nil, nil); rec.Code != 200 {
		t.Fatalf("store list: %d %s", rec.Code, rec.Body)
	}
	if _, recorded := q.totals(); recorded != int64(len(content)) {
		t.Errorf("RecordUsage booked %d bytes for a %d byte object that IS in storage — "+
			"dropExpiredLocked releases a grant on expiry without asking whether the object "+
			"arrived, and nothing else ever looks, so these bytes are free forever",
			recorded, len(content))
	}
}
