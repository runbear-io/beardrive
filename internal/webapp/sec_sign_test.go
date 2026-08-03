package webapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/remote"
)

// Round 4, row 5 — the presign path, never executed by a security test in
// three rounds because every fixture uses file://, which cannot sign. On a
// production hub (S3/GCS) this branch runs on every single blob a device or a
// browser writes: it hands the caller a URL that writes straight into the
// object store, past every check this package makes.
//
// All helpers here are prefixed secsign.

// ---- a backend that can sign, and records everything it is asked ----

type secsignPut struct {
	Key  string
	Size int64
	TTL  time.Duration
}

// secsignBackend is the fake S3: a real backend for storage, plus PutSigner.
// The URL it mints points at storage.invalid — the test writes the "direct
// upload" through the backend itself, which is exactly what an object store
// does when a presigned PUT arrives.
type secsignBackend struct {
	remote.Backend
	mu     sync.Mutex
	signed []secsignPut
}

func (b *secsignBackend) SignPut(_ context.Context, key string, size int64, ttl time.Duration) (*remote.SignedPut, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.signed = append(b.signed, secsignPut{Key: key, Size: size, TTL: ttl})
	return &remote.SignedPut{
		URL:     "https://storage.invalid/bucket/" + key + "?X-Amz-Signature=fake",
		Method:  http.MethodPut,
		Headers: map[string]string{"Content-Length": strconv.FormatInt(size, 10)},
		Expires: time.Now().Add(ttl),
	}, nil
}

func (b *secsignBackend) calls() []secsignPut {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]secsignPut(nil), b.signed...)
}

// secsignHub is newHub with a storage backend that can presign.
func secsignHub(t *testing.T) (http.Handler, *Server, Project, *secsignBackend) {
	t.Helper()
	var signer *secsignBackend
	srv, p, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
		signer = &secsignBackend{Backend: be}
		return signer
	})
	return srv.Handler(), srv, p, signer
}

// secsignQuota records every byte the hub charges and every byte it books.
type secsignQuota struct {
	mu                sync.Mutex
	checked, recorded int64
}

func (q *secsignQuota) CheckWrite(_ string, n int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.checked += n
	return nil
}
func (q *secsignQuota) CheckSeat(string, int) error { return nil }
func (q *secsignQuota) RecordUsage(_ string, n int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.recorded += n
}
func (q *secsignQuota) totals() (int64, int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.checked, q.recorded
}

func secsignReq(t *testing.T, h http.Handler, method, url string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, url, bytes.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func secsignPlan(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("sign response %d is not JSON: %s", rec.Code, rec.Body)
	}
	return out
}

// ---- the invariants CLAUDE.md states for this path ----

// "journals are never presigned, only immutable blobs". Round 1 asserted the
// response says mode:server — but on a backend that cannot sign, EVERY answer
// is mode:server, so the test could not fail. This one watches the signer.
func TestSec_Sign_JournalKeyIsNeverPresigned(t *testing.T) {
	h, _, p, signer := secsignHub(t)
	for _, tc := range []struct{ name, key, dev string }{
		{"own journal", "journal/dev-aaaaaaaaaaaa.jsonl", "dev-aaaaaaaaaaaa"},
		{"peer journal", "journal/dev-bbbbbbbbbbbb.jsonl", "dev-aaaaaaaaaaaa"},
		{"unnamed device", "journal/dev-cccccccccccc.jsonl", ""},
		{"the hub's own journal", "journal/" + webDevice.ID + ".jsonl", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"key": tc.key, "size": 10})
			hdr := map[string]string{"Content-Type": "application/json"}
			if tc.dev != "" {
				hdr["X-Bdrive-Device"] = tc.dev
			}
			rec := secsignReq(t, h, "POST", "/api/p/"+p.ID+"/store/sign", body, hdr)
			if rec.Code == 200 {
				plan := secsignPlan(t, rec)
				if plan["mode"] != "server" || plan["url"] != nil {
					t.Errorf("journal key got a direct plan: %s", rec.Body)
				}
			}
			for _, c := range signer.calls() {
				if strings.Contains(c.Key, "journal/") {
					t.Errorf("the signer was asked to presign %q", c.Key)
				}
			}
		})
	}
}

// The signed target must stay under <root>/<project-id>/ — that prefix is the
// only wall between two tenants on one storage root.
func TestSec_Sign_SignedTargetStaysUnderTheProjectPrefix(t *testing.T) {
	h, srv, p, signer := secsignHub(t)
	other, _, err := srv.Projects.GetOrCreate("victim", "")
	if err != nil {
		t.Fatal(err)
	}
	sha := shaOf("content")

	// A well-formed key signs, and lands under this project only.
	body, _ := json.Marshal(map[string]any{"key": "blobs/" + sha, "size": 7})
	rec := secsignReq(t, h, "POST", "/api/p/"+p.ID+"/store/sign", body, map[string]string{"Content-Type": "application/json"})
	if rec.Code != 200 {
		t.Fatalf("sign: %d %s", rec.Code, rec.Body)
	}
	calls := signer.calls()
	if len(calls) != 1 || calls[0].Key != p.ID+"/blobs/"+sha {
		t.Fatalf("signed key = %+v, want %s/blobs/%s", calls, p.ID, sha)
	}

	// Every shape that would resolve outside it must be refused before the
	// signer ever sees it.
	for _, key := range []string{
		"../" + other.ID + "/blobs/" + sha,
		"blobs/../../" + other.ID + "/blobs/" + sha,
		"blobs/" + sha + "/../../x",
		"/blobs/" + sha,
		"blobs/" + strings.ToUpper(sha),
		"blobs/" + sha + "%00",
		other.ID + "/blobs/" + sha,
	} {
		body, _ := json.Marshal(map[string]any{"key": key, "size": 7})
		rec := secsignReq(t, h, "POST", "/api/p/"+p.ID+"/store/sign", body, map[string]string{"Content-Type": "application/json"})
		if rec.Code == 200 {
			if plan := secsignPlan(t, rec); plan["url"] != nil {
				t.Errorf("key %q got a signed URL: %s", key, rec.Body)
			}
		}
		for _, c := range signer.calls()[1:] {
			if !strings.HasPrefix(c.Key, p.ID+"/") || strings.Contains(c.Key, "..") {
				t.Errorf("key %q reached the signer as %q", key, c.Key)
			}
		}
	}
}

// The URL must die on the configured TTL, and the expiry the hub reports must
// be the one it asked for — a client that trusts a longer window keeps a stale
// URL alive in its retry queue.
func TestSec_Sign_ExpiryIsTheConfiguredTTL(t *testing.T) {
	h, srv, p, signer := secsignHub(t)
	if srv.Upload.ttl() != DefaultUploadTTL {
		t.Fatalf("default ttl = %v", srv.Upload.ttl())
	}
	body, _ := json.Marshal(map[string]any{"key": "blobs/" + shaOf("x"), "size": 1})
	rec := secsignReq(t, h, "POST", "/api/p/"+p.ID+"/store/sign", body, map[string]string{"Content-Type": "application/json"})
	if rec.Code != 200 {
		t.Fatalf("sign: %d %s", rec.Code, rec.Body)
	}
	calls := signer.calls()
	if len(calls) != 1 || calls[0].TTL != DefaultUploadTTL {
		t.Fatalf("signer ttl = %+v, want %v", calls, DefaultUploadTTL)
	}
	exp, ok := secsignPlan(t, rec)["expires"].(string)
	if !ok {
		t.Fatalf("no expiry in the plan: %s", rec.Body)
	}
	at, err := time.Parse(time.RFC3339, exp)
	if err != nil {
		t.Fatal(err)
	}
	if at.After(time.Now().Add(DefaultUploadTTL + time.Minute)) {
		t.Errorf("reported expiry %v outlives the %v ttl", at, DefaultUploadTTL)
	}
}

// A caller with read but not write must not be handed a write capability.
func TestSec_Sign_ReadOnlyMemberAndOutsiderGetNoSignedURL(t *testing.T) {
	h, srv, c, p := permHub(t)
	signer := &secsignBackend{Backend: srv.Root}
	srv.Root = signer // before any project volume is built
	if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermRead); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"key": "blobs/" + shaOf("x"), "size": 1})
	for _, who := range []string{"bob", "dave"} {
		rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/store/sign", body, c[who])
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s sign: %d %s, want 403", who, rec.Code, rec.Body)
		}
	}
	if got := signer.calls(); len(got) != 0 {
		t.Errorf("the signer was asked anyway: %+v", got)
	}
	// Control: alice, who does have write, is served — so the 403s above are
	// the server's decision, not a broken fixture.
	if rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/store/sign", body, c["alice"]); rec.Code != 200 {
		t.Fatalf("alice sign: %d %s", rec.Code, rec.Body)
	}
}

// ---- what the presign path skips ----

// handleUploadInit refuses a zero size for content that is not the empty blob:
// it is "exactly the lie that buys an unmetered presigned URL". /store/sign,
// the door every DEVICE uses, takes the same lie and mints the URL. Same lie,
// two doors, one refuses.
func TestSec_Sign_DeclaredSizeCannotUnderstateTheContent(t *testing.T) {
	h, _, p, signer := secsignHub(t)
	sha := shaOf(strings.Repeat("A", 4096))
	body, _ := json.Marshal(map[string]any{"path": "big.txt", "sha256": sha, "size": 0, "key": "blobs/" + sha})
	hdr := map[string]string{"Content-Type": "application/json"}

	// The browser door, for reference: refused.
	if rec := secsignReq(t, h, "POST", "/api/p/"+p.ID+"/upload/init", body, hdr); rec.Code != http.StatusForbidden {
		t.Fatalf("upload/init with a zero size: %d %s, want 403", rec.Code, rec.Body)
	}
	// The device door, same lie.
	rec := secsignReq(t, h, "POST", "/api/p/"+p.ID+"/store/sign", body, hdr)
	if rec.Code == 200 {
		if plan := secsignPlan(t, rec); plan["url"] != nil {
			t.Errorf("store/sign minted a URL for 4096 bytes declared as 0: %s", rec.Body)
		}
	}
	for _, c := range signer.calls() {
		if c.Size == 0 {
			t.Errorf("the signer was asked for a 0-byte grant on %q", c.Key)
		}
	}
}

// Round 2 closed "a blob's content must hash to its key" — in handleStorePut,
// the relay path. On a signing hub the bytes never pass through the hub at
// all, so that guard is simply absent: a device names a content address and
// writes different bytes of the same length under it. What comes back out of
// the hub afterwards is content that does not hash to the address it is
// stored at, served to the viewer, to share links and to every peer.
func TestSec_Sign_DirectUploadCannotPoisonAContentAddress(t *testing.T) {
	h, srv, p, signer := secsignHub(t)
	honest, forged := "hello", "world" // same length: a signed Content-Length does not help
	sha := shaOf(honest)
	hdr := map[string]string{"Content-Type": "application/json"}

	// The relay path refuses the swap outright.
	rec := secsignReq(t, h, "PUT", "/api/p/"+p.ID+"/store/object?key=blobs/"+sha, []byte(forged), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("relay put of mismatched content: %d %s, want 400", rec.Code, rec.Body)
	}

	// The presigned path: ask for the URL, then do what an object store does
	// when that PUT arrives — store the body at the signed key, unexamined.
	body, _ := json.Marshal(map[string]any{"key": "blobs/" + sha, "size": len(honest)})
	rec = secsignReq(t, h, "POST", "/api/p/"+p.ID+"/store/sign", body, hdr)
	if rec.Code != 200 || secsignPlan(t, rec)["url"] == nil {
		t.Fatalf("expected a signed URL: %d %s", rec.Code, rec.Body)
	}
	calls := signer.calls()
	if len(calls) != 1 {
		t.Fatalf("signer calls = %+v", calls)
	}
	if err := srv.Root.(*secsignBackend).Backend.Put(
		context.Background(), calls[0].Key, strings.NewReader(forged), int64(len(forged))); err != nil {
		t.Fatal(err)
	}

	// The hub now serves content that does not hash to the key it is under.
	got := secsignReq(t, h, "GET", "/api/p/"+p.ID+"/store/object?key=blobs/"+sha, nil, nil)
	if got.Code == 200 && got.Body.String() != honest {
		t.Errorf("blobs/%s (sha256 of %q) serves %q", sha[:12], honest, got.Body.String())
	}
	if blob := secsignReq(t, h, "GET", "/api/p/"+p.ID+"/blob?sha="+sha, nil, nil); blob.Code == 200 {
		if sum := sha256.Sum256(blob.Body.Bytes()); hex.EncodeToString(sum[:]) != sha {
			t.Errorf("/blob?sha=%s serves content hashing to %x", sha[:12], sum[:6])
		}
	}
}

// QuotaProvider is the plan-enforcement seam, and CheckWrite/RecordUsage are
// documented to fire "on every write path (browser uploads, the device sync
// store proxy)". On a presigning hub the device's bytes never reach the hub,
// and unlike the browser flow there is no commit step to book them afterwards
// — so every blob a device pushes is free. The relay hub, doing the identical
// device-side flow, books them.
func TestSec_Sign_DirectDeviceUploadIsBookedAgainstTheQuota(t *testing.T) {
	content := strings.Repeat("Z", 100_000)
	sha := shaOf(content)
	hdr := map[string]string{"Content-Type": "application/json"}

	// Control: a hub whose storage cannot presign relays the bytes and books
	// them.
	relaySrv, relayP, _ := newHub(t, true, nil)
	relayQ := &secsignQuota{}
	relaySrv.Quota = relayQ
	relayH := relaySrv.Handler()
	if rec := secsignReq(t, relayH, "PUT", "/api/p/"+relayP.ID+"/store/object?key=blobs/"+sha,
		[]byte(content), nil); rec.Code != 200 {
		t.Fatalf("relay put: %d %s", rec.Code, rec.Body)
	}
	if _, booked := relayQ.totals(); booked < int64(len(content)) {
		t.Fatalf("relay hub booked %d of %d bytes", booked, len(content))
	}

	// The same device flow against a hub that can presign.
	var signer *secsignBackend
	srv, p, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
		signer = &secsignBackend{Backend: be}
		return signer
	})
	q := &secsignQuota{}
	srv.Quota = q
	h := srv.Handler()

	body, _ := json.Marshal(map[string]any{"key": "blobs/" + sha, "size": len(content)})
	rec := secsignReq(t, h, "POST", "/api/p/"+p.ID+"/store/sign", body, hdr)
	if rec.Code != 200 || secsignPlan(t, rec)["url"] == nil {
		t.Fatalf("expected a signed URL: %d %s", rec.Code, rec.Body)
	}
	calls := signer.calls()
	if err := signer.Backend.Put(context.Background(), calls[0].Key,
		strings.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	// The device then pushes its journal through the hub, as it always does —
	// the only part of the write the hub still sees.
	op := `{"kind":"put","path":"big.txt","blob":"` + sha + `","size":100000,"device":"dev-aaaaaaaaaaaa","seq":1,"lamport":1}` + "\n"
	if rec := secsignReq(t, h, "PUT", "/api/p/"+p.ID+"/store/object?key=journal/dev-aaaaaaaaaaaa.jsonl",
		[]byte(op), map[string]string{"X-Bdrive-Device": "dev-aaaaaaaaaaaa"}); rec.Code != 200 {
		t.Fatalf("journal push: %d %s", rec.Code, rec.Body)
	}

	if _, booked := q.totals(); booked < int64(len(content)) {
		t.Errorf("signing hub booked %d bytes for a %d-byte blob (relay hub booked it all)",
			booked, len(content))
	}
}

// A blob presigned in one project must never be reachable in another. The
// prefix is what guarantees that, so this is the end-to-end proof that the
// signed key is the one the storage actually gets.
func TestSec_Sign_BlobSignedForOneProjectIsNotVisibleInAnother(t *testing.T) {
	h, srv, p, signer := secsignHub(t)
	other, _, err := srv.Projects.GetOrCreate("other", "")
	if err != nil {
		t.Fatal(err)
	}
	secret := "project A only"
	sha := shaOf(secret)
	body, _ := json.Marshal(map[string]any{"key": "blobs/" + sha, "size": len(secret)})
	rec := secsignReq(t, h, "POST", "/api/p/"+p.ID+"/store/sign", body, map[string]string{"Content-Type": "application/json"})
	if rec.Code != 200 {
		t.Fatalf("sign: %d %s", rec.Code, rec.Body)
	}
	calls := signer.calls()
	if err := signer.Backend.Put(context.Background(), calls[0].Key, strings.NewReader(secret), int64(len(secret))); err != nil {
		t.Fatal(err)
	}
	got := secsignReq(t, h, "GET", "/api/p/"+other.ID+"/store/object?key=blobs/"+sha, nil, nil)
	if got.Code == 200 {
		t.Errorf("project %s reads project %s's presigned blob: %q", other.ID, p.ID, got.Body)
	}
	if ex := secsignReq(t, h, "GET", "/api/p/"+other.ID+"/store/exists?key=blobs/"+sha, nil, nil); ex.Code == 200 {
		var out struct {
			Exists bool `json:"exists"`
		}
		json.Unmarshal(ex.Body.Bytes(), &out)
		if out.Exists {
			t.Errorf("project %s can probe project %s's blob", other.ID, p.ID)
		}
	}
}
