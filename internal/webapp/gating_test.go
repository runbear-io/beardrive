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
