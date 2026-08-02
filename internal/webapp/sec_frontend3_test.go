package webapp

// Round 13 — the surfaces nobody has ever *rendered*.
//
// The browser half of this round lives in
// internal/webapp/frontend/e2e/sec13fe.spec.ts. This file holds everything the
// server decides on its own, where a Go test is the honest instrument:
//
//   1. The device-approval page as a DOCUMENT. It is the entire consent surface
//      for minting a device credential, and every fact on it — the device name,
//      the OS — is a string the REQUESTER chose over an unauthenticated route.
//      The hub already has a choke point for peer-written display text
//      (projects.go:trimText, which account display names were routed through in
//      an earlier round precisely so a second rule would not grow). This page's
//      inputs never reach it.
//
//   2. The device registry's name, which reaches the same reader through
//      History. printableOnly() is C0+DEL only; the bidi and zero-width runes
//      trimText refuses go straight through.
//
//   3. POST /api/admin/policy, named "thin" by the round-12 report: three files
//      touch it, none drives a change and re-probes signup. The startup
//      validator refuses one specific hub posture; the live setter does not.
//
// Slug for every helper here: sec13fe.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// ------------------------------------------------------------------ helpers

// sec13feGet fetches a page as a signed-in browser.
func sec13feGet(t *testing.T, h http.Handler, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// sec13feLink turns the verify_url the CLI is handed into the path a browser
// would open.
func sec13feLink(t *testing.T, verifyURL string) string {
	t.Helper()
	i := strings.Index(verifyURL, "/auth/device/")
	if i < 0 {
		t.Fatalf("verify_url %q does not carry an /auth/device/ link", verifyURL)
	}
	return verifyURL[i:]
}

// sec13feBadRunes is the set trimText already refuses in a project name and an
// account display name, minus the ASCII quote and the path separators (which
// are a prompt-assembly rule, not a rendering one). Every one of these changes
// what a human READS without changing the bytes they could inspect:
//
//	U+202E RIGHT-TO-LEFT OVERRIDE  — reverses the run that follows it
//	U+2066..U+2069 isolates        — same, scoped
//	U+200B/U+200D/U+FEFF           — invisible; two names render identically
//	U+0085 NEL, U+009B CSI         — C1; a terminal reading the same string acts on them
//	U+0000..U+001F, U+007F         — C0 and DEL
//	U+2028/U+2029                  — forced line breaks in CSS Text
var sec13feBadRunes = []struct {
	r    rune
	name string
}{
	{0x202e, "U+202E RIGHT-TO-LEFT OVERRIDE"},
	{0x2066, "U+2066 LEFT-TO-RIGHT ISOLATE"},
	{0x200b, "U+200B ZERO WIDTH SPACE"},
	{0x200d, "U+200D ZERO WIDTH JOINER"},
	{0xfeff, "U+FEFF ZERO WIDTH NO-BREAK SPACE"},
	{0x0085, "U+0085 NEXT LINE (C1)"},
	{0x009b, "U+009B CONTROL SEQUENCE INTRODUCER (C1)"},
	{0x2028, "U+2028 LINE SEPARATOR"},
	{0x0007, "U+0007 BEL (C0)"},
	{0x001b, "U+001B ESC (C0)"},
	{0x007f, "U+007F DELETE"},
}

// ---------------------------------------------------------------------------
// 1. The device-approval page as a document.
//
// The threat this page exists for is stated in its own doc comment
// (authcli.go:pageDevice): "a stranger can send you their own pending link, and
// a page that just says Approve gives you nothing to notice with". The device
// name, the OS and the address ARE the notice. Two of the three are chosen by
// whoever called POST /api/auth/device/start, which needs no account at all.
//
// html.EscapeString (authcli.go:rows) stops markup. It does not stop text that
// renders as something other than itself. A name of
//
//	"macbook-pro‮ gnitset-hcnarb"
//
// paints on the consent page as "macbook-pro branch-testing" — the reversal is
// invisible and the bytes are unremarkable — and there is no second place on
// this page for the reader to cross-check it against.
func TestSec_DeviceConsent_RequesterChosenNameRendersControlAndBidiRunes(t *testing.T) {
	h, _, cookie := secdevHub(t)

	// CONTROL: the page renders at all, and it renders the name it was given.
	// Without this the assertions below would pass against an error page.
	_, benignURL := secdevStart(t, h, "macbook-pro", "darwin")
	benign := sec13feGet(t, h, sec13feLink(t, benignURL), cookie)
	if benign.Code != http.StatusOK {
		t.Fatalf("control: consent page for a benign device name: %d %s", benign.Code, benign.Body)
	}
	for _, want := range []string{"Connect a device", "macbook-pro", "darwin", "Approve"} {
		if !strings.Contains(benign.Body.String(), want) {
			t.Fatalf("control: consent page did not render %q — the fixture, not the finding, is wrong:\n%s",
				want, benign.Body)
		}
	}

	// ATTACK: the same page, with the same route, for a name the requester
	// chose out of the set the hub already refuses everywhere a name is shown.
	for _, bad := range sec13feBadRunes {
		t.Run(fmt.Sprintf("%U", bad.r), func(t *testing.T) {
			name := "macbook-pro" + string(bad.r) + "kcatta"
			os := "darwin" + string(bad.r) + "xunil"
			_, verify := secdevStart(t, h, name, os)
			rec := sec13feGet(t, h, sec13feLink(t, verify), cookie)
			if rec.Code != http.StatusOK {
				t.Fatalf("consent page: %d %s", rec.Code, rec.Body)
			}
			body := rec.Body.String()
			if !strings.Contains(body, "Approve") {
				t.Fatalf("consent page did not render (no Approve button) — fixture problem:\n%s", body)
			}
			// The SECURE behaviour: the rune never reaches the document. The
			// page may show the name, truncated or with the rune dropped; it
			// may not hand the reader a string that renders as a lie.
			if strings.ContainsRune(body, bad.r) {
				t.Errorf("the device-approval page renders %s verbatim.\n"+
					"POST /api/auth/device/start is unauthenticated, so a stranger chooses this text,\n"+
					"and this page is the ONLY thing a human has to decide on before a device token is minted.\n"+
					"Project names and account display names are routed through projects.go:trimText\n"+
					"for exactly this reason (see the comment on BuiltinAuth.createAccount); device\n"+
					"name and OS are not.\ndevice=%q os=%q", bad.name, name, os)
			}
		})
	}
}

// The same route, the same page, the other half of the input: length. trimText
// caps a project name at 128 runes and an account name at 128. device/start
// accepts a 64 KiB body and records whatever fits, and the consent page renders
// all of it — so a stranger's link can bury the OS and Address rows, and the
// Approve button, below however much text they like.
func TestSec_DeviceConsent_RequesterChoosesHowLongTheConsentPageIs(t *testing.T) {
	h, _, cookie := secdevHub(t)

	_, shortURL := secdevStart(t, h, "laptop", "darwin")
	short := sec13feGet(t, h, sec13feLink(t, shortURL), cookie)
	if short.Code != http.StatusOK || !strings.Contains(short.Body.String(), "Approve") {
		t.Fatalf("control: short consent page did not render: %d %s", short.Code, short.Body)
	}

	huge := strings.Repeat("A", 32<<10)
	_, hugeURL := secdevStart(t, h, huge, "darwin")
	rec := sec13feGet(t, h, sec13feLink(t, hugeURL), cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("consent page for a long device name: %d", rec.Code)
	}
	grew := rec.Body.Len() - short.Body.Len()
	// 512 bytes of slack: a bounded name is allowed to be longer than "laptop".
	if grew > 512 {
		t.Errorf("a stranger's device name grew the consent page by %d bytes (32 KiB of 'A' in, %d out).\n"+
			"The name row is the disclosure; whoever chooses it also chooses how far down the page\n"+
			"the OS row, the Address row and the Approve button end up. trimText caps a project name\n"+
			"at 128 runes; nothing caps this.", grew, rec.Body.Len())
	}
}

// ---------------------------------------------------------------------------
// 2. The device registry's name, which reaches a project member through
// History. deviceFromRequest runs X-Bdrive-Device-Name through printableOnly,
// which drops C0 and DEL and nothing else — so the bidi overrides and the
// zero-widths trimText refuses land in every History row that names the device.
//
// The reader here is a project member looking at who changed a file. A device
// named "alice-laptop<RLO>..." is a row that says one thing and means another,
// and unlike a project name it can be planted by any account with a token.
func TestSec_DeviceRegistry_DeviceNameCarriesBidiIntoHistory(t *testing.T) {
	h, _, c, p := permHub(t)

	// CONTROL: a benign device name reaches History. If this does not hold, the
	// attack assertion below is measuring the wrong thing.
	sec13feSync(t, h, p.ID, c["bob"], "bob-dev-benign", "bob-laptop", "darwin", "notes/a.md")
	if got := sec13feHistoryBody(t, h, p.ID, c["bob"]); !strings.Contains(got, "bob-laptop") {
		t.Fatalf("control: History does not carry the device name at all:\n%s", got)
	}

	for _, bad := range sec13feBadRunes {
		t.Run(fmt.Sprintf("%U", bad.r), func(t *testing.T) {
			name := "bob-laptop" + string(bad.r) + "detsurt"
			id := fmt.Sprintf("bob-dev-%04x", bad.r)
			sec13feSync(t, h, p.ID, c["bob"], id, name, "darwin", "notes/"+id+".md")
			body := sec13feHistoryBody(t, h, p.ID, c["bob"])
			if !strings.Contains(body, "bob-laptop") {
				t.Fatalf("History did not render any device name — fixture problem:\n%s", body)
			}
			// The API returns JSON, so the rune arrives either literally or as
			// a \uXXXX escape; both put it in the browser's string.
			esc := fmt.Sprintf(`\u%04x`, bad.r)
			if strings.ContainsRune(body, bad.r) || strings.Contains(strings.ToLower(body), esc) {
				t.Errorf("History serves a device name carrying %s.\n"+
					"devices.go:printableOnly drops C0 and DEL only; trimText — the rule project names\n"+
					"and account display names go through — refuses this rune, and the History row is\n"+
					"read by the same people in the same list.\nname=%q", bad.name, name)
			}
		})
	}
}

// sec13feSync binds a device to bob's account the way `bdrive login` does
// (which is what stamps its name into the registry) and then pushes its own
// journal, leaving one op behind for History to render.
func sec13feSync(t *testing.T, h http.Handler, projectID string, c *http.Cookie, id, name, os, path string) {
	t.Helper()
	if rec := secRegisterDevice(t, h, projectID, c, id, name, os); rec.Code != http.StatusOK {
		t.Fatalf("binding device %q: %d %s", id, rec.Code, rec.Body)
	}
	line := secaudOpLine(1, id, "put", path, strings.Repeat("a", 64))
	req := httptest.NewRequest(http.MethodPut,
		"/api/p/"+projectID+"/store/object?key=journal/"+id+".jsonl", strings.NewReader(line))
	req.Header.Set("X-Bdrive-Device", id)
	req.Header.Set("X-Bdrive-Device-Name", name)
	req.Header.Set("X-Bdrive-Os", os)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code >= 300 {
		t.Fatalf("journal push for %q: %d %s", id, rec.Code, rec.Body)
	}
}

func sec13feHistoryBody(t *testing.T, h http.Handler, projectID string, c *http.Cookie) string {
	t.Helper()
	rec := sec13feGet(t, h, "/api/p/"+projectID+"/history?prefix=", c)
	if rec.Code != http.StatusOK {
		t.Fatalf("history: %d %s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// ---------------------------------------------------------------------------
// 3. POST /api/admin/policy — the live setter versus the startup validator.
//
// BuiltinAuth.ValidateSignupPolicy refuses to START a hub whose signup is open
// with no gate at all ("anyone could register any email"). CLAUDE.md states the
// posture as a guarantee: the hub "refuses an ungated open hub ... rather than
// silently leaving the door open".
//
// SetPolicy — the only runtime writer, reachable from a browser session — never
// consults it. A hub that booted legally as {allow_signup:true, no domains,
// require_approval:true} can be moved, from a browser, into the exact posture
// the same binary refuses to boot in. The gate is then gone, and it is gone
// across a restart too, because SetPolicy persists first.
//
// The admin is trusted to LOOSEN the two toggles. What they are not able to do
// from a browser — per the same doc comment on handleAdminPolicy, "so a browser
// session can't widen access" — is arrive at a configuration the server itself
// calls ungated.
func TestSec_AdminPolicy_LiveChangeReachesTheUngatedHubStartupRefuses(t *testing.T) {
	auth, err := OpenBuiltinAuth(filepath.Join(t.TempDir(), "auth.json"), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	auth.AllowSignup = true          // open signup …
	auth.AllowedDomains = nil        // … with no domain allowlist …
	auth.RequireApproval = true      // … gated only by approval.
	auth.RequireVerification = false // (no mailer, so this cannot be the gate)
	auth.Admins = map[string]bool{"root@x.io": true}

	// This posture is legal at startup — the control for the whole test.
	if err := auth.ValidateSignupPolicy(); err != nil {
		t.Fatalf("control: the starting posture should boot, but ValidateSignupPolicy says: %v", err)
	}

	srv, _, _ := newHub(t, true, nil)
	srv.Auth = auth
	h := srv.Handler()
	root := signupAndSession(t, h, "root@x.io", "Root", "password1")

	// CONTROL: root really is a hub admin on this hub — the route answers.
	if rec := sec13feGet(t, h, "/api/admin/policy", root); rec.Code != http.StatusOK {
		t.Fatalf("control: admin cannot read the policy: %d %s", rec.Code, rec.Body)
	}

	// The one move: turn the only gate off, from a browser.
	rec := secdevPost(t, h, "/api/admin/policy",
		map[string]bool{"require_verification": false, "require_approval": false}, root)

	if rec.Code == http.StatusOK {
		// The hub is now in a state it would refuse to boot in. Say so with the
		// validator's own words rather than asserting a posture by hand.
		if err := auth.ValidateSignupPolicy(); err != nil {
			// And the gate really is gone: a stranger self-registers active.
			u, serr := auth.signup("stranger@anywhere.example", "S", "password1")
			state := "signup refused: " + fmt.Sprint(serr)
			if serr == nil {
				state = "account status = " + u.Status
			}
			t.Fatalf("POST /api/admin/policy returned 200 and left the hub in a posture "+
				"the same binary refuses to start in.\n"+
				"ValidateSignupPolicy on the resulting config: %v\n"+
				"A stranger signing up afterwards: %s\n"+
				"handleAdminPolicy validates require_verification against the mailer and nothing else; "+
				"SetPolicy persists and never re-runs ValidateSignupPolicy.", err, state)
		}
	}
	// The SECURE behaviour: the change is refused (4xx) and the gate survives.
	if rec.Code < 400 {
		t.Errorf("policy change accepted with %d; want a refusal", rec.Code)
	}
	if err := auth.ValidateSignupPolicy(); err != nil {
		t.Errorf("after the change the hub no longer satisfies its own startup rule: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 4. HubSettings' server half, asserted so the browser spec has a floor.
//
// The component returns null until /api/admin/policy answers and on any error,
// so a non-admin who reaches the panel sees an empty pane. That is only safe if
// the two routes behind it really refuse — including the one that carries the
// hub's admin roster. Round 3 proved the /api/admin/* gate; this pins the
// specific thing HubSettings would have rendered: the admin email list and the
// pending-account queue must not reach a plain member by any shape.
func TestSec_HubSettings_MemberReachesNoAdminRosterOrQueue(t *testing.T) {
	auth, err := OpenBuiltinAuth(filepath.Join(t.TempDir(), "auth.json"), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	auth.Admins = map[string]bool{"root@x.io": true}
	auth.RequireApproval = true
	srv, _, _ := newHub(t, true, nil)
	srv.Auth = auth
	h := srv.Handler()

	root := signupAndSession(t, h, "root@x.io", "Root", "password1")
	// A pending account for the queue to hold.
	if _, err := auth.signup("waiting@x.io", "Waiting", "password1"); err != nil {
		t.Fatal(err)
	}
	// The member signs up while the gate is open, then the gate goes back on —
	// so "turn require_approval off" is a real widening move for them to try.
	auth.RequireApproval = false
	member := signupAndSession(t, h, "member@x.io", "Member", "password1")
	auth.RequireApproval = true

	// CONTROL: an admin sees both, so "not present" below means refused, not absent.
	pol := sec13feGet(t, h, "/api/admin/policy", root)
	if pol.Code != http.StatusOK || !strings.Contains(pol.Body.String(), "root@x.io") {
		t.Fatalf("control: admin's own policy read does not carry the roster: %d %s", pol.Code, pol.Body)
	}
	pend := sec13feGet(t, h, "/api/admin/pending", root)
	if pend.Code != http.StatusOK || !strings.Contains(pend.Body.String(), "waiting@x.io") {
		t.Fatalf("control: admin's pending queue is empty: %d %s", pend.Code, pend.Body)
	}

	for _, path := range []string{"/api/admin/policy", "/api/admin/pending"} {
		rec := sec13feGet(t, h, path, member)
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET %s as a plain member: %d, want 403 — body %s", path, rec.Code, rec.Body)
		}
		for _, leak := range []string{"root@x.io", "waiting@x.io"} {
			if strings.Contains(rec.Body.String(), leak) {
				t.Errorf("GET %s as a plain member leaked %q", path, leak)
			}
		}
	}
	// And the write half, which is what HubSettings' Save button posts.
	if rec := secdevPost(t, h, "/api/admin/policy",
		map[string]bool{"require_approval": false}, member); rec.Code != http.StatusForbidden {
		t.Errorf("POST /api/admin/policy as a plain member: %d, want 403", rec.Code)
	}
	// And the refusal was real, not cosmetic: the toggle did not move.
	var got SignupPolicy
	if err := json.Unmarshal(sec13feGet(t, h, "/api/admin/policy", root).Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.RequireApproval {
		t.Errorf("a plain member's POST turned require_approval off — the hub's signup gate " +
			"moved on a browser session with no admin rights")
	}
}
