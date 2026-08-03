package webapp

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/templates"
)

// Round 10, row 22's hub half — internal/webapp/templates.go's seedTemplate.
//
// Round 9's sweep had no internal/webapp slot, so a reverted guard in this
// function had nowhere to land and the row's 33% score was internal/templates
// alone. This file sweeps the hub half: each of seedTemplate's four guards
// (cleanUploadPath, the quota CheckWrite, the already-exists skip, the
// RecordUsage) was reverted in turn and the whole TestSec suite re-run.
//
//	cleanUploadPath removed  → CAUGHT (TestSec_Seed_TemplateSeedingUsesThe
//	                           SameGuardAsEveryOtherWriteDoor fails). Round 6's
//	                           fix is genuinely pinned.
//	CheckWrite removed       → suite still green: NOTHING pins it.
//	RecordUsage removed      → suite still green: NOTHING pins it.
//	existing[clean] skip     → suite still green: NOTHING pins it.
//
// All helpers here are prefixed sec10.

// sec10CapQuota is a QuotaProvider with a real ceiling — the only kind that
// can answer "would this write have been refused". Same shape as
// secaudCapQuota; a separate copy so this file's arithmetic is readable at the
// assertion.
type sec10CapQuota struct {
	// Embedded so the read-side hooks (CheckRead/RecordEgress) come for
	// free: this fake exercises the write path, and a widened interface
	// should not need a no-op added here every time.
	UnlimitedQuota

	mu       sync.Mutex
	cap      int64
	recorded int64
	checks   []int64
}

func (q *sec10CapQuota) CheckWrite(_ string, n int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.checks = append(q.checks, n)
	if q.recorded+n > q.cap {
		return fmt.Errorf("storage limit reached")
	}
	return nil
}
func (q *sec10CapQuota) CheckSeat(string, int) error { return nil }
func (q *sec10CapQuota) RecordUsage(_ string, n int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.recorded += n
}
func (q *sec10CapQuota) asked() []int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]int64(nil), q.checks...)
}

// sec10TemplateBytes is the byte total seedTemplate computes for a template —
// the number the test has to size the cap against.
func sec10TemplateBytes(t *testing.T, name string) int64 {
	t.Helper()
	tpl, err := templates.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, f := range tpl.Files {
		total += int64(len(f.Content))
	}
	if total == 0 {
		t.Fatalf("template %q has no content: this test cannot size a cap", name)
	}
	return total
}

// TestSec_Seed_TemplateSeedingCountsOutstandingStorageReservations
//
// reserve.go states the invariant in its own package comment:
//
//	"it counts against the cap the moment it is granted — reservedBytes is
//	 added to every CheckWrite, so the allowance cannot be oversubscribed"
//
// Every write door obeys it: upload.go:380 and :445, store.go:257 and :391 all
// pass `size + s.reservedBytes(org)`. seedTemplate (templates.go:34) passes
// bare `total`. So an org that has already reserved its entire allowance with
// an outstanding presigned grant still gets a whole template written into it,
// through a door that never looks at the reservation.
//
// The delta this asserts: with the same org in the same state, the upload door
// refuses these bytes and the seeding door must refuse them too.
func TestSec_Seed_TemplateSeedingCountsOutstandingStorageReservations(t *testing.T) {
	const tplName = "para"
	tplBytes := sec10TemplateBytes(t, tplName)

	srv, p, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
		return &signingBackend{Backend: be}
	})
	// Cap: the reservation alone fits, the reservation plus one template does
	// not. Nothing has been recorded yet, so only reservedBytes can make the
	// difference — which is exactly the guard under test.
	reserved := int64(10_000)
	q := &sec10CapQuota{cap: reserved + tplBytes - 1}
	srv.Quota = q
	h := srv.Handler()

	// 1. Alice reserves the org's allowance with a presigned grant she never
	//    completes. This is an ordinary browser upload; the hub books it as
	//    outstanding by design.
	body := map[string]any{"path": "big.bin", "sha256": shaOf("big"), "size": reserved}
	if rec := do(t, h, "POST", "/api/p/"+p.ID+"/upload/init", body); rec.Code != 200 {
		t.Fatalf("upload/init for the reservation: %d %s", rec.Code, rec.Body)
	}
	if got := srv.reservedBytes(""); got != reserved {
		t.Fatalf("hub holds %d outstanding bytes, want %d — the premise of this test is gone", got, reserved)
	}

	// 2. Control: the SAME bytes through the upload door are refused, because
	//    that door adds the reservation.
	ctl := map[string]any{"path": "more.bin", "sha256": shaOf("more"), "size": tplBytes}
	ctlRec := do(t, h, "POST", "/api/p/"+p.ID+"/upload/init", ctl)
	if ctlRec.Code == 200 {
		t.Fatalf("upload door accepted %d bytes over a full reservation (%d) — premise gone",
			tplBytes, reserved)
	}

	// 3. Attack: the same bytes through the seeding door.
	rec := do(t, h, "POST", "/api/projects", map[string]any{"name": "seeded", "template": tplName})
	if rec.Code == 200 {
		var out struct {
			Project Project `json:"project"`
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		t.Errorf("seedTemplate wrote %d bytes into project %s while the org's allowance was fully reserved;\n"+
			"the upload door refused the identical byte count with %d %q.\nCheckWrite was asked about %v (never about %d = total+reserved)",
			tplBytes, out.Project.ID, ctlRec.Code, strings.TrimSpace(ctlRec.Body.String()),
			q.asked(), tplBytes+reserved)
	}
}

// TestSec_Seed_TemplateSeedingBooksWhatItWrote
//
// The other half of the same accounting: seeding must RecordUsage what it
// journaled, or the next CheckWrite is answered against a ledger that has
// forgotten the template. Removing seedTemplate's RecordUsage leaves the whole
// TestSec suite green today, so this is the test that pins it: seed until the
// cap is reached, and the door has to close.
//
// This one passes on the current tree — it is the regression test that keeps
// the guard from being deleted, not an exploit.
func TestSec_Seed_TemplateSeedingBooksWhatItWrote(t *testing.T) {
	const tplName = "para"
	tplBytes := sec10TemplateBytes(t, tplName)

	srv, _, _ := newHub(t, true, nil)
	// Room for two templates and not a byte more.
	q := &sec10CapQuota{cap: 2*tplBytes + 1}
	srv.Quota = q
	h := srv.Handler()

	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("seeded-%d", i)
		rec := do(t, h, "POST", "/api/projects", map[string]any{"name": name, "template": tplName})
		switch {
		case i <= 2 && rec.Code != 200:
			t.Fatalf("create #%d refused below the cap: %d %s", i, rec.Code, rec.Body)
		case i == 3 && rec.Code == 200:
			t.Errorf("create #%d seeded %d more bytes into an org capped at %d;\n"+
				"CheckWrite was asked about %v, so the first two templates were never booked",
				i, tplBytes, q.cap, q.asked())
		}
	}
}
