package webapp

// Round 3 — the target is round 1 and round 2's own fixes.
//
// A security fix is new code written under time pressure at exactly the point
// where the system makes a trust decision, and nobody has attacked it yet.
// Everything here aims at code that did not exist before 4abcb68/773f1ec:
// ownsDevice + the DeviceRegistry.Observe owner guard, shareCreatorStillBelongs,
// safeNext, storageErr's new log lines, the blobRe guard in RemoteSource,
// Org.clone/Project.clone + db.put's rollback, Server.joinMu, and underRoot.
//
// Every test asserts the SECURE behavior, so each goes green the moment the
// hole is closed and stays as a permanent regression test. Helpers are
// prefixed `secfix` per the harness rules; no existing file is touched.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- helpers (secfix* so they cannot collide with another agent's file) ----

// secfixDo is doAs plus request headers — every device-identity attack needs
// X-Bdrive-Device on an otherwise ordinary request.
func secfixDo(t *testing.T, h http.Handler, method, url string, body any, c *http.Cookie, hdr map[string]string) *httptest.ResponseRecorder {
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

// secfixTelemetry turns a permHub into a hub with read heat and a device
// registry — the two tables ownsDevice sits between.
func secfixTelemetry(t *testing.T, srv *Server) {
	t.Helper()
	var err error
	if srv.Reads, err = OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 0); err != nil {
		t.Fatal(err)
	}
	if srv.Devices, err = OpenDeviceRegistry(filepath.Join(t.TempDir(), "devices.json")); err != nil {
		t.Fatal(err)
	}
}

// secfixProject creates a project owned by whoever the cookie belongs to.
func secfixProject(t *testing.T, h http.Handler, c *http.Cookie, name string) Project {
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

// secfixSync is the most ordinary request a syncing device makes: a store
// listing, carrying the device headers remote/http.go always sends. This is
// the ONLY thing that registers a device in the hub-wide registry.
func secfixSync(t *testing.T, h http.Handler, projectID string, c *http.Cookie, id, name, os string) *httptest.ResponseRecorder {
	t.Helper()
	return secfixDo(t, h, "GET", "/api/p/"+projectID+"/store/list?prefix=blobs/", nil, c, map[string]string{
		"X-Bdrive-Device":      id,
		"X-Bdrive-Device-Name": name,
		"X-Bdrive-Os":          os,
	})
}

// ---- fix 1: ownsDevice / DeviceRegistry.Observe — first-caller-wins ----

// Round 2 made Observe refuse to touch a row owned by another account. The row
// is created by whoever names the id FIRST, and the only thing that names an
// id is a client header on a sync request — which every project member can
// send, in their own project, about any id they like.
//
// So the guard hands out permanent ownership of an unregistered device id to
// whoever asks first. dave, in a different org, has never met alice: he names
// her laptop's id once on a request inside HIS OWN project, and from then on
// the hub believes that device is his. Alice's real laptop can never correct
// it — Observe now returns early for her.
//
// The control is the whole finding: the same first sync by alice, with an id
// nobody squatted, registers exactly as it should.
func TestSec_Devices_IdCannotBeSquattedBeforeItsOwnerRegisters(t *testing.T) {
	h, srv, c, p := permHub(t)
	secfixTelemetry(t, srv)
	dp := secfixProject(t, h, c["dave"], "daves-notes")
	if dp.Org == "" || dp.Org == p.Org {
		t.Fatalf("fixture broken: dave's org = %q, alice's = %q", dp.Org, p.Org)
	}

	// Control: an unsquatted id registers to its owner on first sync.
	if rec := secRegisterDevice(t, h, p.ID, c["alice"], "dev-clean", "alice-mini", "darwin/arm64"); rec.Code != 200 {
		t.Fatalf("control sync: %d %s", rec.Code, rec.Body)
	}
	if got, _ := srv.Devices.Get("dev-clean"); got.User != "alice@x.io" || got.Name != "alice-mini" {
		t.Fatalf("control device row = %+v, want alice's", got)
	}

	// Attack: dave names an id that does not exist yet, in his own project.
	const id = "dev-alice-laptop"
	if rec := secfixSync(t, h, dp.ID, c["dave"], id, "daves-squat", "evil/os"); rec.Code != 200 {
		t.Fatalf("dave's sync: %d %s", rec.Code, rec.Body)
	}
	// Alice's real laptop syncs her own project for the first time.
	if rec := secRegisterDevice(t, h, p.ID, c["alice"], id, "alice-macbook", "darwin/arm64"); rec.Code != 200 {
		t.Fatalf("alice's sync: %d %s", rec.Code, rec.Body)
	}

	got, ok := srv.Devices.Get(id)
	if !ok {
		t.Fatal("device row vanished")
	}
	if got.User != "alice@x.io" || got.Name != "alice-macbook" || got.OS != "darwin/arm64" {
		t.Errorf("an account in another org owns a device it never had:\n"+
			"  got  %+v\n  want User=alice@x.io Name=alice-macbook OS=darwin/arm64\n"+
			"  History joins this row, so every op alice's laptop pushes is now labelled %q",
			got, got.Name)
	}
}

// The squat is not only cosmetic: ownsDevice is the ingest gate for read
// telemetry, so a row owned by someone else makes the real owner's reports
// vanish. handleReadReport answers 200 either way ("telemetry never fails a
// request"), so nothing anywhere tells alice her agent reads stopped counting.
func TestSec_Devices_SquattedIdStillCountsItsOwnersReads(t *testing.T) {
	h, srv, c, p := permHub(t)
	secfixTelemetry(t, srv)
	dp := secfixProject(t, h, c["dave"], "daves-notes")

	const id = "dev-alice-laptop"
	// dave squats the id from his own org.
	if rec := secfixSync(t, h, dp.ID, c["dave"], id, "daves-squat", "evil/os"); rec.Code != 200 {
		t.Fatalf("dave's sync: %d %s", rec.Code, rec.Body)
	}
	// Alice's laptop syncs her project, then reports what her agent read.
	if rec := secRegisterDevice(t, h, p.ID, c["alice"], id, "alice-macbook", "darwin/arm64"); rec.Code != 200 {
		t.Fatalf("alice's sync: %d %s", rec.Code, rec.Body)
	}
	// Round 12 fixture update: handleReadReport now drops a report for a path
	// that is not in the project's replayed state, so the read has to be a read
	// of a real file. The subject — a squatted row must not silence its real
	// owner's telemetry — is unchanged.
	secauthzUpload(t, h, p.ID, "notes/plan.md", "the plan", c["alice"])
	rec := secfixDo(t, h, "POST", "/api/p/"+p.ID+"/reads",
		map[string]any{"reads": []map[string]string{{"path": "notes/plan.md"}}},
		c["alice"], map[string]string{"X-Bdrive-Device": id})
	if rec.Code != 200 {
		t.Fatalf("alice read report: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Accepted int `json:"accepted"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Accepted != 1 {
		t.Errorf("alice's own device reported a read and the hub counted %d of 1 —\n"+
			"  an outsider owning the registry row silently disables her read heat forever",
			out.Accepted)
	}
}

// Two devices registering the same id at the same instant must leave the
// registry consistent — one owner, and the row that owner observed. Run with
// -race: Observe's early return added a second read of cur under the same
// lock, and putting a row back is a read-modify-write.
func TestSec_Devices_ConcurrentRegistrationLeavesOneConsistentOwner(t *testing.T) {
	h, srv, c, p := permHub(t)
	secfixTelemetry(t, srv)
	dp := secfixProject(t, h, c["dave"], "daves-notes")

	const id = "dev-contested"
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			secRegisterDevice(t, h, p.ID, c["alice"], id, "alice-macbook", "darwin/arm64")
		}()
		go func() { defer wg.Done(); secRegisterDevice(t, h, dp.ID, c["dave"], id, "daves-squat", "evil/os") }()
	}
	wg.Wait()

	got, ok := srv.Devices.Get(id)
	if !ok {
		t.Fatal("contested device row vanished")
	}
	// The row must belong to exactly one of them, and its name must be the
	// one THAT account sent — never a mix of the two.
	switch got.User {
	case "alice@x.io":
		if got.Name != "alice-macbook" || got.OS != "darwin/arm64" {
			t.Errorf("row owned by alice carries dave's metadata: %+v", got)
		}
	case "dave@x.io":
		if got.Name != "daves-squat" || got.OS != "evil/os" {
			t.Errorf("row owned by dave carries alice's metadata: %+v", got)
		}
	default:
		t.Errorf("contested row has no owner: %+v", got)
	}
}

// ownsDevice is a first-request speed bump, not a gate: the very request it
// refuses goes on to CALL observeDevice, which registers the refused id to the
// caller. So the second attempt passes. Round 2's own comment says the check
// runs "before observeDevice, which would otherwise register the forged id" —
// but observeDevice still runs, unconditionally, one line later.
//
// The consequence is exactly the invariant row 10 exists for: CLAUDE.md says
// the actor column "must never appear in an API response" and permits ?by=device
// only because ingest validates the id. Two requests from a READ-ONLY member
// put an arbitrary string — here another person's account email — into the heat
// API as a reader of a named file, served to every member of the project.
func TestSec_Heat_PlantedIdentityCannotBeSelfRegisteredThenReported(t *testing.T) {
	h, srv, c, p := permHub(t)
	secfixTelemetry(t, srv)
	if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermRead); err != nil {
		t.Fatal(err)
	}
	base := "/api/p/" + p.ID + "/"
	report := func() *httptest.ResponseRecorder {
		return secfixDo(t, h, "POST", base+"reads",
			map[string]any{"reads": []map[string]string{{"path": "payroll.md"}}},
			c["bob"], map[string]string{"X-Bdrive-Device": "alice@x.io"})
	}
	// First report: refused (the id is unknown) — and registered on the way out.
	if rec := report(); rec.Code != 200 {
		t.Fatalf("bob's first report: %d %s", rec.Code, rec.Body)
	}
	// Second report: the id is now "his", so it counts.
	if rec := report(); rec.Code != 200 {
		t.Fatalf("bob's second report: %d %s", rec.Code, rec.Body)
	}

	rec := doAs(t, h, "GET", base+"heat?by=device", nil, c["carol"])
	if rec.Code != 200 {
		t.Fatalf("carol heat: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "alice@x.io") {
		t.Errorf("a read-only member planted an account identity as a heat actor in two requests\n"+
			"  (the refused first report is what registered the id): %s", rec.Body)
	}
}

// The same bypass in one request, through the route that actually registers
// devices. /store/* calls observeDevice with no ownership check at all, so a
// member can mint any actor string first and report reads with it after.
func TestSec_Heat_StoreRouteCannotMintAnArbitraryHeatActor(t *testing.T) {
	h, srv, c, p := permHub(t)
	secfixTelemetry(t, srv)
	if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermRead); err != nil {
		t.Fatal(err)
	}
	// A read-only member cannot write the store, so use his own project.
	bp := secfixProject(t, h, c["bob"], "bobs-scratch")
	const planted = "carol@x.io"
	if rec := secfixSync(t, h, bp.ID, c["bob"], planted, "", ""); rec.Code != 200 {
		t.Fatalf("bob's sync: %d %s", rec.Code, rec.Body)
	}
	rec := secfixDo(t, h, "POST", "/api/p/"+p.ID+"/reads",
		map[string]any{"reads": []map[string]string{{"path": "payroll.md"}}},
		c["bob"], map[string]string{"X-Bdrive-Device": planted})
	if rec.Code != 200 {
		t.Fatalf("bob's report: %d %s", rec.Code, rec.Body)
	}
	rec = doAs(t, h, "GET", "/api/p/"+p.ID+"/heat?by=device", nil, c["carol"])
	if strings.Contains(rec.Body.String(), planted) {
		t.Errorf("a store request minted an arbitrary heat actor that /heat then served: %s", rec.Body)
	}
}

// ---- fix 2: shareCreatorStillBelongs — a read-time check on a public route ----

// The offboarding fix must survive the states a real org passes through. Each
// case below is a way the creator stops belonging, or a way the lookup can
// answer nothing; the link must serve in exactly the cases where the creator
// is still a member, and 404 in every other.
func TestSec_Share_CreatorMembershipIsResolvedFailClosed(t *testing.T) {
	h, srv, c, p := permHub(t)
	secfixShareFile(t, srv, p.ID, "report.md", "# quarterly numbers")

	mint := func(who string) string {
		t.Helper()
		rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/shares", map[string]string{"path": "report.md"}, c[who])
		if rec.Code != 200 {
			t.Fatalf("%s mints a share: %d %s", who, rec.Code, rec.Body)
		}
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		tok, _ := out["token"].(string)
		if tok == "" {
			t.Fatalf("no token in %s", rec.Body)
		}
		return tok
	}
	hit := func(tok string) int { return doAs(t, h, "GET", "/s/"+tok, nil, nil).Code }

	// Control: bob is a member, his link serves.
	bobTok := mint("bob")
	if code := hit(bobTok); code != 200 {
		t.Fatalf("control: a member's link answered %d, want 200", code)
	}

	// (a) bob leaves the org — round 2's fix, re-asserted as the baseline.
	carolTok := mint("carol")
	if err := srv.Dir.RemoveMember(p.Org, "bob@x.io"); err != nil {
		t.Fatal(err)
	}
	if code := hit(bobTok); code != http.StatusNotFound {
		t.Errorf("a link minted by an offboarded member still serves: %d", code)
	}

	// (b) the project is moved into an org carol is not in. Her link must die
	// with the move: the content now belongs to a team she is not on.
	other, err := srv.Dir.Create("acquirer", "dave@x.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Projects.SetOrg(p.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	if code := hit(carolTok); code != http.StatusNotFound {
		t.Errorf("a link kept serving after the project moved to an org its creator is not in: %d", code)
	}
	if err := srv.Projects.SetOrg(p.ID, p.Org); err != nil {
		t.Fatal(err)
	}

	// (c) the org the project points at no longer resolves — the directory
	// answers "nothing". A public route that cannot establish membership must
	// fail CLOSED; "the lookup returned nothing" is not "everyone belongs".
	if err := srv.Projects.SetOrg(p.ID, "org-that-does-not-exist"); err != nil {
		t.Fatal(err)
	}
	if code := hit(carolTok); code != http.StatusNotFound {
		t.Errorf("an unresolvable org made the membership check fail OPEN on a public route: %d\n"+
			"  shareCreatorStillBelongs returns true whenever orgOf() is empty or unknown",
			code)
	}

	// (d) same shape, reached without touching the org table at all: a
	// project whose Org is cleared. Every share on it becomes public-forever
	// regardless of who minted it or whether they are still around.
	if err := srv.Projects.SetOrg(p.ID, ""); err != nil {
		t.Fatal(err)
	}
	if code := hit(bobTok); code != http.StatusNotFound {
		t.Errorf("clearing a project's org resurrected an offboarded member's public link: %d", code)
	}
}

// secfixShareFile puts one file into a hub project through the store API, the
// way a device would, so /s/ has something to serve.
func secfixShareFile(t *testing.T, srv *Server, projectID, path, content string) {
	t.Helper()
	h := srv.Handler()
	sha := shaOf(content)
	rec := doAs(t, h, "PUT", "/api/p/"+projectID+"/store/object?key=blobs/"+sha, []byte(content), nil)
	if rec.Code != 200 {
		// hub with auth: push through the server's own backend instead.
		v, err := srv.projectVolume(projectID)
		if err != nil {
			t.Fatal(err)
		}
		rs := v.source.(*RemoteSource)
		if err := rs.Backend.Put(t.Context(), "blobs/"+sha, strings.NewReader(content), int64(len(content))); err != nil {
			t.Fatal(err)
		}
		op := fmt.Sprintf(`{"seq":1,"lamport":1,"time":%q,"kind":"put","path":%q,"blob":%q,"size":%d}`+"\n",
			time.Now().UTC().Format(time.RFC3339Nano), path, sha, len(content))
		if err := rs.Backend.Put(t.Context(), "journal/seed.jsonl", strings.NewReader(op), int64(len(op))); err != nil {
			t.Fatal(err)
		}
		v.mu.Lock()
		v.snap = nil
		v.mu.Unlock()
	}
}

// ---- fix 3: safeNext ----

// Round 2's own gap list: safeNext was verified against tab/CR/LF and a
// backslash, never against percent-encoded or unicode separators. Drive the
// whole battery through every route that actually takes a `next`, and judge
// the Location header the way a browser does — strip what a browser strips,
// then ask whether an authority slot got filled.
func TestSec_Path_NextCannotLeaveTheHubOnAnyAuthRoute(t *testing.T) {
	h, _, c, _ := permHub(t)

	hostile := []string{
		"//evil.example/",
		`/\evil.example/`,
		`/\/evil.example/`,
		"///evil.example/",
		"/\t//evil.example/",
		"/\r\n//evil.example/",
		`/%2f%2fevil.example/`,
		`/%5cevil.example/`,
		`/%09/%2fevil.example/`,
		`/%0d%0aSet-Cookie:pwn=1`,
		"/⁄⁄evil.example/",  // FRACTION SLASH
		"/／／evil.example/",  // FULLWIDTH SOLIDUS
		"///evil.example/", // NEL
		"/ //evil.example/", // LINE SEPARATOR
		"///evil.example/", // vertical tab
		"///evil.example/",// form feed
		"https://evil.example/",
		"javascript:alert(1)",
		`/\t\\evil.example/`,
		"/ //evil.example/",
	}

	redirects := 0
	for _, next := range hostile {
		q := "next=" + urlQuery(next)
		for _, tc := range []struct {
			method, url string
			body        string
		}{
			{"POST", "/auth/login?" + q, "email=alice%40x.io&password=password1"},
			{"POST", "/auth/signup?" + q, "email=eve%40x.io&name=Eve&password=password1"},
			{"GET", "/auth/logout?" + q, ""},
		} {
			req := httptest.NewRequest(tc.method, tc.url, strings.NewReader(tc.body))
			if tc.method == "POST" {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			if tc.method == "GET" {
				req.AddCookie(c["alice"])
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			loc := rec.Header().Get("Location")
			if loc != "" {
				redirects++
			}
			if off, why := secfixOffSite(loc); off {
				t.Errorf("%s %s with next=%q redirected off-site\n  Location: %q\n  reason: %s",
					tc.method, tc.url, next, loc, why)
			}
			if v := rec.Header().Get("Set-Cookie"); strings.Contains(v, "pwn=1") {
				t.Errorf("%s next=%q injected a Set-Cookie: %q", tc.method, next, v)
			}
		}
	}
	// Guard the guard: a route family that stopped redirecting at all would
	// make every assertion above vacuous.
	if want := len(hostile); redirects < want {
		t.Fatalf("only %d of the %d hostile next values produced a redirect at all — "+
			"the battery is not exercising the redirect path", redirects, want)
	}
}

// urlQuery percent-encodes a value for a query string without importing
// net/url's escaping rules into the table above (which must stay literal).
func urlQuery(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9',
			ch == '-', ch == '_', ch == '.', ch == '~':
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

// secfixOffSite judges a Location header the way a browser resolves one
// against the hub's origin: tab/CR/LF are removed anywhere in a URL, a
// backslash is a separator, and anything that ends up with an authority slot
// (a scheme, or a leading "//") leaves the site.
func secfixOffSite(loc string) (bool, string) {
	if loc == "" {
		return false, ""
	}
	u := strings.Map(func(r rune) rune {
		if r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, loc)
	u = strings.ReplaceAll(u, `\`, "/")
	if u == "" {
		return false, ""
	}
	if i := strings.IndexAny(u, ":/?#"); i > 0 && u[i] == ':' {
		return true, "absolute URL with a scheme"
	}
	if strings.HasPrefix(u, "//") {
		return true, "scheme-relative: the authority slot is filled by the attacker"
	}
	return false, ""
}

// ---- fix 4: storageErr's new log lines ----

// Round 2 fixed path leakage by MOVING the detail into the log. Nothing checks
// what else ends up there. The log is read by operators, shipped to log
// aggregators, and on a managed hub leaves the machine — so a credential, a
// signed URL or a session token in it is the same leak in a different pipe.
func TestSec_Leak_NewLogLinesCarryNoCredential(t *testing.T) {
	h, srv, c, p := permHub(t)
	secfixTelemetry(t, srv)

	var buf bytes.Buffer
	old := log.Writer()
	flags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(old); log.SetFlags(flags) })

	// Drive every route that now funnels a storage error through storageErr.
	miss := strings.Repeat("ab", 32)
	for _, u := range []string{
		"/api/p/" + p.ID + "/store/object?key=blobs/" + miss,
		"/api/p/" + p.ID + "/store/list?prefix=blobs/",
		"/api/p/" + p.ID + "/file?path=nope.md",
		"/api/p/" + p.ID + "/download?path=nope.md",
		"/api/p/" + p.ID + "/render?path=nope.md",
		"/api/p/" + p.ID + "/blob?sha=" + miss,
		"/api/p/" + p.ID + "/history?prefix=",
	} {
		secfixDo(t, h, "GET", u, nil, c["alice"], map[string]string{
			"X-Bdrive-Device": "dev-alice", "Authorization": "Bearer secret-device-token-value",
		})
	}

	logged := buf.String()
	for _, secret := range []string{
		c["alice"].Value,            // the session cookie the request carried
		"secret-device-token-value", // the bearer token the request carried
		"X-Amz-Signature", "Signature=", "AWS4-HMAC",
		"password", "Cookie:", "Authorization",
	} {
		if secret != "" && strings.Contains(logged, secret) {
			t.Errorf("a round-2 log line carried credential material %q\n  log: %s", secret, logged)
		}
	}
	// A log line must not be forgeable either: nothing a caller supplies may
	// carry a newline into it.
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		if line != "" && !strings.HasPrefix(line, "beardrive: ") {
			t.Errorf("a log line does not start with the server's own prefix — a caller split it:\n  %q\n  full log: %s", line, logged)
		}
	}
}

// ---- fix 5: the blobRe guard in RemoteSource.Files / Open ----

// blobRe stops a Blob field being a path. It must ALSO be true that a
// well-formed sha is still scoped to the project it was pushed into — the
// guard is worthless if the prefix is applied before the check rather than
// after. dave stores content in his own project; bob names its exact hash from
// alice's project, where that blob does not exist.
func TestSec_Path_ValidBlobHashStaysInsideItsProject(t *testing.T) {
	h, _, c, p := permHub(t)
	dp := secfixProject(t, h, c["dave"], "daves-notes")

	const secret = "dave's private board minutes"
	sha := shaOf(secret)
	if rec := doAs(t, h, "PUT", "/api/p/"+dp.ID+"/store/object?key=blobs/"+sha, []byte(secret), c["dave"]); rec.Code != 200 {
		t.Fatalf("dave stores a blob: %d %s", rec.Code, rec.Body)
	}

	// bob pushes a journal op in alice's project naming dave's hash. It passes
	// blobRe — it is a real sha256 — so only the prefix can save this.
	op := map[string]any{
		"seq": 1, "lamport": 1, "time": time.Now().UTC().Format(time.RFC3339Nano),
		"kind": "put", "path": "stolen.md", "blob": sha, "size": len(secret),
		// setup only: permHub now has a device registry, so a first push of an
		// unclaimed id must name that device the way the real client does.
		"device": "bobdev",
	}
	line, _ := json.Marshal(op)
	secRegisterDevice(t, h, p.ID, c["bob"], "bobdev", "bob-box", "linux")
	if rec := secfixDo(t, h, "PUT", "/api/p/"+p.ID+"/store/object?key=journal/bobdev.jsonl",
		append(line, '\n'), c["bob"], map[string]string{"X-Bdrive-Device": "bobdev"}); rec.Code != 200 {
		t.Fatalf("bob pushes his own journal: %d %s", rec.Code, rec.Body)
	}

	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/file?path=stolen.md", nil, c["bob"])
	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("a 64-hex Blob reached another project's storage: %d %s", rec.Code, rec.Body)
	}
}

// Files drops an op whose Blob is not a sha — so the path keeps its PREVIOUS
// version. That must hold on every surface that reads the same snapshot,
// including the public share route, which is the one place the content leaves
// the org. A hostile op must never be able to repoint a live share link.
func TestSec_Path_HostileBlobCannotRepointALiveShare(t *testing.T) {
	h, srv, c, p := permHub(t)
	dp := secfixProject(t, h, c["dave"], "daves-notes")

	const good = "public release notes"
	const secretText = "dave's private board minutes"
	secfixShareFile(t, srv, p.ID, "notes.md", good)
	daveSha := shaOf(secretText)
	dv, err := srv.projectVolume(dp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := dv.source.(*RemoteSource).Backend.Put(t.Context(), "blobs/"+daveSha,
		strings.NewReader(secretText), int64(len(secretText))); err != nil {
		t.Fatal(err)
	}

	rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/shares", map[string]string{"path": "notes.md"}, c["alice"])
	if rec.Code != 200 {
		t.Fatalf("mint share: %d %s", rec.Code, rec.Body)
	}
	var sh map[string]any
	json.Unmarshal(rec.Body.Bytes(), &sh)
	tok, _ := sh["token"].(string)
	if tok == "" {
		t.Fatalf("no token: %s", rec.Body)
	}
	if rec := doAs(t, h, "GET", "/s/"+tok, nil, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "public release notes") {
		t.Fatalf("control share hit: %d %s", rec.Code, rec.Body)
	}

	// bob repoints the shared path at a key outside this project's prefix.
	op := map[string]any{
		"seq": 9, "lamport": 99, "time": time.Now().UTC().Format(time.RFC3339Nano),
		"kind": "put", "path": "notes.md",
		"blob": "../../" + dp.ID + "/blobs/" + daveSha, "size": len(secretText),
		// setup only: see the note in the test above.
		"device": "bobdev",
	}
	line, _ := json.Marshal(op)
	secRegisterDevice(t, h, p.ID, c["bob"], "bobdev", "bob-box", "linux")
	if rec := secfixDo(t, h, "PUT", "/api/p/"+p.ID+"/store/object?key=journal/bobdev.jsonl",
		append(line, '\n'), c["bob"], map[string]string{"X-Bdrive-Device": "bobdev"}); rec.Code != 200 {
		t.Fatalf("bob pushes: %d %s", rec.Code, rec.Body)
	}
	// Force a fresh fold of the journals.
	v, _ := srv.projectVolume(p.ID)
	v.mu.Lock()
	v.snap = nil
	v.mu.Unlock()

	rec = doAs(t, h, "GET", "/s/"+tok, nil, nil)
	if strings.Contains(rec.Body.String(), secretText) {
		t.Errorf("a hostile Blob repointed a PUBLIC share link at another project's content: %d %s", rec.Code, rec.Body)
	}
}

// ---- fix 6: Org.clone / Project.clone and db.put's rollback ----

// The clones must be deep enough on EVERY accessor, not just the one round 2
// tested. A caller who can write into a returned map writes past SetRole, the
// last-owner guard and the store.
func TestSec_DB_EveryRegistryAccessorHandsOutACopy(t *testing.T) {
	orgs, err := OpenOrgDB(filepath.Join(t.TempDir(), "orgs.json"))
	if err != nil {
		t.Fatal(err)
	}
	o, err := orgs.Create("acme", "alice@x.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := orgs.AddMember(o.ID, "bob@x.io", RoleMember); err != nil {
		t.Fatal(err)
	}
	// Every path that hands an Org out.
	o.Members["mallory@x.io"] = RoleOwner // the value Create returned
	got, _ := orgs.Get(o.ID)
	got.Members["mallory@x.io"] = RoleOwner
	for _, of := range orgs.OrgsFor("bob@x.io") {
		of.Members["mallory@x.io"] = RoleOwner
	}
	if role := orgs.Role(o.ID, "mallory@x.io"); role != "" {
		t.Errorf("a stranger promoted herself to %q by writing into a map the registry handed out", role)
	}

	projs, err := OpenProjectDB(filepath.Join(t.TempDir(), "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := projs.GetOrCreate("wiki", o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := projs.SetPerm(p.ID, "bob@x.io", PermRead); err != nil {
		t.Fatal(err)
	}
	if p.Perms != nil { // the value GetOrCreate returned
		p.Perms["mallory@x.io"] = PermAdmin
	}
	g2, _ := projs.Get(p.ID)
	if g2.Perms == nil {
		t.Fatal("Get returned a project with a nil Perms map after SetPerm")
	}
	g2.Perms["mallory@x.io"] = PermAdmin
	for _, lp := range projs.List() {
		if lp.Perms != nil {
			lp.Perms["mallory@x.io"] = PermAdmin
		}
	}
	again, _ := projs.Get(p.ID)
	if lvl := again.Perms["mallory@x.io"]; lvl != "" {
		t.Errorf("a stranger granted herself %q by writing into a Perms map the registry handed out", lvl)
	}
}

// secfixFailingOrgRepo refuses every write after arm() — the state a store
// enters when its disk fills or its database drops the connection.
type secfixFailingOrgRepo struct {
	mu     sync.Mutex
	armed  bool
	orgs   map[string]Org
	invite map[string]OrgInvite
}

func (r *secfixFailingOrgRepo) arm(v bool) { r.mu.Lock(); r.armed = v; r.mu.Unlock() }
func (r *secfixFailingOrgRepo) fail() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.armed }

func (r *secfixFailingOrgRepo) Load() ([]Org, []OrgInvite, error) { return nil, nil, nil }
func (r *secfixFailingOrgRepo) PutOrg(o Org) error {
	if r.fail() {
		return fmt.Errorf("store is down")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.orgs == nil {
		r.orgs = map[string]Org{}
	}
	r.orgs[o.ID] = o.clone()
	return nil
}
func (r *secfixFailingOrgRepo) DeleteOrg(id string) error { return nil }
func (r *secfixFailingOrgRepo) PutInvite(i OrgInvite) error {
	if r.fail() {
		return fmt.Errorf("store is down")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.invite == nil {
		r.invite = map[string]OrgInvite{}
	}
	r.invite[i.Token] = i
	return nil
}
func (r *secfixFailingOrgRepo) DeleteInvite(token string) error {
	if r.fail() {
		return fmt.Errorf("store is down")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.invite, token)
	return nil
}

// putOrg's rollback is a read-modify-write on a shared map. Prove it is race
// free and that a refused write leaves the registry agreeing with the store,
// while other goroutines are reading and writing the same org. Run with -race.
func TestSec_DB_RollbackHoldsUnderConcurrentMutators(t *testing.T) {
	repo := &secfixFailingOrgRepo{}
	db, err := NewOrgDB(repo)
	if err != nil {
		t.Fatal(err)
	}
	o, err := db.Create("acme", "alice@x.io")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []string{"bob@x.io", "carol@x.io", "dave@x.io"} {
		if err := db.AddMember(o.ID, e, RoleMember); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				db.Role(o.ID, "bob@x.io")
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				db.Get(o.ID)
				db.OrgsFor("bob@x.io")
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				repo.arm(j%2 == 0)
				db.SetRole(o.ID, "bob@x.io", RoleOwner)
				db.SetRole(o.ID, "bob@x.io", RoleMember)
			}
		}()
	}
	wg.Wait()
	repo.arm(true) // every write from here on is refused

	before := db.Role(o.ID, "carol@x.io")
	if err := db.SetRole(o.ID, "carol@x.io", RoleOwner); err == nil {
		t.Fatal("a refused store write reported success")
	}
	if after := db.Role(o.ID, "carol@x.io"); after != before {
		t.Errorf("a refused write stuck in memory: role %q -> %q; the hub grants it until a restart silently takes it back",
			before, after)
	}
	repo.arm(false)
	// And the store must agree with what the registry now believes.
	repo.mu.Lock()
	stored := repo.orgs[o.ID].Members["carol@x.io"]
	repo.mu.Unlock()
	if stored != db.Role(o.ID, "carol@x.io") {
		t.Errorf("registry and store disagree after the rollback: memory=%q disk=%q",
			db.Role(o.ID, "carol@x.io"), stored)
	}
}

// ---- fix 7: joinMu and underRoot ----

// The seat check is atomic only if every path that adds a member goes through
// it. Prove no other route can grow an org's membership: the owner-facing
// member routes must refuse to CREATE a member, since only handleInviteAccept
// holds joinMu and only it calls CheckSeat.
func TestSec_Invite_NoRouteAddsAMemberOutsideTheSeatLock(t *testing.T) {
	h, srv, c, p := permHub(t)

	// dave is an outsider. An owner must not be able to conjure him in.
	if rec := doAs(t, h, "PATCH", "/api/orgs/"+p.Org+"/members/dave%40x.io",
		map[string]string{"role": RoleMember}, c["alice"]); rec.Code == 200 {
		t.Errorf("PATCH members/{email} added a brand-new member outside the seat lock: %s", rec.Body)
	}
	if role := srv.Dir.Role(p.Org, "dave@x.io"); role != "" {
		t.Errorf("dave is now %q in an org he was never invited to", role)
	}
}

// underRoot resolves symlinks and refuses a write that lands outside the
// served folder. But DirSource.Upload calls os.MkdirAll BEFORE it asks — so
// the refusal happens after the filesystem has already been changed. On a
// single-volume `--upload` server (which is auth-free) that is an unauthenticated
// stranger creating directories anywhere the hub process can write, through
// any symlink that happens to sit in the served folder.
func TestSec_Path_RefusedUploadCreatesNothingOutsideTheServedFolder(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	h := (&Server{Source: &DirSource{Root: root}, Volume: "local", Refresh: 0,
		Upload: UploadConfig{Enabled: true}}).Handler()

	// Control: a normal upload works, so the route is live.
	if rec := doAs(t, h, "PUT", "/api/upload/content?path=ok.txt", []byte("fine"), nil); rec.Code != 200 {
		t.Fatalf("control upload: %d %s", rec.Code, rec.Body)
	}

	rec := doAs(t, h, "PUT", "/api/upload/content?path=link/pwned/deep/x.txt", []byte("ESCAPED"), nil)
	if rec.Code == 200 {
		t.Fatalf("the escaping upload succeeded: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned", "deep")); err == nil {
		t.Errorf("a REFUSED upload still created %s — os.MkdirAll runs before underRoot,\n"+
			"  so the guard rejects the write after the filesystem has already changed",
			filepath.Join(outside, "pwned", "deep"))
	}
}

// The final path component is where a symlink does the most damage: underRoot
// only resolves the PARENT. Writing onto a symlinked filename must replace the
// link, never follow it into the target.
func TestSec_Path_UploadOntoASymlinkedNameDoesNotFollowIt(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(root, "note.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	h := (&Server{Source: &DirSource{Root: root}, Volume: "local", Refresh: 0,
		Upload: UploadConfig{Enabled: true}}).Handler()

	doAs(t, h, "PUT", "/api/upload/content?path=note.txt", []byte("OVERWRITTEN"), nil)
	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ORIGINAL" {
		t.Errorf("an upload followed a symlinked filename and rewrote %s: %q", victim, data)
	}
}
