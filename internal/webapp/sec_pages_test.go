package webapp

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Round 10 — the three GET pages no TestSec_ request had ever named:
//
//	GET /auth/reset            pageReset
//	GET /auth/reset/confirm    pageResetConfirm   <- carries a single-use grant
//	GET /auth/device           pageDeviceLegacy
//
// Rounds 3, 6, 7 and 8 all hardened the POST half of password reset. Nobody
// ever fetched the page that carries the token, and nobody ever fetched the
// legacy device page at all.

// sec10Auth builds a BuiltinAuth with its own routes on a bare mux — the
// smallest handler that answers /auth/*. Deliberately NOT permHub: these
// pages are pre-authentication and must be judged without a Server around
// them, since a plain-folder viewer serves the same handlers.
func sec10Auth(t *testing.T) (*BuiltinAuth, http.Handler) {
	t.Helper()
	a := gatedAuth(t, nil)
	mux := http.NewServeMux()
	a.Register(mux)
	return a, mux
}

// sec10Get issues a GET and returns the recorder.
func sec10Get(t *testing.T, h http.Handler, target string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// sec10ResetGrant signs an account up and mints a live reset grant for it,
// returning the account's email and the token the mailed link carries.
func sec10ResetGrant(t *testing.T, a *BuiltinAuth) (email, token string) {
	t.Helper()
	email = "victim@example.com"
	u, err := a.signup(email, "Victim", "correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	return email, a.newGrant("reset", u.ID, time.Hour)
}

// sec10Session signs an account up through the HTTP surface and returns its
// session cookie.
func sec10Session(t *testing.T, h http.Handler, email string) *http.Cookie {
	t.Helper()
	return signupAndSession(t, h, email, "Somebody", "correct-horse")
}

// ---------------------------------------------------------------------------
// 1. The token-bearing page is stored by caches.
// ---------------------------------------------------------------------------

// TestSec_ResetPage_TokenBearingPageIsNotCacheable
//
// GET /auth/reset/confirm?token=<single-use reset grant> answers 200 with the
// grant echoed into the HTML (it becomes the hidden field the POST submits).
// authPage sets exactly one header — Content-Type. No Cache-Control, no
// Expires, no Vary. RFC 9111 §4.2.2 lets any cache — the browser's own disk
// cache, a corporate forward proxy, a CDN in front of a self-hosted hub —
// assign heuristic freshness to a 200 GET with no explicit expiry and store
// it. What it stores is the password-reset grant for somebody's account, in
// both the URL and the body.
//
// The comparison that makes this the server's decision and not a missing
// fixture: the SPA shell, which carries the same class of secret (a session),
// sets Cache-Control on every response (server.go:571) because round 3's work
// established that this hub answers for its own cacheability. The reset page
// is served by a handler that was never asked the question.
func TestSec_ResetPage_TokenBearingPageIsNotCacheable(t *testing.T) {
	a, h := sec10Auth(t)
	_, tok := sec10ResetGrant(t, a)

	rec := sec10Get(t, h, "/auth/reset/confirm?token="+tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/reset/confirm = %d, want 200 (fixture)", rec.Code)
	}
	// The premise: the secret really is in the response body, so a stored copy
	// of this response is a stored copy of the grant.
	if !strings.Contains(rec.Body.String(), tok) {
		t.Fatalf("fixture: the page does not carry the token, nothing to protect")
	}

	cc := strings.ToLower(rec.Header().Get("Cache-Control"))
	if !strings.Contains(cc, "no-store") {
		t.Errorf("GET /auth/reset/confirm?token=…: Cache-Control = %q, want no-store — "+
			"the response body carries a single-use password-reset grant and nothing "+
			"forbids a browser disk cache or a shared forward proxy from keeping it",
			rec.Header().Get("Cache-Control"))
	}
}

// TestSec_DevicePage_ApprovalPageIsNotSharedCacheable
//
// The same missing header on a page that is per-account rather than
// per-token. GET /auth/device/{token} renders whoBlock(user, …) — the signed-in
// account's display name AND email address — and is served with no
// Cache-Control and no Vary: Cookie. Two accounts opening the same approval
// link through one shared cache is all it takes for the second to be handed
// the first one's identity block, and to approve a device as themselves while
// looking at somebody else's email.
//
// The delta that proves this is the server's decision: the SPA shell, which is
// also per-session, answers with Cache-Control on every hit.
func TestSec_DevicePage_ApprovalPageIsNotSharedCacheable(t *testing.T) {
	a, h := sec10Auth(t)
	cookie := sec10Session(t, h, "approver@example.com")

	// A pending device grant, exactly as POST /api/auth/device/start mints it.
	link := "abc123def456"
	id := a.cli.newGrant(cliGrant{kind: "device", link: link, device: "laptop", os: "darwin"}, 10*time.Minute)
	if id == "" {
		t.Fatal("fixture: could not mint a device grant")
	}

	rec := sec10Get(t, h, "/auth/device/"+link, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/device/%s = %d, want 200 (fixture) body=%s", link, rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "approver@example.com") {
		t.Fatalf("fixture: the approval page does not name the account, nothing to leak")
	}

	cc := strings.ToLower(rec.Header().Get("Cache-Control"))
	vary := strings.ToLower(rec.Header().Get("Vary"))
	if !strings.Contains(cc, "no-store") && !strings.Contains(cc, "private") {
		t.Errorf("GET /auth/device/{token}: Cache-Control = %q, want no-store (or at least private) — "+
			"the body names the signed-in account's email and a shared cache may hand it to the next visitor",
			rec.Header().Get("Cache-Control"))
	}
	if !strings.Contains(cc, "no-store") && !strings.Contains(vary, "cookie") {
		t.Errorf("GET /auth/device/{token}: Vary = %q — a per-session page that a cache may store "+
			"must at minimum key on the session cookie", rec.Header().Get("Vary"))
	}
}

// ---------------------------------------------------------------------------
// 2. Framing. The SPA shell refuses it since round 3; /auth/* never did.
// ---------------------------------------------------------------------------

// TestSec_AuthPages_RefuseFraming
//
// server.go:561-563 sets nosniff + X-Frame-Options: DENY + frame-ancestors
// 'none' on the app shell, and sec_audit2_test.go pins BOTH because they are
// not interchangeable. Every /auth/* page — the login form, the reset form,
// the two sign-in approval pages that hand a machine a token acting as you —
// is served by authPage, which sets Content-Type and nothing else.
//
// SameSite=Lax on the session cookie means a framed approval page currently
// renders signed-OUT, so this is not a live account takeover today. It is one
// cookie-policy change away from being one, and the hub already decided this
// question for the document next door.
func TestSec_AuthPages_RefuseFraming(t *testing.T) {
	a, h := sec10Auth(t)
	_, tok := sec10ResetGrant(t, a)

	for _, target := range []string{
		"/auth/login",
		"/auth/signup",
		"/auth/reset",
		"/auth/reset/confirm?token=" + tok,
		"/auth/device",
	} {
		rec := sec10Get(t, h, target, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200 (fixture) body=%s", target, rec.Code, rec.Body)
		}
		csp := strings.ToLower(rec.Header().Get("Content-Security-Policy"))
		xfo := rec.Header().Get("X-Frame-Options")
		if !strings.Contains(csp, "frame-ancestors") {
			t.Errorf("GET %s: Content-Security-Policy = %q carries no frame-ancestors — "+
				"the app shell has refused framing since round 3 and these pages hold the "+
				"credential surface", target, rec.Header().Get("Content-Security-Policy"))
		}
		if !strings.EqualFold(xfo, "DENY") && !strings.EqualFold(xfo, "SAMEORIGIN") {
			t.Errorf("GET %s: X-Frame-Options = %q, want DENY — the fallback for engines "+
				"that do not implement frame-ancestors", target, xfo)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); !strings.EqualFold(got, "nosniff") {
			t.Errorf("GET %s: X-Content-Type-Options = %q, want nosniff", target, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 3. Things the reset page does correctly. These assert the refusal.
// ---------------------------------------------------------------------------

// TestSec_ResetPage_GetNeitherConsumesNorValidatesTheGrant
//
// Fetching the page must not spend the single-use grant (a mail client
// prefetching the link would otherwise lock the account owner out of their own
// reset), and must not tell the fetcher whether the token is real (an oracle
// that turns a leaked-token guess into a confirmed one before it is spent).
func TestSec_ResetPage_GetNeitherConsumesNorValidatesTheGrant(t *testing.T) {
	a, h := sec10Auth(t)
	_, tok := sec10ResetGrant(t, a)

	// Prefetch the page several times, as a scanner or a mail client would.
	for i := 0; i < 3; i++ {
		if rec := sec10Get(t, h, "/auth/reset/confirm?token="+tok, nil); rec.Code != http.StatusOK {
			t.Fatalf("prefetch %d: %d", i, rec.Code)
		}
	}
	// The grant must still be spendable by the account owner.
	form := url.Values{"token": {tok}, "password": {"a-brand-new-password"}}
	req := httptest.NewRequest("POST", "/auth/reset/confirm", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "Your password is updated") {
		t.Fatalf("GET consumed or invalidated the reset grant: POST after prefetch says %s", rec.Body)
	}

	// And a bogus token must render exactly like a live one.
	live := sec10Get(t, h, "/auth/reset/confirm?token="+a.newGrant("reset", "nobody", time.Hour), nil)
	dead := sec10Get(t, h, "/auth/reset/confirm?token=deadbeefdeadbeefdeadbeefdeadbeef", nil)
	if live.Code != dead.Code {
		t.Errorf("valid token answers %d, invalid answers %d — the status code is an oracle",
			live.Code, dead.Code)
	}
	// Both tokens are 32 hex characters, so a page that reflects only the token
	// is byte-identical in length. Any difference is a difference in shape.
	if got, want := len(dead.Body.String()), len(live.Body.String()); got != want {
		t.Errorf("valid page is %d bytes and invalid is %d — the page shape is an oracle for "+
			"whether a guessed reset token exists", want, got)
	}
}

// TestSec_ResetPage_TokenIsNotReflectedIntoMarkup
//
// The token lands in a value= attribute. resetForm html-escapes then %q-quotes
// it; assert an attacker-chosen token cannot break out of either layer, and
// that no Set-Cookie rides along with an unauthenticated page.
func TestSec_ResetPage_TokenIsNotReflectedIntoMarkup(t *testing.T) {
	_, h := sec10Auth(t)
	for _, evil := range []string{
		`"><script>alert(1)</script>`,
		`" onfocus="alert(1)" autofocus="`,
		`'><img src=x onerror=alert(1)>`,
		"\\\"><script>alert(1)</script>",
	} {
		rec := sec10Get(t, h, "/auth/reset/confirm?token="+url.QueryEscape(evil), nil)
		body := rec.Body.String()
		// Isolate the attribute the token lands in and assert nothing inside it
		// can terminate the attribute or open a tag. A substring hunt for the
		// payload is not the test — html.EscapeString leaves "onerror=alert(1)"
		// verbatim inside &lt;img …&gt;, which is inert.
		const open = `name="token" value="`
		i := strings.Index(body, open)
		if i < 0 {
			t.Fatalf("token %q: hidden field not rendered at all: %s", evil, body)
		}
		rest := body[i+len(open):]
		j := strings.Index(rest, `"`)
		if j < 0 {
			t.Fatalf("token %q: value attribute never closes — it swallowed the document", evil)
		}
		if attr := rest[:j]; strings.ContainsAny(attr, "<>'\"") {
			t.Errorf("token %q rendered raw markup characters inside value=%q", evil, attr)
		}
		// And whatever follows the attribute must be the rest of the form, not
		// anything the token contributed.
		if after := rest[j+1:]; !strings.HasPrefix(after, `><label for="f-password">`) {
			t.Errorf("token %q broke out of the input tag; what follows value= is %.80q", evil, after)
		}
		if cs := rec.Result().Cookies(); len(cs) != 0 {
			t.Errorf("GET /auth/reset/confirm set %d cookie(s) on an unauthenticated page", len(cs))
		}
	}
}

// TestSec_ResetPage_DuplicateAndSmuggledTokenParams
//
// ?token=A&token=B, an array-form token[]=, and a fragment must never let one
// value be shown to the human and a different one be spent by the POST. GET
// reads r.URL.Query().Get and POST reads r.FormValue, which merges the URL
// query with the body — so a query token on a POST is live. Assert the value
// the page displays is the value the server would consume.
func TestSec_ResetPage_DuplicateAndSmuggledTokenParams(t *testing.T) {
	a, h := sec10Auth(t)
	_, good := sec10ResetGrant(t, a)
	bad := "0000000000000000000000000000000000000000"

	// Displayed value with the real grant second.
	rec := sec10Get(t, h, "/auth/reset/confirm?token="+bad+"&token="+good, nil)
	shown := strings.Contains(rec.Body.String(), good)

	// What a POST carrying the same query would actually spend.
	form := url.Values{"password": {"another-new-password"}}
	req := httptest.NewRequest("POST", "/auth/reset/confirm?token="+bad+"&token="+good,
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	prec := httptest.NewRecorder()
	h.ServeHTTP(prec, req)
	spent := strings.Contains(prec.Body.String(), "Your password is updated")

	if shown != spent {
		t.Errorf("duplicate token params disagree: the page displayed the live grant = %v "+
			"but the POST spent it = %v — one value is shown to the human and another is consumed",
			shown, spent)
	}
}

// ---------------------------------------------------------------------------
// 4. The legacy device page: what it accepts that its sibling refuses.
// ---------------------------------------------------------------------------

// TestSec_DeviceLegacy_CodeIsNotAnOpenRedirect
//
// pageDeviceLegacy takes ?code= straight off the query and builds
// "/auth/device/" + url.PathEscape(code) as a 303 Location. PathEscape leaves
// "/" alone in neither direction is obvious from reading it, so drive it:
// a code that tries to climb out of /auth/device/ or name another origin must
// not produce a Location outside this hub. An unauthenticated redirector on an
// auth host is a phishing primitive and a token-forwarding one.
func TestSec_DeviceLegacy_CodeIsNotAnOpenRedirect(t *testing.T) {
	_, h := sec10Auth(t)
	for _, code := range []string{
		"//evil.example/",
		"/\\evil.example",
		"http://evil.example/x",
		"../../evil",
		"..%2f..%2fevil",
		"a\r\nLocation: http://evil.example",
	} {
		rec := sec10Get(t, h, "/auth/device?code="+url.QueryEscape(code), nil)
		loc := rec.Header().Get("Location")
		if loc == "" {
			continue // rendered a page instead of redirecting: fine
		}
		if !strings.HasPrefix(loc, "/auth/device/") {
			t.Errorf("code %q produced Location %q — pageDeviceLegacy must only ever "+
				"forward to its own path form", code, loc)
		}
		if strings.Contains(loc, "\n") || strings.Contains(loc, "\r") {
			t.Errorf("code %q put a newline in Location %q — header splitting", code, loc)
		}
	}
}

// TestSec_DeviceLegacy_ForwardingDoesNotRequireOrLeakASession
//
// The path-form sibling redirects an anonymous visitor to /auth/login and
// never discloses whether the token exists. Assert the legacy forwarder does
// not become the disclosure the sibling refuses: with no session it must not
// reveal, by status or body, whether the code names a live grant.
func TestSec_DeviceLegacy_ForwardingDoesNotRequireOrLeakASession(t *testing.T) {
	a, h := sec10Auth(t)
	link := "feedfacefeedface"
	if id := a.cli.newGrant(cliGrant{kind: "device", link: link, device: "laptop", os: "linux"}, 10*time.Minute); id == "" {
		t.Fatal("fixture: no grant")
	}

	live := sec10Get(t, h, "/auth/device?code="+link, nil)
	dead := sec10Get(t, h, "/auth/device?code=0123456789abcdef", nil)
	// Location necessarily differs (it embeds the code); only the status may not.
	if live.Code != dead.Code {
		t.Errorf("legacy device page: live code answers %d, unknown code answers %d — "+
			"an unauthenticated existence oracle for pending sign-in links", live.Code, dead.Code)
	}
	if strings.Contains(live.Body.String(), "laptop") {
		t.Errorf("legacy device page disclosed the pending device's name to an anonymous visitor: %s",
			live.Body)
	}
}

// TestSec_ResetRequestPage_IsNotAnAccountEnumerationOracle
//
// GET /auth/reset is the request form. Assert it discloses nothing and, with
// the POST alongside it, that a known and an unknown address are answered
// identically — the round-3-era property, never asserted from the GET side.
func TestSec_ResetRequestPage_IsNotAnAccountEnumerationOracle(t *testing.T) {
	a, h := sec10Auth(t)
	email, _ := sec10ResetGrant(t, a)

	page := sec10Get(t, h, "/auth/reset", nil)
	if page.Code != http.StatusOK {
		t.Fatalf("GET /auth/reset = %d", page.Code)
	}
	if strings.Contains(page.Body.String(), email) {
		t.Errorf("GET /auth/reset leaked an account address: %s", page.Body)
	}

	post := func(addr string) *httptest.ResponseRecorder {
		form := url.Values{"email": {addr}}
		req := httptest.NewRequest("POST", "/auth/reset", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	known, unknown := post(email), post("nobody@example.com")
	if known.Code != unknown.Code || known.Body.String() != unknown.Body.String() {
		t.Errorf("POST /auth/reset distinguishes a known address (%d) from an unknown one (%d) — "+
			"account enumeration", known.Code, unknown.Code)
	}
}
