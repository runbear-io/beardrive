package webapp

// Round 8 — the target is round 7's fixes (382882b) in internal/webapp:
// mailBaseURL's new "stop trusting request hosts once two disagree" rule,
// CLIAuth's grant bound (sweepLocked + the hub-wide/per-IP caps), the
// unchanged apiDevicePoll it sits next to, and OrgDB.EvictMember's
// unconditional drop.
//
// Every test asserts the SECURE behaviour, so it goes green the moment the
// hole is closed and stays as a permanent regression test. Helpers are
// prefixed secfx7; no existing file is touched.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// ---- helpers -------------------------------------------------------------

// secfx7JSON posts JSON from a chosen source address. The device flow's only
// bound is per-IP, and requestIP reads RemoteAddr, so the address is the
// variable the attack turns on.
func secfx7JSON(h http.Handler, remoteAddr, path string, body any) *httptest.ResponseRecorder {
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest("POST", path, rd)
	req.Header.Set("Content-Type", "application/json")
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// secfx7StartDevice runs one /api/auth/device/start and returns its grant code.
func secfx7StartDevice(t *testing.T, h http.Handler, addr, device string) string {
	t.Helper()
	rec := secfx7JSON(h, addr, "/api/auth/device/start", map[string]string{"device": device, "os": "linux"})
	if rec.Code != 200 {
		t.Fatalf("device start from %s: %d %s", addr, rec.Code, rec.Body)
	}
	var out struct {
		Code string `json:"code"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Code == "" {
		t.Fatalf("device start returned no code: %s", rec.Body)
	}
	return out.Code
}

// secfx7Approve drives the real approval page as a signed-in browser would.
func secfx7Approve(t *testing.T, h http.Handler, code string, c *http.Cookie) {
	t.Helper()
	req := httptest.NewRequest("POST", "/auth/device/"+code, strings.NewReader(url.Values{"approve": {"1"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Device connected") {
		t.Fatalf("approval page: %d %s", rec.Code, rec.Body)
	}
}

// secfx7DeviceShown reads the device label the approval page put in front of
// the human, which is the whole consent surface of the headless flow.
func secfx7DeviceShown(t *testing.T, h http.Handler, code string, c *http.Cookie) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/auth/device/"+code, nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("approval page GET: %d %s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// ---------------------------------------------------------------------------
// 1. mailBaseURL: the first mail a fresh process sends.
// ---------------------------------------------------------------------------

// Round 6 pinned the first host the hub was reached on. Round 7 kept the pin
// and added: once a second host disagrees with it, mailed links go out
// root-relative rather than aimed somewhere a stranger picked.
//
// That is the SECOND mail onwards. The pin itself is still taken from r.Host
// on the request that mails FIRST, and it is still used for that very mail —
// so on a fresh process (a deploy, a restart, a crash-loop, a scaled-out
// replica that has just come up) the first request to reach /auth/reset
// chooses both the host and the recipient. An unauthenticated stranger who
// knows one live address — the org owner's, which every mailed invite and
// every commit in History already shows — gets the hub to mail THAT PERSON a
// genuine, valid reset link on the attacker's server.
//
// Round 6's own reproducer cannot see this: it sends the honest request first,
// as a control, which is what pins the base. Round 7's successor test
// (TestSec_Mail_AMemberCannotPinTheHostEveryResetLinkPointsAt) mails to
// mallory herself on the poisoned request. Nobody has yet asked what the
// victim receives when the ATTACKER's request is the first one.
func TestSec_Mail_TheFirstLinkAFreshHubMailsCannotBeAimedAtAnAttackerChosenHost(t *testing.T) {
	box := secapiSMTP(t)
	a, _, h := secapiAuth(t, box.mailer())
	if _, err := a.signup("victim@x.io", "Victim", "password1"); err != nil {
		t.Fatal(err)
	}
	if a.BaseURL != "" {
		t.Fatalf("control: auth.base_url is set, which is the configuration the fix covers")
	}

	// The whole attack: one anonymous POST, before any honest traffic mails
	// anything, naming the victim's address and the attacker's host.
	secapiForm(h, "evil.example", "/auth/reset", url.Values{"email": {"victim@x.io"}})

	got := box.next(t, 0)
	if !strings.Contains(got, "/auth/reset/confirm?token=") {
		t.Fatalf("control: no reset link in the mail at all, so nothing was tested:\n%s", got)
	}
	if strings.Contains(got, "evil.example") {
		t.Errorf("the reset mail DELIVERED TO victim@x.io carries a link on a host the "+
			"anonymous requester chose:\n%s\n"+
			"mailBaseURL still seeds its pin from r.Host and returns it for the same request, so "+
			"the first request that mails anything after a restart both picks the origin and "+
			"picks who receives it. The link is a valid single-use password-reset grant for "+
			"someone else's account, handed to the attacker's server when the victim clicks it.", got)
	}
}

// The other half of the same rule, in the other direction. Once the pin is
// taken, a host that disagrees with it makes mailBaseURL return "" — so the
// link goes out root-relative. That is unusable in a mail client, and the
// stranger who set the pin decides which host is the disagreeing one.
//
// Attacker first (the test above), and from then on every honest mail the hub
// sends — reset links, verification links — is originless for the life of the
// process. One unauthenticated request turns password recovery off hub-wide
// until someone notices and restarts, and a restart re-opens the same race.
func TestSec_Mail_AStrangerCannotStripTheOriginFromEveryLaterMailedLink(t *testing.T) {
	box := secapiSMTP(t)
	a, _, h := secapiAuth(t, box.mailer())
	// The hub's own origin, configured — the only thing a mailed link may be
	// built from, and required whenever smtp is set (ValidateSignupPolicy).
	a.BaseURL = "https://hub.example"
	for _, who := range []string{"victim", "mallory"} {
		if _, err := a.signup(who+"@x.io", who, "password1"); err != nil {
			t.Fatal(err)
		}
	}

	// One anonymous request, for the attacker's own account, on her own host.
	secapiForm(h, "evil.example", "/auth/reset", url.Values{"email": {"mallory@x.io"}})
	if got := box.next(t, 0); strings.Contains(got, "evil.example") {
		// The pin this control was written to establish no longer exists: no
		// request host is ever mailed, so her own mail must not carry hers.
		t.Errorf("mallory's own mail carries the host she chose:\n%s", got)
	}

	// Now every honest request, on the hub's real origin, forever.
	secapiForm(h, "hub.example", "/auth/reset", url.Values{"email": {"victim@x.io"}})
	got := box.next(t, 1)
	if !strings.Contains(got, "://hub.example/auth/reset/confirm?token=") {
		t.Errorf("the honest reset mail no longer carries an absolute link on the hub's own "+
			"host:\n%s\n"+
			"mailBaseURL returns \"\" as soon as a second host disagrees with the pin, and the "+
			"pin was taken from an anonymous stranger's Host header. Every mailed link for the "+
			"rest of the process's life is root-relative and unusable from a mail client — one "+
			"unauthenticated request disables password recovery for the whole hub.", got)
	}
}

// ---------------------------------------------------------------------------
// 2. The device-flow bound: the outage the per-IP cap was chosen to avoid.
// ---------------------------------------------------------------------------

// Round 7's own note: "a hub-wide cap alone converts 'a stranger exhausts
// memory' into 'a stranger denies every bdrive login --device on the hub' —
// the same outage bought more cheaply. Bounding per origin keeps a flood
// inside the address that sent it."
//
// It does not. maxPendingPerIP is 256 and maxPendingGrants is 512, so the
// hub-wide ceiling is reached by TWO addresses — one extra container, one
// extra address out of a /64 — and grants live ten minutes with no rate limit
// on the route that creates them. From then on every `bdrive login --device`
// on the hub, from every address, gets 503. The per-IP cap did not keep the
// flood inside its origin; it only set the price at two addresses instead of
// one.
func TestSec_DeviceFlow_TwoAddressesCannotDenyEveryDeviceLoginOnTheHub(t *testing.T) {
	_, _, h := secapiAuth(t, nil)

	// Control: the flow works for an ordinary user before the flood.
	secfx7StartDevice(t, h, "198.51.100.9:5000", "honest-laptop")

	filled := 0
	for _, addr := range []string{"203.0.113.1", "203.0.113.2"} {
		for i := 0; i < maxPendingPerIP; i++ {
			rec := secfx7JSON(h, fmt.Sprintf("%s:%d", addr, 1024+i),
				"/api/auth/device/start", map[string]string{"device": "flood", "os": "linux"})
			if rec.Code != 200 {
				break
			}
			filled++
		}
	}
	if filled < maxPendingGrants-8 {
		t.Fatalf("control: two addresses only placed %d grants, so the hub-wide cap of %d was "+
			"never reached and this test proves nothing", filled, maxPendingGrants)
	}

	// An untouched third address, an ordinary user running `bdrive login --device`.
	rec := secfx7JSON(h, "198.51.100.9:5001",
		"/api/auth/device/start", map[string]string{"device": "honest-laptop", "os": "macos"})
	if rec.Code == http.StatusServiceUnavailable {
		t.Errorf("`bdrive login --device` from an uninvolved address is refused (%d %s) after "+
			"two addresses placed %d pending grants.\n"+
			"maxPendingPerIP=%d and maxPendingGrants=%d, so the hub-wide ceiling is two "+
			"addresses away; /api/auth/device/start is unauthenticated and unmetered, and "+
			"grants live ten minutes. The per-IP cap was added specifically to stop one "+
			"stranger denying the headless sign-in flow to everybody — at 2x it does not.",
			rec.Code, strings.TrimSpace(rec.Body.String()), filled, maxPendingPerIP, maxPendingGrants)
	}
}

// ---------------------------------------------------------------------------
// 3. apiDevicePoll: what the human approved is not what is issued.
// ---------------------------------------------------------------------------

// The headless flow's entire defence is the approval page: "the account, the
// device name, its OS and its address are the only things standing between an
// approval and a stranger's pending link" (pageDevice's own comment). The page
// renders g.device, recorded at /start.
//
// apiDevicePoll then mints the token under req.Device, chosen at POLL time,
// after the human has already clicked Approve. So the label the human read is
// not the label the credential carries: the token row, the device list, and
// anything an operator later revokes by device all name a string the approver
// never saw. A stranger who talks a victim into approving one link ("it's my
// build box, name's right there") lands a token labelled as the victim's own
// laptop.
func TestSec_DeviceFlow_TheApprovedDeviceIsTheOneTheTokenIsBoundTo(t *testing.T) {
	a, _, h := secapiAuth(t, nil)
	c := signupAndSession(t, h, "owner@x.io", "Owner", "password1")

	code := secfx7StartDevice(t, h, "203.0.113.7:2000", "build-box-ci")

	page := secfx7DeviceShown(t, h, code, c)
	if !strings.Contains(page, "build-box-ci") {
		t.Fatalf("control: the approval page never showed the device name it recorded:\n%s", page)
	}
	secfx7Approve(t, h, code, c)

	// The poll — from the same machine that started it — names a different
	// device entirely.
	rec := secfx7JSON(h, "203.0.113.7:2001", "/api/auth/device/poll",
		map[string]string{"code": code, "device": "owner-macbook"})
	if rec.Code != 200 {
		t.Fatalf("poll: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Token string `json:"token"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Token == "" {
		t.Fatalf("poll returned no token: %s", rec.Body)
	}

	a.mu.Lock()
	got := a.tokens[hashToken(out.Token)].Device
	a.mu.Unlock()
	if got != "build-box-ci" {
		t.Errorf("the approval page showed %q and the credential was issued to %q.\n"+
			"apiDevicePoll takes req.Device from the POLL, which happens after the human has "+
			"clicked Approve — so the device identity on the page is advisory and the one on "+
			"the token row (what an operator sees in the device list, and what a revocation "+
			"names) is whatever the requesting machine says at the last moment.",
			"build-box-ci", got)
	}
}

// One approval is one credential. apiDevicePoll peeks, decides, then takes in
// a SECOND lock acquisition and DISCARDS take's return value:
//
//	g, ok := c.peek("device", req.Code)   // lock, unlock
//	if !g.granted { ... }
//	c.take("device", req.Code)            // lock, unlock — result thrown away
//	c.issue(w, g.user, device)
//
// so two polls that both get past peek both reach issue. That is round 2's
// seat-check race on credential issuance: one human approval, N long-lived
// device tokens, N-1 of them invisible to whoever approved.
func TestSec_DeviceFlow_OneApprovalMintsOneToken(t *testing.T) {
	_, _, h := secapiAuth(t, nil)
	c := signupAndSession(t, h, "owner@x.io", "Owner", "password1")

	// The window between peek releasing the lock and take re-taking it is
	// small, so one approval is raced repeatedly rather than hard. Each round
	// is one full start/approve/poll cycle against the real routes; a hub
	// under any concurrent load at all hits this without trying.
	const rounds, racers = 3000, 32
	for round := 0; round < rounds; round++ {
		code := secfx7StartDevice(t, h, "198.51.100.4:2000", "laptop")
		secfx7Approve(t, h, code, c)

		var wg, start sync.WaitGroup
		start.Add(1)
		tokens := make([]string, racers)
		for i := range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				start.Wait()
				rec := secfx7JSON(h, "198.51.100.4:2001", "/api/auth/device/poll",
					map[string]string{"code": code, "device": "laptop"})
				var out struct {
					Token string `json:"token"`
				}
				json.Unmarshal(rec.Body.Bytes(), &out)
				tokens[i] = out.Token
			}()
		}
		start.Done()
		wg.Wait()

		seen := map[string]bool{}
		for _, tok := range tokens {
			if tok != "" {
				seen[tok] = true
			}
		}
		if len(seen) == 0 {
			t.Fatalf("control: round %d got no token at all, so nothing was raced", round)
		}
		if len(seen) > 1 {
			t.Fatalf("one approved device grant minted %d distinct long-lived tokens from %d "+
				"concurrent polls (round %d of %d).\n"+
				"apiDevicePoll peeks, decides, and only then takes — in a SECOND lock "+
				"acquisition, with take's return value discarded — so consuming the grant is "+
				"not what decides who gets issued. Every extra token is a full-privilege device "+
				"credential nobody approved, that shows up as no second approval anywhere, and "+
				"that survives revoking the one the operator can see.", len(seen), racers, round, rounds)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. EvictMember: an org with no owner at all.
// ---------------------------------------------------------------------------

// Round 7 replaced offboard's RemoveMember (which refuses to drop the last
// owner) with EvictMember (which drops it unconditionally), on the grounds
// that "an org left with no owner is a recovery problem, not an authorization
// one". The removal is right — the address must not keep the grant — but the
// state it leaves behind is reachable and permanent.
//
// Everything that administers an org is gated on RoleOwner: minting invites,
// setting roles, renaming, removing members. With no owner, no request from
// anybody can do any of them. The remaining members keep read/write on the
// projects, so the org is not walled off — it is FROZEN: nobody can be added,
// nobody can be removed (including a member who has since left the company),
// nobody can be promoted, and there is no route that adopts it. A single hub
// admin deleting one account does that to an entire organization, silently.
//
// The secure outcome is that dropping the account leaves the org with somebody
// who can still administer it (or leaves it with nobody at all) — never with
// members and no owner.
func TestSec_Org_EvictingTheSoleOwnerCannotLeaveAnOrgNobodyCanAdminister(t *testing.T) {
	h, srv, c, p := permHub(t)

	orgs := srv.Dir.(LocalDirectory).OrgDB
	if got := srv.Dir.Role(p.Org, "alice@x.io"); got != RoleOwner {
		t.Fatalf("control: alice is %q, not the org owner", got)
	}
	if got := srv.Dir.Role(p.Org, "bob@x.io"); got != RoleMember {
		t.Fatalf("control: bob is %q, not a plain member", got)
	}

	// The hub admin removes alice's account. Deny is the real route: it
	// revokes her tokens and calls Server.offboard, which is what evicts.
	var aliceID string
	for _, u := range srv.Auth.(*BuiltinAuth).Accounts() {
		if u.Email == "alice@x.io" {
			aliceID = u.ID
		}
	}
	if aliceID == "" {
		t.Fatal("control: no account for alice@x.io")
	}
	if err := srv.Auth.(*BuiltinAuth).Deny(aliceID); err != nil {
		t.Fatalf("deny alice: %v", err)
	}
	if got := srv.Dir.Role(p.Org, "alice@x.io"); got != "" {
		t.Fatalf("control: alice still holds %q after her account was removed", got)
	}

	org, ok := orgs.Get(p.Org)
	if !ok {
		t.Fatal("the org disappeared entirely")
	}
	owners := 0
	for _, role := range org.Members {
		if role == RoleOwner {
			owners++
		}
	}
	if len(org.Members) > 0 && owners == 0 {
		// Prove the consequence rather than asserting a shape: nothing the
		// remaining members can send administers this org any more.
		invite := doAs(t, h, "POST", "/api/orgs/"+p.Org+"/invites", map[string]any{}, c["bob"])
		promote := doAs(t, h, "PATCH", "/api/orgs/"+p.Org+"/members/"+url.PathEscape("bob@x.io"),
			map[string]string{"role": RoleOwner}, c["bob"])
		t.Errorf("the org has %d member(s) and no owner after its sole owner's account was "+
			"removed.\n"+
			"  mint an invite as the remaining member: %d %s\n"+
			"  promote the remaining member to owner:  %d %s\n"+
			"offboard now calls EvictMember, which drops the row unconditionally. Every org "+
			"administration route is gated on RoleOwner, so this org can never again gain a "+
			"member, lose one, or change a role — and there is no route that adopts an "+
			"ownerless org. One admin deleting one account freezes an entire organization.",
			len(org.Members), invite.Code, strings.TrimSpace(invite.Body.String()),
			promote.Code, strings.TrimSpace(promote.Body.String()))
	}
}

// ---------------------------------------------------------------------------
// 5. Controls that came back clean, kept as regressions.
// ---------------------------------------------------------------------------

// The /store/* journal door and the browser upload door must agree, which is
// the whole point of collapsing them onto journal.SafePath. This walks the
// same hostile spellings through both and asserts neither takes one.
func TestSec_Path_BothIngestDoorsRefuseTheSameHostilePaths(t *testing.T) {
	h, _, c, p := permHub(t)

	hostile := []string{
		"", ".", "..", "../etc/passwd", "/etc/passwd", "a/../../b", "a//b", "./a",
		"a/", "a/.", "docs/../../x", "nul\x00.md", "del\x7f.md", "tab\tx.md", "nl\nx.md",
	}
	for _, bad := range hostile {
		body := map[string]any{"path": bad, "sha": strings.Repeat("a", 64), "size": 1}
		rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/upload/commit", body, c["alice"])
		if rec.Code == 200 {
			t.Errorf("the upload door accepted %q", bad)
		}
		rec = secfx7StorePut(t, h, p.ID, "m-alicedev", secfx7OpLine(t, bad), c["alice"])
		if rec.Code == 200 {
			t.Errorf("the /store/* journal door accepted an op naming %q while the upload door refused it", bad)
		}
	}
}

// secfx7StorePut pushes a journal object the way the real client does — with
// the X-Bdrive-Device header, which the first-claim rule requires. Without it
// the door answers 400 before it ever looks at the ops.
func secfx7StorePut(t *testing.T, h http.Handler, project, dev, body string, c *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/api/p/"+project+"/store/object?key=journal/"+dev+".jsonl",
		strings.NewReader(body))
	req.Header.Set("X-Bdrive-Device", dev)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func secfx7OpLine(t *testing.T, p string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"seq": 1, "lamport": 1, "time": "2026-01-01T00:00:00Z",
		"device": "m-alicedev", "kind": "put", "path": p, "blob": strings.Repeat("a", 64), "size": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// Round 7 collapsed three spellings of the path rule into journal.SafePath and
// gave the /store/* journal door the result. But the browser door is
// SafePath AND config.ReservedPath:
//
//	if !journal.SafePath(p) { ... }
//	if config.ReservedPath(cl) { ... }   // .git/, .bdrive/, at any depth
//
// and journalOps got only the first half. So the two ingest doors STILL
// disagree, one rule later — the same shape of divergence round 7 closed, on
// the same two doors.
//
// The consequence is not symmetric with an upload, because it is worse: every
// route that could take the entry back out — /remove, /restore, and the share
// mint — runs cleanUploadPath on the path it is given, so all three refuse the
// spelling the journal door accepted. A member plants a tree entry in a
// project that the hub will serve and that no API on the hub can ever remove.
func TestSec_Store_AJournaledPathTheUploadDoorRefusesIsAlsoUnremovable(t *testing.T) {
	h, _, c, p := permHub(t)

	const hostile = ".git/hooks/pre-commit"

	// The browser door, for comparison. It refuses.
	up := doAs(t, h, "POST", "/api/p/"+p.ID+"/upload/commit",
		map[string]any{"path": hostile, "sha": strings.Repeat("a", 64), "size": 1}, c["alice"])
	if up.Code == 200 {
		t.Fatalf("control: the upload door accepted %q, so there is no divergence to measure", hostile)
	}

	// The journal door, same project, same account, same path.
	put := secfx7StorePut(t, h, p.ID, "m-alicedev", secfx7OpLine(t, hostile), c["alice"])
	if put.Code != 200 {
		return // refused: the doors agree, which is the secure outcome
	}

	tree := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, c["alice"])
	if !strings.Contains(tree.Body.String(), hostile) {
		t.Fatalf("control: the op was accepted but never reached the tree: %s", tree.Body)
	}

	rm := doAs(t, h, "POST", "/api/p/"+p.ID+"/remove", map[string]string{"path": hostile}, c["alice"])
	sh := doAs(t, h, "POST", "/api/p/"+p.ID+"/shares", map[string]string{"path": hostile}, c["alice"])
	t.Errorf("the /store/* journal door accepted an op naming %q (%d) that the upload door "+
		"answers %d to, and it is now in the project tree.\n"+
		"  GET /tree:                      %s\n"+
		"  POST /remove for the same path: %d %s\n"+
		"  POST /shares for the same path: %d %s\n"+
		"journalOps applies journal.SafePath and nothing else; cleanUploadPath applies "+
		"journal.SafePath AND config.ReservedPath. Round 7 unified the first rule and left the "+
		"second one spelled on one door only — and because every route that could take the "+
		"entry back out goes through cleanUploadPath, the entry is permanent: the project "+
		"tree carries a .git/ (or .bdrive/) path that the hub serves and that no request can "+
		"delete, un-share or restore over.",
		hostile, put.Code, up.Code, strings.TrimSpace(tree.Body.String()),
		rm.Code, strings.TrimSpace(rm.Body.String()), sh.Code, strings.TrimSpace(sh.Body.String()))
}

// ---------------------------------------------------------------------------
// 6. The read door still first-claims a device id.
// ---------------------------------------------------------------------------

// Round 5's finding was that a handler which REGISTERS the device id it is
// about to judge turns the ownership check into a one-request speed bump. The
// fix moved observeDevice after the decision — on the WRITE doors.
// /store/exists and /store/list still call it first, and they are PermRead:
//
//	func (s *Server) handleStoreExists(v *volume, ...) {
//	    rs := storeSource(v, w)
//	    s.observeDevice(r)      // <- claims the id
//	    ...
//	}
//
// So a read is a claim. Ownership is first-claim-wins hub-wide (OwnerOf picks
// the earliest FirstSeen), and ownJournal refuses a journal push for an id
// another account owns — the refusal explicitly names the outcome it is meant
// to avoid, "a squatted id is a permanent lockout". A member with READ ONLY on
// a project can therefore squat a teammate's device id with one GET, and that
// teammate's device can then never push its journal: not here, and not on any
// other project, because the registry is hub-wide. The victim's only remedy is
// deleting device.json, which orphans every project that device already syncs.
func TestSec_Device_AReadCannotClaimADeviceIdForTheCaller(t *testing.T) {
	h, srv, c, p := permHub(t)

	// bob is demoted to read-only on this project. The store API then offers
	// him exactly one thing: reads.
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions/bob@x.io",
		map[string]string{"level": "read"}, c["alice"]); rec.Code != 200 {
		t.Fatalf("control: could not demote bob: %d %s", rec.Code, rec.Body)
	}
	if rec := secfx7StorePut(t, h, p.ID, "m-bobsown", secfx7OpLine2(t, "m-bobsown", "b.md"), c["bob"]); rec.Code != http.StatusForbidden {
		t.Fatalf("control: read-only bob was not refused a journal write: %d %s", rec.Code, rec.Body)
	}

	const victim = "m-carolbox" // carol's device, which has not synced yet

	// One read, naming carol's device.
	req := httptest.NewRequest("GET", "/api/p/"+p.ID+"/store/exists?key=blobs/"+strings.Repeat("a", 64), nil)
	req.Header.Set("X-Bdrive-Device", victim)
	req.AddCookie(c["bob"])
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("control: read-only bob could not read /store/exists at all: %d %s", rec.Code, rec.Body)
	}

	// Carol's device comes online and pushes its own journal, every op naming
	// itself — the ordinary first sync of a new device.
	push := secfx7StorePut(t, h, p.ID, victim, secfx7OpLine2(t, victim, "carol.md"), c["carol"])
	if push.Code != 200 {
		owner, known := srv.Devices.OwnerOf(victim)
		t.Errorf("carol's device cannot push its own journal: %d %s\n"+
			"  registry says %q owns %q (known=%v)\n"+
			"bob holds READ on this project and wrote nothing. One GET /store/exists carrying "+
			"X-Bdrive-Device: %s registered the id to him, because handleStoreExists calls "+
			"observeDevice BEFORE it decides anything — the exact shape round 5 fixed on the "+
			"write doors and left on the read doors. Device ownership is hub-wide and "+
			"first-claim-wins, so this locks carol's machine out of every project on the hub, "+
			"and ownJournal's own message calls that a permanent lockout.",
			push.Code, strings.TrimSpace(push.Body.String()), owner, victim, known, victim)
	}
}

// secfx7OpLine2 is secfx7OpLine with the device and path both chosen, since a
// first journal claim requires every op to name the device the key does.
func secfx7OpLine2(t *testing.T, dev, p string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"seq": 1, "lamport": 1, "time": "2026-01-01T00:00:00Z",
		"device": dev, "kind": "put", "path": p, "blob": strings.Repeat("a", 64), "size": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

