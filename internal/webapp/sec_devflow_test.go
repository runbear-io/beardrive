package webapp

// Round 8 — the headless sign-in flow (`bdrive login --device`), attacked on
// the hub side.
//
// Why these live in internal/webapp and not cmd/bdrive: every property below
// is a decision `CLIAuth` makes, and the only honest way to observe the
// outcome is to look at what BuiltinAuth actually minted — how many tokens one
// approval produced, and under which device name. That is
// `BuiltinAuth.tokens`, an unexported field, so the test has to be in this
// package. The CLI half is driven from cmd/bdrive/sec_login_test.go.
//
// This flow is where a permanent device credential is created out of one human
// click. It is also the only sign-in path an agent or a headless machine can
// use, so it is the one a README-following agent actually walks.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------- harness

// secdevHub builds a hub with real accounts and the real route table, plus
// alice's browser session — the human who approves.
func secdevHub(t *testing.T) (http.Handler, *BuiltinAuth, *http.Cookie) {
	t.Helper()
	srv, auth, _ := authHub(t, true)
	h := srv.Handler()
	cookie := signupAndSession(t, h, "alice@example.com", "Alice", "password1")
	return h, auth, cookie
}

// secdevPost sends a JSON body to path, optionally with alice's cookie.
func secdevPost(t *testing.T, h http.Handler, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = strings.NewReader(string(raw))
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(http.MethodPost, path, rd)
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// secdevStart begins a headless sign-in and returns the poll code and the URL
// the human is told to open.
func secdevStart(t *testing.T, h http.Handler, device, os string) (code, verifyURL string) {
	t.Helper()
	rec := secdevPost(t, h, "/api/auth/device/start", map[string]string{"device": device, "os": os}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("device/start: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Code      string `json:"code"`
		VerifyURL string `json:"verify_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("device/start body %q: %v", rec.Body, err)
	}
	if out.Code == "" {
		t.Fatalf("device/start returned no code: %s", rec.Body)
	}
	return out.Code, out.VerifyURL
}

// secdevApprove is the human clicking Approve on the page the link opens.
func secdevApprove(t *testing.T, h http.Handler, code string, cookie *http.Cookie) {
	t.Helper()
	rec := secdevPost(t, h, "/auth/device/"+code, nil, cookie)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Device connected") {
		t.Fatalf("approval did not take: %d %s", rec.Code, rec.Body)
	}
}

// secdevTokens snapshots the hashes of every device token the hub holds.
func secdevTokens(auth *BuiltinAuth) map[string]authToken {
	auth.mu.Lock()
	defer auth.mu.Unlock()
	out := make(map[string]authToken, len(auth.tokens))
	for k, v := range auth.tokens {
		out[k] = v
	}
	return out
}

// ------------------------------------------- 1. one approval, one token

// TestSec_DeviceFlow_OneApprovalMintsExactlyOneToken
//
// apiDevicePoll reads the grant and consumes it in two separate acquisitions
// of c.mu:
//
//	g, ok := c.peek("device", req.Code)   // lock / unlock
//	...
//	c.take("device", req.Code)            // lock / unlock — return discarded
//	c.issue(w, g.user, device)
//
// Nothing between them is atomic, and take's answer ("was I the one who
// consumed it?") is thrown away, so every caller that got past peek goes on to
// issue. One human approval therefore mints as many long-lived device tokens
// as there are polls in flight, each independently valid and independently
// unrevokable (there is no revocation route at all — see the CLI half).
//
// This is round 2's seat-check race, on credential issuance. A grant is a
// single-use authorisation: consuming it has to be the thing that decides who
// gets the token.
func TestSec_DeviceFlow_OneApprovalMintsExactlyOneToken(t *testing.T) {
	h, auth, cookie := secdevHub(t)

	// Control: the sequential flow works exactly once. If this half breaks,
	// the concurrent half below is measuring the harness, not the server.
	code, _ := secdevStart(t, h, "sam-laptop", "darwin")
	if rec := secdevPost(t, h, "/api/auth/device/poll", map[string]string{"code": code}, nil); !strings.Contains(rec.Body.String(), `"pending":true`) {
		t.Fatalf("poll before approval should be pending: %d %s", rec.Code, rec.Body)
	}
	secdevApprove(t, h, code, cookie)
	if rec := secdevPost(t, h, "/api/auth/device/poll", map[string]string{"code": code}, nil); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"token"`) {
		t.Fatalf("poll after approval should mint a token: %d %s", rec.Code, rec.Body)
	}
	if rec := secdevPost(t, h, "/api/auth/device/poll", map[string]string{"code": code}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a consumed grant must be dead, got %d %s", rec.Code, rec.Body)
	}

	// The attack: poll one approved grant from several callers at once.
	//
	// Several grants are raced per round and the map is kept near its working
	// size, because peek and take each sweep the whole map under c.mu: that is
	// what makes the lock contended enough for the scheduler to land another
	// caller between one caller's peek and its take, instead of letting the
	// unlock/relock barge straight through.
	const rounds, grants, callers = 12, 24, 8
	worst := 1
	for round := 0; round < rounds; round++ {
		var filler []string
		for len(filler) < 120 {
			c, _ := secdevStart(t, h, "filler", "linux")
			filler = append(filler, c)
		}

		codes := make([]string, 0, grants)
		for i := 0; i < grants; i++ {
			c, _ := secdevStart(t, h, "sam-laptop", "darwin")
			secdevApprove(t, h, c, cookie)
			codes = append(codes, c)
		}

		before := secdevTokens(auth)

		var wg, ready sync.WaitGroup
		start := make(chan struct{})
		ready.Add(grants * callers)
		wg.Add(grants * callers)
		for _, c := range codes {
			for i := 0; i < callers; i++ {
				go func(code string) {
					defer wg.Done()
					ready.Done()
					<-start
					body := strings.NewReader(`{"code":"` + code + `","device":"sam-laptop"}`)
					req := httptest.NewRequest(http.MethodPost, "/api/auth/device/poll", body)
					req.Header.Set("Content-Type", "application/json")
					h.ServeHTTP(httptest.NewRecorder(), req)
				}(c)
			}
		}
		ready.Wait()
		close(start)
		wg.Wait()

		minted := 0
		for hash := range secdevTokens(auth) {
			if _, had := before[hash]; !had {
				minted++
			}
		}
		if minted > worst {
			worst = minted
		}
		// Drain the fillers so the next round starts from the same shape.
		for _, c := range filler {
			secdevPost(t, h, "/api/auth/exchange", map[string]string{"code": c}, nil)
		}
	}
	if worst > grants {
		t.Fatalf("%d approvals minted %d device tokens across concurrent polls; "+
			"a single-use grant must mint exactly one token per approval", grants, worst)
	}
}

// ----------------------------------- 2. the approved device is the device

// TestSec_DeviceFlow_TheDeviceTheHumanApprovedIsTheDeviceTheTokenRecords
//
// apiDeviceStart records the requesting device's name and OS, and the approval
// page renders them — that disclosure is the whole defence of this flow, whose
// stated weakness is "a stranger can send you their own pending link, and a
// page that just says Approve gives you nothing to notice with"
// (authcli.go:394).
//
// But the poll then re-chooses:
//
//	device := req.Device        // whatever the POLLER says, at poll time
//	if device == "" { device = g.device }
//	c.issue(w, g.user, device)
//
// so the string the human read and the string the credential is filed under
// are two different values, picked by two different parties, and the poller
// picks second. The approval page says "Device: sam-laptop"; the token lands
// in the account's credential list as something else entirely. Every later
// question an operator can ask — which of my devices is this, which one do I
// distrust — is answered from the second value.
func TestSec_DeviceFlow_TheDeviceTheHumanApprovedIsTheDeviceTheTokenRecords(t *testing.T) {
	h, auth, cookie := secdevHub(t)

	code, _ := secdevStart(t, h, "sam-laptop", "darwin")

	// What the human is shown. Control: the page really does name the device
	// that started the flow, so the mismatch below is the server's doing.
	req := httptest.NewRequest(http.MethodGet, "/auth/device/"+code, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "sam-laptop") {
		t.Fatalf("approval page must name the requesting device: %d %s", rec.Code, rec.Body)
	}

	before := secdevTokens(auth)
	secdevApprove(t, h, code, cookie)

	// The poller names something else. Nothing was approved under this name.
	const forged = "ci-runner-prod"
	if rec := secdevPost(t, h, "/api/auth/device/poll",
		map[string]string{"code": code, "device": forged}, nil); rec.Code != http.StatusOK {
		t.Fatalf("poll: %d %s", rec.Code, rec.Body)
	}

	var got []string
	for hash, tok := range secdevTokens(auth) {
		if _, had := before[hash]; !had {
			got = append(got, tok.Device)
		}
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly one new token, got %d (%v)", len(got), got)
	}
	if got[0] != "sam-laptop" {
		t.Fatalf("token filed under device %q, but the human approved %q — "+
			"what was approved is not what was issued", got[0], "sam-laptop")
	}
}

// --------------------------- 3. the browser link is not the poll credential

// TestSec_DeviceFlow_TheLinkTheHumanOpensIsNotAlsoThePollCredential
//
// RFC 8628 splits the headless flow into two secrets on purpose: a device_code
// the client keeps and polls with, and a user_code carried in the URL the human
// opens. This hub issues ONE value and uses it for both — the id in
// verify_url is the same string /api/auth/device/poll accepts.
//
// So the URL is a bearer credential for a permanent device token, and it is
// handled like a URL: printed to a terminal (scrollback, agent transcripts, CI
// logs), pasted into an address bar (history, profile sync, extensions),
// forwarded to whoever is being asked to approve it. Anyone who reads it
// between approval and the CLI's next poll — and, with the race above, even
// after — mints a token that acts as the approver, on a hub with no way to
// revoke it.
//
// The secure shape: the value in the link authorises the human's approval; a
// separate value, never displayed, authorises the poll.
func TestSec_DeviceFlow_TheLinkTheHumanOpensIsNotAlsoThePollCredential(t *testing.T) {
	h, auth, cookie := secdevHub(t)

	code, verifyURL := secdevStart(t, h, "sam-laptop", "darwin")
	if verifyURL == "" {
		t.Fatal("device/start returned no verify_url")
	}
	if strings.Contains(verifyURL, code) {
		t.Errorf("the poll credential %q appears verbatim in the link the human opens (%q): "+
			"one value cannot be both the secret the client keeps and the string a browser displays",
			code, verifyURL)
	}

	// Behavioural half: whatever is in the link must not, by itself, buy a
	// token. Everyone in the approval's path sees this string.
	fromLink := verifyURL[strings.LastIndex(verifyURL, "/")+1:]
	secdevApprove(t, h, code, cookie)

	before := secdevTokens(auth)
	rec := secdevPost(t, h, "/api/auth/device/poll", map[string]string{"code": fromLink}, nil)
	minted := 0
	for hash := range secdevTokens(auth) {
		if _, had := before[hash]; !had {
			minted++
		}
	}
	if minted != 0 {
		t.Fatalf("a caller holding only the browser link (%q) minted %d device token(s) (poll answered %d): "+
			"the approval URL must not be the poll credential", fromLink, minted, rec.Code)
	}
}

// ------------------------------------------ 4. the approval page's own gate

// TestSec_DeviceFlow_ApprovalNeedsAPostFromACookieSession
//
// The clean half, asserted so it stays clean: nothing about a pending grant is
// approved by a GET (so a link someone got you to open grants nothing), the
// session is resolved from the cookie only (so a device token cannot approve
// the next device), and the cookie is SameSite=Lax, which is the entire
// defence against a cross-site POST to /auth/device/{token} — the classic
// device-flow CSRF, where the attacker starts the flow and has the victim's
// browser approve it.
func TestSec_DeviceFlow_ApprovalNeedsAPostFromACookieSession(t *testing.T) {
	h, auth, cookie := secdevHub(t)

	// (a) a GET does not approve.
	code, _ := secdevStart(t, h, "sam-laptop", "darwin")
	req := httptest.NewRequest(http.MethodGet, "/auth/device/"+code, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("approval page GET: %d", rec.Code)
	}
	if r := secdevPost(t, h, "/api/auth/device/poll", map[string]string{"code": code}, nil); !strings.Contains(r.Body.String(), `"pending":true`) {
		t.Fatalf("a GET on the approval page granted something: %d %s", r.Code, r.Body)
	}

	// (b) a device token is not a browser session. Mint one the honest way.
	secdevApprove(t, h, code, cookie)
	pr := secdevPost(t, h, "/api/auth/device/poll", map[string]string{"code": code}, nil)
	var issued struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(pr.Body.Bytes(), &issued); err != nil || issued.Token == "" {
		t.Fatalf("poll did not issue a token: %d %s", pr.Code, pr.Body)
	}
	next, _ := secdevStart(t, h, "attacker-box", "linux")
	req = httptest.NewRequest(http.MethodPost, "/auth/device/"+next, nil)
	req.Header.Set("Authorization", "Bearer "+issued.Token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if r := secdevPost(t, h, "/api/auth/device/poll", map[string]string{"code": next}, nil); !strings.Contains(r.Body.String(), `"pending":true`) {
		t.Fatalf("a device token approved the next device: %d %s", r.Code, r.Body)
	}

	// (c) no session at all is bounced to sign-in, not granted.
	rec = secdevPost(t, h, "/auth/device/"+next, nil, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated approval POST: %d %s", rec.Code, rec.Body)
	}

	// (d) the cookie that gates all of the above is SameSite=Lax, which is
	// what stops a cross-site form POST from carrying it.
	form := strings.NewReader("email=bob@example.com&name=Bob&password=password1")
	req = httptest.NewRequest(http.MethodPost, "/auth/signup", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var found bool
	for _, c := range rec.Result().Cookies() {
		if c.Name != sessionCookie {
			continue
		}
		found = true
		if c.SameSite != http.SameSiteLaxMode && c.SameSite != http.SameSiteStrictMode {
			t.Fatalf("session cookie SameSite = %v; a cross-site POST to /auth/device/{token} would approve a stranger's grant", c.SameSite)
		}
		if !c.HttpOnly {
			t.Fatal("session cookie is not HttpOnly")
		}
	}
	if !found {
		t.Fatal("no session cookie to check")
	}
	_ = auth
}

// ---------------------------------------- 5. the loopback redirect allowlist

// TestSec_CLIAuth_TheLoopbackRedirectAcceptsOnlyLoopback
//
// pageCLI bounces a freshly minted one-time code to whatever ?redirect= says,
// so that parameter decides where a credential lands. It is guarded by a
// scheme check plus a three-name host allowlist; this pins what that guard
// accepts and — more importantly — that the classic ways of spelling a foreign
// host as a loopback one do not get through.
func TestSec_CLIAuth_TheLoopbackRedirectAcceptsOnlyLoopback(t *testing.T) {
	h, _, cookie := secdevHub(t)

	refused := []string{
		"",                                  // absent
		"http://127.0.0.1@evil.example/cb",  // userinfo
		"http://127.0.0.1.evil.example/cb",  // suffix
		"http://localhost.evil.example/cb",  // suffix
		"https://evil.example/cb",           // plainly elsewhere
		"//evil.example/cb",                 // scheme-relative
		"/callback",                         // same-origin relative
		"javascript:fetch('//evil')",        // non-http scheme
		"data:text/html,x",                  // non-http scheme
		"http://2130706433/cb",              // decimal loopback
		"http://0177.0.0.1/cb",              // octal loopback
		"http://[::ffff:127.0.0.1]/cb",      // v4-mapped v6
		"http://0.0.0.0/cb",                 // wildcard
		"http://127.0.0.2/cb",               // loopback /8, not the allowlist
		"http:127.0.0.1:9/cb",               // opaque
		"http://evil.example#@127.0.0.1/cb", // fragment trick
	}
	for _, bad := range refused {
		req := httptest.NewRequest(http.MethodGet, "/auth/cli?state=s&redirect="+urlQ(bad), nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("redirect=%q answered %d, want 400 (a code must not be bounced off loopback)", bad, rec.Code)
		}
	}

	// Control: the shape the CLI actually builds is accepted, so the refusals
	// above are the guard and not a broken request.
	req := httptest.NewRequest(http.MethodGet, "/auth/cli?state=s&redirect="+urlQ("http://127.0.0.1:53123/callback"), nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("the CLI's own redirect shape was refused: %d %s", rec.Code, rec.Body)
	}
	// And a GET still grants nothing: the approval is the POST.
	if strings.Contains(rec.Body.String(), "code=") {
		t.Fatalf("GET /auth/cli handed out a code: %s", rec.Body)
	}
}

// urlQ percent-encodes a query value.
func urlQ(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			const hex = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		}
	}
	return b.String()
}
