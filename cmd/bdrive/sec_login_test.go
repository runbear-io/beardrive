package main

// Round 8 — the authenticated half of the front door.
//
// Round 7 drove `bdrive init` end to end for the first time and held two
// criticals, but every one of those tests ran against a fixture hub answering
// `auth.enabled: false` — so the login flow inside init never executed, and
// round 7's own critical A lived in exactly the branch that skips it.
//
// The hub here is not a hand-rolled fake for the parts that matter: accounts,
// token issuance, token validation and both CLI sign-in flows are the REAL
// webapp.BuiltinAuth and webapp.CLIAuth, mounted on a test server. So "the hub
// still accepts this token" is the hub's own answer, not the fixture's
// opinion. Only /api/config, the project registry and the store proxy are
// stubbed, because none of them is what is under attack here.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/webapp"
)

// ---------------------------------------------------------------- harness

// secloginCapture runs fn with os.Stdout redirected and returns what it
// printed. seccliRun does this for a cobra command; these attacks call
// unexported functions directly, so they need the same trick without one.
func secloginCapture(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stdout = old
	w.Close()
	out := <-done
	r.Close()
	return out
}

type secloginOpts struct {
	breakDeviceStart bool // /api/auth/device/start answers 500: sign-in cannot complete
	projectStatus    int  // non-zero: every /api/projects* answers this status
}

type secloginHub struct {
	url  string
	auth *webapp.BuiltinAuth

	mu     sync.Mutex
	auths  []string // every Authorization header this hub was handed
	store  map[string][]byte
	cookie *http.Cookie // the approving human's browser session
}

func (h *secloginHub) sentAuth() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.auths...)
}

// accepts asks the hub's real account store whether a bearer token is live.
func (h *secloginHub) accepts(token string) bool {
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	_, ok := h.auth.Authenticate(req)
	return ok
}

// secloginNewHub starts a hub whose /auth/* and /api/auth/* are the real
// implementation, with one signed-in human (alice) who approves every headless
// sign-in the moment it starts — which is what a person with the link does.
func secloginNewHub(t *testing.T, opts secloginOpts) *secloginHub {
	t.Helper()
	auth, err := webapp.OpenBuiltinAuth(filepath.Join(t.TempDir(), "auth.json"), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := &secloginHub{auth: auth, store: map[string][]byte{}}

	inner := http.NewServeMux()
	auth.Register(inner)
	inner.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSONTo(w, map[string]any{"mode": "hub", "auth": map[string]any{
			"enabled": true, "cli_login": "/auth/cli",
		}})
	})
	project := func(name string) map[string]any {
		return map[string]any{"id": "7f3a2c91-4d5e-4b8a-9c17-2ad0f6b3e901", "name": name, "template": ""}
	}
	inner.HandleFunc("/api/projects", func(w http.ResponseWriter, r *http.Request) {
		if opts.projectStatus != 0 {
			http.Error(w, "no", opts.projectStatus)
			return
		}
		if r.Method == http.MethodGet {
			writeJSONTo(w, map[string]any{"projects": []any{project("listed")}})
			return
		}
		var body struct{ Name string }
		json.NewDecoder(r.Body).Decode(&body)
		writeJSONTo(w, map[string]any{"project": project(body.Name), "created": true})
	})
	inner.HandleFunc("/api/projects/", func(w http.ResponseWriter, r *http.Request) {
		if opts.projectStatus != 0 {
			http.Error(w, "no", opts.projectStatus)
			return
		}
		writeJSONTo(w, project(strings.TrimPrefix(r.URL.Path, "/api/projects/")))
	})
	// Just enough store proxy for one sync cycle against an empty project.
	inner.HandleFunc("/api/p/", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		switch {
		case strings.HasSuffix(r.URL.Path, "/store/list"):
			objects := []any{}
			h.mu.Lock()
			for k, v := range h.store {
				if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
					objects = append(objects, map[string]any{"key": k, "size": len(v)})
				}
			}
			h.mu.Unlock()
			writeJSONTo(w, map[string]any{"objects": objects})
		case strings.HasSuffix(r.URL.Path, "/store/exists"):
			h.mu.Lock()
			_, ok := h.store[key]
			h.mu.Unlock()
			writeJSONTo(w, map[string]any{"exists": ok})
		case strings.HasSuffix(r.URL.Path, "/store/sign"):
			writeJSONTo(w, map[string]any{"mode": "server"})
		case strings.HasSuffix(r.URL.Path, "/store/object"):
			if r.Method == http.MethodGet {
				h.mu.Lock()
				body, ok := h.store[key]
				h.mu.Unlock()
				if !ok {
					http.Error(w, "no such object", http.StatusNotFound)
					return
				}
				w.Write(body)
				return
			}
			data, _ := io.ReadAll(r.Body)
			h.mu.Lock()
			h.store[key] = data
			h.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "no route", http.StatusNotFound)
		}
	})

	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		if a := r.Header.Get("Authorization"); a != "" {
			h.auths = append(h.auths, a)
		}
		h.mu.Unlock()

		if r.URL.Path == "/api/auth/device/start" {
			if opts.breakDeviceStart {
				http.Error(w, "device sign-in is down", http.StatusInternalServerError)
				return
			}
			// Run the real handler, read the grant it minted, then have the
			// human approve it before the CLI's first poll.
			rec := httptest.NewRecorder()
			inner.ServeHTTP(rec, r)
			var out struct {
				Code string `json:"code"`
			}
			json.Unmarshal(rec.Body.Bytes(), &out)
			if out.Code != "" {
				h.approve(t, inner, out.Code)
			}
			for k, vs := range rec.Header() {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(rec.Code)
			w.Write(rec.Body.Bytes())
			return
		}
		inner.ServeHTTP(w, r)
	})

	ts := httptest.NewServer(outer)
	t.Cleanup(ts.Close)
	h.url = ts.URL
	h.cookie = secloginSession(t, inner, "alice@example.com", "Alice", "password1")
	return h
}

// secloginSession signs a human up through the real signup page.
func secloginSession(t *testing.T, h http.Handler, email, name, pass string) *http.Cookie {
	t.Helper()
	form := url.Values{"email": {email}, "name": {name}, "password": {pass}}
	req := httptest.NewRequest(http.MethodPost, "/auth/signup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == "bdrive_session" && c.Value != "" {
			return c
		}
	}
	t.Fatalf("signup set no session cookie: %d %s", rec.Code, rec.Body)
	return nil
}

// approve is the human clicking Approve on the link the CLI printed.
func (h *secloginHub) approve(t *testing.T, inner http.Handler, code string) {
	req := httptest.NewRequest(http.MethodPost, "/auth/device/"+code, nil)
	req.AddCookie(h.cookie)
	rec := httptest.NewRecorder()
	inner.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "Device connected") {
		t.Errorf("approval of %s did not take: %d %s", code, rec.Code, rec.Body)
	}
}

// secloginEnv is an isolated HOME/BDRIVE_HOME plus a runner for the real
// binary. It reuses secinitBinary and secinitEnvWithout from sec_init_test.go.
type secloginEnv struct {
	bin   string
	home  string
	bhome string
	env   []string
}

func secloginNewEnv(t *testing.T) *secloginEnv {
	t.Helper()
	bin := secinitBinary(t)
	home := t.TempDir()
	bhome := filepath.Join(home, ".bdrive")
	return &secloginEnv{
		bin: bin, home: home, bhome: bhome,
		env: append(secinitEnvWithout("HOME", "BDRIVE_HOME", "BDRIVE_TOKEN", "XDG_CONFIG_HOME"),
			"HOME="+home, "BDRIVE_HOME="+bhome, "XDG_CONFIG_HOME="+filepath.Join(home, ".config")),
	}
}

func (e *secloginEnv) run(dir string, args ...string) (string, error) {
	cmd := exec.Command(e.bin, args...)
	cmd.Dir = dir
	cmd.Env = e.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// settings reads the device's saved session.
func (e *secloginEnv) settings(t *testing.T) (server, token, email string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.bhome, "settings.json"))
	if err != nil {
		return "", "", ""
	}
	var s struct {
		Server string `json:"server"`
		Token  string `json:"token"`
		Email  string `json:"email"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("settings.json is not readable JSON: %v (%s)", err, raw)
	}
	return s.Server, s.Token, s.Email
}

// ------------------------------------- 1. the hub renders nothing to the tty

// TestSec_Login_HubChosenLoginPathNeverRendersRawToTheTerminal
//
// runLogin takes the sign-in path from the SERVER:
//
//	loginPath := cfg.Auth.CLILogin          // login.go:169, straight off /api/config
//	loginURL := fmt.Sprintf("%s%s?redirect=%s&state=%s", server, loginPath, ...)
//	fmt.Println("  " + loginURL)            // login.go:292 — a bare concatenation
//
// Round 7 closed this class on every other hub-chosen string the login flow
// prints — `whoami`, `status`, `login --status`, runLogin's closing line and
// the device flow's verify_url all go through safeField
// (TestSec_Login_HubChosenAccountStringsAreNotRenderedToTheTerminal) — and
// missed this one, which is the FIRST thing a hub gets to print on a machine
// that has never talked to it. `bdrive login <url>` is the one command whose
// whole job is to point the CLI at a server it does not yet trust.
//
// Row 21's premise: a peer- or hub-chosen string must not be able to repaint
// the operator's terminal, write their clipboard (OSC 52), or reorder the row
// it appears on (the bidi overrides).
func TestSec_Login_HubChosenLoginPathNeverRendersRawToTheTerminal(t *testing.T) {
	// openBrowser must fail so browserLogin returns instead of waiting five
	// minutes for a callback; it resolves its opener off PATH.
	t.Setenv("PATH", "")

	// Control: a benign path prints, and prints cleanly. If this trips, the
	// capture below is measuring the harness.
	clean := secloginCapture(t, func() {
		browserLogin("https://hub.example", "/auth/cli")
	})
	if !strings.Contains(clean, "https://hub.example/auth/cli?redirect=") {
		t.Fatalf("browserLogin printed no sign-in URL: %q", clean)
	}
	if bad := secinitDangerous(clean); len(bad) > 0 {
		t.Fatalf("the benign control itself carried %v", bad)
	}

	out := secloginCapture(t, func() {
		browserLogin("https://hub.example", "/auth/cli"+secinitHostileName)
	})
	if bad := secinitDangerous(out); len(bad) > 0 {
		t.Fatalf("the hub's cli_login reached the terminal raw: %v\nprinted: %q", bad, out)
	}
}

// ------------------- 1b. the loopback callback is bound to nothing local

// TestSec_Login_TheLoopbackCallbackOnlyCompletesTheFlowItStarted
//
// browserLogin's whole binding is the `state` it generates:
//
//	loginURL := fmt.Sprintf("%s%s?redirect=%s&state=%s", server, loginPath, redirect, state)
//	fmt.Println("  " + loginURL)              // stdout: scrollback, CI logs, agent transcripts
//	openBrowser(loginURL)                     // argv[1] of `open`/`xdg-open` — visible in `ps`
//	...
//	if r.URL.Path != "/callback" || r.URL.Query().Get("state") != state { 404 }
//	return exchangeCode(server, r.URL.Query().Get("code"))
//
// So `state` is not a secret at all: it is printed, and it is handed to a
// child process as a command-line argument, which every local account can read
// with `ps`. Together with the listener's port — in the same string — it is
// everything needed to hand this CLI a callback.
//
// And the code the CLI then exchanges is bound to NOTHING it chose. A one-time
// code minted by a different browser session, for a different account, is
// accepted and exchanged, and the device is signed in as that account: from
// then on the victim's folders sync into the ATTACKER's project, and journals
// carry the attacker's identity.
//
// This test plays the local attacker exactly as one would: it learns the URL
// from the argv the browser launcher was given, then completes the sign-in as
// its own account against the victim's loopback listener.
//
// The standard fix is RFC 7636/8252 PKCE: the CLI sends a code_challenge with
// the request and the exchange requires the matching code_verifier, so a code
// minted by any other flow cannot be redeemed here.
func TestSec_Login_TheLoopbackCallbackOnlyCompletesTheFlowItStarted(t *testing.T) {
	hub := secloginNewHub(t, secloginOpts{})

	// A browser launcher that does what every launcher does: receives the URL
	// as argv[1]. It records it, which is exactly what `ps` gives any local
	// process on this machine.
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	script := "#!/bin/sh\nprintf '%s' \"$1\" > " + argvFile + "\n"
	for _, name := range []string{"open", "xdg-open", "rundll32"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)

	// The attacker's own account on the same hub. Nothing privileged.
	mallory := secloginBrowser(t)
	secloginSignup(t, mallory, hub.url, "mallory@evil.example", "Mallory", "password1")

	go func() {
		var raw []byte
		for i := 0; i < 600; i++ {
			if b, err := os.ReadFile(argvFile); err == nil && len(b) > 0 {
				raw = b
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if len(raw) == 0 {
			return
		}
		u, err := url.Parse(string(raw))
		if err != nil {
			return
		}
		// Everything the attacker needs was in that one string.
		back := u.Query().Get("redirect")
		state := u.Query().Get("state")

		// Approve the sign-in as MALLORY, aimed at the victim's listener.
		approve := hub.url + "/auth/cli?redirect=" + url.QueryEscape(back) + "&state=" + url.QueryEscape(state)
		req, _ := http.NewRequest(http.MethodPost, approve, nil)
		resp, err := mallory.Do(req)
		if err != nil {
			return
		}
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		if loc == "" {
			return
		}
		// Deliver mallory's one-time code to the victim's callback.
		plain := &http.Client{}
		if r2, err := plain.Get(loc); err == nil {
			r2.Body.Close()
		}
	}()

	var user serverUser
	done := make(chan struct{})
	secloginCapture(t, func() {
		go func() {
			defer close(done)
			_, user, _ = browserLogin(hub.url, "/auth/cli")
		}()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("browserLogin never returned")
		}
	})

	if user.Email == "mallory@evil.example" {
		t.Fatalf("a local process that only read the sign-in URL signed this device in as %q — "+
			"the callback accepts a one-time code minted by any other flow", user.Email)
	}
}

// secloginBrowser is an HTTP client with a cookie jar that does not follow
// redirects, i.e. one browser session.
func secloginBrowser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// secloginSignup creates an account over real HTTP, leaving the session in the
// client's jar.
func secloginSignup(t *testing.T, c *http.Client, base, email, name, pass string) {
	t.Helper()
	form := url.Values{"email": {email}, "name": {name}, "password": {pass}}
	resp, err := c.PostForm(base+"/auth/signup", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		t.Fatalf("signup for %s: %d %s", email, resp.StatusCode, body)
	}
}

// --------------------------------------------- 2. logout, on the wire

// TestSec_Logout_SigningTheDeviceOutEndsItsTokenOnTheHub
//
// `bdrive logout` rewrites settings.json and stops. There is no revocation
// route on the hub at all, device tokens carry no expiry, and the daemon that
// is already running holds its own copy for its lifetime — so the credential
// this command exists to retire stays live forever.
//
// The hub in this test is the real BuiltinAuth, so "still accepts it" is its
// answer and not the fixture's. The single decision under test is whether the
// documented way to sign a device out means anything past the local file:
// CLAUDE.md says logout leaves the device "no longer authenticated to the
// bdrive server", and the browser half of the product already got this right
// (row 1, TestSec_Path_LogoutRevokesTheTokenNotJustTheCookie).
//
// It matters most in the case operators actually have: a laptop is lost, a
// contractor rotates off, a token is pasted into a log. Today the only remedy
// is a hub-wide password reset of the account.
func TestSec_Logout_SigningTheDeviceOutEndsItsTokenOnTheHub(t *testing.T) {
	hub := secloginNewHub(t, secloginOpts{})
	e := secloginNewEnv(t)

	if out, err := e.run(e.home, "login", hub.url); err != nil {
		t.Fatalf("login: %v\n%s", err, out)
	}
	server, token, email := e.settings(t)
	if server != hub.url || token == "" || email != "alice@example.com" {
		t.Fatalf("login did not store a session: server=%q token=%q email=%q", server, token, email)
	}
	// Control: the hub really did mint this credential and really does honour
	// it, so the assertion after logout is about revocation and nothing else.
	if !hub.accepts(token) {
		t.Fatalf("the hub does not accept the token it just issued")
	}

	out, err := e.run(e.home, "logout")
	if err != nil {
		t.Fatalf("logout: %v\n%s", err, out)
	}
	if _, tok, _ := e.settings(t); tok != "" {
		t.Fatalf("logout left the token in settings.json: %q", tok)
	}

	if hub.accepts(token) {
		t.Fatalf("after `bdrive logout` the hub still accepts this device's token — "+
			"the credential outlives the sign-out that was supposed to end it\nlogout said: %s", out)
	}
}

// ------------------------------- 3. switching hubs, with auth on both ends

// TestSec_Init_ServerSwitchNeverCarriesTheOldHubsTokenWhenAuthIsOn
//
// Round 7's critical A: `init --server <url>` handed the PREVIOUS hub's device
// token to a server that answered `auth: disabled`, because settings.Server is
// the entirety of the token binding and the no-auth branch of ensureLogin
// wrote the new server without clearing the token. The fix clears it on both
// branches — but every round-7 test ran with auth off, so the branch that
// actually runs a sign-in has never been exercised.
//
// This drives the real thing: two hubs, both requiring accounts, the device
// signed in to the first, then pointed at the second. The second hub records
// every Authorization header it is ever handed.
func TestSec_Init_ServerSwitchNeverCarriesTheOldHubsTokenWhenAuthIsOn(t *testing.T) {
	hubA := secloginNewHub(t, secloginOpts{})
	hubB := secloginNewHub(t, secloginOpts{})
	e := secloginNewEnv(t)

	if out, err := e.run(e.home, "login", hubA.url); err != nil {
		t.Fatalf("login to A: %v\n%s", err, out)
	}
	_, tokenA, _ := e.settings(t)
	if tokenA == "" {
		t.Fatal("no token from hub A")
	}
	// Control: this harness does record bearer headers. `login --status` asks
	// the hub who we are, which is the one call that carries the token.
	if out, err := e.run(e.home, "login", "--status"); err != nil {
		t.Fatalf("login --status: %v\n%s", err, out)
	}
	if !secloginCarries(hubA.sentAuth(), tokenA) {
		t.Fatalf("hub A never saw its own token; the recorder is not working: %v", hubA.sentAuth())
	}

	work := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	defer e.run(work, "stop", work)
	out, err := e.run(work, "init", "--name", "wiki", "--server", hubB.url, "--yes", "--no-hooks", "--no-autostart")
	if err != nil {
		t.Fatalf("init against hub B: %v\n%s", err, out)
	}

	if secloginCarries(hubB.sentAuth(), tokenA) {
		t.Fatalf("hub B was handed hub A's device token\nB saw: %v\nA's token: %s", hubB.sentAuth(), tokenA)
	}
	server, tokenNew, _ := e.settings(t)
	if server != hubB.url {
		t.Fatalf("settings.Server = %q, want %q", server, hubB.url)
	}
	if tokenNew == tokenA {
		t.Fatal("settings still hold hub A's token while pointing at hub B")
	}
	if !hubB.accepts(tokenNew) || hubA.accepts(tokenNew) {
		t.Fatalf("the stored token is not a hub-B credential (B accepts: %v, A accepts: %v)",
			hubB.accepts(tokenNew), hubA.accepts(tokenNew))
	}
}

// secloginCarries reports whether any recorded header carried this token.
func secloginCarries(headers []string, token string) bool {
	for _, h := range headers {
		if strings.Contains(h, token) {
			return true
		}
	}
	return false
}

// ------------------------------- 4. a sign-in that fails, and one that 403s

// TestSec_Init_AFailedSignInLeavesNoHalfConfiguredDevice
//
// Two ways the authenticated front door can end badly, neither of which
// round 7 could reach:
//
//	sign-in itself fails      — the new hub's device flow is down
//	sign-in works, project 403s — you are authenticated and then refused
//
// In the first, the device must be left exactly as it was: still signed in to
// the hub it was signed in to, with a token that still belongs to that hub —
// not stranded with a cleared session pointing at a server it never reached.
// In the second, the folder must not become a half-mount: no .bdrive that a
// later `bdrive init` would silently "resume".
func TestSec_Init_AFailedSignInLeavesNoHalfConfiguredDevice(t *testing.T) {
	t.Run("sign-in fails", func(t *testing.T) {
		hubA := secloginNewHub(t, secloginOpts{})
		hubB := secloginNewHub(t, secloginOpts{breakDeviceStart: true})
		e := secloginNewEnv(t)

		if out, err := e.run(e.home, "login", hubA.url); err != nil {
			t.Fatalf("login to A: %v\n%s", err, out)
		}
		_, tokenA, _ := e.settings(t)

		work := filepath.Join(t.TempDir(), "proj")
		if err := os.MkdirAll(work, 0o755); err != nil {
			t.Fatal(err)
		}
		defer e.run(work, "stop", work)
		out, err := e.run(work, "init", "--name", "wiki", "--server", hubB.url, "--yes", "--no-hooks", "--no-autostart")
		if err == nil {
			t.Fatalf("init succeeded against a hub whose sign-in is down:\n%s", out)
		}
		if secloginCarries(hubB.sentAuth(), tokenA) {
			t.Fatalf("the failed hub was handed hub A's token: %v", hubB.sentAuth())
		}
		server, token, _ := e.settings(t)
		if server != hubA.url || token != tokenA {
			t.Fatalf("a failed sign-in moved this device's session: server=%q (want %q), token changed=%v",
				server, hubA.url, token != tokenA)
		}
		if _, err := os.Stat(filepath.Join(work, ".bdrive", "config.json")); err == nil {
			t.Fatal("a failed init left a folder config behind")
		}
	})

	t.Run("authenticated then refused", func(t *testing.T) {
		hub := secloginNewHub(t, secloginOpts{projectStatus: http.StatusForbidden})
		e := secloginNewEnv(t)

		work := filepath.Join(t.TempDir(), "proj")
		if err := os.MkdirAll(work, 0o755); err != nil {
			t.Fatal(err)
		}
		defer e.run(work, "stop", work)
		out, err := e.run(work, "init", "--name", "wiki", "--server", hub.url, "--yes", "--no-hooks", "--no-autostart")
		if err == nil {
			t.Fatalf("init succeeded on a hub that refused the project:\n%s", out)
		}
		if _, err := os.Stat(filepath.Join(work, ".bdrive", "config.json")); err == nil {
			t.Fatal("init wrote a folder config for a project the hub refused")
		}
		// The sign-in itself DID happen and is the device's session now; that
		// is correct. What must not happen is a session the hub disowns.
		server, token, _ := e.settings(t)
		if server != hub.url {
			t.Fatalf("settings.Server = %q, want %q", server, hub.url)
		}
		if token != "" && !hub.accepts(token) {
			t.Fatalf("init stored a token this hub does not accept")
		}
		if strings.Contains(out, "syncing") {
			t.Fatalf("init reported syncing after a refusal:\n%s", out)
		}
	})
}
