package webapp

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// CLIAuth is the CLI-facing half of signing in, whole: the loopback browser
// flow (/auth/cli → one-time code → /api/auth/exchange), the headless device
// flow (/api/auth/device/start → approval link → /api/auth/device/poll), and
// the approval page both of them show.
//
// It is its own type rather than methods on an AuthProvider because this half
// of the protocol is identical no matter where the accounts live: `bdrive
// login` POSTs fixed paths and expects fixed JSON, so a provider differs only
// in who the browser session is and how a device token is minted — the two
// hooks below. The managed hub's provider used to carry its own copy of all
// of this, and the copy drifted: months after the OSS flow moved to a single
// approval link that names the device, the copy was still printing a
// four-byte code to retype into a text box. One implementation, every
// provider, nothing to keep in sync.
type CLIAuth struct {
	session func(*http.Request) (User, bool)
	issue   func(w http.ResponseWriter, userID, device string)

	// Ephemeral single-use state; a server restart just cancels pending
	// logins.
	mu      sync.Mutex
	pending map[string]cliGrant
}

// cliGrant is one pending sign-in: a browser-flow code (granted at birth,
// consumed by the exchange) or a device-flow link (granted when its approval
// page is POSTed, consumed by the poll).
type cliGrant struct {
	kind    string // "code" (browser callback) | "device" (poll flow)
	user    string // set once granted
	device  string // device flow: requested device name
	os      string // device flow: requested device's OS
	ip      string // device flow: where the request came from, as the server saw it
	granted bool
	expires time.Time
}

// NewCLIAuth wires the two provider-specific pieces. session resolves the
// browser session — cookie only, never a Bearer token, or a device token
// could approve the next device. issue writes the CLI's {token, user}
// response for an approved grant.
func NewCLIAuth(session func(*http.Request) (User, bool), issue func(w http.ResponseWriter, userID, device string)) *CLIAuth {
	return &CLIAuth{session: session, issue: issue, pending: make(map[string]cliGrant)}
}

// Register mounts the paths `bdrive login` knows. They are fixed: an older
// CLI on a newer hub must still find them.
func (c *CLIAuth) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/cli", c.pageCLI)
	mux.HandleFunc("POST /auth/cli", c.pageCLI)
	mux.HandleFunc("GET /auth/device/{token}", c.pageDevice)
	mux.HandleFunc("POST /auth/device/{token}", c.pageDevice)
	mux.HandleFunc("GET /auth/device", c.pageDeviceLegacy)
	mux.HandleFunc("POST /api/auth/exchange", c.apiExchange)
	mux.HandleFunc("POST /api/auth/device/start", c.apiDeviceStart)
	mux.HandleFunc("POST /api/auth/device/poll", c.apiDevicePoll)
}

// ---- grants ----

func (c *CLIAuth) newGrant(g cliGrant, ttl time.Duration) string {
	id := randHex(16)
	g.expires = time.Now().Add(ttl)
	c.mu.Lock()
	c.pending[id] = g
	c.mu.Unlock()
	return id
}

// take consumes a grant; peek reads one without consuming (device polling).
func (c *CLIAuth) take(kind, id string) (cliGrant, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, ok := c.pending[id]
	if !ok || g.kind != kind || time.Now().After(g.expires) {
		delete(c.pending, id)
		return cliGrant{}, false
	}
	delete(c.pending, id)
	return g, true
}

func (c *CLIAuth) peek(kind, id string) (cliGrant, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, ok := c.pending[id]
	if !ok || g.kind != kind || time.Now().After(g.expires) {
		return cliGrant{}, false
	}
	return g, true
}

func (c *CLIAuth) approveDevice(id, userID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, ok := c.pending[id]
	if !ok || g.kind != "device" || time.Now().After(g.expires) {
		return false
	}
	g.user, g.granted = userID, true
	c.pending[id] = g
	return true
}

// ---- the approval page ----

// authRequest describes a pending sign-in to pageAuth. Both flows ask the
// user the same question — "shall this thing act as you?" — and differ only
// in how the request is identified, what is asking, and what approving does.
// Keeping that difference in data rather than in two copies of the page is
// what stops the two from drifting apart, which matters here: a flow whose
// disclosure quietly falls behind the other's is the failure mode this page
// exists to prevent.
type authRequest struct {
	title string // heading
	lede  string // one line naming what is asking (plain text)
	note  string // when approving is the right call — trusted markup

	// detail is what is asking, in detail. A function because the device flow
	// reads it off the pending grant, which only exists once live() has found
	// it — so it must be evaluated at render time, not at call time.
	detail func() [][2]string

	// live, when set, runs once the session is known and before anything is
	// shown or granted, reporting whether the request still exists — having
	// already written its own explanation when it doesn't. The device flow's
	// link expires; the CLI flow carries its whole request in the URL and has
	// nothing to expire.
	live func() bool

	// approve performs the grant and writes the response.
	approve func(user User)
}

// pageAuth is the approval page both sign-in flows share.
func (c *CLIAuth) pageAuth(w http.ResponseWriter, r *http.Request, req authRequest) {
	user, ok := c.session(r)
	if !ok {
		http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	if req.live != nil && !req.live() {
		return
	}
	if r.Method == http.MethodPost {
		req.approve(user)
		return
	}
	authPage(w, req.title, fmt.Sprintf(`<p class="lede">%s</p>
%s%s
<form method="post"><button type="submit">Approve</button></form>
<p class="alt">%s</p>`,
		html.EscapeString(req.lede), whoBlock(user, r.URL.RequestURI()), rows(req.detail()...), req.note))
}

// pageCLI completes `bdrive login`: confirm who the terminal will act as, then
// mint a one-time code and bounce it to the CLI's loopback listener. Redirects
// are restricted to loopback addresses so the code can't be sent anywhere else.
//
// The confirmation is the point, not ceremony. Whoever the browser happens to
// be signed in as is who the terminal becomes, and that is frequently not the
// account the user meant — a personal login left open, a teammate's session on
// a shared machine. Granting silently means the mistake surfaces later, as a
// synced folder full of commits authored by the wrong person, which is far
// more work to undo than one click now.
//
// It also means a GET no longer grants anything, so a link someone else got
// you to open can't mint a code on your behalf.
func (c *CLIAuth) pageCLI(w http.ResponseWriter, r *http.Request) {
	u, err := url.Parse(r.URL.Query().Get("redirect"))
	if err != nil || (u.Scheme != "http") || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") {
		http.Error(w, "invalid redirect (must be a loopback URL)", http.StatusBadRequest)
		return
	}
	c.pageAuth(w, r, authRequest{
		title: "Sign in on this computer",
		lede:  "A terminal on this computer is asking to sign in to BearDrive.",
		detail: func() [][2]string {
			return [][2]string{{"Application", "bdrive command line"}, {"Waiting at", u.Host}}
		},
		note: `Approve this only if you just ran ` +
			`<code style="white-space:nowrap">bdrive login</code> yourself.`,
		approve: func(user User) {
			code := c.newGrant(cliGrant{kind: "code", user: user.ID, granted: true}, time.Minute)
			q := u.Query()
			q.Set("code", code)
			q.Set("state", r.URL.Query().Get("state"))
			u.RawQuery = q.Encode()
			http.Redirect(w, r, u.String(), http.StatusSeeOther)
		},
	})
}

// pageDevice is the headless-login approval page, reached by opening the link
// `bdrive login` printed: the token lives in the path, so there is no code to
// read off one screen and type into another.
//
// Unlike the local flow this one never skips the page. The machine being
// granted is not the one reading this, so the account, the device name, its OS
// and its address are the only things standing between an approval and a
// stranger's pending link.
func (c *CLIAuth) pageDevice(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	var g cliGrant
	expired := func(when string) {
		authPage(w, "Link expired", `<p class="err">This sign-in link `+when+`.</p>
<p class="alt">Run <code style="white-space:nowrap">bdrive login --device</code> again for a fresh one.</p>`)
	}
	c.pageAuth(w, r, authRequest{
		title: "Connect a device",
		lede:  "A device is asking to sign in to BearDrive.",
		note:  "Approve this only if you just started a sign-in on that machine.",
		detail: func() [][2]string {
			return [][2]string{{"Device", g.device}, {"System", g.os}, {"Address", g.ip}}
		},
		live: func() bool {
			var ok bool
			if g, ok = c.peek("device", token); !ok {
				expired("is invalid, already used, or older than 10 minutes")
				return false
			}
			return true
		},
		approve: func(user User) {
			if !c.approveDevice(token, user.ID) {
				expired("expired while the page was open")
				return
			}
			authPage(w, "Device connected", fmt.Sprintf(`<p class="msg">%s can now sync as %s.</p>
<p class="alt">You can close this tab — the terminal finishes on its own.</p>`,
				html.EscapeString(orDash(g.device)), html.EscapeString(user.Email)))
		},
	})
}

// pageDeviceLegacy forwards the pre-0.13 link shape (/auth/device?code=…),
// which older CLIs still print, to the path form.
func (c *CLIAuth) pageDeviceLegacy(w http.ResponseWriter, r *http.Request) {
	code := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("code")))
	if code == "" {
		authPage(w, "Connect a device", `<p>Run <code>bdrive login --device</code> on the machine you want to connect; it prints a link to open here.</p>`)
		return
	}
	http.Redirect(w, r, "/auth/device/"+url.PathEscape(code), http.StatusSeeOther)
}

// whoBlock renders who the approver would be granting as, with an escape hatch
// back to this same page. Approving on either flow hands a machine a token
// that acts as you, so "as whom" is the question worth answering loudest — and
// the browser's session is often not the account the user meant to use.
//
// What is asking differs per flow (a device has a name and an OS; a CLI on
// this computer has a loopback port), so each page renders its own rows rather
// than this pretending to a shape neither quite fits.
func whoBlock(user User, back string) string {
	name := user.Name
	if name == "" {
		name = user.Email
	}
	return fmt.Sprintf(`<div class="who">
<div class="who-id"><span class="who-l">Signing in as</span><b>%s</b><span class="who-sub">%s</span></div>
<a class="who-swap" href="/auth/logout?next=%s">Switch account</a>
</div>`,
		html.EscapeString(name), html.EscapeString(user.Email), url.QueryEscape(safeNext(back)))
}

// rows renders a label/value list, dashing out the blanks.
func rows(pairs ...[2]string) string {
	var b strings.Builder
	b.WriteString(`<dl class="rows">`)
	for _, p := range pairs {
		fmt.Fprintf(&b, "<dt>%s</dt><dd>%s</dd>",
			html.EscapeString(p[0]), html.EscapeString(orDash(p[1])))
	}
	b.WriteString(`</dl>`)
	return b.String()
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// ---- CLI API ----

// apiExchange trades the one-time code from the browser redirect for a
// long-lived device token.
func (c *CLIAuth) apiExchange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code   string `json:"code"`
		Device string `json:"device"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	g, ok := c.take("code", req.Code)
	if !ok || !g.granted {
		http.Error(w, "invalid or expired code", http.StatusUnauthorized)
		return
	}
	c.issue(w, g.user, req.Device)
}

// apiDeviceStart begins the headless flow: the CLI prints the approval link,
// the user opens it in any signed-in browser, the CLI polls. The link itself
// is the secret (RFC 8628 calls this verification_uri_complete), so it is a
// full-length token, not something short enough to retype — nobody has to
// read a code off one screen and type it into another.
//
// The requesting device's name, OS, and address are recorded here so the
// approval page can show WHAT is being approved: this flow's weakness is that
// a stranger can send you their own pending link, and a page that just says
// "Approve" gives you nothing to notice with.
func (c *CLIAuth) apiDeviceStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Device string `json:"device"`
		OS     string `json:"os"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	code := c.newGrant(cliGrant{
		kind: "device", device: req.Device, os: req.OS, ip: requestIP(r),
	}, 10*time.Minute)
	writeJSON(w, map[string]any{
		// "code" keeps its wire name: it is what the CLI polls with, and
		// older clients still print it.
		"code":       code,
		"verify_url": requestBaseURL(r) + "/auth/device/" + code,
		"interval":   2,
	})
}

func (c *CLIAuth) apiDevicePoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code   string `json:"code"`
		Device string `json:"device"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	g, ok := c.peek("device", req.Code)
	if !ok {
		http.Error(w, "invalid or expired code", http.StatusUnauthorized)
		return
	}
	if !g.granted {
		writeJSON(w, map[string]any{"pending": true})
		return
	}
	c.take("device", req.Code)
	device := req.Device
	if device == "" {
		device = g.device
	}
	c.issue(w, g.user, device)
}
