package webapp

// Round 6 — the target is the 247 green TestSec_* tests themselves.
//
// The loop's stopping condition ("every row clean or fixed, and two dry
// rounds") is only meaningful if a green test would go red when its fix is
// removed. Each test below was written because a sabotage run — revert the
// fix in a scratch copy, re-run the suite — showed that the fix it guards can
// be deleted with the whole suite still green.
//
// Every test asserts the SECURE behaviour, so it goes green the moment the
// gap is closed and stays as a permanent regression test. Helpers are
// prefixed secaud; no existing file is touched.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/remote"
)

// ---- helpers -------------------------------------------------------------

// secaudOpLine is secfx4OpLine plus the one field that file omits: "device".
// That omission is why TestSec_Journal_AnOwnerlessLegacyRowDoesNotDisableThe
// AccountBinding passes — journalNames() refuses the push because no op names
// the device, so OwnerOf's answer is never the reason for the 403.
func secaudOpLine(seq int, dev, kind, path, blob string) string {
	b, _ := json.Marshal(map[string]any{
		"seq": seq, "lamport": seq, "time": time.Now().UTC().Format(time.RFC3339Nano),
		"kind": kind, "path": path, "blob": blob, "size": 1, "device": dev,
	})
	return string(b) + "\n"
}

// secaudCapQuota is a QuotaProvider with a real ceiling — the only kind that
// can show whether an outstanding presigned grant counts against it.
type secaudCapQuota struct {
	// Embedded so the read-side hooks (CheckRead/RecordEgress) come for
	// free: this fake exercises the write path, and a widened interface
	// should not need a no-op added here every time.
	UnlimitedQuota

	mu       sync.Mutex
	cap      int64
	recorded int64
	checks   []int64
}

func (q *secaudCapQuota) CheckWrite(_ string, n int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.checks = append(q.checks, n)
	if q.recorded+n > q.cap {
		return fmt.Errorf("storage limit reached")
	}
	return nil
}
func (q *secaudCapQuota) CheckSeat(string, int) error { return nil }
func (q *secaudCapQuota) RecordUsage(_ string, n int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.recorded += n
}

// secaudSpyBackend records every key it is asked for and serves nothing. It
// isolates one guard: whether the caller refused the key BEFORE storage was
// ever consulted. Downstream guards (remote.Prefixed's safeKey, localBackend's
// UnderRoot) cannot mask the answer, because this backend has neither.
type secaudSpyBackend struct {
	mu   sync.Mutex
	gets []string
}

func (b *secaudSpyBackend) Get(_ context.Context, key string) (io.ReadCloser, error) {
	b.mu.Lock()
	b.gets = append(b.gets, key)
	b.mu.Unlock()
	return nil, errors.New("not found")
}
func (b *secaudSpyBackend) Put(context.Context, string, io.Reader, int64) error {
	return errors.New("read-only")
}
func (b *secaudSpyBackend) Exists(context.Context, string) (bool, error) { return false, nil }
func (b *secaudSpyBackend) Close() error                                 { return nil }
func (b *secaudSpyBackend) List(context.Context, string) ([]remote.Object, error) {
	return nil, nil
}
func (b *secaudSpyBackend) asked() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.gets...)
}

// ---------------------------------------------------------------------------
// 1. The fixture defect itself, as a test.
// ---------------------------------------------------------------------------

// permHub leaves srv.Devices nil, so ownJournal's account binding returns
// before it can decide anything and the hub falls back to the key/header
// match round 4 showed is satisfiable by construction. Every permHub-based
// test that pushes a journal therefore proves org/project PERMISSION and
// never DEVICE OWNERSHIP — the CISO named this after round 5 and chose not to
// change the fixture.
//
// This is the property stated as a test rather than as a paragraph: through
// the fixture the suite actually uses, a member must not be able to replace a
// teammate's journal object. cmd/bdrive/web.go always sets Devices in hub
// mode, so this is what a served hub does; the fixture is what disagrees.
//
// Fix: wire a DeviceRegistry into permHub (secfx4Registry already builds one
// in three lines) so the dozen tests that push journals through it measure
// the binding as well as the permission.
func TestSec_Audit_PermHubRefusesAForeignJournalOutOfTheBox(t *testing.T) {
	h, _, c, p := permHub(t)

	const aliceDev = "alice-laptop-6f2a"
	// Alice syncs first: on a served hub this is the claim that binds the id
	// to her account.
	if rec := secfx4Store(t, h, "GET", "/api/p/"+p.ID+"/store/list", "", c["alice"], aliceDev); rec.Code != 200 {
		t.Fatalf("control: alice's own sync: %d %s", rec.Code, rec.Body)
	}
	body := secaudOpLine(1, aliceDev, "put", "quarterly-plan.md", strings.Repeat("a", 64))
	if rec := secfx4Store(t, h, "PUT",
		"/api/p/"+p.ID+"/store/object?key=journal/"+aliceDev+".jsonl", body, c["alice"], aliceDev); rec.Code != 200 {
		t.Fatalf("control: alice writes her own journal: %d %s", rec.Code, rec.Body)
	}

	// Bob is an ordinary member of the same org. Same request, wrong account.
	forged := secaudOpLine(2, aliceDev, "delete", "quarterly-plan.md", "")
	rec := secfx4Store(t, h, "PUT",
		"/api/p/"+p.ID+"/store/object?key=journal/"+aliceDev+".jsonl", forged, c["bob"], aliceDev)
	if rec.Code != http.StatusForbidden {
		t.Errorf("bob replaced journal/%s.jsonl through permHub: %d %s\n"+
			"permHub builds a Server with Devices == nil, so ownJournal returns before the account\n"+
			"binding runs and only the key<->header match applies — which the same request supplies\n"+
			"both halves of. Every permHub test that pushes a journal is measuring permission only.",
			aliceDev, rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}

// ---------------------------------------------------------------------------
// 2. Replacement: the ownerless legacy row.
// ---------------------------------------------------------------------------

// TestSec_Journal_AnOwnerlessLegacyRowDoesNotDisableTheAccountBinding survives
// its own fix being removed. Its push carries ops built by secfx4OpLine, which
// never sets "device", so journalNames(dev, ops) is false and ownJournal
// refuses on the first-claim rule — whatever OwnerOf answered. Make OwnerOf
// report a pre-accounts row as unclaimed again (the exact round-5 hole: the
// binding switched off for every device an upgraded hub already had) and that
// test stays green.
//
// This is the same attack with the one field filled in, so the refusal can
// only come from OwnerOf reporting the id as claimed.
func TestSec_Audit_OwnerlessLegacyRowStillClaimsTheDeviceId(t *testing.T) {
	h, srv, c, p := permHub(t)

	const aliceDev = "alice-laptop-legacy"
	// devices.json exactly as a release before accounts wrote it: an id, a
	// name, an OS, no user and no first_seen.
	path := filepath.Join(t.TempDir(), "devices.json")
	legacy := `{"devices":[{"id":"` + aliceDev + `","name":"Alice's MacBook","os":"darwin","last_seen":"2026-01-02T03:04:05Z"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := OpenDeviceRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	srv.Devices = reg

	// Control: the request shape is valid and reaches the handler — bob may
	// write a journal for an id that is unambiguously his own.
	const bobDev = "bob-desktop-11cc"
	// A device is registered when its machine signs in, so bob's does.
	secRegisterDevice(t, h, p.ID, c["bob"], bobDev, "bob-desktop", "linux")
	mine := secaudOpLine(1, bobDev, "put", "bobs-notes.md", strings.Repeat("b", 64))
	if rec := secfx4Store(t, h, "PUT",
		"/api/p/"+p.ID+"/store/object?key=journal/"+bobDev+".jsonl", mine, c["bob"], bobDev); rec.Code != 200 {
		t.Fatalf("control: bob writes his own journal: %d %s", rec.Code, rec.Body)
	}

	// The attack: same account, same route, alice's legacy id — and this time
	// every op names that device, so the first-claim rule has no objection and
	// only OwnerOf can refuse.
	forged := secaudOpLine(1, aliceDev, "delete", "quarterly-plan.md", "")
	rec := secfx4Store(t, h, "PUT",
		"/api/p/"+p.ID+"/store/object?key=journal/"+aliceDev+".jsonl", forged, c["bob"], aliceDev)
	if rec.Code != http.StatusForbidden {
		t.Errorf("bob replaced journal/%s.jsonl on a hub upgraded from before device rows had owners: %d %s\n"+
			"a row with no user must still mark the id as KNOWN, or the id reads as unclaimed and the\n"+
			"first-claim rule hands it to whoever names it in their own ops",
			aliceDev, rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}

// ---------------------------------------------------------------------------
// 3. Replacement: an outstanding presigned grant counts against the cap.
// ---------------------------------------------------------------------------

// reserve.go's stated contract has two halves. The second — "charged when the
// bytes arrive" — is covered by TestSec_Sign_DirectDeviceUploadIsBooked
// AgainstTheQuota and TestSec_Browser_PresignedGrantIsBookedEvenWithoutACommit,
// both of which go red when reconcileGrants is neutered. The first —
// "reservedBytes is added to every CheckWrite, so concurrent grants cannot
// oversubscribe an allowance none of them exceeds" — is covered by nothing:
// make reservedBytes return 0 and all 247 tests stay green.
//
// A presigned URL is a write the hub has already authorized and cannot recall.
// N concurrent grants that each fit under the cap must not be granted when
// their sum does not.
func TestSec_Audit_OutstandingPresignedGrantsCountAgainstTheCap(t *testing.T) {
	h, srv, p, _ := secsignHub(t)
	q := &secaudCapQuota{cap: 1000}
	srv.Quota = q

	sign := func(sha string, size int64) (int, map[string]any) {
		body, _ := json.Marshal(map[string]any{"key": "blobs/" + sha, "size": size})
		rec := secfx4Store(t, h, "POST", "/api/p/"+p.ID+"/store/sign", string(body), nil, "")
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// Control: one grant of 600 against a 1000-byte allowance is legal, and
	// the hub really does presign it (mode "direct" with a URL).
	code, out := sign(strings.Repeat("1", 64), 600)
	if code != 200 || out["url"] == nil {
		t.Fatalf("control: first grant of 600/1000: %d %v", code, out)
	}

	// Nothing has been uploaded and nothing charged, but 600 bytes of write
	// have been authorized and the URL is live for the whole TTL. A second
	// 600-byte grant would put 1200 bytes into a 1000-byte allowance.
	code, out = sign(strings.Repeat("2", 64), 600)
	if code != http.StatusForbidden {
		t.Errorf("a second 600-byte grant against a 1000-byte cap: %d %v\n"+
			"the cap must be checked against this write PLUS everything already granted and not yet\n"+
			"accounted for; without reservedBytes a caller mints unlimited concurrent presigned URLs\n"+
			"and blows past the plan before any of them commits", code, out)
	}
	if out["url"] != nil {
		t.Errorf("the oversubscribing grant was signed anyway: %v", out["url"])
	}
}

// ---------------------------------------------------------------------------
// 4. Replacement: cleanUploadPath refuses control characters.
// ---------------------------------------------------------------------------

// Row 6 claims this as fixed in round 5 and names no test, and there is none:
// let cleanUploadPath accept control characters again and the suite stays
// green. It is the guard that keeps row 14's known-open Postgres divergence
// unreachable through the API — a NUL-named file reaching the journal makes a
// share on it 500 on Postgres and round-trip differently on sqlite.
func TestSec_Audit_UploadPathRefusesControlCharacters(t *testing.T) {
	// Control: an ordinary path is accepted and comes back unchanged, so a
	// refusal below is about the character and not about the shape.
	for _, ok := range []string{"notes.md", "wiki/q3/plan.md", "a b.md", "héllo.md"} {
		got, err := cleanUploadPath(ok)
		if err != nil || got != ok {
			t.Fatalf("control: cleanUploadPath(%q) = %q, %v; want it accepted unchanged", ok, got, err)
		}
	}
	for _, bad := range []struct{ name, path string }{
		{"NUL", "notes\x00.md"},
		{"newline", "notes\n.md"},
		{"carriage return", "notes\r.md"},
		{"tab", "notes\t.md"},
		{"ESC", "notes\x1b[2J.md"},
		{"DEL", "notes\x7f.md"},
		{"NUL in a directory", "wiki\x00/plan.md"},
		{"bell", "notes\a.md"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			if got, err := cleanUploadPath(bad.path); err == nil {
				t.Errorf("cleanUploadPath(%q) accepted it as %q; a control character must be refused "+
					"at ingest — it is not a filename anybody types and the metadata backends "+
					"disagree about whether it survives a round trip", bad.path, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 5. Replacement: Op.Blob is refused before it becomes a storage key.
// ---------------------------------------------------------------------------

// Row 11's TestSec_Path_ViewerBlobEscapesProjectPrefix and …MemberReadsAnother
// OrgsBlob (round 2) survive blobRe being deleted from OpenBlob: since round 4
// the escape is also caught downstream by remote.Prefixed's safeKey, so those
// tests now measure the round-4 guard and say nothing about the round-2 one.
// Defence in depth is right; a test that silently changes which layer it
// measures is not, because the layer it no longer measures can be removed by a
// refactor with the suite still green.
//
// This asserts the round-2 guard on its own: a backend with no containment of
// its own must never be asked for a key OpenBlob should have refused.
func TestSec_Audit_OpBlobIsRefusedBeforeItReachesStorage(t *testing.T) {
	be := &secaudSpyBackend{}
	rs := &RemoteSource{Backend: be}

	// Control: a well-formed sha IS passed through, so a "never asked" result
	// below is about the guard and not about OpenBlob being inert.
	good := strings.Repeat("ab", 32)
	if _, err := rs.OpenBlob(context.Background(), good); err == nil {
		t.Fatal("control: the spy backend must fail the fetch")
	}
	if asked := be.asked(); len(asked) != 1 || asked[0] != "blobs/"+good {
		t.Fatalf("control: a valid sha was not fetched as blobs/<sha>: %v", asked)
	}

	for _, bad := range []string{
		"../../etc/passwd",
		"../victim/journal/dev.jsonl",
		"/etc/passwd",
		"..",
		"a", // shorter than the sha slice every caller assumes
		strings.Repeat("ab", 32) + "/../../secret",
		"ABCDEF" + strings.Repeat("0", 58), // uppercase: not the hex we mint
		"",
	} {
		t.Run(bad, func(t *testing.T) {
			before := len(be.asked())
			if _, err := rs.OpenBlob(context.Background(), bad); err == nil {
				t.Errorf("OpenBlob(%q) returned no error", bad)
			}
			for _, k := range be.asked()[before:] {
				t.Errorf("OpenBlob(%q) asked storage for %q; Op.Blob is a string a peer chose, "+
					"and it must be refused here rather than relied on being refused by whatever "+
					"backend happens to be underneath", bad, k)
			}
		})
	}
}
