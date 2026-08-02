package webapp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/remote"
)

// Round 5, scoreboard row 6 — the BROWSER's presigned upload, the gap the
// board names outright: "The browser's presigned commit flow
// (SignBlobPut→BlobSize→Commit) is still untested — round 4 attacked the
// device door only."
//
// The two doors share no guard. /store/sign is one handler that signs and
// books in the same breath; the browser flow is three round trips —
// POST /upload/init signs, the browser PUTs straight into the object store,
// POST /upload/commit reads the size back out of storage and journals it.
// Everything between step 1 and step 3 happens where the hub cannot see it,
// and step 3 decides what the volume contains from a key the caller names.
//
// Helpers are prefixed secbrw. secsignHub/secsignBackend/secsignQuota/
// secsignReq/secsignPlan (sec_sign_test.go, round 4) and newHub/permHub/doAs/
// shaOf are reused.

var secbrwJSON = map[string]string{"Content-Type": "application/json"}

// secbrwUpload is one browser upload request body — the same shape
// /upload/init and /upload/commit both take.
func secbrwUpload(p, content string) []byte {
	body, _ := json.Marshal(map[string]any{"path": p, "sha256": shaOf(content), "size": len(content)})
	return body
}

// secbrwInit runs POST /upload/init and returns the storage key the signer was
// asked to sign — i.e. exactly where the browser's PUT will land.
func secbrwInit(t *testing.T, h http.Handler, signer *secsignBackend, project, path, content string) string {
	t.Helper()
	before := len(signer.calls())
	rec := secsignReq(t, h, "POST", "/api/p/"+project+"/upload/init", secbrwUpload(path, content), secbrwJSON)
	if rec.Code != 200 {
		t.Fatalf("upload/init: %d %s", rec.Code, rec.Body)
	}
	if plan := secsignPlan(t, rec); plan["url"] == nil {
		t.Fatalf("upload/init did not hand out a direct URL: %s", rec.Body)
	}
	calls := signer.calls()
	if len(calls) != before+1 {
		t.Fatalf("signer calls %d → %d", before, len(calls))
	}
	return calls[len(calls)-1].Key
}

// secbrwPutSigned does what an object store does when a presigned PUT arrives:
// store the body at the signed key, unexamined. The hub never sees these bytes.
func secbrwPutSigned(t *testing.T, signer *secsignBackend, key, body string) {
	t.Helper()
	if err := signer.Backend.Put(context.Background(), key, strings.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
}

// secbrwGet reads a path back through every viewer route that serves content.
func secbrwGet(t *testing.T, h http.Handler, project, path, sha string) map[string]*httptest.ResponseRecorder {
	t.Helper()
	return map[string]*httptest.ResponseRecorder{
		"file":     secsignReq(t, h, "GET", "/api/p/"+project+"/file?path="+path, nil, nil),
		"download": secsignReq(t, h, "GET", "/api/p/"+project+"/download?path="+path, nil, nil),
		"blob":     secsignReq(t, h, "GET", "/api/p/"+project+"/blob?sha="+sha, nil, nil),
	}
}

// ---- content addressing across the three-step flow ----

// The device door's version of this is round 4's
// TestSec_Sign_DirectUploadCannotPoisonAContentAddress, closed by verifying a
// blob's bytes on READ. The browser door reaches the same content through the
// same RemoteSource, so this asserts the fix actually covers it end to end:
// init one sha, PUT different bytes of the same length, commit, and read.
func TestSec_Browser_PresignedCommitCannotPoisonAContentAddress(t *testing.T) {
	h, _, p, signer := secsignHub(t)
	const honest, forged = "hello", "world" // same length: a signed Content-Length does not help
	sha := shaOf(honest)

	key := secbrwInit(t, h, signer, p.ID, "notes/x.md", honest)
	if key != p.ID+"/blobs/"+sha {
		t.Fatalf("signed key = %q, want %s/blobs/%s", key, p.ID, sha)
	}
	secbrwPutSigned(t, signer, key, forged)

	// commit reads the size back out of storage; it never sees the content.
	rec := secsignReq(t, h, "POST", "/api/p/"+p.ID+"/upload/commit", secbrwUpload("notes/x.md", honest), secbrwJSON)
	if rec.Code != 200 {
		t.Logf("commit refused the poisoned blob outright: %d %s", rec.Code, rec.Body)
	}
	for name, got := range secbrwGet(t, h, p.ID, "notes/x.md", sha) {
		if got.Code == 200 && got.Body.String() == forged {
			t.Errorf("%s serves %q for a path whose content address is sha256(%q)", name, forged, honest)
		}
	}
}

// The presigned URL is a write capability that lives for its whole TTL, and
// nothing about a PUT is one-shot. So the interesting order is honest first:
// upload real content, commit it, let the hub read it once — and only then
// replay the URL you were handed at step 1 with different bytes.
//
// Round 4's guard is a per-process verified-set (RemoteSource.verify): a blob
// is checked the first time it is read and never again, because blobs are
// immutable. A live signed URL is exactly the thing that makes them not
// immutable, so the check runs before the swap and never after it. Every
// later reader — viewer, share link, history, and every device pulling the
// blob through /store/object — is served content that does not hash to the
// address the journal names, for as long as the hub process lives.
func TestSec_Browser_ReplayedSignedURLCannotRewriteAVerifiedBlob(t *testing.T) {
	h, _, p, signer := secsignHub(t)
	const honest, forged = "quarterly numbers: 41", "quarterly numbers: 99"
	sha := shaOf(honest)

	key := secbrwInit(t, h, signer, p.ID, "notes/report.md", honest)
	secbrwPutSigned(t, signer, key, honest)
	if rec := secsignReq(t, h, "POST", "/api/p/"+p.ID+"/upload/commit",
		secbrwUpload("notes/report.md", honest), secbrwJSON); rec.Code != 200 {
		t.Fatalf("commit: %d %s", rec.Code, rec.Body)
	}
	// A reader looks at the file. This is the only thing the attack needs the
	// victim to do, and it is the normal use of the product.
	if got := secsignReq(t, h, "GET", "/api/p/"+p.ID+"/file?path=notes/report.md", nil, nil); got.Body.String() != honest {
		t.Fatalf("first read = %q, want %q", got.Body, honest)
	}

	// The URL from step 1 is still inside its TTL. Replay it.
	secbrwPutSigned(t, signer, key, forged)

	for name, got := range secbrwGet(t, h, p.ID, "notes/report.md", sha) {
		if got.Code == 200 && got.Body.String() == forged {
			t.Errorf("%s serves the replayed content %q under sha256(%q)", name, forged, honest)
		}
	}
	// And the device path, which every teammate replicates through.
	if got := secsignReq(t, h, "GET", "/api/p/"+p.ID+"/store/object?key=blobs/"+sha, nil, nil); got.Code == 200 &&
		got.Body.String() == forged {
		t.Errorf("/store/object hands every syncing device %q under sha256(%q)", forged, honest)
	}
}

// ---- the quota, the other half of what round 4 fixed on the device door ----

// /store/sign books the declared size at signing time, "here or never",
// because the bytes go straight to storage and /store/* has no commit step.
// The browser flow has a commit step — but nothing makes the browser reach it.
// init hands out a live write grant, the object store accepts the bytes, and
// if commit is never called the hub has stored content it charged nobody for.
// Blobs are retained forever, so those bytes are permanent.
func TestSec_Browser_PresignedGrantIsBookedEvenWithoutACommit(t *testing.T) {
	content := strings.Repeat("Z", 250_000)

	// Control: the device door on an identical hub books the same write.
	var devSigner *secsignBackend
	devSrv, devP, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
		devSigner = &secsignBackend{Backend: be}
		return devSigner
	})
	devQ := &secsignQuota{}
	devSrv.Quota = devQ
	devH := devSrv.Handler()
	body, _ := json.Marshal(map[string]any{"key": "blobs/" + shaOf(content), "size": len(content)})
	if rec := secsignReq(t, devH, "POST", "/api/p/"+devP.ID+"/store/sign", body, secbrwJSON); rec.Code != 200 {
		t.Fatalf("store/sign: %d %s", rec.Code, rec.Body)
	}
	secbrwPutSigned(t, devSigner, devSigner.calls()[0].Key, content)
	// A presigned upload never passes through the hub, so the hub learns the
	// bytes arrived the next time it talks to this project's storage — that is
	// where a grant is confirmed and charged (reserve.go). One ordinary
	// request, and emphatically not a commit, which is what this test is about.
	secsignReq(t, devH, "GET", "/api/p/"+devP.ID+"/store/list?prefix=blobs/", nil, nil)
	if _, booked := devQ.totals(); booked < int64(len(content)) {
		t.Fatalf("the device door booked %d of %d bytes — fixture is wrong", booked, len(content))
	}

	// The browser door, same hub shape, same bytes, no commit.
	var signer *secsignBackend
	srv, p, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
		signer = &secsignBackend{Backend: be}
		return signer
	})
	q := &secsignQuota{}
	srv.Quota = q
	h := srv.Handler()

	key := secbrwInit(t, h, signer, p.ID, "big.bin", content)
	secbrwPutSigned(t, signer, key, content)
	secsignReq(t, h, "GET", "/api/p/"+p.ID+"/store/list?prefix=blobs/", nil, nil)
	if _, booked := q.totals(); booked < int64(len(content)) {
		t.Errorf("the browser door booked %d bytes for a %d-byte direct upload it authorized (the device door booked it all)",
			booked, len(content))
	}

	// Committing must not then charge for the same bytes a second time.
	if rec := secsignReq(t, h, "POST", "/api/p/"+p.ID+"/upload/commit",
		secbrwUpload("big.bin", content), secbrwJSON); rec.Code != 200 {
		t.Fatalf("commit: %d %s", rec.Code, rec.Body)
	}
	if _, booked := q.totals(); booked > 2*int64(len(content)) {
		t.Errorf("init+commit booked %d bytes for one %d-byte upload", booked, len(content))
	}
}

// ---- what commit will agree to journal ----

// commit names a content address and a path. Neither is proof the caller ever
// uploaded anything: the only question the handler asks is "is there an object
// at blobs/<sha> in this project". Every shape below is a way to answer yes
// without having written the bytes here.
func TestSec_Browser_CommitCannotClaimContentItNeverUploaded(t *testing.T) {
	h, srv, p, signer := secsignHub(t)
	other, _, err := srv.Projects.GetOrCreate("victim", "")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a sha nobody uploaded", func(t *testing.T) {
		rec := secsignReq(t, h, "POST", "/api/p/"+p.ID+"/upload/commit",
			secbrwUpload("ghost.md", "never uploaded"), secbrwJSON)
		if rec.Code == 200 {
			t.Errorf("commit journaled an op for content that is not in the store: %s", rec.Body)
		}
	})

	t.Run("a blob that only exists in another project", func(t *testing.T) {
		const secret = "the other project's private content"
		key := secbrwInit(t, h, signer, other.ID, "secret.md", secret)
		secbrwPutSigned(t, signer, key, secret)
		if rec := secsignReq(t, h, "POST", "/api/p/"+other.ID+"/upload/commit",
			secbrwUpload("secret.md", secret), secbrwJSON); rec.Code != 200 {
			t.Fatalf("commit in the owning project: %d %s", rec.Code, rec.Body)
		}
		// Same sha, different project: the prefix is the only wall.
		rec := secsignReq(t, h, "POST", "/api/p/"+p.ID+"/upload/commit",
			secbrwUpload("stolen.md", secret), secbrwJSON)
		if rec.Code == 200 {
			t.Errorf("commit adopted another project's blob: %s", rec.Body)
		}
		if got := secsignReq(t, h, "GET", "/api/p/"+p.ID+"/file?path=stolen.md", nil, nil); got.Code == 200 {
			t.Errorf("the other project's content is now readable here: %q", got.Body)
		}
	})

	t.Run("init here, commit there", func(t *testing.T) {
		const content = "content signed for one project"
		secbrwInit(t, h, signer, p.ID, "a.md", content) // signed, never uploaded
		rec := secsignReq(t, h, "POST", "/api/p/"+other.ID+"/upload/commit",
			secbrwUpload("a.md", content), secbrwJSON)
		if rec.Code == 200 {
			t.Errorf("a grant minted for %s committed in %s: %s", p.ID, other.ID, rec.Body)
		}
	})

	t.Run("a path that differs from the one init validated", func(t *testing.T) {
		const content = "retargeted"
		key := secbrwInit(t, h, signer, p.ID, "ok.md", content)
		secbrwPutSigned(t, signer, key, content)
		for _, bad := range []string{
			"../escape.md", "/etc/passwd", "notes/../../escape.md",
			".bdrive/config.json", ".git/hooks/pre-commit", "a/./b.md", "",
		} {
			body, _ := json.Marshal(map[string]any{"path": bad, "sha256": shaOf(content), "size": len(content)})
			if rec := secsignReq(t, h, "POST", "/api/p/"+p.ID+"/upload/commit", body, secbrwJSON); rec.Code == 200 {
				t.Errorf("commit accepted path %q: %s", bad, rec.Body)
			}
		}
	})
}

// The object can also stop existing between the two calls — a lifecycle rule,
// a manual cleanup, a failed multipart. commit must then journal nothing: an
// op whose blob is missing is the one thing the blobs-before-journal invariant
// exists to prevent, and every peer that pulls it stalls on that path.
func TestSec_Browser_CommitAfterTheObjectVanishesJournalsNothing(t *testing.T) {
	var signer *secsignBackend
	srv, p, root := newHub(t, true, func(be remote.Backend) remote.Backend {
		signer = &secsignBackend{Backend: be}
		return signer
	})
	h := srv.Handler()
	const content = "here, then gone"

	key := secbrwInit(t, h, signer, p.ID, "gone.md", content)
	secbrwPutSigned(t, signer, key, content)
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(key))); err != nil {
		t.Fatal(err)
	}
	rec := secsignReq(t, h, "POST", "/api/p/"+p.ID+"/upload/commit", secbrwUpload("gone.md", content), secbrwJSON)
	if rec.Code == 200 {
		t.Errorf("commit journaled an op for a blob that is no longer in storage: %s", rec.Body)
	}
	if got := secsignReq(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, nil); strings.Contains(got.Body.String(), "gone.md") {
		t.Errorf("the volume lists a file with no content: %s", got.Body)
	}
}

// A grant is not a permission: the three steps are three requests, and the
// permission the route requires is re-decided on each one. A member demoted to
// read between init and commit must not be able to finish the write.
func TestSec_Browser_CommitIsRefusedAfterWritePermissionIsLost(t *testing.T) {
	h, srv, c, p := permHub(t)
	signer := &secsignBackend{Backend: srv.Root}
	srv.Root = signer // before any project volume is built
	const content = "written while still allowed"

	// bob, a full member, gets a grant and uploads.
	rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/upload/init", secbrwUpload("bob.md", content), c["bob"])
	if rec.Code != 200 {
		t.Fatalf("bob init: %d %s", rec.Code, rec.Body)
	}
	calls := signer.calls()
	if len(calls) != 1 {
		t.Fatalf("signer calls = %+v", calls)
	}
	secbrwPutSigned(t, signer, calls[0].Key, content)

	// Offboarding happens: bob is now read-only.
	if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermRead); err != nil {
		t.Fatal(err)
	}
	if rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/upload/commit", secbrwUpload("bob.md", content), c["bob"]); rec.Code != http.StatusForbidden {
		t.Errorf("demoted bob commit: %d %s, want 403", rec.Code, rec.Body)
	}
	// dave is in another org entirely.
	if rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/upload/commit", secbrwUpload("bob.md", content), c["dave"]); rec.Code == 200 {
		t.Errorf("outsider dave committed: %s", rec.Body)
	}
	if rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/upload/init", secbrwUpload("dave.md", content), c["dave"]); rec.Code == 200 {
		t.Errorf("outsider dave was handed a grant: %s", rec.Body)
	}
	// Control: alice, who still has write, is served — so the refusals above
	// are the server's decision, not a broken fixture.
	if rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/upload/commit", secbrwUpload("bob.md", content), c["alice"]); rec.Code != 200 {
		t.Fatalf("alice commit: %d %s", rec.Code, rec.Body)
	}
}

// ---- what commit writes into the journal ----

// appendOp derives the hub's own Lamport from every op it can see —
// maxLamport+1 over all journals, including the ones members push. int64
// addition wraps, so one op carrying math.MaxInt64 makes the hub's next
// lamport math.MinInt64: every browser upload from then on sorts BELOW that
// op and loses last-writer-wins on the path it names, forever, on the hub and
// on every device that replays the same journals.
//
// The secure behavior is the one the API already promises: an upload the hub
// accepted and answered 200 to is what the volume then serves.
func TestSec_Browser_CommittedUploadIsWhatTheVolumeServes(t *testing.T) {
	h, _, p, signer := secsignHub(t)
	const planted, honest = "the attacker's version", "the browser's version"

	// A member pushes their own journal — the one write every member may make
	// — carrying a ceiling lamport for the path they want to own.
	blob := shaOf(planted)
	if rec := secsignReq(t, h, "PUT", "/api/p/"+p.ID+"/store/object?key=blobs/"+blob,
		[]byte(planted), nil); rec.Code != 200 {
		t.Fatalf("blob push: %d %s", rec.Code, rec.Body)
	}
	op := fmt.Sprintf(`{"kind":"put","path":"notes/x.md","blob":%q,"size":%d,"mode":420,`+
		`"device":"dev-attacker01","seq":1,"lamport":%d,"time":"2026-01-01T00:00:00Z"}`+"\n",
		blob, len(planted), int64(math.MaxInt64))
	if rec := secsignReq(t, h, "PUT", "/api/p/"+p.ID+"/store/object?key=journal/dev-attacker01.jsonl",
		[]byte(op), map[string]string{"X-Bdrive-Device": "dev-attacker01"}); rec.Code != 200 {
		t.Fatalf("journal push: %d %s", rec.Code, rec.Body)
	}

	// Now an ordinary browser upload of the same path.
	key := secbrwInit(t, h, signer, p.ID, "notes/x.md", honest)
	secbrwPutSigned(t, signer, key, honest)
	if rec := secsignReq(t, h, "POST", "/api/p/"+p.ID+"/upload/commit",
		secbrwUpload("notes/x.md", honest), secbrwJSON); rec.Code != 200 {
		t.Fatalf("commit: %d %s", rec.Code, rec.Body)
	}

	got := secsignReq(t, h, "GET", "/api/p/"+p.ID+"/file?path=notes/x.md", nil, nil)
	if got.Body.String() != honest {
		t.Errorf("the hub accepted the upload and serves %q instead", got.Body)
	}
}

// ---- the two validators flagged in Suspicions and never tested ----

// handleStoreList's prefix check reads as a traversal check and is not one: it
// only asks that the string START with blobs/ or journal/, which "blobs/../.."
// does. What stops it is remote.Prefixed (round 4). Assert that from the
// outside, because the day someone hands List a raw backend the reading of
// this validator is what they will trust.
func TestSec_Browser_StoreListPrefixCannotLeaveTheProject(t *testing.T) {
	h, srv, p, _ := secsignHub(t)
	other, _, err := srv.Projects.GetOrCreate("victim", "")
	if err != nil {
		t.Fatal(err)
	}
	// Something to find on the other side of the wall.
	secret := "the other project's blob"
	if rec := secsignReq(t, h, "PUT", "/api/p/"+other.ID+"/store/object?key=blobs/"+shaOf(secret),
		[]byte(secret), nil); rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}

	for _, prefix := range []string{
		"blobs/../..", "blobs/../../" + other.ID + "/blobs", "journal/../../" + other.ID,
		"blobs/./../../", "journal/..", "blobs//../" + other.ID, "/", "..", "../",
	} {
		rec := secsignReq(t, h, "GET", "/api/p/"+p.ID+"/store/list?prefix="+prefix, nil, nil)
		if rec.Code != 200 {
			continue // refused outright: fine
		}
		var out struct {
			Objects []remote.Object `json:"objects"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("prefix %q: %v", prefix, err)
		}
		for _, o := range out.Objects {
			if !strings.HasPrefix(o.Key, "blobs/") && !strings.HasPrefix(o.Key, "journal/") {
				t.Errorf("prefix %q listed %q, which is not in this project's key space", prefix, o.Key)
			}
			if strings.Contains(o.Key, other.ID) || strings.Contains(o.Key, "..") {
				t.Errorf("prefix %q listed %q", prefix, o.Key)
			}
		}
	}
}

// journalKeyRe accepts [A-Za-z0-9._-]+ with no length cap; validDeviceID caps
// the same character class at 64. So a member can sync a journal under an id
// that can never be registered as a device — and ownership, the round-4 fix,
// is resolved through the registry. No row can ever exist for that id, so
// LookupIn always misses and EVERY member may rewrite that journal object.
// The hub then echoes the un-registrable string to every project member as the
// device that made each change.
func TestSec_Browser_JournalKeyMustNameARegistrableDevice(t *testing.T) {
	h, srv, c, p := permHub(t)
	// permHub leaves Devices nil; ownership is resolved through the registry,
	// so a hub without one cannot exercise the fix at all.
	devs, err := OpenDeviceRegistry(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Devices = devs
	long := strings.Repeat("d", 200)

	// Control: an ordinary device id. bob syncs under it, and carol — an equal
	// member of the same org — cannot take it over. This is round 4's fix.
	push := func(who, dev, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/api/p/"+p.ID+"/store/object?key=journal/"+dev+".jsonl",
			strings.NewReader(body))
		req.Header.Set("X-Bdrive-Device", dev)
		req.AddCookie(c[who])
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	const line = `{"kind":"put","path":"a.md","blob":"` +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		`","size":1,"device":"%s","seq":1,"lamport":1}` + "\n"

	if rec := push("bob", "dev-bob12345678", fmt.Sprintf(line, "dev-bob12345678")); rec.Code != 200 {
		t.Fatalf("bob push: %d %s", rec.Code, rec.Body)
	}
	if rec := push("carol", "dev-bob12345678", fmt.Sprintf(line, "dev-bob12345678")); rec.Code != http.StatusForbidden {
		t.Fatalf("carol overwriting bob's journal: %d %s, want 403 — fixture is wrong", rec.Code, rec.Body)
	}

	// The same sequence with an id one character over the registrable limit.
	if rec := push("bob", long, fmt.Sprintf(line, long)); rec.Code == 200 {
		if again := push("carol", long, fmt.Sprintf(line, long)); again.Code == 200 {
			t.Errorf("carol rewrote bob's journal at journal/<%d chars>.jsonl: no device row can ever exist for it, so ownership can never be established",
				len(long))
		}
	}
	// And whatever the hub stores, it must not hand a 200-character device id
	// to every member as an attribution.
	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/history?prefix=", nil, c["carol"])
	if rec.Code == 200 && strings.Contains(rec.Body.String(), long) {
		t.Errorf("history reports a device id of %d characters that could never be registered", len(long))
	}
}
