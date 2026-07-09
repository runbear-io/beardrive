package webapp

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// gatedAuth builds a BuiltinAuth with the given gating knobs.
func gatedAuth(t *testing.T, tune func(*BuiltinAuth)) *BuiltinAuth {
	t.Helper()
	a, err := OpenBuiltinAuth(filepath.Join(t.TempDir(), "auth.json"), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tune != nil {
		tune(a)
	}
	return a
}

func TestSignupDomainAllowlist(t *testing.T) {
	a := gatedAuth(t, func(a *BuiltinAuth) { a.AllowedDomains = []string{"runbear.io"} })
	if _, err := a.signup("mallory@evil.example", "M", "password1"); err == nil {
		t.Fatal("outside-domain signup should be rejected")
	}
	if _, err := a.signup("dev@runbear.io", "D", "password1"); err != nil {
		t.Fatalf("allowed-domain signup rejected: %v", err)
	}
	// case-insensitive, tolerant of a leading @ in config
	a2 := gatedAuth(t, func(a *BuiltinAuth) { a.AllowedDomains = []string{"@Runbear.IO"} })
	if _, err := a2.signup("x@RUNBEAR.io", "X", "password1"); err != nil {
		t.Fatalf("case-insensitive domain match failed: %v", err)
	}
}

func TestSignupVerificationGate(t *testing.T) {
	a := gatedAuth(t, func(a *BuiltinAuth) { a.RequireVerification = true })
	u, err := a.signup("dev@x.io", "D", "password1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != statusUnverified {
		t.Fatalf("status = %q, want unverified", u.Status)
	}
	// an unverified account cannot authenticate even with a token
	tok, _ := a.issueToken(u.ID, "cli")
	if _, ok := a.userForToken(tok); ok {
		t.Fatal("unverified account authenticated")
	}
	// verifying activates it
	grant := a.newGrant("verify", u.ID, "", true, 0)
	_ = grant
	a.mu.Lock()
	a.users[u.ID].Status = statusActive
	a.mu.Unlock()
	if _, ok := a.userForToken(tok); !ok {
		t.Fatal("activated account still cannot authenticate")
	}
}

func TestSignupApprovalGate(t *testing.T) {
	a := gatedAuth(t, func(a *BuiltinAuth) { a.RequireApproval = true })
	u, _ := a.signup("dev@x.io", "D", "password1")
	if u.Status != statusPending {
		t.Fatalf("status = %q, want pending", u.Status)
	}
	if len(a.PendingUsers()) != 1 {
		t.Fatal("pending user not listed")
	}
	tok, _ := a.issueToken(u.ID, "cli")
	if _, ok := a.userForToken(tok); ok {
		t.Fatal("pending account authenticated before approval")
	}
	if err := a.Approve(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.userForToken(tok); !ok {
		t.Fatal("approved account cannot authenticate")
	}
	if len(a.PendingUsers()) != 0 {
		t.Fatal("approved user still pending")
	}
}

// The signup page reflects the gate: verification shows "verify your email"
// and does NOT set a session cookie.
func TestSignupPageVerificationFlow(t *testing.T) {
	srv, _, _ := newHub(t, true, nil)
	srv.Auth = gatedAuth(t, func(a *BuiltinAuth) { a.RequireVerification = true })
	h := srv.Handler()
	form := url.Values{"email": {"a@x.io"}, "name": {"A"}, "password": {"password1"}}
	req := httptest.NewRequest("POST", "/auth/signup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Verify your email") {
		t.Fatalf("expected verify page: %d %s", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatal("verification-gated signup set a live session")
		}
	}
}

// The policy toggles persist to auth.json and reload; the domain allowlist
// and admin list are reported but not mutated by the policy API.
func TestPolicyPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	a, _ := OpenBuiltinAuth(path, true, nil)
	a.AllowedDomains = []string{"x.io"}
	a.Admins = map[string]bool{"admin@x.io": true}
	if err := a.SetPolicy(true, true); err != nil {
		t.Fatal(err)
	}
	// reload: toggles survive, and initialStatus reflects them
	a2, _ := OpenBuiltinAuth(path, true, nil)
	if !a2.RequireVerification || !a2.RequireApproval {
		t.Fatal("policy toggles did not persist across reload")
	}
	if a2.initialStatus() != statusUnverified {
		t.Fatalf("initialStatus = %q", a2.initialStatus())
	}
}

// A hub admin can read and flip the policy over HTTP; a non-admin cannot.
func TestPolicyAPIAdminOnly(t *testing.T) {
	srv, _, _ := newHub(t, true, nil)
	auth := gatedAuth(t, func(a *BuiltinAuth) { a.Admins = map[string]bool{"boss@x.io": true} })
	srv.Auth = auth
	h := srv.Handler()
	boss := signupAndSession(t, h, "boss@x.io", "Boss", "password1")
	pleb := signupAndSession(t, h, "pleb@x.io", "Pleb", "password1")

	if rec := doAs(t, h, "GET", "/api/admin/policy", nil, pleb); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin policy read: %d", rec.Code)
	}
	if rec := doAs(t, h, "POST", "/api/admin/policy", map[string]bool{"require_approval": true}, boss); rec.Code != 200 {
		t.Fatalf("admin policy write: %d %s", rec.Code, rec.Body)
	}
	if !auth.RequireApproval {
		t.Fatal("policy POST did not take effect")
	}
}

// The three supported postures validate; an ungated open hub and
// verification-without-a-mailer are refused.
func TestValidateSignupPolicy(t *testing.T) {
	ok := func(name string, tune func(*BuiltinAuth)) {
		t.Helper()
		a := gatedAuth(t, tune)
		if err := a.ValidateSignupPolicy(); err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
	}
	bad := func(name string, tune func(*BuiltinAuth)) {
		t.Helper()
		a := gatedAuth(t, tune)
		if err := a.ValidateSignupPolicy(); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
	ok("invite-only", func(a *BuiltinAuth) { a.AllowSignup = false })
	ok("open+domain", func(a *BuiltinAuth) { a.AllowedDomains = []string{"x.io"} })
	ok("open+approval", func(a *BuiltinAuth) { a.RequireApproval = true })
	ok("open+verify+mailer", func(a *BuiltinAuth) { a.RequireVerification = true; a.Mail = &Mailer{Host: "smtp"} })
	bad("open+no-gate", func(a *BuiltinAuth) { a.AllowSignup = true })
	bad("verify-without-mailer", func(a *BuiltinAuth) { a.RequireVerification = true })
}

// On an invite-only hub, a valid invite link lets a brand-new person create an
// account (bypassing the closed signup and the domain allowlist); an invalid
// or absent invite still hits the disabled page.
func TestInviteBootstrapsAccountWhenSignupClosed(t *testing.T) {
	srv, auth, _ := authHub(t, false) // AllowSignup=false (invite-only)
	auth.AllowedDomains = []string{"runbear.io"}
	auth.InviteValid = func(tok string) bool { return tok == "abc123" }
	h := srv.Handler()

	signupVia := func(next, email string) *httptest.ResponseRecorder {
		form := url.Values{"email": {email}, "name": {"New"}, "password": {"password1"}}
		u := "/auth/signup"
		if next != "" {
			u += "?next=" + url.QueryEscape(next)
		}
		req := httptest.NewRequest("POST", u, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// Valid invite → account created and active, even for an outside domain.
	rec := signupVia("/join/abc123", "contractor@elsewhere.com")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("invited signup: %d %s", rec.Code, rec.Body)
	}
	u := auth.findByEmail("contractor@elsewhere.com")
	if u == nil || !u.active() {
		t.Fatalf("invited account should exist and be active: %+v", u)
	}

	// No invite → disabled, no account.
	if rec := signupVia("", "nobody@runbear.io"); !strings.Contains(rec.Body.String(), "invite-only") {
		t.Fatalf("uninvited signup should be disabled: %s", rec.Body)
	}
	// Invalid/expired invite token → disabled too.
	if rec := signupVia("/join/deadbeef", "nobody@runbear.io"); !strings.Contains(rec.Body.String(), "invite-only") {
		t.Fatalf("bad-invite signup should be disabled: %s", rec.Body)
	}
	if auth.findByEmail("nobody@runbear.io") != nil {
		t.Fatal("account created without a valid invite")
	}
}

// Turning on email verification without a mailer is refused over the policy API.
func TestPolicyVerificationNeedsMailer(t *testing.T) {
	srv, auth, _ := authHub(t, true)
	auth.Admins = map[string]bool{"boss@x.io": true}
	h := srv.Handler()
	boss := signupAndSession(t, h, "boss@x.io", "Boss", "password1")
	rec := doAs(t, h, "POST", "/api/admin/policy", map[string]bool{"require_verification": true}, boss)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("verification without mailer should be refused: %d %s", rec.Code, rec.Body)
	}
	if auth.RequireVerification {
		t.Fatal("verification was enabled despite having no mailer")
	}
}

func TestAuthRateLimit(t *testing.T) {
	srv, _, _ := newHub(t, true, nil)
	srv.Auth = gatedAuth(t, nil)
	h := srv.Handler()
	last := 0
	for i := 0; i < 12; i++ {
		req := httptest.NewRequest("POST", "/auth/login", strings.NewReader("email=x@x.io&password=nope1234"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "9.9.9.9:1"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		last = rec.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("12th login attempt from one IP: %d, want 429", last)
	}
}
