package webapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/runbear-io/beardrive/internal/remote"
)

// Security tests for scoreboard rows 5 (the /store/* sync proxy) and 6
// (uploads). Every test asserts the SECURE behavior, so it goes green when
// the hole is closed and stays as a regression test.

// secstoreDo is do() with request headers — the store proxy identifies the
// calling device through X-Bdrive-Device, which do() cannot set.
func secstoreDo(t *testing.T, h http.Handler, method, url string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, url, bytes.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// secstoreUnsized sends a body of unknown length (chunked transfer encoding),
// which is what any streaming client does. ContentLength is -1.
func secstoreUnsized(t *testing.T, h http.Handler, method, url string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, url, io.NopCloser(bytes.NewReader(body)))
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// secstoreQuota is a plan-limited quota provider: the seam a managed
// deployment plugs in. It refuses writes that would exceed limit.
type secstoreQuota struct {
	mu    sync.Mutex
	limit int64
	used  int64
}

func (q *secstoreQuota) CheckWrite(_ string, n int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.used+n > q.limit {
		return fmt.Errorf("storage quota exceeded")
	}
	return nil
}
func (q *secstoreQuota) CheckSeat(string, int) error { return nil }
func (q *secstoreQuota) RecordUsage(_ string, n int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.used += n
}

// secstoreJournal reads a device's journal straight out of the storage root.
func secstoreJournal(t *testing.T, root, project, device string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, project, "journal", device+".jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(b)
}

// ---- Row 5: the /store/* sync proxy ----

// A device may write only its own journal. That is the repo's core
// concurrency invariant ("each device writes only its own journal") and the
// only reason no locking service is needed: no journal object ever has two
// writers. The proxy must therefore bind the journal key to the calling
// device, not merely to "someone with write permission on the project".
func TestSec_Store_ForeignDeviceJournalWrite(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/store/"
	attacker := map[string]string{"X-Bdrive-Device": "attacker-device", "X-Bdrive-Device-Name": "evil-laptop"}

	// Seed a victim device's journal the way a real peer would.
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.put("victim-device", "salary.md", "confidential")
	victimBefore := secstoreJournal(t, root, p.ID, "victim-device")
	if victimBefore == "" {
		t.Fatal("fixture: victim journal not seeded")
	}

	// Control: the attacker writing its OWN journal is legitimate and works.
	own := []byte(`{"kind":"put","path":"mine.md","seq":1,"lamport":1,"device":"attacker-device"}` + "\n")
	if rec := secstoreDo(t, h, "PUT", base+"object?key=journal/attacker-device.jsonl", own, attacker); rec.Code != 200 {
		t.Fatalf("attacker writing its own journal: %d %s, want 200", rec.Code, rec.Body)
	}

	// Attack: same caller, same credential, another device's journal key.
	forged := []byte(`{"kind":"delete","path":"salary.md","seq":99,"lamport":9999,"device":"victim-device","user":"alice@example.com"}` + "\n")
	rec := secstoreDo(t, h, "PUT", base+"object?key=journal/victim-device.jsonl", forged, attacker)
	if rec.Code != http.StatusForbidden {
		t.Errorf("PUT another device's journal: %d %s, want 403", rec.Code, rec.Body)
	}
	if after := secstoreJournal(t, root, p.ID, "victim-device"); after != victimBefore {
		t.Errorf("victim journal was rewritten by another device:\n before=%q\n after =%q", victimBefore, after)
	}

	// The hub's own journal (browser uploads, restores, removes) is just as
	// writable, so a device can rewrite the server's own history.
	srvKey := "journal/" + webDevice.ID + ".jsonl"
	if rec := secstoreDo(t, h, "PUT", base+"object?key="+srvKey, forged, attacker); rec.Code != http.StatusForbidden {
		t.Errorf("PUT the server's own journal: %d %s, want 403", rec.Code, rec.Body)
	}
}

// Blob keys are content addresses and the whole store treats them as
// immutable: sign/init answer "already there, skip the upload" on a sha
// match, and history serves every old version straight out of blobs/<sha>.
// So the proxy must refuse a blob whose bytes do not hash to its key —
// otherwise one member permanently substitutes the content of every past and
// future version that hashes there.
func TestSec_Store_BlobContentMustMatchItsKey(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/store/"
	key := "blobs/" + shaOf("the real contract")

	// Control: honest content at its own address is accepted.
	if rec := do(t, h, "PUT", base+"object?key="+key, []byte("the real contract")); rec.Code != 200 {
		t.Fatalf("honest blob put: %d %s", rec.Code, rec.Body)
	}
	// Attack: overwrite that address with different bytes.
	rec := do(t, h, "PUT", base+"object?key="+key, []byte("the forged contract"))
	if rec.Code == 200 {
		t.Errorf("PUT blob whose content does not hash to its key: %d, want 4xx", rec.Code)
	}
	got, err := os.ReadFile(filepath.Join(root, p.ID, "blobs", strings.TrimPrefix(key, "blobs/")))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "the real contract" {
		t.Errorf("blob at %s = %q; a content address now serves content that does not hash to it", key, got)
	}
}

// The quota seam (CheckWrite/RecordUsage) is the only thing between a plan
// and the storage bill, and the store proxy is the highest-volume write path
// on the hub. It sizes writes from Content-Length, which the client controls:
// a chunked request declares no length at all.
func TestSec_Store_QuotaHonorsUnsizedPut(t *testing.T) {
	q := &secstoreQuota{limit: 16}
	srv, p, root := newHub(t, true, nil)
	srv.Quota = q
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/store/"
	big := bytes.Repeat([]byte("A"), 4096)
	key := "blobs/" + shaOf(string(big))

	// Control: the same oversized write with a declared length is refused.
	if rec := do(t, h, "PUT", base+"object?key="+key, big); rec.Code != http.StatusForbidden {
		t.Fatalf("oversized sized put: %d %s, want 403", rec.Code, rec.Body)
	}
	// Attack: identical bytes, no declared length.
	rec := secstoreUnsized(t, h, "PUT", base+"object?key="+key, big)
	if rec.Code != http.StatusForbidden {
		t.Errorf("oversized chunked put: %d %s, want 403", rec.Code, rec.Body)
	}
	if fi, err := os.Stat(filepath.Join(root, p.ID, "blobs", strings.TrimPrefix(key, "blobs/"))); err == nil {
		t.Errorf("over-quota blob landed in storage anyway (%d bytes)", fi.Size())
	}
	if q.used != 0 {
		t.Errorf("recorded usage = %d, want 0 (nothing should have been stored)", q.used)
	}
}

// Key escapes TestStoreAPIKeyValidation does not cover: encoded separators,
// backslashes, doubled and absolute separators, a NUL suffix, and a list
// prefix that passes the HasPrefix check but climbs upward. None may reach
// outside <root>/<project-id>/.
func TestSec_Store_KeyEscapesRefused(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	other, _, err := srv.Projects.GetOrCreate("victim-project", "")
	if err != nil {
		t.Fatal(err)
	}
	// Something worth stealing in the other project, plus a hub-level file.
	newFakeRemoteAt(t, filepath.Join(root, other.ID)).put("devx", "secret.md", "top secret")
	if err := os.WriteFile(filepath.Join(root, "hub-secret.txt"), []byte("creds"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/store/"

	bad := []string{
		"blobs%2f..%2f..%2fhub-secret.txt",
		"journal%2F..%2F..%2F" + other.ID + "%2Fjournal%2Fdevx.jsonl",
		`journal\..\..\` + other.ID + `\journal\devx.jsonl`,
		"/blobs/" + shaOf("x"),
		"blobs//" + shaOf("x"),
		"../" + other.ID + "/journal/devx.jsonl",
		"blobs/" + shaOf("x") + "/../../hub-secret.txt",
		"journal/devx.jsonl%00.png",
		"JOURNAL/devx.jsonl",
	}
	for _, key := range bad {
		for _, m := range []string{"GET", "PUT"} {
			rec := do(t, h, m, base+"object?key="+key, []byte("x"))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s %q: %d %s, want 400", m, key, rec.Code, rec.Body)
			}
		}
		if rec := do(t, h, "POST", base+"sign", map[string]any{"key": key, "size": 1}); rec.Code != http.StatusBadRequest {
			t.Errorf("sign %q: %d %s, want 400", key, rec.Code, rec.Body)
		}
	}

	// A list prefix that HasPrefix accepts but climbs out of the project.
	rec := do(t, h, "GET", base+"list?prefix=journal/../../"+other.ID+"/journal/", nil)
	if rec.Code == 200 {
		var list struct {
			Objects []struct {
				Key string `json:"key"`
			} `json:"objects"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		if len(list.Objects) != 0 {
			t.Errorf("traversing list prefix leaked %+v from another project", list.Objects)
		}
	}

	// Reading another project's blob by its sha, through this project.
	if rec := do(t, h, "GET", base+"exists?key=blobs/"+shaOf("top secret"), nil); !strings.Contains(rec.Body.String(), "false") {
		t.Errorf("exists on a foreign project's blob: %d %s, want false", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", base+"object?key=blobs/"+shaOf("top secret"), nil); rec.Code == 200 {
		t.Errorf("read a foreign project's blob by sha: %d %q", rec.Code, rec.Body)
	}
}

// ---- Row 6: uploads ----

// cleanUploadPath is the only validator between a client-supplied
// destination and an op every device materializes into its working folder.
// Two directory names are never sync material: .bdrive (the mount's own
// identity — "syncing it would let one device silently repoint another",
// syncer.go:116) and .git (materializing a hook script runs code on every
// teammate's next commit). The scan side excludes both via syncer.neverSync;
// materialize re-checks only the .bdriveignore filter, so an op that names
// one is written to disk on every device that pulls it.
func TestSec_Upload_ReservedDirsRefused(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/"
	reserved := []string{
		".bdrive/config.json",
		".git/hooks/pre-commit",
		"docs/.bdrive/config.json",
	}
	for _, path := range reserved {
		body := "repointed"
		if rec := do(t, h, "PUT", base+"upload/content?path="+path, []byte(body)); rec.Code != http.StatusBadRequest {
			t.Errorf("upload/content %q: %d %s, want 400", path, rec.Code, rec.Body)
		}
		if rec := do(t, h, "POST", base+"upload/init", initReq(path, body)); rec.Code != http.StatusBadRequest {
			t.Errorf("upload/init %q: %d %s, want 400", path, rec.Code, rec.Body)
		}
		if rec := do(t, h, "POST", base+"upload/commit", initReq(path, body)); rec.Code != http.StatusBadRequest {
			t.Errorf("upload/commit %q: %d %s, want 400", path, rec.Code, rec.Body)
		}
	}
	// Nothing reserved may have reached the hub's journal, where every
	// device would pull and materialize it.
	if j := secstoreJournal(t, root, p.ID, webDevice.ID); strings.Contains(j, ".bdrive") || strings.Contains(j, ".git") {
		t.Errorf("reserved path journaled for every device to materialize: %s", j)
	}
}

// The blob a direct upload targets is named by the client's sha256 and the
// key is built server-side as "blobs/"+sha, so the presigned target must
// never leave the project prefix. And the three steps carry the project in
// the URL, so a swapped project id between steps must not let project B
// claim content that only exists in project A.
func TestSec_Upload_TargetStaysInProject(t *testing.T) {
	srv, a, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
		return &signingBackend{Backend: be}
	})
	b, _, err := srv.Projects.GetOrCreate("project-b", "")
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	content := "project A's private content"

	// A sha that tries to climb out of blobs/ or into another project.
	for _, sha := range []string{
		"../../" + b.ID + "/blobs/" + shaOf(content),
		"../journal/devx.jsonl",
		strings.ToUpper(shaOf(content)),
	} {
		body := map[string]any{"path": "x.txt", "sha256": sha, "size": 1}
		if rec := do(t, h, "POST", "/api/p/"+a.ID+"/upload/init", body); rec.Code != http.StatusBadRequest {
			t.Errorf("init sha %q: %d %s, want 400", sha, rec.Code, rec.Body)
		}
	}

	// Swapped project id between steps: content stored in A, committed in B.
	if rec := do(t, h, "PUT", "/api/p/"+a.ID+"/upload/content?path=secret.md", []byte(content)); rec.Code != 200 {
		t.Fatalf("upload into project A: %d %s", rec.Code, rec.Body)
	}
	rec := do(t, h, "POST", "/api/p/"+b.ID+"/upload/commit", initReq("stolen.md", content))
	if rec.Code == 200 {
		t.Errorf("project B committed a blob that only exists in project A: %d %s", rec.Code, rec.Body)
	}
}

// upload/init and upload/commit take the byte count from the client's own
// JSON and hand it straight to the quota provider, as both the check and the
// recorded usage. In direct mode the server never sees the bytes, so commit
// is the only accounting point there is — and a client that declares 0
// stores whatever it likes for free.
func TestSec_Upload_QuotaUsesRealSize(t *testing.T) {
	q := &secstoreQuota{limit: 16}
	srv, p, root := newHub(t, true, func(be remote.Backend) remote.Backend {
		return &signingBackend{Backend: be} // storage that presigns, like S3
	})
	srv.Quota = q
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/"
	big := strings.Repeat("A", 4096)

	// Control: an honest declaration is refused, and so is the relayed body.
	if rec := do(t, h, "POST", base+"upload/init", initReq("big.txt", big)); rec.Code != http.StatusForbidden {
		t.Fatalf("honest oversized init: %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := do(t, h, "PUT", base+"upload/content?path=big.txt", []byte(big)); rec.Code != http.StatusForbidden {
		t.Fatalf("oversized upload/content: %d %s, want 403", rec.Code, rec.Body)
	}

	// Attack: same content, declared as 0 bytes. init hands out a presigned
	// URL for it despite the quota.
	lie := map[string]any{"path": "big.txt", "sha256": shaOf(big), "size": 0}
	if rec := do(t, h, "POST", base+"upload/init", lie); rec.Code != http.StatusForbidden {
		t.Errorf("init with an understated size: %d %s, want 403", rec.Code, rec.Body)
	}
	// The client PUTs straight to the object store through that URL; the
	// server never sees the bytes. That is what lands in storage:
	if err := os.MkdirAll(filepath.Join(root, p.ID, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, p.ID, "blobs", shaOf(big)), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := do(t, h, "POST", base+"upload/commit", lie); rec.Code != http.StatusForbidden {
		t.Errorf("commit with an understated size: %d %s, want 403", rec.Code, rec.Body)
	}
	if j := secstoreJournal(t, root, p.ID, webDevice.ID); strings.Contains(j, "big.txt") {
		t.Errorf("over-quota file joined the volume: %s", j)
	}
}
