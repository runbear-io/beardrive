package webapp

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/remote"
)

// authHub builds an auth-enabled hub server with one project.
func authHub(t *testing.T, allowSignup bool) (*Server, *BuiltinAuth, Project) {
	t.Helper()
	auth, err := OpenBuiltinAuth(filepath.Join(t.TempDir(), "auth.json"), allowSignup, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, p, _ := newHub(t, true, nil)
	srv.Auth = auth
	return srv, auth, p
}

// signupAndSession creates an account through the real signup page and
// returns its session cookie.
func signupAndSession(t *testing.T, h http.Handler, email, name, pass string) *http.Cookie {
	t.Helper()
	form := url.Values{"email": {email}, "name": {name}, "password": {pass}}
	req := httptest.NewRequest("POST", "/auth/signup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("signup: %d %s", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("signup set no session cookie")
	return nil
}

func TestAuthGatesAPI(t *testing.T) {
	srv, auth, p := authHub(t, true)
	h := srv.Handler()

	// open surface: config, auth pages, static frontend
	if rec := do(t, h, "GET", "/api/config", nil); rec.Code != 200 ||
		!strings.Contains(rec.Body.String(), `"enabled":true`) ||
		!strings.Contains(rec.Body.String(), "/auth/cli") {
		t.Fatalf("config must stay open and advertise auth: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/auth/login", nil); rec.Code != 200 {
		t.Fatalf("login page: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/", nil); rec.Code != 200 {
		t.Fatalf("frontend: %d", rec.Code)
	}

	// gated surface
	for _, u := range []string{"/api/projects", "/api/p/" + p.ID + "/tree", "/api/p/" + p.ID + "/store/list?prefix=journal/"} {
		if rec := do(t, h, "GET", u, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without auth: %d, want 401", u, rec.Code)
		}
	}

	// a valid Bearer token opens it
	cookie := signupAndSession(t, h, "a@x.io", "Alice", "password1")
	_ = cookie
	u := auth.users // reach in for the user id
	var uid string
	for id := range u {
		uid = id
	}
	tok, err := auth.issueToken(uid, "test-device")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/projects", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("with token: %d %s", rec.Code, rec.Body)
	}
	// a session cookie works too (browser)
	req = httptest.NewRequest("GET", "/api/projects", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("with cookie: %d %s", rec.Code, rec.Body)
	}
	// garbage token stays out
	req = httptest.NewRequest("GET", "/api/projects", nil)
	req.Header.Set("Authorization", "Bearer bdt_nope")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d, want 401", rec.Code)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	srv, _, _ := authHub(t, true)
	h := srv.Handler()
	signupAndSession(t, h, "a@x.io", "Alice", "password1")

	form := url.Values{"email": {"a@x.io"}, "password": {"wrong-pass"}}
	req := httptest.NewRequest("POST", "/auth/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Wrong email or password") {
		t.Fatalf("wrong password: %d", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("wrong password must not create a session")
	}
}

func TestSignupDisabled(t *testing.T) {
	srv, auth, _ := authHub(t, false)
	h := srv.Handler()
	form := url.Values{"email": {"a@x.io"}, "name": {"A"}, "password": {"password1"}}
	req := httptest.NewRequest("POST", "/auth/signup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "invite-only") {
		t.Fatalf("signup while disabled: %d %s", rec.Code, rec.Body)
	}
	if len(auth.users) != 0 {
		t.Fatal("account created despite allow_signup=false")
	}
}

// A fresh approval-gated hub must not strand its first admin: an email on the
// config's admin list activates on signup instead of waiting for an approver
// who doesn't exist yet.
func TestSignupAdminBypassesApproval(t *testing.T) {
	srv, auth, _ := authHub(t, true)
	auth.RequireApproval = true
	auth.Admins = map[string]bool{"admin@x.io": true}
	h := srv.Handler()

	cookie := signupAndSession(t, h, "admin@x.io", "Admin", "password1")
	if cookie == nil {
		t.Fatal("admin signup did not start a session")
	}

	// A non-admin under the same posture still lands pending.
	form := url.Values{"email": {"b@x.io"}, "name": {"B"}, "password": {"password1"}}
	req := httptest.NewRequest("POST", "/auth/signup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "waiting for an administrator") {
		t.Fatalf("non-admin signup should be pending: %d %s", rec.Code, rec.Body)
	}
}

// A brand-new invite-only hub has nobody to mint an invite: until the first
// account exists, config-listed admin emails may sign up directly; everyone
// else stays out, and the door closes again after the first account.
func TestSignupInviteOnlyBootstrap(t *testing.T) {
	srv, auth, _ := authHub(t, false)
	auth.Admins = map[string]bool{"admin@x.io": true}
	h := srv.Handler()

	// a stranger can't take the bootstrap slot
	form := url.Values{"email": {"b@x.io"}, "name": {"B"}, "password": {"password1"}}
	req := httptest.NewRequest("POST", "/auth/signup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "only a hub admin") {
		t.Fatalf("stranger during bootstrap: %d %s", rec.Code, rec.Body)
	}

	// the configured admin signs up and is active immediately
	if signupAndSession(t, h, "admin@x.io", "Admin", "password1") == nil {
		t.Fatal("admin bootstrap signup did not start a session")
	}

	// with the first account in place the hub is invite-only again
	req = httptest.NewRequest("GET", "/auth/signup", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "invite-only") {
		t.Fatalf("signup page after bootstrap should be disabled: %s", rec.Body)
	}
}

// The browser flow bdrive login drives: session → /auth/cli redirect with a
// one-time code → exchange for a device token.
func TestCLICallbackFlow(t *testing.T) {
	srv, _, p := authHub(t, true)
	h := srv.Handler()
	cookie := signupAndSession(t, h, "cli@x.io", "CLI", "password1")

	// non-loopback redirect is refused
	req := httptest.NewRequest("GET", "/auth/cli?redirect="+url.QueryEscape("http://evil.example/cb")+"&state=s", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-loopback redirect: %d, want 400", rec.Code)
	}

	// without a session, /auth/cli sends the browser to the login page
	req = httptest.NewRequest("GET", "/auth/cli?redirect="+url.QueryEscape("http://127.0.0.1:9999/callback")+"&state=s1", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "/auth/login") {
		t.Fatalf("cli without session: %d %s", rec.Code, rec.Header().Get("Location"))
	}

	// with a session, a GET asks first — it names the account the terminal
	// would act as and offers to switch, and grants nothing on its own.
	cliURL := "/auth/cli?redirect=" + url.QueryEscape("http://127.0.0.1:9999/callback") + "&state=s1"
	req = httptest.NewRequest("GET", cliURL, nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cli confirmation page: %d, want 200", rec.Code)
	}
	for _, want := range []string{"cli@x.io", "Switch account", "127.0.0.1:9999", "Approve"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("confirmation page missing %q:\n%s", want, rec.Body)
		}
	}

	// approving posts back to the same URL and lands on the loopback listener
	req = httptest.NewRequest("POST", cliURL, nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("cli redirect: %d", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || loc.Host != "127.0.0.1:9999" || loc.Query().Get("state") != "s1" {
		t.Fatalf("callback location = %v", rec.Header().Get("Location"))
	}
	code := loc.Query().Get("code")

	// exchange the code for a token
	rec = do(t, h, "POST", "/api/auth/exchange", map[string]string{"code": code, "device": "laptop"})
	if rec.Code != 200 {
		t.Fatalf("exchange: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Token string `json:"token"`
		User  User   `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Token == "" || out.User.Email != "cli@x.io" {
		t.Fatalf("exchange = %+v", out)
	}
	// the code is single-use
	if rec := do(t, h, "POST", "/api/auth/exchange", map[string]string{"code": code}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("code reuse: %d, want 401", rec.Code)
	}
	// the token opens the API
	req = httptest.NewRequest("GET", "/api/p/"+p.ID+"/tree", nil)
	req.Header.Set("Authorization", "Bearer "+out.Token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("token on api: %d", rec.Code)
	}
}

func TestDeviceCodeFlow(t *testing.T) {
	srv, _, _ := authHub(t, true)
	h := srv.Handler()
	cookie := signupAndSession(t, h, "dev@x.io", "Dev", "password1")

	rec := do(t, h, "POST", "/api/auth/device/start", map[string]string{"device": "server-1", "os": "linux"})
	if rec.Code != 200 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var start struct {
		Code      string `json:"code"`
		VerifyURL string `json:"verify_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil || start.Code == "" {
		t.Fatalf("start = %s (%v)", rec.Body, err)
	}
	// Both values are real tokens (nobody types these in), and the one in the
	// link is NOT the one the client polls with: the link is displayed,
	// forwarded and logged, and must not by itself buy a device token.
	if len(start.Code) < 32 {
		t.Fatalf("device code %q is too short to be a URL secret", start.Code)
	}
	link, ok := strings.CutPrefix(start.VerifyURL, "http://example.com/auth/device/")
	if !ok || len(link) < 32 {
		t.Fatalf("verify_url = %q, want .../auth/device/<link secret>", start.VerifyURL)
	}
	if link == start.Code {
		t.Fatalf("the link the human opens is also the poll credential (%q)", link)
	}
	approve := "/auth/device/" + link

	// pending until approved
	rec = do(t, h, "POST", "/api/auth/device/poll", map[string]string{"code": start.Code})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "pending") {
		t.Fatalf("poll before approve: %d %s", rec.Code, rec.Body)
	}

	// The approval page names the account being granted, offers a way off it,
	// and says what is asking — approving is handing that box a token.
	req := httptest.NewRequest("GET", approve, nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	for _, want := range []string{"dev@x.io", "server-1", "linux", "Switch account", url.QueryEscape(approve)} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("approval page missing %q:\n%s", want, rec.Body)
		}
	}
	if strings.Contains(rec.Body.String(), `name="code"`) {
		t.Fatal("approval page still asks for a typed code")
	}

	// approve from a signed-in browser
	req = httptest.NewRequest("POST", approve, nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Device connected") {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}

	rec = do(t, h, "POST", "/api/auth/device/poll", map[string]string{"code": start.Code, "device": "server-1"})
	var out struct {
		Token string `json:"token"`
		User  User   `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Token == "" {
		t.Fatalf("poll after approve: %d %s", rec.Code, rec.Body)
	}
	if out.User.Email != "dev@x.io" {
		t.Fatalf("user = %+v", out.User)
	}
	// wrong code 401s
	if rec := do(t, h, "POST", "/api/auth/device/poll", map[string]string{"code": "nope"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad code: %d, want 401", rec.Code)
	}
}

// Reset without SMTP: the link is logged for the admin; the token itself
// must update the password exactly once.
func TestPasswordReset(t *testing.T) {
	srv, auth, _ := authHub(t, true)
	h := srv.Handler()
	signupAndSession(t, h, "r@x.io", "R", "oldpassword")

	form := url.Values{"email": {"r@x.io"}}
	req := httptest.NewRequest("POST", "/auth/reset", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "reset link is on its way") {
		t.Fatalf("reset request: %d", rec.Code)
	}
	// grab the pending reset token (in production it arrives by email/log)
	var tok string
	auth.mu.Lock()
	for id, g := range auth.pending {
		if g.kind == "reset" {
			tok = id
		}
	}
	auth.mu.Unlock()
	if tok == "" {
		t.Fatal("no reset token minted")
	}

	form = url.Values{"token": {tok}, "password": {"newpassword9"}}
	req = httptest.NewRequest("POST", "/auth/reset/confirm", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "password is updated") {
		t.Fatalf("reset confirm: %d %s", rec.Code, rec.Body)
	}
	if auth.verifyPassword("r@x.io", "oldpassword") != nil {
		t.Fatal("old password still works")
	}
	if auth.verifyPassword("r@x.io", "newpassword9") == nil {
		t.Fatal("new password does not work")
	}
	// token is single-use
	req = httptest.NewRequest("POST", "/auth/reset/confirm", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "invalid or expired") {
		t.Fatal("reset token must be single-use")
	}
	// unknown emails get the same neutral answer (no account probing)
	form = url.Values{"email": {"ghost@x.io"}}
	req = httptest.NewRequest("POST", "/auth/reset", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "reset link is on its way") {
		t.Fatalf("reset for unknown email must look identical: %d", rec.Code)
	}
}

// Accounts and tokens survive a server restart; revocation sticks.
func TestAuthPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	a1, err := OpenBuiltinAuth(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := a1.signup("p@x.io", "P", "password1")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := a1.issueToken(u.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}

	a2, err := OpenBuiltinAuth(path, true, nil) // "restart"
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := a2.userForToken(tok); !ok || got.Email != "p@x.io" {
		t.Fatalf("token after reload = %+v %v", got, ok)
	}
	if a2.verifyPassword("p@x.io", "password1") == nil {
		t.Fatal("password lost across reload")
	}
	a2.revokeToken(tok)
	a3, err := OpenBuiltinAuth(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a3.userForToken(tok); ok {
		t.Fatal("revoked token still valid after reload")
	}
	// the auth file must never contain the plaintext token or password
	data, _ := readFileString(path)
	if strings.Contains(data, tok) || strings.Contains(data, "password1") {
		t.Fatal("auth.json leaks a plaintext credential")
	}
}

// A syncing device authenticates the same way: token via BDRIVE_TOKEN.
func TestSyncBackendWithToken(t *testing.T) {
	srv, auth, p := authHub(t, true)
	u, err := auth.signup("s@x.io", "S", "password1")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := auth.issueToken(u.ID, "sync-box")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// without a token the backend is rejected
	t.Setenv("BDRIVE_TOKEN", "")
	t.Setenv("BDRIVE_HOME", t.TempDir()) // no settings.json token either
	be, err := remote.Open(context.Background(), ts.URL+"/p/"+p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.List(context.Background(), "journal/"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("unauthenticated list = %v, want 401", err)
	}
	be.Close()

	// with the token everything works
	t.Setenv("BDRIVE_TOKEN", tok)
	be, err = remote.Open(context.Background(), ts.URL+"/p/"+p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	if _, err := be.List(context.Background(), "journal/"); err != nil {
		t.Fatalf("authenticated list: %v", err)
	}
	content := "authed content"
	if err := be.Put(context.Background(), "blobs/"+shaOf(content), strings.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("authenticated put: %v", err)
	}
}

func readFileString(path string) (string, error) {
	data, err := os.ReadFile(path)
	return string(data), err
}

// TestConfigBillingSeam: the billing block appears in /api/config only when
// the hook is set, the caller is signed in, and the hook says ok.
func TestConfigBillingSeam(t *testing.T) {
	srv, _, _ := authHub(t, true)
	srv.Billing = func(email string) (string, string, bool) {
		if email == "a@x.io" {
			return "Team", "/billing", true
		}
		return "", "", false
	}
	h := srv.Handler()

	get := func(cookie *http.Cookie) map[string]json.RawMessage {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/config", nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("config: %d %s", rec.Code, rec.Body)
		}
		var out map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Signed out: no billing key, even with the hook set.
	if _, ok := get(nil)["billing"]; ok {
		t.Fatal("billing leaked to a signed-out config")
	}
	// Signed in and the hook says ok: plan + url.
	cookie := signupAndSession(t, h, "a@x.io", "Alice", "password1")
	if got := string(get(cookie)["billing"]); got != `{"plan":"Team","url":"/billing"}` {
		t.Fatalf("billing block = %s", got)
	}
	// A user the hook declines (no org yet): key absent.
	other := signupAndSession(t, h, "b@x.io", "Bob", "password1")
	if _, ok := get(other)["billing"]; ok {
		t.Fatal("billing shown to a user the hook declined")
	}
}

// TestConfigAnalyticsSeam: an unconfigured hub says nothing about analytics —
// that silence is what keeps a self-hosted frontend from loading a tracker —
// and a configured one hands over the key with a default host.
func TestConfigAnalyticsSeam(t *testing.T) {
	config := func(srv *Server) map[string]json.RawMessage {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/config", nil))
		if rec.Code != 200 {
			t.Fatalf("config: %d %s", rec.Code, rec.Body)
		}
		var out map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	srv, _, _ := authHub(t, true)
	if _, ok := config(srv)["analytics"]; ok {
		t.Fatal("an unconfigured hub advertised analytics")
	}

	// Signed out on purpose: the block must not depend on a session, or a
	// hub with auth off would never be measurable.
	srv.Analytics = AnalyticsConfig{Key: "phc_test"}
	if got := string(config(srv)["analytics"]); got != `{"host":"`+DefaultAnalyticsHost+`","key":"phc_test"}` {
		t.Fatalf("analytics block = %s", got)
	}

	srv.Analytics.Host = "https://eu.i.posthog.com"
	if got := string(config(srv)["analytics"]); got != `{"host":"https://eu.i.posthog.com","key":"phc_test"}` {
		t.Fatalf("analytics host override = %s", got)
	}
}

// The reason the CLI flow confirms at all: the browser's session is often not
// the account the user meant the terminal to act as. Switching must come back
// to the same pending sign-in rather than dumping them on the home page.
func TestCLILoginSwitchAccount(t *testing.T) {
	srv, _, _ := authHub(t, true)
	h := srv.Handler()
	personal := signupAndSession(t, h, "me@personal.io", "Me", "password1")

	cliURL := "/auth/cli?redirect=" + url.QueryEscape("http://127.0.0.1:9999/callback") + "&state=s1"

	// the page offers a way out, carrying this sign-in along
	req := httptest.NewRequest("GET", cliURL, nil)
	req.AddCookie(personal)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	swap := regexp.MustCompile(`href="(/auth/logout\?next=[^"]+)"`).FindStringSubmatch(rec.Body.String())
	if swap == nil {
		t.Fatalf("no switch-account link on the confirmation page:\n%s", rec.Body)
	}

	// following it drops the session and heads for a fresh login
	req = httptest.NewRequest("GET", html.UnescapeString(swap[1]), nil)
	req.AddCookie(personal)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/auth/login?next=") {
		t.Fatalf("switch account = %d %s", rec.Code, loc)
	}
	next, err := url.QueryUnescape(strings.TrimPrefix(loc, "/auth/login?next="))
	if err != nil || next != cliURL {
		t.Fatalf("switch account loses the pending sign-in: next=%q want %q", next, cliURL)
	}

	// signing in as someone else returns to the same confirmation, now naming them
	work := signupAndSession(t, h, "me@work.io", "Me At Work", "password2")
	req = httptest.NewRequest("GET", next, nil)
	req.AddCookie(work)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "me@work.io") {
		t.Fatalf("after switching, confirmation does not name the new account: %d\n%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "me@personal.io") {
		t.Fatalf("confirmation still shows the old account:\n%s", rec.Body)
	}

	// and approving as them grants to them
	req = httptest.NewRequest("POST", next, nil)
	req.AddCookie(work)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve after switch: %d", rec.Code)
	}
	cb, _ := url.Parse(rec.Header().Get("Location"))
	out := do(t, h, "POST", "/api/auth/exchange", map[string]string{"code": cb.Query().Get("code"), "device": "laptop"})
	if !strings.Contains(out.Body.String(), "me@work.io") {
		t.Fatalf("token issued to the wrong account: %s", out.Body)
	}
}

// Both sign-in flows must behave identically for someone with no web session:
// sign in, then explicitly approve. Nothing may shortcut the approval — that
// page is where the user sees which account a machine is about to act as.
func TestBothFlowsAlwaysAskToApprove(t *testing.T) {
	srv, auth, _ := authHub(t, true)
	h := srv.Handler()
	signupAndSession(t, h, "first@x.io", "First", "password1")

	// a pending device request, so both flows have something real to approve
	rec := do(t, h, "POST", "/api/auth/device/start", map[string]string{"device": "laptop", "os": "linux"})
	var start struct{ Code string }
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil || start.Code == "" {
		t.Fatalf("device start: %s", rec.Body)
	}

	for _, tc := range []struct{ name, url string }{
		{"cli", "/auth/cli?redirect=" + url.QueryEscape("http://127.0.0.1:9999/callback") + "&state=s1"},
		{"device", "/auth/device/" + start.Code},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// no session: sent to sign in, carrying this request
			req := httptest.NewRequest("GET", tc.url, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			loginURL := rec.Header().Get("Location")
			if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loginURL, "/auth/login?next=") {
				t.Fatalf("without a session = %d %s", rec.Code, loginURL)
			}

			// signing in returns to the request and must NOT grant on the way
			form := url.Values{"email": {"first@x.io"}, "password": {"password1"}, "next": {tc.url}}
			req = httptest.NewRequest("POST", loginURL, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != tc.url {
				t.Fatalf("login should return to the pending request: %d %s", rec.Code, rec.Header().Get("Location"))
			}
			var session *http.Cookie
			for _, c := range rec.Result().Cookies() {
				if c.Name == sessionCookie {
					session = c
				}
			}
			if session == nil {
				t.Fatal("login started no session")
			}

			// and there the approval page waits — every time, for both flows
			req = httptest.NewRequest("GET", tc.url, nil)
			req.AddCookie(session)
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("signing in must not shortcut the approval: %d %s", rec.Code, rec.Header().Get("Location"))
			}
			for _, want := range []string{"first@x.io", "Switch account", "Approve"} {
				if !strings.Contains(rec.Body.String(), want) {
					t.Fatalf("approval page missing %q:\n%s", want, rec.Body)
				}
			}

			// only the POST grants
			req = httptest.NewRequest("POST", tc.url, nil)
			req.AddCookie(session)
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if tc.name == "cli" {
				if rec.Code != http.StatusSeeOther {
					t.Fatalf("approve: %d", rec.Code)
				}
			} else if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Device connected") {
				t.Fatalf("approve: %d\n%s", rec.Code, rec.Body)
			}
		})
	}
	_ = auth
}
