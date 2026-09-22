package webapp

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
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
	issue   func(w http.ResponseWriter, r *http.Request, userID, device string)

	/* Where pending sign-ins live.

	   `bdrive login` is three requests — mint, approve in a browser, poll —
	   and while this was a map inside one process every one of them had to
	   land on that process. Behind two instances that is a coin flip per hop,
	   which is why raising the instance cap was blocked on it. A hub with no
	   metadata store gets memPending and behaves exactly as it always did:
	   ONE path, not two, because two is how they come to disagree.

	   `seen` is deliberately NOT the store. It is this process's own index of
	   what it has outstanding, used only for the caps below — those bound
	   MEMORY, which is a per-process resource, so a shared counter would be
	   the wrong shape as well as another round trip. */
	pending PendingRepo

	mu   sync.Mutex
	seen map[string]string // grant id -> ip, for this process only
}

// cliGrant is one pending sign-in: a browser-flow code (granted at birth,
// consumed by the exchange) or a device-flow link (granted when its approval
// page is POSTed, consumed by the poll).
// Serialized into a PendingGrant payload, so the fields carry tags: this is
// written by whichever process the CLI reached and read by whichever one the
// human's browser reached.
type cliGrant struct {
	Kind      string    `json:"kind"`      // "code" (browser callback) | "device" (poll flow)
	Challenge string    `json:"challenge"` // browser flow: PKCE S256 code_challenge, "" from a pre-PKCE CLI
	Link      string    `json:"link"`      // device flow: the value in the URL the human opens (never the poll credential)
	User      string    `json:"user"`      // set once granted
	Device    string    `json:"device"`    // device flow: requested device name
	OS        string    `json:"os"`        // device flow: requested device's OS
	IP        string    `json:"ip"`        // device flow: where the request came from, as the server saw it
	Granted   bool      `json:"granted"`
	Expires   time.Time `json:"expires"`
}

// pendingCLI is the PendingGrant kind a sign-in lives under; pendingCLILink
// is the sidecar row that resolves the value in the URL a human opens back to
// its grant id. A second row rather than a scan: the store has no "find by
// field", and the scan this replaces was already flagged as the thing to fix
// if the ceiling ever rose.
const (
	pendingCLI     = "cli"
	pendingCLILink = "cli-link"
)

// UsePending gives this CLIAuth a durable, shared home for pending sign-ins.
// Without it grants live in memory and a login cannot span two processes —
// which is correct for a single-instance hub and fatal behind a load balancer.
func (c *CLIAuth) UsePending(r PendingRepo) {
	if r != nil {
		c.pending = r
	}
}

// NewCLIAuth wires the two provider-specific pieces. session resolves the
// browser session — cookie only, never a Bearer token, or a device token
// could approve the next device. issue writes the CLI's {token, user}
// response for an approved grant.
func NewCLIAuth(session func(*http.Request) (User, bool), issue func(w http.ResponseWriter, r *http.Request, userID, device string)) *CLIAuth {
	return &CLIAuth{session: session, issue: issue, pending: newMemPending(), seen: map[string]string{}}
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

// maxPendingGrants bounds the map. POST /api/auth/device/start needs no
// credential, so every unpolled start is hub memory an anonymous stranger
// allocated; without a ceiling a loop of them is unbounded growth. Far above
// any real hub's concurrent sign-ins.
//
// maxPendingPerIP is the half that matters for availability. A hub-wide cap
// alone turns "one stranger exhausts memory" into "one stranger denies every
// `bdrive login --device` on the hub", which is the same outage bought more
// cheaply. Bounding per origin keeps a flood inside the address that sent it.
//
// A hub-wide cap that REFUSES is that same outage at 2x the price: two
// addresses reach 512 and every honest sign-in is 503 for ten minutes. So the
// hub-wide bound never refuses — it evicts, and it evicts from whichever
// address is holding the most, which is the flooder by definition. A stranger
// can cost himself his own pending grants and nobody else's.
const (
	maxPendingGrants = 512
	maxPendingPerIP  = 256
)

// atGrantCap reports whether another grant from ip would exceed its per-origin
// bound. Counted from THIS process's index, not the store: the cap bounds
// memory, which is per-process, and a shared counter would be both the wrong
// shape and a round trip on the mint path. Callers hold mu.
func (c *CLIAuth) atGrantCap(ip string) bool {
	if ip == "" {
		return false
	}
	n := 0
	for _, held := range c.seen {
		if held == ip {
			if n++; n >= maxPendingPerIP {
				return true
			}
		}
	}
	return false
}

// evictHeaviestLocked makes room for one grant by dropping one held by the
// address holding the most, which is the flooder by definition. Callers hold
// mu.
func (c *CLIAuth) evictHeaviestLocked() {
	byIP := map[string]int{}
	for _, ip := range c.seen {
		byIP[ip]++
	}
	worst, n := "", 0
	for ip, count := range byIP {
		if count > n {
			worst, n = ip, count
		}
	}
	for id, ip := range c.seen {
		if ip == worst {
			delete(c.seen, id)
			c.drop(id)
			return
		}
	}
}

// drop removes a grant and its link sidecar. Best effort on purpose: a store
// that cannot delete must not fail a sign-in, and the row expires anyway.
func (c *CLIAuth) drop(id string) {
	if g, ok, err := c.pending.Get(pendingCLI, id); err == nil && ok && g.Payload != nil {
		var cg cliGrant
		if json.Unmarshal(g.Payload, &cg) == nil && cg.Link != "" {
			_ = c.pending.Delete(pendingCLILink, cg.Link)
		}
	}
	_ = c.pending.Delete(pendingCLI, id)
}

// load reads one grant of a kind, or reports that there is none. Expiry is the
// store's job, so a grant that is past its deadline simply is not there.
func (c *CLIAuth) load(kind, id string) (cliGrant, bool) {
	row, ok, err := c.pending.Get(pendingCLI, id)
	if err != nil || !ok {
		return cliGrant{}, false
	}
	var g cliGrant
	if json.Unmarshal(row.Payload, &g) != nil || g.Kind != kind {
		return cliGrant{}, false
	}
	return g, true
}

// save writes a grant back under an id it already holds.
func (c *CLIAuth) save(id string, g cliGrant) error {
	payload, err := json.Marshal(g)
	if err != nil {
		return err
	}
	return c.pending.Put(PendingGrant{
		Kind: pendingCLI, Key: id, Payload: payload, Expires: g.Expires,
	})
}

// newGrant records a pending sign-in, or returns "" when too many are already
// outstanding.
func (c *CLIAuth) newGrant(g cliGrant, ttl time.Duration) string {
	id := randHex(16)
	g.Expires = time.Now().Add(ttl)

	c.mu.Lock()
	c.sweepLocked()
	if c.atGrantCap(g.IP) {
		c.mu.Unlock()
		return ""
	}
	for len(c.seen) >= maxPendingGrants {
		c.evictHeaviestLocked()
	}
	c.seen[id] = g.IP
	c.mu.Unlock()

	if err := c.save(id, g); err != nil {
		c.mu.Lock()
		delete(c.seen, id)
		c.mu.Unlock()
		return ""
	}
	// The link resolves to the id through its own row rather than a scan: the
	// store has no "find by field", and a scan was already the thing to fix
	// here if the ceiling ever rose.
	if g.Link != "" {
		_ = c.pending.Put(PendingGrant{
			Kind: pendingCLILink, Key: g.Link, Payload: []byte(id), Expires: g.Expires,
		})
	}
	return id
}

// sweepLocked drops this process's index entries for grants that have expired.
// The STORE expires on read and prunes on its own; this only keeps the caps
// from counting grants that are already gone. Callers hold mu.
func (c *CLIAuth) sweepLocked() {
	if len(c.seen) == 0 {
		return
	}
	_ = c.pending.Prune(time.Now())
	for id := range c.seen {
		if _, ok, err := c.pending.Get(pendingCLI, id); err == nil && !ok {
			delete(c.seen, id)
		}
	}
}

// take consumes a grant; peek reads one without consuming (device polling).
func (c *CLIAuth) take(kind, id string) (cliGrant, bool) {
	row, ok, err := c.pending.Take(pendingCLI, id)
	c.forget(id)
	if err != nil || !ok {
		return cliGrant{}, false
	}
	var g cliGrant
	if json.Unmarshal(row.Payload, &g) != nil || g.Kind != kind {
		return cliGrant{}, false
	}
	if g.Link != "" {
		_ = c.pending.Delete(pendingCLILink, g.Link)
	}
	return g, true
}

/*
takeGranted consumes a granted grant, and tells the caller whether IT was

	the one that consumed it. Polling used to peek, decide, and then take in a
	second acquisition of mu with the result discarded, so every poll that got
	past peek reached issue: one human approval minted a token per poll in
	flight, each independently valid and (there being no revocation route)
	independently permanent. A single-use authorisation has to be consumed by
	the same step that decides.

	That is why this reads through Take and not Get: the store's Take is
	atomic, so the race the original fix closed inside one process stays closed
	ACROSS processes — two pollers on two instances cannot both win. A grant
	that turns out not to be approved yet is written straight back, which is the
	one place this is not a pure consume.

	exists distinguishes "no such grant" (401) from "not approved yet" (keep
	polling); only the caller that gets ok == true may issue.
*/
func (c *CLIAuth) takeGranted(kind, id string) (g cliGrant, exists, ok bool) {
	row, found, err := c.pending.Take(pendingCLI, id)
	if err != nil || !found {
		c.forget(id)
		return cliGrant{}, false, false
	}
	if json.Unmarshal(row.Payload, &g) != nil || g.Kind != kind {
		c.forget(id)
		return cliGrant{}, false, false
	}
	if !g.Granted {
		// Not ours to consume yet: put it back exactly as it was.
		_ = c.save(id, g)
		return g, true, false
	}
	c.forget(id)
	if g.Link != "" {
		_ = c.pending.Delete(pendingCLILink, g.Link)
	}
	return g, true, true
}

// forget drops this process's index entry. The store is the truth; this only
// keeps the caps honest.
func (c *CLIAuth) forget(id string) {
	c.mu.Lock()
	delete(c.seen, id)
	c.mu.Unlock()
}

/*
grantByLink resolves the value carried in the URL the human opens. It is

	deliberately not a poll credential: everyone in an approval's path sees that
	string (terminal scrollback, browser history, a forwarded message), and a
	value that both displays and buys a permanent token is one leak away from
	being the whole flow. The poll id still resolves here, because it is the
	requesting client's own secret and older CLIs print it as the link.
*/
func (c *CLIAuth) grantByLink(kind, token string) (id string, ok bool) {
	if g, hit := c.load(kind, token); hit {
		_ = g
		return token, true
	}
	row, found, err := c.pending.Get(pendingCLILink, token)
	if err != nil || !found {
		return "", false
	}
	id = string(row.Payload)
	if _, hit := c.load(kind, id); !hit {
		return "", false
	}
	return id, true
}

func (c *CLIAuth) peek(kind, id string) (cliGrant, bool) {
	return c.load(kind, id)
}

func (c *CLIAuth) approveDevice(id, userID string) bool {
	g, ok := c.load("device", id)
	if !ok {
		return false
	}
	g.User, g.Granted = userID, true
	return c.save(id, g) == nil
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
			// PKCE: whatever the requesting CLI bound this flow to travels
			// with the grant, and only that CLI can redeem the code.
			code := c.newGrant(cliGrant{
				Kind: "code", User: user.ID, Granted: true,
				Challenge: r.URL.Query().Get("code_challenge"),
			}, time.Minute)
			if code == "" {
				authPage(w, "Try again", `<p class="err">Too many sign-ins are pending on this server right now.</p>
<p class="alt">Run <code style="white-space:nowrap">bdrive login</code> again in a few minutes.</p>`)
				return
			}
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
			return [][2]string{{"Device", g.Device}, {"System", g.OS}, {"Address", g.IP}}
		},
		live: func() bool {
			var ok bool
			id, hit := c.grantByLink("device", token)
			if !hit {
				expired("is invalid, already used, or older than 10 minutes")
				return false
			}
			token = id
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
				html.EscapeString(orDash(g.Device)), html.EscapeString(user.Email)))
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
		Code     string `json:"code"`
		Device   string `json:"device"`
		Verifier string `json:"code_verifier"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	g, ok := c.take("code", req.Code)
	if !ok || !g.Granted || !pkceOK(g.Challenge, req.Verifier) {
		http.Error(w, "invalid or expired code", http.StatusUnauthorized)
		return
	}
	c.issue(w, r, g.User, req.Device)
}

// pkceOK checks RFC 7636 S256 proof-of-possession for the loopback flow.
//
// The `state` the CLI generates binds nothing: it is printed to the terminal
// and handed to `open`/`xdg-open` as argv[1], where `ps` shows it to every
// local account — so with it and the port (same string) any local process
// could complete a sign-in of ITS OWN account into somebody else's CLI, and
// that CLI's folders then sync into the attacker's project.
//
// There is no compat arm. A challenge-less grant used to be redeemable by a
// challenge-less exchange so a pre-PKCE CLI kept working — but the hub cannot
// tell a pre-PKCE binary from a caller that simply left the parameter out, so
// the arm was a documented way to ask for no proof of possession and be given
// none: no forged grant needed, just an omitted parameter.
//
// It applies to the loopback flow only — apiExchange is the sole caller and it
// only ever takes a "code" grant. The device and invite flows prove possession
// with a one-time code delivered to the machine itself and are untouched. A
// binary too old to send a challenge must be upgraded, which is one `bdrive
// login` away; the in-repo CLI has always sent one.
func pkceOK(challenge, verifier string) bool {
	if challenge == "" || verifier == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare(
		[]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
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
	link := randHex(16)
	// trimText, at the door, because the approval page is the hub's ONLY consent
	// surface before a device credential is minted and every word on it except
	// the address is chosen by an unauthenticated stranger. html.EscapeString
	// stops markup, not text that renders as something other than itself: an RLO
	// reorders the row, the isolates and zero-widths hide the rest of it, and NEL
	// and the C1s are CSI to the terminal a copy-paste lands in. And the 128-rune
	// cap is half the fix — 32 KiB of "A" pushed the System row, the Address row
	// and the Approve button off the screen, so the reader approves a page whose
	// evidence they never saw.
	code := c.newGrant(cliGrant{
		Kind: "device", Link: link,
		Device: trimText(req.Device, 128), OS: trimText(req.OS, 128),
		IP: requestIP(r),
	}, 10*time.Minute)
	if code == "" {
		// This route is unauthenticated, so the map is the one thing a
		// stranger can grow: refuse rather than retain.
		http.Error(w, "too many pending sign-ins — try again in a few minutes",
			http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{
		// "code" keeps its wire name: it is what the CLI polls with, and
		// older clients still print it.
		"code":       code,
		"verify_url": requestBaseURL(r) + "/auth/device/" + link,
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
	g, exists, ok := c.takeGranted("device", req.Code)
	if !exists {
		http.Error(w, "invalid or expired code", http.StatusUnauthorized)
		return
	}
	if !ok {
		writeJSON(w, map[string]any{"pending": true})
		return
	}
	// g.Device, not req.Device: the approval page is this flow's entire
	// consent surface and it disclosed the name recorded at start time. A
	// device name chosen at poll time — after the human has clicked — is what
	// the token row, the device list and any later revocation would name.
	c.issue(w, r, g.User, g.Device)
}
