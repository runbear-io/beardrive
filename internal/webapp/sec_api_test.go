package webapp

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Round 6 attacks the exported API surface directly rather than the routes:
// a function reachable from two callers is only as safe as the weaker one.
// Everything here calls the function the route calls, with the fixture the
// route would have built, and asserts the SECURE outcome.

// ---- helpers (secapi prefix) ----

// secapiAccountID resolves an account id by email, the way the admin routes
// get the id they hand to Approve/Deny.
func secapiAccountID(t *testing.T, a *BuiltinAuth, email string) string {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	u := a.findByEmail(email)
	if u == nil {
		t.Fatalf("no account for %s", email)
	}
	return u.ID
}

// secapiFlakyAccounts is an AccountRepo whose writes can be made to fail, so a
// store that refuses a record can be exercised the way a real one does when
// the disk is full or Postgres is unreachable.
type secapiFlakyAccounts struct {
	AccountRepo
	failAccount atomic.Bool
	failPolicy  atomic.Bool
	failDelete  atomic.Bool
}

func (r *secapiFlakyAccounts) PutAccount(u *authUser) error {
	if r.failAccount.Load() {
		return fmt.Errorf("account store unavailable")
	}
	return r.AccountRepo.PutAccount(u)
}

func (r *secapiFlakyAccounts) DeleteAccount(id string) error {
	if r.failDelete.Load() {
		return fmt.Errorf("account store unavailable")
	}
	return r.AccountRepo.DeleteAccount(id)
}

func (r *secapiFlakyAccounts) PutPolicy(p authPolicy) error {
	if r.failPolicy.Load() {
		return fmt.Errorf("account store unavailable")
	}
	return r.AccountRepo.PutPolicy(p)
}

// secapiAuth builds a BuiltinAuth over a flaky repo plus a mux carrying its
// own /auth/* pages — the same wiring Server.Handler() does.
func secapiAuth(t *testing.T, mail *Mailer) (*BuiltinAuth, *secapiFlakyAccounts, http.Handler) {
	t.Helper()
	repo := &secapiFlakyAccounts{AccountRepo: newFileAccountRepo(filepath.Join(t.TempDir(), "auth.json"))}
	a, err := NewBuiltinAuth(repo, true, mail)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	a.Register(mux)
	return a, repo, mux
}

// secapiForm posts an html form the way a browser does, with an explicit Host.
func secapiForm(h http.Handler, host, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if host != "" {
		req.Host = host
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ---- a minimal SMTP sink, so the mail the hub actually sends can be read ----

type secapiMailbox struct {
	addr string
	mu   sync.Mutex
	msgs []string
}

// secapiSMTP starts a plaintext SMTP server that accepts everything and keeps
// the message bodies. It advertises neither STARTTLS nor AUTH, which is what
// net/smtp's SendMail needs to proceed without credentials.
func secapiSMTP(t *testing.T) *secapiMailbox {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	box := &secapiMailbox{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go box.serve(c)
		}
	}()
	return box
}

func (b *secapiMailbox) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	fmt.Fprint(c, "220 secapi ready\r\n")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		verb := strings.ToUpper(strings.TrimSpace(line))
		if i := strings.IndexAny(verb, " :"); i >= 0 {
			verb = verb[:i]
		}
		switch verb {
		case "EHLO":
			fmt.Fprint(c, "250-secapi\r\n250 SIZE 10485760\r\n")
		case "HELO":
			fmt.Fprint(c, "250 secapi\r\n")
		case "DATA":
			fmt.Fprint(c, "354 go ahead\r\n")
			var msg strings.Builder
			for {
				l, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" || l == ".\n" {
					break
				}
				msg.WriteString(l)
			}
			b.mu.Lock()
			b.msgs = append(b.msgs, msg.String())
			b.mu.Unlock()
			fmt.Fprint(c, "250 queued\r\n")
		case "QUIT":
			fmt.Fprint(c, "221 bye\r\n")
			return
		default:
			fmt.Fprint(c, "250 ok\r\n")
		}
	}
}

func (b *secapiMailbox) mailer() *Mailer {
	host, port, _ := net.SplitHostPort(b.addr)
	p := 0
	fmt.Sscanf(port, "%d", &p)
	return &Mailer{Host: host, Port: p, From: "hub@example.test"}
}

// next waits for message number n (0-based) and returns it.
func (b *secapiMailbox) next(t *testing.T, n int) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		if len(b.msgs) > n {
			m := b.msgs[n]
			b.mu.Unlock()
			return m
		}
		b.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no message %d arrived at the smtp sink", n)
	return ""
}

func (b *secapiMailbox) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.msgs)
}

// ---- Deny: what an account removal does NOT take with it ----

// Deny is the only account-removal path on the hub. Every authorization
// decision downstream of it — org role, project grant, share-link liveness —
// is resolved by EMAIL, and Deny touches none of them: it drops the account
// row and (since round 5) its tokens.
//
// So an offboarding leaves a full set of live grants attached to an address
// with no account, and the moment that address has an account again — a
// re-signup on an open hub, an invite redeemed, an admin re-adding someone
// who then leaves again — the new account inherits everything the removed one
// held, including an explicit project admin grant, with no owner action.
// Round 1 established that a grant must not outlive the org membership; this
// is the same rule one level up: it must not outlive the ACCOUNT.
func TestSec_Account_RemovedAccountsGrantsDoNotOutliveIt(t *testing.T) {
	h, srv, c, p := permHub(t)
	auth := srv.Auth.(*BuiltinAuth)

	// bob is a plain org member holding an explicit admin grant on the project.
	if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermAdmin); err != nil {
		t.Fatal(err)
	}
	// Control: he can read it today, so the fixture is wired.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, c["bob"]); rec.Code != 200 {
		t.Fatalf("control: member read before removal: %d %s", rec.Code, rec.Body)
	}

	// The hub admin removes bob's account.
	if err := auth.Deny(secapiAccountID(t, auth, "bob@x.io")); err != nil {
		t.Fatalf("deny: %v", err)
	}

	if role := srv.Dir.Role(p.Org, "bob@x.io"); role != "" {
		t.Errorf("a removed account still holds org role %q", role)
	}
	if got, _ := srv.Projects.Get(p.ID); got.Perms["bob@x.io"] != "" {
		t.Errorf("a removed account still holds project grant %q", got.Perms["bob@x.io"])
	}

	// Control: an untouched member is unaffected by the removal, so anything
	// asked for below is the server deciding about bob, not a broken fixture.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, c["carol"]); rec.Code != 200 {
		t.Fatalf("control: untouched member read: %d %s", rec.Code, rec.Body)
	}

	// The consequence, end to end: a brand-new account on the same address
	// walks straight back into the project as its admin.
	fresh := signupAndSession(t, h, "bob@x.io", "Bob", "password1")
	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, fresh)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a brand-new account on a removed member's address reached the project: %d %s; want 403",
			rec.Code, rec.Body)
	}
	perms := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions/carol@x.io",
		map[string]string{"level": PermNone}, fresh)
	if perms.Code == 200 {
		t.Errorf("a brand-new account on a removed member's address inherited the project ADMIN grant "+
			"and edited permissions: %d %s", perms.Code, perms.Body)
	}
}

// A share link is the strongest grant on the hub: the org's live content, to
// anyone with the URL, forever. Round 2 made offboarding reach it and round 3
// made that resolution fail closed — but both resolve the creator's MEMBERSHIP,
// and deleting the account leaves the membership standing. So the one action an
// operator takes when someone must lose access immediately ("remove the
// account") is the one that leaves their public links serving.
func TestSec_Share_RemovedAccountsPublicLinkStopsServing(t *testing.T) {
	srv, p, _, _, h := shareHub(t)
	auth := srv.Auth.(*BuiltinAuth)

	orgs, err := OpenOrgDB(filepath.Join(t.TempDir(), "orgs.json"))
	if err != nil {
		t.Fatal(err)
	}
	org, err := orgs.Create("acme", "owner@x.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := orgs.AddMember(org.ID, "s@x.io", RoleMember); err != nil {
		t.Fatal(err)
	}
	srv.Dir = LocalDirectory{OrgDB: orgs}
	if err := srv.Projects.SetOrg(p.ID, org.ID); err != nil {
		t.Fatal(err)
	}

	token, _ := authedShare(t, srv, h, p.ID, "wiki/notes.md")
	get := func() int {
		return doHTTP(h, httptest.NewRequest("GET", "/s/"+token, nil)).Code
	}
	if code := get(); code != 200 {
		t.Fatalf("control: a live link must serve before anything is removed: %d", code)
	}

	// The account that minted it is removed from the hub.
	if err := auth.Deny(secapiAccountID(t, auth, "s@x.io")); err != nil {
		t.Fatalf("deny: %v", err)
	}
	if code := get(); code == 200 {
		t.Errorf("a public share link minted by a REMOVED account still serves the file: GET /s/%s = 200", token)
	}

	// Control that the check exists and only this door misses it: dropping the
	// same address from the org does end the link.
	if err := orgs.RemoveMember(org.ID, "s@x.io"); err != nil {
		t.Fatal(err)
	}
	if code := get(); code == 200 {
		t.Fatalf("control: removing the creator from the org did not end the link either (%d) — "+
			"the round 2/3 fix is gone, not just bypassed", code)
	}
}

// ---- writes the store refused, applied anyway ----

// Every service struct here keeps its state in memory and persists each change
// as one record. Round 2 (invite revocation) and round 3 (project grants)
// established the rule: a change the store REFUSED must not be in effect.
// Three account-administration paths still break it, each in the direction
// that widens access.
func TestSec_Admin_AChangeTheStoreRefusedIsNotInEffect(t *testing.T) {
	t.Run("approve", func(t *testing.T) {
		a, repo, _ := secapiAuth(t, nil)
		a.RequireApproval = true
		u, err := a.signup("waiting@x.io", "Waiting", "password1")
		if err != nil {
			t.Fatal(err)
		}
		if u.Status != statusPending {
			t.Fatalf("fixture: new account status = %q, want pending", u.Status)
		}
		tok, err := a.issueToken(u.ID, "browser")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := a.userForToken(tok); ok {
			t.Fatal("control: a pending account must not authenticate")
		}

		// The admin approves; the store refuses the record.
		repo.failAccount.Store(true)
		if err := a.Approve(u.ID); err == nil {
			t.Fatal("control: Approve did not surface the store's refusal")
		}

		if _, ok := a.userForToken(tok); ok {
			t.Error("an approval the store REFUSED activated the account anyway: " +
				"the admin was told it failed and the account can authenticate")
		}
	})

	t.Run("policy", func(t *testing.T) {
		a, repo, _ := secapiAuth(t, nil)
		if err := a.SetPolicy(false, true); err != nil {
			t.Fatal(err)
		}
		if u, err := a.signup("first@x.io", "First", "password1"); err != nil || u.Status != statusPending {
			t.Fatalf("control: approval gate not in force (%v, %+v)", err, u)
		}

		// The admin turns the approval gate OFF; the store refuses.
		repo.failPolicy.Store(true)
		if err := a.SetPolicy(false, false); err == nil {
			t.Fatal("control: SetPolicy did not surface the store's refusal")
		}

		u, err := a.signup("second@x.io", "Second", "password1")
		if err != nil {
			t.Fatal(err)
		}
		if u.Status != statusPending {
			t.Errorf("a gating change the store REFUSED took effect anyway: a new signup is %q, "+
				"want pending — the hub is now un-gated across a restart it does not agree with", u.Status)
		}
	})

	t.Run("deny", func(t *testing.T) {
		a, repo, _ := secapiAuth(t, nil)
		u, err := a.signup("leaving@x.io", "Leaving", "password1")
		if err != nil {
			t.Fatal(err)
		}

		repo.failDelete.Store(true)
		if err := a.Deny(u.ID); err == nil {
			t.Fatal("control: Deny did not surface the store's refusal")
		}
		repo.failDelete.Store(false)

		// The hub restarts. A removal the store refused must not have looked
		// done: either it never happened (and the admin retries) or it is
		// durable. What must never happen is "gone until the next restart".
		reloaded, err := NewBuiltinAuth(repo, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		// verifyPassword returns *authUser, not error: non-nil means the
		// account is still on disk and would sign in again after the restart.
		// (The original spelling compared it to nil as if it were an error, so
		// BOTH outcomes failed — the assertion below is the one the comment
		// above describes, and it still fails on the code that shipped.)
		onDisk := reloaded.verifyPassword("leaving@x.io", "password1") != nil
		a.mu.Lock()
		_, inMemory := a.users[u.ID]
		a.mu.Unlock()
		if onDisk && !inMemory {
			t.Error("a removal the store REFUSED emptied the in-memory registry anyway: " +
				"the account is gone until the next restart, then signs in again with its old password")
		}
	})
}

// A password reset is the documented recovery for a stolen account. Round 5
// made the token revocation half durable — a logout that reported success had
// been leaving the credential alive on disk. The password half is still the
// old shape: pageResetConfirm discards PutAccount's error, tells the human
// "Your password is updated", and the hub comes back at the next restart with
// the password the thief chose still live.
func TestSec_Password_ResetThatWasNotPersistedIsNotReportedAsDone(t *testing.T) {
	a, repo, h := secapiAuth(t, nil)

	reset := func(a *BuiltinAuth, userID, pass string) *httptest.ResponseRecorder {
		t.Helper()
		tok := a.newGrant("reset", userID, time.Hour)
		return secapiForm(h, "hub.example", "/auth/reset/confirm",
			url.Values{"token": {tok}, "password": {pass}})
	}

	// Control: a reset the store accepts really does survive a restart.
	ok, err := a.signup("ok@x.io", "Ok", "password1")
	if err != nil {
		t.Fatal(err)
	}
	if rec := reset(a, ok.ID, "password2"); !strings.Contains(rec.Body.String(), "Password updated") {
		t.Fatalf("control: an ordinary reset did not report success: %d %s", rec.Code, rec.Body)
	}
	if control, err := NewBuiltinAuth(repo, true, nil); err != nil {
		t.Fatal(err)
	} else if control.verifyPassword("ok@x.io", "password2") == nil {
		t.Fatal("control: a persisted reset did not survive a reload — the fixture cannot see the disk")
	}

	// Attack: the same flow while the store refuses the write.
	victim, err := a.signup("victim@x.io", "Victim", "oldpassword")
	if err != nil {
		t.Fatal(err)
	}
	repo.failAccount.Store(true)
	rec := reset(a, victim.ID, "thiefchosen1")
	claimed := strings.Contains(rec.Body.String(), "Password updated")
	repo.failAccount.Store(false)

	reloaded, err := NewBuiltinAuth(repo, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if claimed && reloaded.verifyPassword("victim@x.io", "thiefchosen1") == nil {
		t.Errorf("the reset page reported \"Password updated\" for a write the store refused: "+
			"after a restart %q is not the password", "thiefchosen1")
	}
	if claimed && reloaded.verifyPassword("victim@x.io", "oldpassword") != nil {
		t.Errorf("the reset page reported \"Password updated\" but the OLD password is live again " +
			"after a restart — the documented recovery from a stolen account does not recover it")
	}
}

// secapiFlakyShares is a ShareRepo whose delete can be made to fail.
type secapiFlakyShares struct {
	ShareRepo
	failDelete atomic.Bool
}

func (r *secapiFlakyShares) Delete(token string) error {
	if r.failDelete.Load() {
		return fmt.Errorf("share store unavailable")
	}
	return r.ShareRepo.Delete(token)
}

// Round 5 found that revoking a device token dropped the row from memory and
// DISCARDED the store's error, so a logout reported success while the
// credential survived on disk. ShareDB.Revoke is the same three lines on the
// hub's most public grant — a /s/ URL serving an org's live content to anyone
// — and it is the emergency stop for a leaked link. OrgDB.RevokeInvite, the
// sibling emergency stop, already puts the row back and reports the failure;
// this one returns true and moves on.
func TestSec_Share_RevocationMustNotSurviveOnlyInMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shares.json")
	repo := &secapiFlakyShares{ShareRepo: newFileShareRepo(path)}
	db, err := NewShareDB(repo)
	if err != nil {
		t.Fatal(err)
	}

	// Control: an ordinary revocation is durable.
	ok, err := db.Create("p1", "wiki/ok.md", "alice@x.io", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !db.Revoke(ok.Token) {
		t.Fatal("control: Revoke reported failure on a live share")
	}
	if reloaded, err := NewShareDB(repo); err != nil {
		t.Fatal(err)
	} else if _, live := reloaded.lookup(ok.Token); live {
		t.Fatal("control: an ordinary revocation did not survive a reload")
	}

	// Attack: the store refuses the delete.
	leaked, err := db.Create("p1", "secret/salaries.md", "alice@x.io", 0)
	if err != nil {
		t.Fatal(err)
	}
	repo.failDelete.Store(true)
	reported := db.Revoke(leaked.Token)
	repo.failDelete.Store(false)

	reloaded, err := NewShareDB(repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, live := reloaded.lookup(leaked.Token); live && reported {
		t.Errorf("Revoke reported the link dead, but the row survived the store's refusal: "+
			"/s/%s serves %s again after a restart", leaked.Token, leaked.Path)
	}
}

// ---- the AuthProvider seam a managed deployment swaps ----

type secapiStubAuth struct{ user User }

func (a secapiStubAuth) CLILoginPath() string                    { return "/auth/login" }
func (a secapiStubAuth) Authenticate(*http.Request) (User, bool) { return a.user, a.user.ID != "" }
func (a secapiStubAuth) Register(*http.ServeMux)                 {}
func (a secapiStubAuth) Accounts() []User                        { return nil }

// Every authorization decision on the hub is keyed on the email an
// AuthProvider hands back. BuiltinAuth happens to guarantee a non-empty,
// lowercased, unique address; the interface promises none of that, and a
// managed deployment swaps the implementation. The properties the hub relies
// on have to hold for an identity it did not mint.
func TestSec_Auth_AProviderIdentityTheHubCannotResolveReachesNothing(t *testing.T) {
	h, srv, c, p := permHub(t)
	real := srv.Auth

	// Control: with the real provider, bob is in.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, c["bob"]); rec.Code != 200 {
		t.Fatalf("control: member read: %d %s", rec.Code, rec.Body)
	}
	t.Cleanup(func() { srv.Auth = real })

	for _, email := range []string{
		"",                       // provider returned no address at all
		"   ",                    // whitespace only
		"bob@x.io.attacker.test", // a superset of a member's address
		"bob",                    // the local part alone
		"@x.io",                  // the domain alone
	} {
		srv.Auth = secapiStubAuth{user: User{ID: "u-stub", Email: email, Name: "Stub"}}
		for _, route := range []string{"/api/p/" + p.ID + "/tree", "/api/p/" + p.ID + "/history", "/api/p/" + p.ID + "/store/list"} {
			rec := doAs(t, h, "GET", route, nil, nil)
			if rec.Code != http.StatusForbidden {
				t.Errorf("a provider identity with email %q reached %s: %d %s; want 403",
					email, route, rec.Code, rec.Body)
			}
		}
	}

	// Case must not mint a second principal: a provider that upper-cases the
	// same person is the same person, not a stranger with a fresh grant map.
	srv.Auth = secapiStubAuth{user: User{ID: "u-stub", Email: "BOB@X.IO", Name: "Bob"}}
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, nil); rec.Code != 200 {
		t.Errorf("the same address in upper case resolved to a different principal: %d %s", rec.Code, rec.Body)
	}
}

// ---- mail: where the links in it point ----

// requestBaseURL builds every link the hub mails from r.Host (and an
// unconditionally trusted X-Forwarded-Proto). For the three URLs handed back
// to the caller that is self-inflicted; for the two that are MAILED it is
// classic reset poisoning: an unauthenticated stranger posts /auth/reset for
// a victim's address with a Host of their choosing, and the hub sends the
// victim a genuine reset mail whose link hands the single-use grant to the
// attacker's server.
func TestSec_Mail_ResetLinkCannotBeAimedAtAnAttackerChosenHost(t *testing.T) {
	box := secapiSMTP(t)
	a, _, h := secapiAuth(t, box.mailer())
	// The hub's public origin, which is what a mailed link is built from (and
	// which a hub with smtp configured must now set: ValidateSignupPolicy).
	a.BaseURL = "http://hub.example"
	if _, err := a.signup("victim@x.io", "Victim", "password1"); err != nil {
		t.Fatal(err)
	}

	// Control: an ordinary request mails a link on the hub's own host, so the
	// sink and the flow both work.
	secapiForm(h, "hub.example", "/auth/reset", url.Values{"email": {"victim@x.io"}})
	if got := box.next(t, 0); !strings.Contains(got, "hub.example/auth/reset/confirm?token=") {
		t.Fatalf("control: no reset link for the hub's own host in the mail:\n%s", got)
	}

	// Attack: the same anonymous request, a Host the attacker chose.
	secapiForm(h, "evil.example", "/auth/reset", url.Values{"email": {"victim@x.io"}})
	got := box.next(t, 1)
	if strings.Contains(got, "evil.example") {
		t.Errorf("the reset mail delivered to the victim carries a link on an attacker-chosen host:\n%s", got)
	}
}

// Same seam, the other mailed link: a verification mail is sent on every login
// attempt against an unverified account, so anyone who knows an address on an
// approval/verification-gated hub can aim its activation link anywhere.
func TestSec_Mail_VerificationLinkCannotBeAimedAtAnAttackerChosenHost(t *testing.T) {
	box := secapiSMTP(t)
	a, _, h := secapiAuth(t, box.mailer())
	a.BaseURL = "http://hub.example"
	a.RequireVerification = true
	if _, err := a.signup("newbie@x.io", "Newbie", "password1"); err != nil {
		t.Fatal(err)
	}
	// Control: signing in re-sends the verification link, on the hub's host.
	secapiForm(h, "hub.example", "/auth/login",
		url.Values{"email": {"newbie@x.io"}, "password": {"password1"}})
	if got := box.next(t, 0); !strings.Contains(got, "hub.example/auth/verify?token=") {
		t.Fatalf("control: no verification link for the hub's own host in the mail:\n%s", got)
	}

	secapiForm(h, "evil.example", "/auth/login",
		url.Values{"email": {"newbie@x.io"}, "password": {"password1"}})
	got := box.next(t, 1)
	if strings.Contains(got, "evil.example") {
		t.Errorf("the verification mail carries a link on an attacker-chosen host:\n%s", got)
	}
}

// Mailer.Send joins its arguments into headers with CRLF and validates
// nothing, and createAccount accepts an email address carrying a bare CRLF.
// Assert the property that matters: a recipient with an embedded newline never
// produces a delivered message (net/smtp refuses the envelope today, which is
// the only thing standing between that address and an injected Bcc).
func TestSec_Mail_RecipientCRLFNeverBecomesAHeader(t *testing.T) {
	box := secapiSMTP(t)
	m := box.mailer()

	// Control: an ordinary address is delivered, so the sink counts.
	if err := m.Send("ordinary@x.io", "Subject", "body"); err != nil {
		t.Fatalf("control: an ordinary send failed: %v", err)
	}
	box.next(t, 0)

	if err := m.Send("victim@x.io\r\nBcc: boss@corp.example", "Subject", "body"); err == nil {
		t.Error("Send accepted a recipient carrying CRLF")
	}
	if n := box.count(); n != 1 {
		t.Errorf("a message went out for a CRLF recipient (%d delivered, want 1)", n)
	}
	for _, msg := range func() []string { box.mu.Lock(); defer box.mu.Unlock(); return append([]string(nil), box.msgs...) }() {
		if strings.Contains(msg, "Bcc:") {
			t.Errorf("an injected header reached the wire:\n%s", msg)
		}
	}
}
