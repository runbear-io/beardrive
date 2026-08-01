package webapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Round 2 attacks on scoreboard rows 10 (read-heat privacy) and 12 (secret
// leakage). Every test asserts the SECURE behavior, so it turns green the
// moment the hole is closed and stays as a regression test.

// secprivDo is doAs plus request headers — the read-report route takes both a
// session cookie and a device identity, and no existing helper does both.
func secprivDo(t *testing.T, h http.Handler, method, url string, body any, c *http.Cookie, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, url, rd)
	if c != nil {
		req.AddCookie(c)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// secprivFSPath matches an absolute server filesystem path (three or more
// segments) in a response body. A sanitized error never contains one;
// os.Open's "open /var/.../blobs/<sha>: no such file or directory" does.
var secprivFSPath = regexp.MustCompile(`(?:^|[\s"'(=])(/(?:[A-Za-z0-9._@%+-]+/){2,}[A-Za-z0-9._@%+-]*)`)

func secprivNoServerPath(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if m := secprivFSPath.FindStringSubmatch(rec.Body.String()); m != nil {
		t.Errorf("%s leaked a server filesystem path %q (status %d)\n  body: %s",
			what, m[1], rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}

// secprivReads turns a permHub into a read-telemetry hub: a ledger plus a
// device registry.
func secprivReads(t *testing.T, srv *Server) {
	t.Helper()
	var err error
	if srv.Reads, err = OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 0); err != nil {
		t.Fatal(err)
	}
	if srv.Devices, err = OpenDeviceRegistry(filepath.Join(t.TempDir(), "devices.json")); err != nil {
		t.Fatal(err)
	}
}

// secprivProjectFor creates a project owned by whoever the cookie belongs to,
// in that account's own org.
func secprivProjectFor(t *testing.T, h http.Handler, c *http.Cookie, name string) Project {
	t.Helper()
	rec := doAs(t, h, "POST", "/api/projects", map[string]string{"name": name}, c)
	if rec.Code != 200 {
		t.Fatalf("create project %q: %d %s", name, rec.Code, rec.Body)
	}
	var out struct {
		Project Project `json:"project"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Project
}

// ---- row 10: read-heat privacy ----

// /heat?by=device joins every actor string found in the project's agent
// buckets against the HUB-WIDE device registry. The actor is whatever the
// reporting client put in X-Bdrive-Device, and nothing checks that the device
// has any relationship to this project — so an outsider can name a device he
// does not own, inside a project of his own, and read that device's registry
// metadata (name, OS) straight back out of his own heat view.
func TestSec_Heat_ByDeviceLeaksForeignDeviceMetadata(t *testing.T) {
	h, srv, c, p := permHub(t)
	secprivReads(t, srv)
	// alice's laptop is known to the hub because it syncs her org's project.
	srv.Devices.Observe(DeviceInfo{ID: "dev-alice", Name: "alice-laptop", OS: "darwin/arm64", User: "alice@x.io"})

	// dave is in another org entirely; give him a project of his own.
	dp := secprivProjectFor(t, h, c["dave"], "daves-notes")
	if dp.Org == "" || dp.Org == p.Org {
		t.Fatalf("fixture broken: dave's project org = %q, alice's = %q", dp.Org, p.Org)
	}

	// The server's own decision, for contrast: dave cannot read alice's heat.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/heat?by=device", nil, c["dave"]); rec.Code != http.StatusForbidden {
		t.Fatalf("dave on alice's heat: %d, want 403", rec.Code)
	}

	// The attack: report an agent read in HIS project, claiming HER device id.
	rec := secprivDo(t, h, "POST", "/api/p/"+dp.ID+"/reads",
		map[string]any{"reads": []map[string]string{{"path": "note.md"}}},
		c["dave"], map[string]string{"X-Bdrive-Device": "dev-alice"})
	if rec.Code != 200 {
		t.Fatalf("dave read report: %d %s", rec.Code, rec.Body)
	}

	rec = doAs(t, h, "GET", "/api/p/"+dp.ID+"/heat?by=device", nil, c["dave"])
	if rec.Code != 200 {
		t.Fatalf("dave heat by=device: %d %s", rec.Code, rec.Body)
	}
	for _, leak := range []string{"alice-laptop", "darwin/arm64"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("heat?by=device handed an outsider %q about a device in another org: %s",
				leak, rec.Body)
		}
	}
}

// The same unvalidated device header is also a WRITE into the hub-wide
// registry: handleReadReport calls observeDevice, which merges the caller's
// headers over whatever the registry holds for that id. So an account in a
// different org can rename a device it has never touched — and that registry
// is exactly what History joins against, so the forged name surfaces in the
// victim org's change feed.
func TestSec_Reads_ReportCannotRewriteAnotherOrgsDevice(t *testing.T) {
	h, srv, c, p := permHub(t)
	secprivReads(t, srv)
	srv.Devices.Observe(DeviceInfo{ID: "dev-alice", Name: "alice-laptop", OS: "darwin/arm64", User: "alice@x.io"})
	dp := secprivProjectFor(t, h, c["dave"], "daves-notes")
	if dp.Org == "" || dp.Org == p.Org {
		t.Fatalf("fixture broken: dave's project org = %q, alice's = %q", dp.Org, p.Org)
	}

	rec := secprivDo(t, h, "POST", "/api/p/"+dp.ID+"/reads",
		map[string]any{"reads": []map[string]string{{"path": "note.md"}}},
		c["dave"], map[string]string{
			"X-Bdrive-Device":      "dev-alice",
			"X-Bdrive-Device-Name": "pwned-by-dave",
			"X-Bdrive-Os":          "evil/os",
		})
	if rec.Code != 200 {
		t.Fatalf("dave read report: %d %s", rec.Code, rec.Body)
	}

	got, ok := srv.Devices.Get("dev-alice")
	if !ok {
		t.Fatal("dev-alice vanished from the registry")
	}
	if got.Name != "alice-laptop" || got.OS != "darwin/arm64" || got.User != "alice@x.io" {
		t.Errorf("an account in another org rewrote a device registry entry: %+v", got)
	}
}

// CLAUDE.md's rule for this API is absolute: the actor column (account email,
// device id, share token) never appears in a response. Because the agent
// actor is an unvalidated request header, any project member — including a
// read-only one — can plant an arbitrary identity as an "actor" and have the
// heat API serve it back to every other member as fact.
func TestSec_Heat_ReadReportCannotInjectAnIdentity(t *testing.T) {
	h, srv, c, p := permHub(t)
	secprivReads(t, srv)
	if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermRead); err != nil {
		t.Fatal(err)
	}
	base := "/api/p/" + p.ID + "/"

	// bob, read-only, reports a read of the payroll file as "alice@x.io".
	rec := secprivDo(t, h, "POST", base+"reads",
		map[string]any{"reads": []map[string]string{{"path": "payroll.md"}}},
		c["bob"], map[string]string{"X-Bdrive-Device": "alice@x.io"})
	if rec.Code != 200 {
		t.Fatalf("bob read report: %d %s", rec.Code, rec.Body)
	}

	// carol, an ordinary member, opens the heat view.
	rec = doAs(t, h, "GET", base+"heat?by=device", nil, c["carol"])
	if rec.Code != 200 {
		t.Fatalf("carol heat: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "alice@x.io") {
		t.Errorf("heat served a planted account identity as a reader: %s", rec.Body)
	}
}

// Whatever the query shape, the identities the server itself recorded must
// never come back out. The pure-privacy sweep: seed human, share and agent
// buckets, then hammer /heat with every query shape a client can send.
func TestSec_Heat_NoQueryShapeLeaksAnActor(t *testing.T) {
	h, srv, c, p := permHub(t)
	secprivReads(t, srv)
	srv.Reads.Record(p.ID, "hr/payroll.md", ReadKindHuman, "alice@x.io")
	srv.Reads.Record(p.ID, "hr/payroll.md", ReadKindShare, "sharetok-abc/198.51.100.9")
	srv.Reads.Record(p.ID, "hr/payroll.md", ReadKindAgent, "dev-secret-agent")
	srv.Reads.Record(p.ID, "top.md", ReadKindHuman, "carol@x.io")

	base := "/api/p/" + p.ID + "/heat"
	shapes := []string{
		"", "?days=0", "?days=1", "?days=30", "?days=3650", "?days=999999999",
		"?prefix=hr", "?prefix=hr/", "?prefix=", "?prefix=/", "?prefix=..",
		"?prefix=hr&days=0", "?prefix=" + strings.Repeat("a", 2048),
		"?prefix=alice@x.io", "?days=&prefix=hr", "?days=%2B1", "?days=0x10",
	}
	secrets := []string{"alice@x.io", "carol@x.io", "sharetok-abc", "198.51.100.9", "dev-secret-agent", `"actor"`}
	for _, q := range shapes {
		rec := doAs(t, h, "GET", base+q, nil, c["bob"])
		if rec.Code >= 500 {
			t.Errorf("GET heat%s: %d %s", q, rec.Code, rec.Body)
		}
		for _, leak := range secrets {
			if strings.Contains(rec.Body.String(), leak) {
				t.Errorf("GET heat%s leaked %q: %d %s", q, leak, rec.Code, rec.Body)
			}
		}
	}
}

// Heat is membership-gated: a member demoted to none, and an outsider, reach
// neither the aggregates nor the ingest endpoint.
func TestSec_Heat_RefusedWithoutReadPermission(t *testing.T) {
	h, srv, c, p := permHub(t)
	secprivReads(t, srv)
	if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermNone); err != nil {
		t.Fatal(err)
	}
	base := "/api/p/" + p.ID + "/"
	for _, who := range []string{"bob", "dave"} {
		for _, q := range []string{"heat", "heat?by=device", "heat?days=0&prefix=hr"} {
			if rec := doAs(t, h, "GET", base+q, nil, c[who]); rec.Code != http.StatusForbidden {
				t.Errorf("GET %s as %s: %d, want 403 (%s)", q, who, rec.Code, rec.Body)
			}
		}
		rec := secprivDo(t, h, "POST", base+"reads",
			map[string]any{"reads": []map[string]string{{"path": "x.md"}}},
			c[who], map[string]string{"X-Bdrive-Device": "d1"})
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST reads as %s: %d, want 403 (%s)", who, rec.Code, rec.Body)
		}
	}
	if got := srv.Reads.Heat(p.ID, "", time.Time{}); len(got) != 0 {
		t.Errorf("refused reports were recorded anyway: %+v", got)
	}
}

// Telemetry must never fail the request that carries it, and a malformed
// report must not echo the server's internals back.
func TestSec_Reads_MalformedReportsStayHarmless(t *testing.T) {
	h, srv, c, p := permHub(t)
	secprivReads(t, srv)
	base := "/api/p/" + p.ID + "/"
	hdr := map[string]string{"X-Bdrive-Device": "dev1"}
	bodies := [][]byte{
		[]byte(`{`),
		[]byte(`{"reads":"not-an-array"}`),
		[]byte(`{"reads":[{"path":123}]}`),
		[]byte(`{"reads":null}`),
		[]byte(`{"reads":[{"path":"a.md","time":"not-a-time"}]}`),
		[]byte(`{"reads":[` + strings.Repeat(`{"path":"a.md"},`, 5000) + `{"path":"b.md"}]}`),
		bytes.Repeat([]byte("A"), 2<<20),
	}
	for i, b := range bodies {
		rec := secprivDo(t, h, "POST", base+"reads", b, c["alice"], hdr)
		if rec.Code >= 500 {
			t.Errorf("body %d: %d %s", i, rec.Code, rec.Body)
		}
		secprivNoServerPath(t, fmt.Sprintf("POST reads (body %d)", i), rec)
	}
	// the endpoint still works afterwards
	rec := secprivDo(t, h, "POST", base+"reads",
		map[string]any{"reads": []map[string]string{{"path": "ok.md"}}}, c["alice"], hdr)
	if rec.Code != 200 {
		t.Fatalf("report after the malformed ones: %d %s", rec.Code, rec.Body)
	}
}

// ---- row 12: secret / internal-detail leakage ----

// Storage errors are relayed to the client verbatim. On a file:// hub that is
// os.Open's message, which names the hub's absolute storage path (on an
// s3:// hub, the bucket and key) — handed to any ordinary project member who
// asks for an object that isn't there.
func TestSec_Leak_ErrorBodiesRevealServerFilesystemPaths(t *testing.T) {
	h, _, c, p := permHub(t)
	base := "/api/p/" + p.ID + "/"
	missing := strings.Repeat("b", 64)

	// 1. the sync proxy: any read-capable member can ask for a missing blob
	rec := doAs(t, h, "GET", base+"store/object?key=blobs/"+missing, nil, c["bob"])
	secprivNoServerPath(t, "GET store/object (missing blob)", rec)

	// 2. the viewer: a journal op pointing at content that is not in storage.
	// bob writes his own journal through the documented sync route.
	line := `{"seq":1,"lamport":1,"time":"2026-01-01T00:00:00Z","device":"devx",` +
		`"kind":"put","path":"secret.md","blob":"` + missing + `","size":5}` + "\n"
	rec = secprivDo(t, h, "PUT", base+"store/object?key=journal/devx.jsonl", []byte(line),
		c["bob"], map[string]string{"X-Bdrive-Device": "devx"})
	if rec.Code != 200 {
		t.Fatalf("seed journal: %d %s", rec.Code, rec.Body)
	}
	for _, u := range []string{
		"file?path=secret.md", "download?path=secret.md",
		"render?path=secret.md", "blob?sha=" + missing,
	} {
		rec := doAs(t, h, "GET", base+u, nil, c["bob"])
		secprivNoServerPath(t, "GET "+u, rec)
	}
}

// An authenticated ordinary member — not just an anonymous stranger — must
// not find storage locations, credentials, DSNs, SMTP settings or server
// paths in any routine response, nor in any error one.
func TestSec_Leak_NothingSensitiveForAnOrdinaryMember(t *testing.T) {
	h, srv, c, p := permHub(t)
	secprivReads(t, srv)
	if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermRead); err != nil {
		t.Fatal(err)
	}
	base := "/api/p/" + p.ID + "/"

	probes := []struct {
		method, url string
		body        any
	}{
		{"GET", "/api/config", nil},
		{"GET", "/api/projects", nil},
		{"GET", "/api/projects/" + p.ID, nil},
		{"GET", "/api/orgs", nil},
		{"GET", "/api/orgs/" + p.Org + "/shares", nil},
		{"GET", "/api/admin/policy", nil},
		{"GET", "/api/admin/pending", nil},
		{"GET", base + "permissions", nil},
		{"GET", base + "tree", nil},
		{"GET", base + "history", nil},
		{"GET", base + "shares", nil},
		{"GET", base + "heat", nil},
		{"GET", base + "store/list?prefix=journal/", nil},
		{"GET", base + "store/exists?key=blobs/" + strings.Repeat("c", 64), nil},
		// error paths: bad ids, malformed bodies, wrong types
		{"GET", "/api/p/not-a-project/tree", nil},
		{"GET", base + "file?path=", nil},
		{"GET", base + "file?path=" + strings.Repeat("../", 40) + "etc/passwd", nil},
		{"GET", base + "store/object?key=../../etc/passwd", nil},
		{"POST", "/api/projects", []byte(`{"name":`)},
		{"POST", "/api/projects", []byte(`{"name":{"a":1}}`)},
		{"POST", base + "upload/init", []byte(`nonsense`)},
		{"POST", base + "upload/commit", []byte(`{"path":123}`)},
		{"POST", base + "shares", []byte(`{`)},
		{"POST", base + "reads", []byte(`{"reads":[{"path":`)},
		{"PATCH", "/api/projects/" + p.ID, []byte(`{"name":[]}`)},
		{"POST", "/api/admin/policy", []byte(`{"require_verification":"yes"}`)},
	}
	// Nothing a hub is configured with belongs in a response body.
	needles := []string{
		"postgres://", "postgresql://", "sqlite://", "file://", "s3://", "gs://",
		"AKIA", "aws_secret", "secret_access_key",
		"smtp", "password", "dsn",
		"goroutine ", ".go:", "runtime.", "panic:",
	}
	for _, pr := range probes {
		rec := doAs(t, h, pr.method, pr.url, pr.body, c["bob"])
		if rec.Code >= 500 {
			t.Errorf("%s %s: %d %s", pr.method, pr.url, rec.Code, rec.Body)
		}
		body := strings.ToLower(rec.Body.String())
		for _, n := range needles {
			if strings.Contains(body, strings.ToLower(n)) {
				t.Errorf("%s %s leaked %q: %d %s", pr.method, pr.url, n, rec.Code, rec.Body)
			}
		}
		secprivNoServerPath(t, pr.method+" "+pr.url, rec)
	}
}
