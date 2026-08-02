package webapp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Round 7 — attacking round 6's own fixes.
//
// Round 6 shipped three things this file goes after:
//
//   - Server.offboard, "one choke point rather than N sweeps", wired into
//     BuiltinAuth.Deny so a removed account's org role and project grants die
//     with it;
//   - BuiltinAuth.mailBaseURL's pin-on-first-use, so a stranger's Host header
//     cannot aim a password-reset link;
//   - cleanUploadPath's control-character refusal, whose stated purpose is to
//     keep a path the metadata backends disagree about out of the hub.
//
// Each is attacked on the path its fix did not cover.

// secfx6UserID resolves an account id from its address, the way an admin
// console would before removing it.
func secfx6UserID(t *testing.T, a *BuiltinAuth, email string) string {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	u := a.findByEmail(email)
	if u == nil {
		t.Fatalf("no account for %s", email)
	}
	return u.ID
}

// TestSec_Offboard_ASoleOwnersGrantsDoNotOutliveHerAccount attacks
// Server.offboard on the one account whose grants matter most.
//
// offboard's teeth are s.Dir.RemoveMember: projectPerm resolves org membership
// BEFORE any grant, so dropping the membership is what actually revokes
// everything. But RemoveMember refuses to remove the last owner of an org
// ("an org must always have someone who can administer it"), and offboard
// treats that refusal as a log line:
//
//	if err := s.Dir.RemoveMember(o.ID, e); err != nil {
//	        log.Printf("beardrive: offboard %s: org %s: %v", e, o.ID, err)
//	}
//
// So for a SOLE org owner — the highest-privilege account on the hub — Deny
// removes the account and every credential it held, and leaves
// Members["alice@x.io"] = "owner" behind. The address is now an ownerless
// grant sitting in orgs.json, and the next account on that address inherits
// org ownership and admin on every project in it. That is verbatim the hole
// round 6 wrote offboard to close ("the next account on that address — a
// re-signup, a redeemed invite, an admin re-adding someone — inherited them,
// project admin included"), still open for the one account it matters for.
func TestSec_Offboard_ASoleOwnersGrantsDoNotOutliveHerAccount(t *testing.T) {
	h, srv, c, p := permHub(t)
	auth := srv.Auth.(*BuiltinAuth)

	// Control 1: a stranger reaches nothing on this project, so a 200 below is
	// about the inherited grant and not about the fixture being open.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, c["dave"]); rec.Code != http.StatusForbidden {
		t.Fatalf("control: outsider read = %d, want 403 — the fixture is not walled", rec.Code)
	}
	// Control 2: alice really is the org's only owner, so RemoveMember will
	// refuse. (If this ever stops holding the test still means what it says.)
	if o, ok := srv.Dir.(LocalDirectory).OrgDB.Get(p.Org); !ok || o.Members["alice@x.io"] != RoleOwner {
		t.Fatalf("control: alice is not the org owner in the fixture")
	}

	aliceID := secfx6UserID(t, auth, "alice@x.io")
	if err := auth.Deny(aliceID); err != nil {
		t.Fatalf("removing the account failed: %v", err)
	}

	// A fresh, unrelated account on the same address. On this hub that is one
	// signup form; on an invite-only hub it is one invite to an address the
	// admin believes is retired.
	newbie := signupAndSession(t, h, "alice@x.io", "Somebody Else", "password2")

	// Control 3: an address that never held a grant gets nothing from the same
	// signup path, so the delta below is the inherited grant.
	stranger := signupAndSession(t, h, "mallory@x.io", "Mallory", "password3")
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, stranger); rec.Code != http.StatusForbidden {
		t.Fatalf("control: a brand-new account read the project: %d, want 403", rec.Code)
	}

	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, newbie); rec.Code != http.StatusForbidden {
		t.Errorf("a brand-new account that merely signed up on a removed owner's address read "+
			"the project: %d, want 403.\nRemoveMember refuses to drop the last owner and offboard "+
			"only logs the refusal, so orgs.json still carries alice@x.io=owner after the account "+
			"is gone.", rec.Code)
	}
	// Ownership, not just read: the level projectPerm hands a role=owner.
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions",
		map[string]any{"default": PermNone}, newbie); rec.Code != http.StatusForbidden {
		t.Errorf("the same account rewrote the project's permissions: %d, want 403 — it inherited "+
			"PermAdmin through the org-owner row the removed account left behind", rec.Code)
	}
	// And it can invite: full org ownership, inherited by signing up.
	if rec := doAs(t, h, "POST", "/api/orgs/"+p.Org+"/invites",
		map[string]any{"days": 7}, newbie); rec.Code != http.StatusForbidden {
		t.Errorf("the same account minted an org invite: %d, want 403 — it is the org's owner", rec.Code)
	}
}

// TestSec_Mail_AMemberCannotPinTheHostEveryResetLinkPointsAt attacks round 6's
// answer to reset poisoning.
//
// Round 6 stopped building mailed links from the request's Host per request,
// and pinned the FIRST host the hub is reached on instead:
//
//	if a.pinnedBase == "" { a.pinnedBase = requestBaseURL(r) }
//
// "First" is a race an attacker wins by simply going first. The pin is taken
// from r.Host — still attacker-chosen — not from the listener, and it is set
// on the first request that MAILS anything, which /auth/reset does for any
// address that exists. So one ordinary member (or anyone who can name one live
// address) posts a reset for their own account with a Host of their choosing,
// and every reset link the hub mails for the rest of the process's life is
// addressed to the attacker's server — including the org owner's.
//
// It is also per-process: a restart re-opens the window, so the attacker only
// has to win a race that happens on every deploy.
func TestSec_Mail_AMemberCannotPinTheHostEveryResetLinkPointsAt(t *testing.T) {
	box := secapiSMTP(t)
	a, _, h := secapiAuth(t, box.mailer())
	if _, err := a.signup("victim@x.io", "Victim", "password1"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.signup("mallory@x.io", "Mallory", "password1"); err != nil {
		t.Fatal(err)
	}
	if a.BaseURL != "" {
		t.Fatalf("control: auth.base_url is set, which is the case the fix actually covers")
	}

	// Round 6's own reproducer (TestSec_Mail_ResetLinkCannotBeAimedAtAnAttacker
	// ChosenHost) sends the honest request FIRST, which is what pins the base.
	// Reverse the order and nothing else. Mallory has an account of her own —
	// so she needs no victim, no timing and no knowledge of anyone else's
	// address — and asks for a reset of HER OWN password on a host she owns.
	// /auth/reset takes this request from anyone at all.
	secapiForm(h, "evil.example", "/auth/reset", url.Values{"email": {"mallory@x.io"}})
	if got := box.next(t, 0); strings.Contains(got, "evil.example") {
		// Round 7 expected this mail to carry her host (that was the pin being
		// taken). With no auth.base_url the hub now trusts no request host at
		// all, so her own mail must not carry it either.
		t.Errorf("mallory's own reset mail carries the host she chose:\n%s", got)
	}

	// Now the honest flow, on the hub's real origin, for a different account.
	secapiForm(h, "hub.example", "/auth/reset", url.Values{"email": {"victim@x.io"}})
	got := box.next(t, 1)
	if strings.Contains(got, "evil.example") {
		t.Errorf("the reset mail delivered to victim@x.io carries a link on the host MALLORY "+
			"chose, on a request that arrived at hub.example:\n%s\n"+
			"The pin is taken from r.Host — still attacker-supplied — on whichever request mails "+
			"first, and /auth/reset mails for any address that exists. Round 6 moved reset "+
			"poisoning from per-request to first-request-wins; it did not remove it, and the pin "+
			"is per-process so every restart re-opens the race.", got)
	}
}

// TestSec_Store_AJournalCannotNameAPathTheUploadDoorRefuses is the CISO's own
// closing observation, made concrete.
//
// The hub has one rule for a path a client may write, spelled three times:
// templates.SafePath, webapp.cleanUploadPath and syncer.unsafeRel. Round 6
// gave cleanUploadPath a control-character refusal with an explicit reason:
//
//	a NUL is a value the metadata backends disagree about: Postgres refuses it
//	in a text column (a share on such a path 500s) while sqlite and the file
//	backend keep it [...] Refusing at ingest is what keeps that divergence
//	unreachable.
//
// There are two ingests. The browser upload goes through cleanUploadPath; the
// /store/* sync proxy takes a whole journal and validates the KEY, the device
// binding and the quota — never an op's Path. unsafeRel, which is the rule on
// that side, has no control-character clause at all. So the same hub that
// answers 400 to an uploaded "notes\x00.md" journals it happily when the same
// member pushes it as a device, and it is then a path in the project tree, on
// a share, and in every metadata backend the comment says disagree.
func TestSec_Store_AJournalCannotNameAPathTheUploadDoorRefuses(t *testing.T) {
	h, _, c, p := permHub(t)
	const dev = "d-secfx6ctl"
	blob := strings.Repeat("b", 64)

	for _, bad := range []struct{ name, path string }{
		{"nul", "notes\x00.md"},
		{"newline", "notes\n.md"},
		{"delete", "notes\x7f.md"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			// Control: the browser door refuses this exact path for this exact
			// member, so any difference below is the door and not the caller.
			up := doAs(t, h, "POST", "/api/p/"+p.ID+"/upload/commit",
				map[string]any{"path": bad.path, "sha256": blob, "size": 1}, c["bob"])
			if up.Code != http.StatusBadRequest {
				t.Fatalf("control: upload of %q = %d, want 400 — round 6's refusal is not there", bad.path, up.Code)
			}

			// Same member, same hub, same project, same path — as a device.
			body := secaudOpLine(1, dev, "put", bad.path, blob)
			rec := secfx4PushJournal(t, h, p.ID, dev, body, c["bob"])
			if rec.Code == http.StatusOK {
				// Where the accepted path ends up, so the harm is shown and
				// not asserted: the project's own tree, which every member
				// reads and the Share button mints links from.
				tree := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, c["bob"])
				t.Logf("tree after the push (%d): %s", tree.Code, strings.TrimSpace(tree.Body.String()))
				t.Errorf("the /store/* proxy journaled %q that /upload/commit answers 400 to. "+
					"One rule, three spellings: cleanUploadPath refuses control characters, "+
					"templates.SafePath refuses them, syncer.unsafeRel — the rule this door "+
					"relies on — has no clause for them. The path is now in the project's tree "+
					"and mintable as a share, which is the divergence round 6 said refusing at "+
					"ingest made unreachable.", bad.path)
			}
		})
	}
}

// ---- round 6's reservation ledger: what now runs under resMu ----

// secfx6BlockQuota is a QuotaProvider that is slow exactly once. It is the
// only shape needed to ask the question: QuotaProvider is a documented seam a
// managed deployment swaps ("billing and plan logic live in the managed
// service"), and round 6 moved the call to it INSIDE the ledger mutex.
type secfx6BlockQuota struct {
	mu      sync.Mutex
	n       int
	blockOn int
	entered chan struct{}
	release chan struct{}
}

func (q *secfx6BlockQuota) CheckWrite(string, int64) error {
	q.mu.Lock()
	q.n++
	hit := q.n == q.blockOn
	q.mu.Unlock()
	if hit {
		close(q.entered)
		<-q.release
	}
	return nil
}
func (q *secfx6BlockQuota) CheckSeat(string, int) error { return nil }
func (q *secfx6BlockQuota) RecordUsage(string, int64)   {}

// TestSec_Quota_ASlowProviderCannotWedgeUnrelatedProjects.
//
// Round 6's fix for the oversubscription race was to hold s.resMu across
// s.quota().CheckWrite. The ledger is hub-wide — one mutex for every project
// of every org — and CheckWrite is not the hub's code. So one provider call
// that is slow (a network hop to a billing service, a retry, a hung
// connection) stops every other project on the hub from reconciling grants,
// which is the first thing GET /store/list does on every sync cycle.
//
// The property asserted is the narrow one: the hub must not hold a hub-wide
// lock across a call into third-party code. A project that never asked the
// provider anything must still answer.
func TestSec_Quota_ASlowProviderCannotWedgeUnrelatedProjects(t *testing.T) {
	h, srv, p, _ := secsignHub(t)
	// A second, unrelated project on the same hub.
	rec := do(t, h, "POST", "/api/projects", map[string]string{"name": "other"})
	if rec.Code != 200 {
		t.Fatalf("control: creating the second project: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Project Project `json:"project"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	other := out.Project.ID

	// Control: with no provider configured, both routes answer immediately.
	if rec := do(t, h, "GET", "/api/p/"+other+"/store/list", nil); rec.Code != 200 {
		t.Fatalf("control: the second project's store list: %d %s", rec.Code, rec.Body)
	}

	// handleStoreSign calls CheckWrite twice: once outside the ledger lock
	// (the pre-check) and once inside it, from reserveIfFits. Block the second.
	q := &secfx6BlockQuota{blockOn: 2, entered: make(chan struct{}), release: make(chan struct{})}
	srv.Quota = q
	defer close(q.release)

	go func() {
		secfx5Sign(t, h, p.ID, "blobs/"+strings.Repeat("a", 64), 1024, "d-secfx6q", nil)
	}()
	select {
	case <-q.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("control: the provider was never called from inside reserveIfFits")
	}

	// A different project, a different sync cycle, nothing to do with the
	// grant above.
	done := make(chan int, 1)
	go func() {
		done <- do(t, h, "GET", "/api/p/"+other+"/store/list", nil).Code
	}()
	select {
	case code := <-done:
		if code != 200 {
			t.Fatalf("the unrelated project answered %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("an unrelated project's sync cycle (GET /api/p/%s/store/list) is blocked while a "+
			"quota provider is inside CheckWrite for a DIFFERENT project. Round 6 moved the "+
			"provider call inside s.resMu, which is one mutex for the whole hub, so arbitrary "+
			"third-party code now runs holding it — and reconcileGrants, which every sync cycle "+
			"calls, waits behind it.", other)
	}
}

// TestSec_Quota_ConcurrentGrantsCannotOversubscribeTheCap re-attacks the
// property round 6's fix exists for, under contention and -race: twelve
// signers racing one allowance that fits three of them.
func TestSec_Quota_ConcurrentGrantsCannotOversubscribeTheCap(t *testing.T) {
	h, srv, p, _ := secsignHub(t)
	const size = int64(32)
	const ceiling = int64(100) // three grants fit; the fourth must not
	srv.Quota = &secaudCapQuota{cap: ceiling}

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			secfx5Sign(t, h, p.ID, "blobs/"+fmt.Sprintf("%064x", i), size, "d-secfx6cap", nil)
		}(i)
	}
	wg.Wait()

	if got := srv.reservedBytes(""); got > ceiling {
		t.Errorf("concurrent signers booked %d bytes against a %d-byte allowance", got, ceiling)
	}
}
