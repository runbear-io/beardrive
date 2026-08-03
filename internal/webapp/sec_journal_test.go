package webapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Scoreboard row 11, the hub half. Round 2 audited exactly one field of an
// incoming journal op — Blob — and left the rest of the record entering the
// model unchecked. These tests take the remaining fields to the surfaces the
// hub builds out of a journal: the viewer snapshot (RemoteSource.Files/Open)
// and the History feed.
//
// Helpers are prefixed secjrn* so they cannot collide with another agent's
// file in this package.

// secjrnHub is permHub plus a device registry — the piece production always
// has (cmd/bdrive/web.go wires one) and the permission fixture omits, so the
// journal→registry join is reachable.
func secjrnHub(t *testing.T) (http.Handler, *Server, map[string]*http.Cookie, Project) {
	t.Helper()
	h, srv, c, p := permHub(t)
	reg, err := OpenDeviceRegistry(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Devices = reg
	return h, srv, c, p
}

// secjrnSync makes one store request as a real syncing device would: with the
// X-Bdrive-Device headers that populate the hub's device registry.
func secjrnSync(t *testing.T, h http.Handler, project, dev, name, os string, c *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	// A device becomes known by pushing its own journal — a blob put says
	// nothing about who a device is and no longer claims an id.
	req := httptest.NewRequest("PUT",
		"/api/p/"+project+"/store/object?key=journal/"+dev+".jsonl", nil)
	req.Header.Set("X-Bdrive-Device", dev)
	req.Header.Set("X-Bdrive-Device-Name", name)
	req.Header.Set("X-Bdrive-Os", os)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("device %s sync: %d %s", dev, rec.Code, rec.Body)
	}
	return rec
}

// secjrnPushJournal writes a whole journal (one op per element) for device dev
// into project id, through the public store API. The device header matches the
// key, so the round-1 own-journal rule is satisfied — this is a legitimate
// push whose CONTENT is hostile.
func secjrnPushJournal(t *testing.T, h http.Handler, id, dev string, ops []map[string]any, c *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var b strings.Builder
	for _, op := range ops {
		line, err := json.Marshal(op)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	req := httptest.NewRequest("PUT",
		"/api/p/"+id+"/store/object?key=journal/"+dev+".jsonl",
		strings.NewReader(b.String()))
	req.Header.Set("X-Bdrive-Device", dev)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// secjrnOp builds a put op with every field a device sets, so a test can
// override just the one it is attacking.
func secjrnOp(seq int64, path, blob string, size int) map[string]any {
	return map[string]any{
		"seq": seq, "lamport": seq,
		"time":   time.Now().UTC().Format(time.RFC3339Nano),
		"device": "alice-desk", "device_name": "Alice Desktop",
		"kind": "put", "path": path, "blob": blob, "size": size,
	}
}

type secjrnHistory struct {
	Entries []HistoryEntry `json:"entries"`
}

func secjrnFetchHistory(t *testing.T, h http.Handler, id string, c *http.Cookie) secjrnHistory {
	t.Helper()
	rec := doAs(t, h, "GET", "/api/p/"+id+"/history", nil, c)
	if rec.Code != 200 {
		t.Fatalf("history: %d %s", rec.Code, rec.Body)
	}
	var out secjrnHistory
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// ---- Op.Device: the registry join History performs ----

// handleHistory resolves every op's Device field against the hub-wide device
// registry (s.Devices.Get(op.Device)) and reports the row's name and OS. The
// Device field inside an op is arbitrary JSON a client pushed — it is not the
// X-Bdrive-Device header the round-1 fix bound the journal KEY to, and nothing
// checks that the caller owns the id it names.
//
// Round 2 closed exactly this shape on the read-heat side: reads.go now runs
// devices.go's ownsDevice before a client-reported device id becomes an actor
// (TestSec_Heat_ByDeviceLeaksForeignDeviceMetadata). History has the same join
// and no such check, so a member of org A learns the machine name and OS of a
// device belonging to org B — and confirms which device ids exist on the hub —
// by naming it in an op in their own project.
func TestSec_Journal_HistoryDeviceFieldLeaksForeignDeviceMetadata(t *testing.T) {
	h, _, c, alice := secjrnHub(t)

	// dave is in a different org entirely. He syncs his own project from his
	// own laptop; that registers his device row, owned by him.
	dave := secpathNewProject(t, h, "dave-notes", c["dave"])
	const (
		daveDev  = "dave-laptop-7f3a"
		daveName = "Dave's Personal MacBook"
		daveOS   = "darwin 26.1 (nixos-hybrid)"
	)
	secjrnSync(t, h, dave.ID, daveDev, daveName, daveOS, c["dave"])

	// alice registers her own device the same way — the control.
	const aliceDev = "alice-desk"
	secjrnSync(t, h, alice.ID, aliceDev, "Alice Desktop", "linux 6.9", c["alice"])
	blob := secpathStoreBlob(t, h, alice.ID, "notes body", c["alice"])

	// alice pushes her own journal, to her own project, under her own device
	// key — every authorization check passes. One op simply CLAIMS to come
	// from dave's device.
	honest := secjrnOp(1, "mine.md", blob, len("notes body"))
	forged := secjrnOp(2, "probe.md", blob, len("notes body"))
	forged["device"] = daveDev
	forged["device_name"] = "whatever"
	if rec := secjrnPushJournal(t, h, alice.ID, aliceDev, []map[string]any{honest, forged}, c["alice"]); rec.Code != 200 {
		t.Fatalf("push own journal: %d %s", rec.Code, rec.Body)
	}

	hist := secjrnFetchHistory(t, h, alice.ID, c["alice"])
	byPath := map[string]HistoryEntry{}
	for _, e := range hist.Entries {
		byPath[e.Path] = e
	}
	// Control: the honest op resolves against the registry, proving the join
	// is live and the rest of this test is measuring the same code path.
	if got := byPath["mine.md"].Device.Name; got != "Alice Desktop" {
		t.Fatalf("control: history did not join alice's own device row (name %q)", got)
	}
	got := byPath["probe.md"].Device
	if got.Name == daveName || got.OS == daveOS {
		t.Errorf("history reported another org's device metadata: name=%q os=%q — an op's Device field is client-asserted and is not checked against the caller",
			got.Name, got.OS)
	}
}

// The same join is an existence oracle even when the registry row carries no
// name: whatever History echoes back for a device id the caller does not own
// must come from the op, never from the hub's registry. Asserting the shape
// separately keeps a fix that only blanks Name (and leaves OS, or leaks
// "exists") from looking green.
func TestSec_Journal_HistoryDeviceFieldIsNotAnExistenceOracle(t *testing.T) {
	h, _, c, alice := secjrnHub(t)

	dave := secpathNewProject(t, h, "dave-notes-2", c["dave"])
	const daveDev = "dave-laptop-aa11"
	secjrnSync(t, h, dave.ID, daveDev, "Dave Box", "openbsd 7.5", c["dave"])

	const aliceDev = "alice-desk"
	secjrnSync(t, h, alice.ID, aliceDev, "Alice Desktop", "linux 6.9", c["alice"])
	blob := secpathStoreBlob(t, h, alice.ID, "body", c["alice"])

	// Two ops naming device ids alice does not own: one that exists on the
	// hub (dave's) and one that does not. If the answers differ, the response
	// is an oracle for which devices exist across every org on the hub.
	real := secjrnOp(1, "real.md", blob, 4)
	real["device"], real["device_name"] = daveDev, "claimed"
	fake := secjrnOp(2, "fake.md", blob, 4)
	fake["device"], fake["device_name"] = "no-such-device-9999", "claimed"
	if rec := secjrnPushJournal(t, h, alice.ID, aliceDev, []map[string]any{real, fake}, c["alice"]); rec.Code != 200 {
		t.Fatalf("push: %d %s", rec.Code, rec.Body)
	}

	hist := secjrnFetchHistory(t, h, alice.ID, c["alice"])
	byPath := map[string]HistoryEntry{}
	for _, e := range hist.Entries {
		byPath[e.Path] = e
	}
	if byPath["real.md"].Device != byPath["fake.md"].Device {
		t.Errorf("an existing foreign device id (%+v) reads back differently from a nonexistent one (%+v): History tells any member which device ids exist hub-wide",
			byPath["real.md"].Device, byPath["fake.md"].Device)
	}
}

// ---- Op.Path: what the hub does with an unchecked path ----

// The viewer snapshot is keyed by op.Path, and the hub never validates it. A
// path is not a storage key here (content comes from blobs/<sha>), so the
// exposure is narrower than Blob's — but a hostile path must still not steer
// the hub's own writes. cleanUploadPath already refuses one on the way IN;
// this pins that a path that only exists because it arrived through a journal
// cannot be laundered back out through restore or remove.
func TestSec_Journal_HostilePathCannotBeLaunderedThroughRestoreOrRemove(t *testing.T) {
	h, srv, c, alice := secjrnHub(t)
	const body = "hostile body"
	blob := secpathStoreBlob(t, h, alice.ID, body, c["alice"])

	hostile := []string{
		"../../../etc/bdrive-owned",
		"/etc/bdrive-owned",
		".bdrive/config.json",
		".git/hooks/pre-commit",
		"docs/../.bdrive/config.json",
	}
	ops := make([]map[string]any, 0, len(hostile))
	for i, p := range hostile {
		op := secjrnOp(int64(i+1), p, blob, len(body))
		ops = append(ops, op)
	}

	// SETUP CHANGED IN ROUND 7, ASSERTIONS UNTOUCHED. The hub's /store/* door
	// used to accept these paths (its comment above said "the hub never
	// validates it"), and round 7 gave that door the same journal.SafePath
	// rule /upload/commit and the device already apply — so the push below is
	// now refused, which is strictly stronger and is asserted as a control.
	// The subject of this test is the way OUT, not the way in, so the hostile
	// journal is planted directly in storage, behind the door, exactly as
	// several other tests plant objects. Everything below this block is
	// unchanged and still fails if restore or remove stop refusing.
	if rec := secjrnPushJournal(t, h, alice.ID, "alice-desk", ops, c["alice"]); rec.Code == 200 {
		t.Errorf("the /store/* journal door accepted hostile paths %v — round 7's one-rule parity is gone", hostile)
	}
	var planted strings.Builder
	for _, op := range ops {
		line, err := json.Marshal(op)
		if err != nil {
			t.Fatal(err)
		}
		planted.Write(line)
		planted.WriteByte('\n')
	}
	_, v, err := srv.projectVolume(alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.source.(*RemoteSource).Backend.Put(t.Context(), "journal/alice-desk.jsonl",
		strings.NewReader(planted.String()), int64(planted.Len())); err != nil {
		t.Fatal(err)
	}

	for _, p := range hostile {
		rec := doAs(t, h, "POST", "/api/p/"+alice.ID+"/restore",
			map[string]string{"path": p, "sha": blob}, c["alice"])
		if rec.Code == 200 {
			t.Errorf("restore re-journaled hostile path %q", p)
		}
		rec = doAs(t, h, "POST", "/api/p/"+alice.ID+"/remove",
			map[string]string{"path": p}, c["alice"])
		if rec.Code == 200 {
			t.Errorf("remove journaled a delete for hostile path %q", p)
		}
	}
}

// ---- Op.Size: the length the hub promises for content it did not measure ----

// FileInfo.Size comes straight off the journal, and serveBlob sets
// Content-Length from it before streaming the blob's real bytes. Round 2 fixed
// the UPLOAD path to size from storage; the journal field never was. A
// Content-Length that disagrees with the body is a protocol violation the hub
// commits on behalf of any member who can push a journal.
func TestSec_Journal_SizeFieldCannotForgeContentLength(t *testing.T) {
	h, _, c, alice := secjrnHub(t)
	const body = "twenty-three bytes here"
	blob := secpathStoreBlob(t, h, alice.ID, body, c["alice"])

	op := secjrnOp(1, "lie.txt", blob, len(body))
	op["size"] = 999999
	if rec := secjrnPushJournal(t, h, alice.ID, "alice-desk", []map[string]any{op}, c["alice"]); rec.Code != 200 {
		t.Fatalf("push: %d %s", rec.Code, rec.Body)
	}

	rec := doAs(t, h, "GET", "/api/p/"+alice.ID+"/file?path=lie.txt", nil, c["alice"])
	if rec.Code != 200 {
		t.Fatalf("file: %d %s", rec.Code, rec.Body)
	}
	if cl := rec.Header().Get("Content-Length"); cl != "" && cl != "23" {
		t.Errorf("Content-Length %s does not match the %d bytes served — the journal's Size field is echoed as a wire promise the hub cannot keep",
			cl, rec.Body.Len())
	}
}
