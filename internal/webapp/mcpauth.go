package webapp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// OAuth for the MCP door.
//
// The hub already had a PKCE authorization-code flow — /auth/cli → one-time
// code → /api/auth/exchange — but it is BearDrive-shaped: the client is
// `bdrive`, the redirect is a loopback port, and the token it mints is an
// unscoped account credential. A remote MCP client is none of those. It has
// never heard of this hub, holds no client id, and will not proceed without
// the discovery documents. So this file adds the standard surface —
// RFC 9728 protected-resource metadata, RFC 8414 AS metadata, RFC 7591
// dynamic client registration, authorize/token with PKCE S256 and RFC 8707
// resource indicators.
//
// The one piece that is ours rather than the spec's is the consent screen:
// it lists the caller's projects as checkboxes, and the selection IS the
// grant. That is the whole reason this is not just "sign in with BearDrive".
//
// Shaped like CLIAuth deliberately: the protocol half is identical wherever
// accounts live, so it takes the provider-specific bits as hooks rather than
// reaching for a concrete type. A managed hub running its own authorization
// server declines to register these routes and still inherits the grant
// ceiling, because that lives in projectPermOf, not here.

// MCPGrant is one client's standing permission to act as one account over a
// chosen set of projects.
//
// Projects is the answer to "which projects did the user tick at consent",
// and it is a CEILING, never a source of permission: capByGrant can only
// lower what projectPermFor already resolved. A grant naming a project the
// account later loses access to grants nothing the moment membership changes,
// with no revocation step and nothing to keep in sync.
type MCPGrant struct {
	ID         string    `json:"id"`
	Account    string    `json:"account"` // normalized email
	ClientID   string    `json:"client_id"`
	ClientName string    `json:"client_name,omitempty"`
	Projects   []string  `json:"projects"`
	Created    time.Time `json:"created"`
	LastUsed   time.Time `json:"last_used,omitempty"`
	Expires    time.Time `json:"expires,omitempty"`
	// Digests, never plaintext — the same rule device tokens follow. A
	// metadata store that leaks gives up who connected what, not the ability
	// to act as them.
	TokenDigest   string `json:"token_digest,omitempty"`
	RefreshDigest string `json:"refresh_digest,omitempty"`
}

// MCPClient is an OAuth client that registered itself (RFC 7591). There is no
// client secret: MCP clients are public clients and PKCE is what binds an
// authorization code to the requester.
type MCPClient struct {
	ID           string    `json:"id"`
	Name         string    `json:"name,omitempty"`
	RedirectURIs []string  `json:"redirect_uris,omitempty"`
	Created      time.Time `json:"created"`
}

// MCPProject is one row of the consent screen: a project the account can
// connect, and the level it already holds there.
type MCPProject struct {
	Project
	Level string
}

// Token lifetimes. Access tokens are short because a leaked one is a live
// credential with no user in the loop; refresh is long because the whole
// point of this door is an agent that keeps working without re-consent.
const (
	mcpAccessTTL  = time.Hour
	mcpRefreshTTL = 30 * 24 * time.Hour
	mcpCodeTTL    = 10 * time.Minute
)

// MCPAuth owns grants, registered clients, and the OAuth endpoints.
type MCPAuth struct {
	repo MCPRepo
	// session resolves the browser session for the consent page — cookie
	// only, never a Bearer token, or an MCP access token could approve the
	// next grant and quietly widen its own project set.
	session func(*http.Request) (User, bool)
	// projects lists what this account may connect, each with the level the
	// account already holds, so consent can only ever offer projects the
	// account has — and can show the ceiling next to each row.
	projects func(email string) []MCPProject
	// OrgName turns an org id into its display name for the consent screen. A
	// func rather than a Directory so this file stays unaware of where orgs
	// come from.
	OrgName func(string) string
	// caller resolves ANY authenticated credential — a browser cookie or a
	// device token — for the list/revoke API, so `bdrive mcp` can manage
	// connections without a browser. Deliberately separate from session: the
	// consent screen must stay cookie-only.
	caller func(*http.Request) (User, bool)

	mu sync.Mutex
	// ver is the change-token gate every other registry already uses
	// (ProjectDB, OrgDB, ShareDB, DeviceRegistry). Without it this one loaded
	// at construction and never read the store again, so a grant revoked by
	// anything else — `bdrive mcp revoke` in another process, an admin action
	// on another instance — stayed honoured for the life of the process.
	ver    versionGate
	warned bool

	grants  map[string]MCPGrant // by id
	byToken map[string]string   // access-token digest → grant id
	byFresh map[string]string   // refresh-token digest → grant id
	clients map[string]MCPClient
	/* pending holds authorization codes, which are ephemeral and single-use
	   but NOT process-local: the browser POSTs consent to one instance and
	   the MCP client exchanges the code from another, so a code in a map is
	   a coin flip per sign-in behind more than one instance. It also meant a
	   hub restart cancelled an in-flight consent. */
	pending PendingRepo
}

// mcpCode is one pending consent. Serialized into a PendingGrant payload, so
// the fields carry tags — it is written by one process and read by another.
type mcpCode struct {
	GrantID   string    `json:"grant_id"`
	ClientID  string    `json:"client_id"`
	Redirect  string    `json:"redirect"`
	Challenge string    `json:"challenge"` // PKCE S256
	Expires   time.Time `json:"expires"`
}

// pendingMCPCode is the PendingGrant kind these live under.
const pendingMCPCode = "mcp-code"

// NewMCPAuth loads the grants and clients already on disk.
func NewMCPAuth(repo MCPRepo, pending PendingRepo, session func(*http.Request) (User, bool), projects func(string) []MCPProject) (*MCPAuth, error) {
	m := &MCPAuth{
		repo: repo, session: session, projects: projects,
		caller: session, // until UseCaller widens it; cookie-only is the safe default
		grants: map[string]MCPGrant{}, byToken: map[string]string{},
		byFresh: map[string]string{}, clients: map[string]MCPClient{},
		pending: pending,
	}
	gs, err := repo.LoadGrants()
	if err != nil {
		return nil, err
	}
	for _, g := range gs {
		m.index(g)
	}
	cs, err := repo.LoadClients()
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		m.clients[c.ID] = c
	}
	return m, nil
}

/*
refresh re-reads the grants and clients when the store says they moved.

	Callers hold mu.

	Modelled on ProjectDB.refresh: a change token (one primary-key lookup on
	SQL, and "always stale" for a repo that cannot answer), then a full re-read
	only when it moves. A failed re-read keeps what we have and says so once —
	serving the last known grants is strictly better than refusing everybody
	because the metadata store hiccuped.

	`codes` is deliberately NOT rebuilt: pending authorization codes are
	ephemeral, single-use and never persisted, so a re-read must not drop a
	consent that is mid-flight.
*/
func (m *MCPAuth) refresh() {
	token, stale := m.ver.stale(m.repo)
	if !stale {
		return
	}
	gs, err := m.repo.LoadGrants()
	if err != nil {
		if !m.warned {
			m.warned = true
			log.Printf("beardrive: MCP grant re-read failed, serving the last known grants: %v", err)
		}
		return
	}
	cs, err := m.repo.LoadClients()
	if err != nil {
		if !m.warned {
			m.warned = true
			log.Printf("beardrive: MCP client re-read failed, serving the last known clients: %v", err)
		}
		return
	}
	m.warned = false
	m.ver.fresh(token)

	// Rebuilt rather than merged: a revocation is an ABSENCE, and merging can
	// only ever add. Leaving the old maps in place is the bug this fixes.
	m.grants = make(map[string]MCPGrant, len(gs))
	m.byToken = make(map[string]string, len(gs))
	m.byFresh = make(map[string]string, len(gs))
	for _, g := range gs {
		m.index(g)
	}
	m.clients = make(map[string]MCPClient, len(cs))
	for _, c := range cs {
		m.clients[c.ID] = c
	}
}

// index puts a grant in all three maps. Callers hold mu (or are constructing).
func (m *MCPAuth) index(g MCPGrant) {
	m.grants[g.ID] = g
	if g.TokenDigest != "" {
		m.byToken[g.TokenDigest] = g.ID
	}
	if g.RefreshDigest != "" {
		m.byFresh[g.RefreshDigest] = g.ID
	}
}

// deindex removes every trace of a grant's current credentials. Rotating a
// token without this leaves the OLD digest pointing at the grant, which is
// the difference between "refresh issued a new token" and "refresh issued a
// second, equally valid token that nothing can revoke".
func (m *MCPAuth) deindex(id string) {
	g, ok := m.grants[id]
	if !ok {
		return
	}
	delete(m.byToken, g.TokenDigest)
	delete(m.byFresh, g.RefreshDigest)
}

// ---- lookup ----

// Grant resolves a bearer token to a live grant. Expiry is checked here so
// that every caller gets it right by construction.
func (m *MCPAuth) Grant(token string) (MCPGrant, bool) {
	if token == "" {
		return MCPGrant{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refresh()
	id, ok := m.byToken[hashToken(token)]
	if !ok {
		return MCPGrant{}, false
	}
	g, ok := m.grants[id]
	if !ok {
		return MCPGrant{}, false
	}
	if !g.Expires.IsZero() && time.Now().After(g.Expires) {
		return MCPGrant{}, false
	}
	return g, true
}

// touch records that a grant was used, at most once a minute. Every MCP
// request would otherwise be a metadata write.
func (m *MCPAuth) touch(id string) {
	m.mu.Lock()
	g, ok := m.grants[id]
	if !ok || time.Since(g.LastUsed) < time.Minute {
		m.mu.Unlock()
		return
	}
	g.LastUsed = time.Now().UTC()
	m.grants[id] = g
	m.mu.Unlock()
	_ = m.repo.PutGrant(g) // telemetry: never fail a request over it
}

// List reports an account's grants, newest first, for the connections page.
func (m *MCPAuth) List(email string) []MCPGrant {
	email = normEmail(email)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refresh() // a connection revoked elsewhere must not still be listed
	var out []MCPGrant
	for _, g := range m.grants {
		if g.Account == email {
			g.TokenDigest, g.RefreshDigest = "", "" // never leaves the server
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// Revoke kills a grant and both its tokens at once. A revocation that left
// the refresh token alive would be a revocation in name only.
func (m *MCPAuth) Revoke(email, id string) bool {
	m.mu.Lock()
	m.refresh() // revoking something this process has not heard of must still work
	g, ok := m.grants[id]
	// An empty email is the server revoking on its own behalf (an expired
	// refresh), not a wildcard match on accounts with no email.
	if !ok || (email != "" && g.Account != normEmail(email)) {
		m.mu.Unlock()
		return false
	}
	m.deindex(id)
	delete(m.grants, id)
	m.mu.Unlock()
	_ = m.repo.DeleteGrant(id)
	return true
}

// ---- request context ----

type mcpGrantKey struct{}

func withGrant(r *http.Request, g MCPGrant) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), mcpGrantKey{}, g))
}

func grantFrom(ctx context.Context) (MCPGrant, bool) {
	g, ok := ctx.Value(mcpGrantKey{}).(MCPGrant)
	return g, ok
}

// covers reports whether this grant may touch a project at all.
func (g MCPGrant) covers(projectID string) bool {
	for _, p := range g.Projects {
		if p == projectID {
			return true
		}
	}
	return false
}

// capByGrant lowers a resolved permission level to PermNone when the request
// carries an MCP grant that does not name this project.
//
// This is THE enforcement point for the whole feature, and it lives in
// projectPermOf rather than in the MCP handlers on purpose: proj() routes
// every per-project request through that resolver, so a scoped token replayed
// against /api/p/<other>/download or /store/object is denied by the same line
// that denies it over MCP. Checking inside the tools instead would secure the
// door and leave the windows open.
//
// It only ever lowers. A request with no grant (browser session, device
// token) is returned unchanged, so this cannot become a second permission
// model — only a ceiling on the existing one.
func capByGrant(r *http.Request, projectID, have string) string {
	g, ok := grantFrom(r.Context())
	if !ok || g.covers(projectID) {
		return have
	}
	return PermNone
}

// ---- routes ----

func (m *MCPAuth) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", m.metaResource)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", m.metaResource)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", m.metaAS)
	// Clients disagree about whether the resource path is appended to the
	// well-known segment. The issuer here has no path, so the bare form is the
	// correct one — serving the suffixed form too costs a line and saves a
	// client that probes only the other spelling from concluding the hub has
	// no authorization server at all.
	mux.HandleFunc("GET /.well-known/oauth-authorization-server/mcp", m.metaAS)
	mux.HandleFunc("POST /oauth/register", m.register)
	mux.HandleFunc("GET /oauth/authorize", m.authorize)
	mux.HandleFunc("POST /oauth/authorize", m.authorize)
	mux.HandleFunc("POST /oauth/token", m.token)
	mux.HandleFunc("GET /api/mcp/grants", m.apiList)
	mux.HandleFunc("DELETE /api/mcp/grants/{id}", m.apiRevoke)
}

// issuer is this hub's own origin. Derived per request rather than
// configured: a hub reached over a tunnel, a LAN address and its public name
// must each hand back discovery documents that point at the name the client
// actually used, or the client fetches metadata from one origin and is
// redirected to another it never trusted.
func issuer(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil && !strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}

func (m *MCPAuth) metaResource(w http.ResponseWriter, r *http.Request) {
	iss := issuer(r)
	writeJSON(w, map[string]any{
		"resource":                 iss + "/mcp",
		"authorization_servers":    []string{iss},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         []string{"bdrive.projects"},
	})
}

func (m *MCPAuth) metaAS(w http.ResponseWriter, r *http.Request) {
	iss := issuer(r)
	writeJSON(w, map[string]any{
		"issuer":                                iss,
		"authorization_endpoint":                iss + "/oauth/authorize",
		"token_endpoint":                        iss + "/oauth/token",
		"registration_endpoint":                 iss + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{"bdrive.projects"},
	})
}

// register is RFC 7591 dynamic client registration. Open by design: an MCP
// client arrives with no credentials and no way to obtain one out of band, so
// refusing unknown clients here means refusing every client. Registration
// grants nothing on its own — a client id is a name, and every byte of access
// still comes from a human ticking boxes on the consent screen.
func (m *MCPAuth) register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		oauthErr(w, http.StatusBadRequest, "invalid_client_metadata", err.Error())
		return
	}
	if len(req.RedirectURIs) == 0 {
		oauthErr(w, http.StatusBadRequest, "invalid_client_metadata", "redirect_uris is required")
		return
	}
	for _, u := range req.RedirectURIs {
		if !validRedirect(u) {
			oauthErr(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri must be an absolute http(s) URI without a fragment")
			return
		}
	}
	c := MCPClient{
		ID: "mcpc_" + randHex(16), Name: strings.TrimSpace(req.ClientName),
		RedirectURIs: req.RedirectURIs, Created: time.Now().UTC(),
	}
	m.mu.Lock()
	m.clients[c.ID] = c
	m.mu.Unlock()
	if err := m.repo.PutClient(c); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// writeJSONStatus, not WriteHeader + writeJSON: writeJSON writes 200
	// itself, so the pair logged "superfluous response.WriteHeader" on every
	// registration and the 201 was the one that stuck only by accident.
	writeJSONStatus(w, http.StatusCreated, map[string]any{
		"client_id":                  c.ID,
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
}

// validRedirect keeps an open redirector out of the authorize endpoint. A
// fragment is refused because the fragment is where an implicit-flow token
// would land, and loopback is allowed on http because that is how a desktop
// MCP client receives its callback.
func validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Fragment != "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		return h == "127.0.0.1" || h == "localhost" || h == "::1"
	case "javascript", "data", "vbscript", "file", "blob":
		return false
	default:
		// A custom scheme (cursor://, vscode://) is how a native client comes
		// back, and it cannot be validated much beyond being well-formed —
		// exact-matching it against what the client registered is what
		// actually protects the authorize endpoint.
		//
		// But it must be HIERARCHICAL. An opaque URI is scheme:payload with no
		// //authority, which is the shape of javascript:alert(1) and
		// data:text/html,… — registering one and then being redirected to it
		// is script execution on the hub's own origin. Requiring //host rules
		// the whole class out rather than blocklisting the three spellings of
		// it anyone has thought of so far.
		return u.Opaque == "" && u.Host != ""
	}
}

func (m *MCPAuth) client(id string) (MCPClient, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refresh() // a client registered elsewhere is a real client
	c, ok := m.clients[id]
	return c, ok
}

// ---- authorize: the consent screen ----

func (m *MCPAuth) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID := q.Get("client_id")
	redirect := q.Get("redirect_uri")
	state := q.Get("state")
	challenge := q.Get("code_challenge")
	resource := q.Get("resource")

	c, ok := m.client(clientID)
	if !ok {
		oauthErr(w, http.StatusBadRequest, "invalid_client", "unknown client_id — register first")
		return
	}
	// Exact match against what the client registered. Anything looser is an
	// open redirector with an authorization code attached to it.
	if !slicesContains(c.RedirectURIs, redirect) {
		oauthErr(w, http.StatusBadRequest, "invalid_request", "redirect_uri does not match a registered URI")
		return
	}
	if q.Get("response_type") != "code" {
		redirectErr(w, r, redirect, state, "unsupported_response_type", "only response_type=code is supported")
		return
	}
	// PKCE is mandatory. These are public clients with no secret: without a
	// challenge, an authorization code intercepted on the way back is enough
	// to mint a token.
	if challenge == "" || q.Get("code_challenge_method") != "S256" {
		redirectErr(w, r, redirect, state, "invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}

	// Who is asking? Cookie session only — see MCPAuth.session.
	u, ok := m.session(r)
	if !ok || u.Email == "" {
		// Bounce through sign-in and come back to this exact consent URL.
		http.Redirect(w, r, "/auth/login?redirect="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}

	available := m.projects(u.Email)
	if r.Method == http.MethodGet {
		m.consentPage(w, r, c, u, available, nil)
		return
	}

	// POST: the user made a selection.
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	picked := r.Form["project"]
	// Only projects this account actually has. A hand-edited form must not be
	// able to name a project the picker never offered — capByGrant would deny
	// it at request time anyway, but a grant that lists projects the user
	// cannot see is a lie on the connections page.
	allowed := map[string]bool{}
	for _, p := range available {
		allowed[p.ID] = true
	}
	var projects []string
	for _, id := range picked {
		if allowed[id] {
			projects = append(projects, id)
		}
	}
	if len(projects) == 0 {
		m.consentPage(w, r, c, u, available, errors.New("Select at least one project to connect."))
		return
	}

	g := MCPGrant{
		ID: "mcpg_" + randHex(16), Account: normEmail(u.Email),
		ClientID: c.ID, ClientName: c.Name, Projects: projects,
		Created: time.Now().UTC(),
	}
	m.mu.Lock()
	m.grants[g.ID] = g
	m.mu.Unlock()
	code := "mcpa_" + randHex(24)
	expires := time.Now().Add(mcpCodeTTL)
	payload, err := json.Marshal(mcpCode{
		GrantID: g.ID, ClientID: c.ID, Redirect: redirect,
		Challenge: challenge, Expires: expires,
	})
	if err == nil {
		err = m.pending.Put(PendingGrant{
			Kind: pendingMCPCode, Key: code, Payload: payload, Expires: expires,
		})
	}
	if err != nil {
		// Nothing to redirect to: without a stored code the exchange cannot
		// succeed, and sending the browser back with one that will be refused
		// is a worse failure than saying so here.
		log.Printf("beardrive: could not record an MCP consent: %v", err)
		http.Error(w, "could not record this authorization; try again", http.StatusInternalServerError)
		return
	}
	m.pruneOrphanGrants()
	_ = resource // bound at token time; recorded here only to echo it back

	u2, _ := url.Parse(redirect)
	qq := u2.Query()
	qq.Set("code", code)
	if state != "" {
		qq.Set("state", state)
	}
	u2.RawQuery = qq.Encode()
	http.Redirect(w, r, u2.String(), http.StatusFound)
}

// pruneCodes drops expired pending codes and the grants they would have
// issued. Called on every mint, so neither map grows without bound.
//
// The grants matter as much as the codes: authorize creates the grant in
// memory and only issue() persists it, so a consent the user completes but the
// client never exchanges left a grant behind that nothing could reach, nothing
// wrote to disk, and nothing cleaned up until a restart. Callers hold mu.
/* pruneOrphanGrants drops grants that a consent created and no exchange ever
   claimed, and prunes the expired codes behind them.

   It used to work the other way round — walk the in-memory codes, and for each
   expired one drop its grant. That cannot survive codes moving into a store
   this process does not hold alone, so the rule is expressed as a property of
   the GRANT instead: a grant with no token digest was never exchanged, and one
   older than a code could possibly still be valid for never will be.

   Safer than the old form as well as simpler. A grant with an empty digest is
   not in byToken and cannot authenticate anything, so the worst an uncollected
   one ever did was occupy a row; and keying on the grant's own age means a
   code that vanished for any reason — an instance that died mid-consent, a
   store that was briefly unreachable — no longer strands its grant forever. */
func (m *MCPAuth) pruneOrphanGrants() {
	if err := m.pending.Prune(time.Now()); err != nil {
		log.Printf("beardrive: could not prune expired MCP consents: %v", err)
	}
	cutoff := time.Now().Add(-mcpCodeTTL)
	m.mu.Lock()
	var dead []string
	for id, g := range m.grants {
		if g.TokenDigest == "" && g.RefreshDigest == "" && g.Created.Before(cutoff) {
			dead = append(dead, id)
		}
	}
	for _, id := range dead {
		delete(m.grants, id)
	}
	m.mu.Unlock()
	for _, id := range dead {
		if err := m.repo.DeleteGrant(id); err != nil {
			log.Printf("beardrive: could not delete an unclaimed MCP grant: %v", err)
		}
	}
}

func (m *MCPAuth) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthErr(w, http.StatusBadRequest, "invalid_request", "bad form")
		return
	}
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		m.tokenFromCode(w, r)
	case "refresh_token":
		m.tokenFromRefresh(w, r)
	default:
		oauthErr(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
	}
}

func (m *MCPAuth) tokenFromCode(w http.ResponseWriter, r *http.Request) {
	code := r.Form.Get("code")
	verifier := r.Form.Get("code_verifier")

	// Single use, consumed even on failure below — and consumed ATOMICALLY,
	// so two clients racing one code cannot both win it.
	row, ok, err := m.pending.Take(pendingMCPCode, code)
	var c mcpCode
	if ok && err == nil {
		ok = json.Unmarshal(row.Payload, &c) == nil
	}
	if err != nil {
		log.Printf("beardrive: could not read an MCP consent: %v", err)
	}

	if !ok || time.Now().After(c.Expires) {
		oauthErr(w, http.StatusBadRequest, "invalid_grant", "unknown or expired code")
		return
	}
	if cid := r.Form.Get("client_id"); cid != "" && cid != c.ClientID {
		oauthErr(w, http.StatusBadRequest, "invalid_grant", "code was issued to a different client")
		return
	}
	if ru := r.Form.Get("redirect_uri"); ru != "" && ru != c.Redirect {
		oauthErr(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		return
	}
	if !verifierMatches(verifier, c.Challenge) {
		oauthErr(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match the challenge")
		return
	}
	m.issue(w, c.GrantID)
}

func (m *MCPAuth) tokenFromRefresh(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	id, ok := m.byFresh[hashToken(r.Form.Get("refresh_token"))]
	var idle time.Duration
	if ok {
		if g, live := m.grants[id]; live {
			// LastUsed is touched on every MCP request, so an actively-used
			// connection never ages out and an abandoned one dies. Without
			// this the refresh token was immortal: mcpRefreshTTL was declared
			// and never read, which is the quiet way a stolen credential
			// becomes permanent.
			since := g.LastUsed
			if since.IsZero() {
				since = g.Created
			}
			idle = time.Since(since)
		}
	}
	m.mu.Unlock()
	if !ok {
		oauthErr(w, http.StatusBadRequest, "invalid_grant", "unknown refresh token")
		return
	}
	if idle > mcpRefreshTTL {
		m.Revoke("", id) // best effort; the refusal below is the gate
		oauthErr(w, http.StatusBadRequest, "invalid_grant", "this connection expired after 30 days unused; reconnect")
		return
	}
	m.issue(w, id)
}

// issue mints a fresh access/refresh pair for a grant and forgets the old
// one. Rotating the refresh token on every use is what makes a stolen
// refresh token detectable-and-finite rather than permanent.
func (m *MCPAuth) issue(w http.ResponseWriter, grantID string) {
	access := "mcpt_" + randHex(32)
	refresh := "mcpr_" + randHex(32)

	m.mu.Lock()
	g, ok := m.grants[grantID]
	if !ok {
		m.mu.Unlock()
		oauthErr(w, http.StatusBadRequest, "invalid_grant", "grant has been revoked")
		return
	}
	m.deindex(grantID)
	g.TokenDigest = hashToken(access)
	g.RefreshDigest = hashToken(refresh)
	g.Expires = time.Now().UTC().Add(mcpAccessTTL)
	m.index(g)
	m.mu.Unlock()

	if err := m.repo.PutGrant(g); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(mcpAccessTTL.Seconds()),
		"refresh_token": refresh,
		"scope":         "bdrive.projects",
	})
}

func verifierMatches(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:]) == challenge
}

// ---- connections API (the frontend's manage-and-revoke surface) ----

// UseCaller widens list/revoke to any authenticated credential, so the CLI can
// manage connections. The consent screen keeps using session (cookie only).
func (m *MCPAuth) UseCaller(f func(*http.Request) (User, bool)) { m.caller = f }

// manager resolves who is managing connections, and refuses an AGENT outright.
// An MCP access token does not authenticate here anyway — no AuthProvider has
// heard of one — but relying on that is relying on a fact about a different
// file. Say it here: a grant must never be able to enumerate or revoke the
// connections of the account it acts for.
func (m *MCPAuth) manager(w http.ResponseWriter, r *http.Request) (User, bool) {
	if _, agent := grantFrom(r.Context()); agent {
		http.Error(w, "connections cannot be managed through the agent API", http.StatusForbidden)
		return User{}, false
	}
	u, ok := m.caller(r)
	if !ok || u.Email == "" {
		http.Error(w, "sign in", http.StatusUnauthorized)
		return User{}, false
	}
	return u, true
}

func (m *MCPAuth) apiList(w http.ResponseWriter, r *http.Request) {
	u, ok := m.manager(w, r)
	if !ok {
		return
	}
	writeJSON(w, map[string]any{"grants": m.List(u.Email)})
}

func (m *MCPAuth) apiRevoke(w http.ResponseWriter, r *http.Request) {
	u, ok := m.manager(w, r)
	if !ok {
		return
	}
	if !m.Revoke(u.Email, r.PathValue("id")) {
		http.Error(w, "no such connection", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- helpers ----

func oauthErr(w http.ResponseWriter, code int, kind, desc string) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	writeJSON(w, map[string]string{"error": kind, "error_description": desc})
}

// redirectErr reports a failure the client can handle, at the redirect URI —
// but only once the URI itself has been validated, which is why every caller
// is downstream of the exact-match check.
func redirectErr(w http.ResponseWriter, r *http.Request, redirect, state, kind, desc string) {
	u, err := url.Parse(redirect)
	if err != nil {
		oauthErr(w, http.StatusBadRequest, kind, desc)
		return
	}
	q := u.Query()
	q.Set("error", kind)
	q.Set("error_description", desc)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func slicesContains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// consentPage is the product surface of this whole feature: the place a human
// decides what an agent may touch. Nothing is pre-checked — an agent silently
// granted every project because the user hit Enter is exactly the failure
// this screen exists to prevent — and each row states the level the account
// already has, because the grant can never exceed it and saying so here is
// cheaper than a support thread later.
//
// Server-rendered rather than a frontend route: it must work before any grant
// exists, and an OAuth consent screen that depends on the SPA loading is a
// consent screen that fails closed in the one flow that has no fallback.
func (m *MCPAuth) consentPage(w http.ResponseWriter, r *http.Request, c MCPClient, u User, available []MCPProject, warn error) {
	byOrg := map[string][]MCPProject{}
	for _, p := range available {
		byOrg[p.Org] = append(byOrg[p.Org], p)
	}
	orgs := make([]string, 0, len(byOrg))
	for o := range byOrg {
		orgs = append(orgs, o)
	}
	sort.Strings(orgs)

	name := c.Name
	if name == "" {
		name = "An application"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<!doctype html><meta charset=utf-8>
<meta name=viewport content="width=device-width,initial-scale=1">
<title>Connect %s to BearDrive</title>
<style>
:root{color-scheme:light dark}
body{font:15px/1.5 system-ui,-apple-system,Segoe UI,sans-serif;margin:0;padding:2.5rem 1rem;
 background:#fafaf9;color:#1c1917;display:flex;justify-content:center}
@media(prefers-color-scheme:dark){body{background:#0c0a09;color:#e7e5e4}}
.card{width:100%%;max-width:34rem;background:#fff;border:1px solid #e7e5e4;border-radius:14px;padding:1.75rem}
@media(prefers-color-scheme:dark){.card{background:#1c1917;border-color:#292524}}
h1{font-size:1.3rem;margin:0 0 .35rem}
.sub{color:#78716c;margin:0 0 1.25rem}
.org{font-size:.75rem;text-transform:uppercase;letter-spacing:.05em;color:#78716c;margin:1.1rem 0 .4rem}
label{display:flex;gap:.7rem;align-items:center;padding:.6rem .7rem;border-radius:9px;cursor:pointer}
label:hover{background:#f5f5f4}
@media(prefers-color-scheme:dark){label:hover{background:#292524}}
.lvl{margin-left:auto;font-size:.8rem;color:#78716c}
.note{margin:1.25rem 0 0;padding:.8rem .9rem;background:#f5f5f4;border-radius:9px;font-size:.87rem;color:#57534e}
@media(prefers-color-scheme:dark){.note{background:#292524;color:#a8a29e}}
.warn{margin:0 0 1rem;padding:.7rem .9rem;border-radius:9px;background:#fef2f2;color:#991b1b;font-size:.88rem}
.row{display:flex;gap:.6rem;justify-content:flex-end;margin-top:1.5rem}
button,a.btn{font:inherit;padding:.6rem 1.1rem;border-radius:9px;border:1px solid #e7e5e4;
 background:#fff;color:inherit;cursor:pointer;text-decoration:none}
button.go{background:#1c1917;color:#fafaf9;border-color:#1c1917;font-weight:500}
@media(prefers-color-scheme:dark){button.go{background:#fafaf9;color:#1c1917}
 button,a.btn{background:#1c1917;border-color:#44403c}}
button[disabled]{opacity:.45;cursor:not-allowed}
.empty{padding:1.2rem;text-align:center;color:#78716c}
</style>
<div class=card>
<h1>%s wants to access your projects</h1>
<p class=sub>Signed in as %s</p>
`, html.EscapeString(name), html.EscapeString(name), html.EscapeString(u.Email))

	if warn != nil {
		fmt.Fprintf(&b, "<p class=warn>%s</p>", html.EscapeString(warn.Error()))
	}
	fmt.Fprintf(&b, `<form method=post action="%s">`, html.EscapeString(r.URL.RequestURI()))

	if len(available) == 0 {
		b.WriteString(`<p class=empty>You have no projects yet. Create one first, then connect.</p>`)
	}
	for _, org := range orgs {
		ps := byOrg[org]
		sort.Slice(ps, func(i, j int) bool { return ps[i].Name < ps[j].Name })
		if org != "" {
			fmt.Fprintf(&b, `<p class=org>%s</p>`, html.EscapeString(m.orgLabel(org)))
		}
		for _, p := range ps {
			fmt.Fprintf(&b,
				`<label><input type=checkbox name=project value="%s"><span>%s</span><span class=lvl>%s</span></label>`,
				html.EscapeString(p.ID), html.EscapeString(p.Name),
				html.EscapeString(levelPhrase(p.Level)))
		}
	}

	fmt.Fprintf(&b, `
<p class=note>%s will be able to read and change files in the projects you select, acting as you.
Folders you cannot see stay hidden.</p>
<div class=row><a class=btn href="/">Cancel</a>
<button class=go type=submit>Connect selected projects</button></div>
</form></div>`, html.EscapeString(name))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// No form-action, deliberately. It covers EVERY hop of the redirect chain a
	// submit sets off, and the hub only ever knows the first one: real clients
	// register an API host that immediately forwards to their app host, so
	// naming the registered origin just moves the silent breakage one hop later
	// (Runbear: app.beardrive.ai -> api.runbear.io -> app.runbear.io, blocked at
	// the third). CSP cannot express "constrain the first hop only".
	//
	// Dropping it costs nothing real. Where the code goes is decided by the
	// exact match against the client's registered redirect_uris in authorize —
	// a server-side control a browser cannot be talked out of — and by the time
	// any later hop runs, the code has already been delivered to that URI and
	// the rest of the chain carries none of it. form-action would only add
	// anything if this page could be made to submit somewhere else, which needs
	// injected markup or script: every interpolation here is escaped, and
	// default-src 'none' (which script-src falls back to) blocks script
	// outright.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'")
	io.WriteString(w, b.String())
}

// orgLabel is set by the server so the consent page can name orgs; without a
// directory it falls back to the id, which is still better than nothing.
func (m *MCPAuth) orgLabel(id string) string {
	if m.OrgName == nil {
		return id
	}
	if n := m.OrgName(id); n != "" {
		return n
	}
	return id
}

func levelPhrase(level string) string {
	switch level {
	case PermAdmin:
		return "you administer"
	case PermWrite, "":
		return "you can edit"
	case PermRead:
		return "you can view"
	default:
		return ""
	}
}

// OpenMCPAuth is NewMCPAuth over the file backend, for a hub running without
// a configured database. Mirrors OpenShareDB.
//
// The pending store lands beside the grants, under the same directory: a
// consent has to outlive the process for the same reason a grant does.
func OpenMCPAuth(path string, session func(*http.Request) (User, bool), projects func(string) []MCPProject) (*MCPAuth, error) {
	pending := newFilePendingRepo(filepath.Join(filepath.Dir(path), "pending.json"))
	return NewMCPAuth(newFileMCPRepo(path), pending, session, projects)
}
