package webapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The authorization surface of the MCP door.
//
// One rule carries the whole feature: a grant names a set of projects, and a
// token holding that grant must not reach anything outside it — over MCP, and
// over every other route the hub serves. The tests below check the second half
// too, because a scoped credential that only behaves inside the door it was
// minted for is not scoped at all.

// secondProject makes a project in the same org that alice also owns, so a
// cross-project test is measuring the GRANT and not membership.
func (f *mcpFixture) secondProject(name string) Project {
	f.t.Helper()
	rec := doAs(f.t, f.h, "POST", "/api/projects", map[string]string{"name": name}, f.cookies["alice"])
	if rec.Code != 200 {
		f.t.Fatalf("create %s: %d %s", name, rec.Code, rec.Body)
	}
	var out struct {
		Project Project `json:"project"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Project
}

// A grant for one project must not reach another the same account owns.
// This is the difference between "sign in with BearDrive" and a scoped grant.
func TestSecMCPGrantDoesNotReachUnselectedProject(t *testing.T) {
	f := newMCPHub(t)
	other := f.secondProject("secrets")

	// alice owns both, but only connects wiki.
	cs := f.session(f.connect("alice", f.wiki.ID))

	// Seed a file in the unselected project through the browser, as alice.
	rec := doAs(t, f.h, "PUT",
		"/api/p/"+other.ID+"/upload/content?path=classified.md", "top secret", f.cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}

	// The root listing names only the granted project.
	out := mustCall(t, cs, "list", map[string]any{"path": "/"})
	if strings.Contains(out, other.ID) {
		t.Fatalf("root listing leaked an unselected project:\n%s", out)
	}

	// Every tool refuses it by name.
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"list", map[string]any{"path": "/" + other.ID}},
		{"read", map[string]any{"path": "/" + other.ID + "/classified.md"}},
		{"write", map[string]any{"path": "/" + other.ID + "/x.md", "content": "x"}},
		{"edit", map[string]any{"path": "/" + other.ID + "/classified.md", "old_string": "top", "new_string": "no"}},
		{"delete", map[string]any{"path": "/" + other.ID + "/classified.md"}},
		{"history", map[string]any{"path": "/" + other.ID + "/classified.md"}},
		{"grep", map[string]any{"pattern": "secret", "path": "/" + other.ID}},
		{"glob", map[string]any{"pattern": "*.md", "path": "/" + other.ID}},
	} {
		out, isErr := callTool(t, cs, tc.tool, tc.args)
		if !isErr {
			t.Errorf("%s reached an unselected project: %s", tc.tool, out)
		}
		// The caller's own path may be echoed back; its CONTENT may not, and
		// neither may a permission verdict that confirms the project exists.
		if strings.Contains(out, "top secret") {
			t.Errorf("%s leaked content of an unselected project: %s", tc.tool, out)
		}
		if strings.Contains(out, "read-only") || strings.Contains(out, "permission") {
			t.Errorf("%s confirmed an unselected project exists: %s", tc.tool, out)
		}
	}

	// And a project-wide search never crosses into it.
	if out := mustCall(t, cs, "grep", map[string]any{"pattern": "secret"}); strings.Contains(out, "classified") {
		t.Fatalf("root grep crossed into an unselected project:\n%s", out)
	}
}

// The ceiling has to hold on EVERY per-project route, not just the ones MCP
// calls. A scoped token that leaks out of a client and is replayed against the
// hub's ordinary API must be just as denied.
//
// Table-driven over the routes the server actually registered, so a route
// added later without thinking about grants is a failing test rather than a
// silent hole.
func TestSecMCPTokenIsScopedOnEveryProjectRoute(t *testing.T) {
	f := newMCPHub(t)
	other := f.secondProject("secrets")
	f.connect("alice", f.wiki.ID)

	grants := f.srv.MCP.List("alice@x.io")
	if len(grants) != 1 {
		t.Fatalf("grants = %d, want 1", len(grants))
	}
	g := MCPGrant{ID: grants[0].ID, Account: "alice@x.io", Projects: []string{f.wiki.ID}}
	alice := User{Email: "alice@x.io", Name: "Alice"}

	// Drive the INNER mux with the grant on the context — exactly the path an
	// MCP tool's internal call takes, and the only path that reaches
	// capByGrant. Going through the outer handler with a Bearer token instead
	// would 401 at the auth gate and the test would pass with the ceiling
	// deleted.
	//
	// Each request carries a deadline. Some per-project routes are long-lived
	// streams (/events, /collab): they 403 instantly while the ceiling holds,
	// so an undeadlined sweep looks fine — and then hangs the whole suite the
	// first time someone breaks the ceiling, which is precisely the run where
	// a clear failure matters most. Ask for the failure, do not wait for it.
	send := func(method, target string) *httptest.ResponseRecorder {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req := httptest.NewRequest(method, target, strings.NewReader("{}")).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		req = withUser(withGrant(req, g), alice)
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { defer close(done); f.srv.apiMux.ServeHTTP(rec, req) }()
		select {
		case <-done:
		case <-ctx.Done():
			t.Errorf("%s %s did not answer within 2s for a project outside the grant "+
				"(a stream that should have been refused)", method, target)
		}
		return rec
	}

	var checked int
	for _, pat := range APIRoutes() {
		method, path, ok := strings.Cut(pat, " ")
		if !ok || !strings.Contains(path, "{project}") {
			continue
		}
		target := strings.ReplaceAll(path, "{project}", other.ID)
		if strings.Contains(target, "{") {
			continue // another wildcard needs a value we cannot invent
		}
		rec := send(method, target)
		checked++
		// Refused, or the door does not admit the project exists. Never a 2xx.
		if rec.Code >= 200 && rec.Code < 300 {
			t.Errorf("%s %s answered %d for a project outside the grant: %s",
				method, target, rec.Code, truncate(rec.Body.String(), 120))
		}
	}
	if checked < 10 {
		t.Fatalf("only %d per-project routes checked; the table is not covering the API", checked)
	}

	// The same grant DOES work on the project it names, or the sweep above
	// would pass by simply breaking every route.
	if rec := send("GET", "/api/p/"+f.wiki.ID+"/tree"); rec.Code != 200 {
		t.Fatalf("granted project answered %d: %s", rec.Code, truncate(rec.Body.String(), 200))
	}

	// And an MCP access token is not a session credential: replayed against
	// the public handler it does not authenticate at all.
	token := f.connect("alice", f.wiki.ID)
	req := httptest.NewRequest("GET", "/api/p/"+f.wiki.ID+"/tree", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code < 400 {
		t.Fatalf("an MCP token authenticated an ordinary API route: %d", rec.Code)
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// A grant can never exceed the level its account already holds. Carol is a
// plain member downgraded to read on the project; her agent gets read too.
func TestSecMCPGrantCannotExceedAccountLevel(t *testing.T) {
	f := newMCPHub(t)

	// alice seeds a file, then makes carol read-only on the project.
	rec := doAs(t, f.h, "PUT",
		"/api/p/"+f.wiki.ID+"/upload/content?path=notes.md", "hello", f.cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	rec = doAs(t, f.h, "PUT", "/api/p/"+f.wiki.ID+"/permissions/carol@x.io",
		map[string]string{"level": PermRead}, f.cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("set carol read-only: %d %s", rec.Code, rec.Body)
	}

	cs := f.session(f.connect("carol", f.wiki.ID))

	// Reading is fine.
	if out := mustCall(t, cs, "read", map[string]any{"path": "/" + f.wiki.ID + "/notes.md"}); !strings.Contains(out, "hello") {
		t.Fatalf("read-only member cannot read: %s", out)
	}
	// Writing is not — through any of the write tools.
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"write", map[string]any{"path": "/" + f.wiki.ID + "/new.md", "content": "x"}},
		{"edit", map[string]any{"path": "/" + f.wiki.ID + "/notes.md", "old_string": "hello", "new_string": "bye"}},
		{"delete", map[string]any{"path": "/" + f.wiki.ID + "/notes.md"}},
		{"move", map[string]any{"from": "/" + f.wiki.ID + "/notes.md", "to": "/" + f.wiki.ID + "/moved.md"}},
	} {
		if out, isErr := callTool(t, cs, tc.tool, tc.args); !isErr {
			t.Errorf("read-only member wrote through %s: %s", tc.tool, out)
		}
	}
	// And the file is untouched.
	if out := mustCall(t, cs, "read", map[string]any{"path": "/" + f.wiki.ID + "/notes.md"}); !strings.Contains(out, "hello") {
		t.Fatalf("file changed despite refusals: %s", out)
	}
}

// A member of another org cannot connect a project at all: the consent screen
// never offers it, and naming it in a hand-edited form grants nothing.
func TestSecMCPConsentCannotBeForgedForAForeignProject(t *testing.T) {
	f := newMCPHub(t)

	// dave is in a different org entirely.
	if got := f.srv.ConnectableProjects("dave@x.io"); len(got) != 0 {
		t.Fatalf("consent screen offers dave %d project(s) he is not a member of", len(got))
	}

	// Hand-editing the form to name alice's project yields no grant at all.
	token := connectExpectingRefusal(t, f, "dave", f.wiki.ID)
	if token != "" {
		t.Fatal("dave obtained a grant for a project he cannot see")
	}
}

// connectExpectingRefusal runs consent for a project the account may not have
// and reports the token if one was (wrongly) issued.
func connectExpectingRefusal(t *testing.T, f *mcpFixture, who, projectID string) string {
	t.Helper()
	regBody, _ := json.Marshal(map[string]any{
		"client_name":   "Forged",
		"redirect_uris": []string{"http://127.0.0.1:9999/callback"},
	})
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest("POST", "/oauth/register", strings.NewReader(string(regBody))))
	var reg struct {
		ClientID string `json:"client_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &reg)

	q := "client_id=" + reg.ClientID +
		"&redirect_uri=" + "http%3A%2F%2F127.0.0.1%3A9999%2Fcallback" +
		"&response_type=code&code_challenge_method=S256&code_challenge=" +
		"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/oauth/authorize?"+q, strings.NewReader("project="+projectID))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(f.cookies[who])
	f.h.ServeHTTP(rec, req)

	// A refusal re-renders the consent page (200) rather than redirecting
	// with a code.
	if rec.Code != http.StatusFound {
		return ""
	}
	return rec.Header().Get("Location")
}

// Revocation has to kill the access token immediately, not at its next expiry.
func TestSecMCPRevocationIsImmediate(t *testing.T) {
	f := newMCPHub(t)
	token := f.connect("alice", f.wiki.ID)
	cs := f.session(token)
	mustCall(t, cs, "list", map[string]any{"path": "/"})

	grants := f.srv.MCP.List("alice@x.io")
	if len(grants) != 1 {
		t.Fatalf("grants = %d, want 1", len(grants))
	}
	// The listing must never carry the credentials themselves.
	if grants[0].TokenDigest != "" || grants[0].RefreshDigest != "" {
		t.Fatal("grant listing leaked token digests")
	}
	if !f.srv.MCP.Revoke("alice@x.io", grants[0].ID) {
		t.Fatal("revoke failed")
	}

	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("revoked token still works: %d %s", rec.Code, rec.Body)
	}
}

// One account must not be able to revoke another's connection.
func TestSecMCPRevokeIsAccountScoped(t *testing.T) {
	f := newMCPHub(t)
	f.connect("alice", f.wiki.ID)
	grants := f.srv.MCP.List("alice@x.io")
	if len(grants) != 1 {
		t.Fatalf("grants = %d", len(grants))
	}
	if f.srv.MCP.Revoke("bob@x.io", grants[0].ID) {
		t.Fatal("bob revoked alice's connection")
	}
	if got := f.srv.MCP.List("bob@x.io"); len(got) != 0 {
		t.Fatalf("bob sees %d of alice's grants", len(got))
	}
}

// PKCE is what binds a code to the client that asked for it. A code redeemed
// with the wrong verifier must not mint a token.
func TestSecMCPPKCEIsEnforced(t *testing.T) {
	f := newMCPHub(t)
	regBody, _ := json.Marshal(map[string]any{
		"client_name":   "Test",
		"redirect_uris": []string{"http://127.0.0.1:9999/callback"},
	})
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest("POST", "/oauth/register", strings.NewReader(string(regBody))))
	var reg struct {
		ClientID string `json:"client_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &reg)

	// No challenge at all is refused up front.
	q := "client_id=" + reg.ClientID + "&redirect_uri=http%3A%2F%2F127.0.0.1%3A9999%2Fcallback&response_type=code"
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/oauth/authorize?"+q, nil)
	req.AddCookie(f.cookies["alice"])
	f.h.ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Fatal("consent screen rendered without PKCE")
	}

	// A valid code with a wrong verifier is refused at the token endpoint.
	token := f.connect("alice", f.wiki.ID)
	if token == "" {
		t.Fatal("setup failed")
	}
	form := "grant_type=authorization_code&code=mcpa_nonsense&code_verifier=wrong"
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	f.h.ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Fatal("token endpoint minted a token for an unknown code")
	}
}

// An unregistered redirect_uri must never receive a code — that is an open
// redirector with an authorization code attached.
func TestSecMCPRedirectURIMustMatchRegistration(t *testing.T) {
	f := newMCPHub(t)
	regBody, _ := json.Marshal(map[string]any{
		"client_name":   "Test",
		"redirect_uris": []string{"http://127.0.0.1:9999/callback"},
	})
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest("POST", "/oauth/register", strings.NewReader(string(regBody))))
	var reg struct {
		ClientID string `json:"client_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &reg)

	q := "client_id=" + reg.ClientID +
		"&redirect_uri=https%3A%2F%2Fevil.example%2Fsteal&response_type=code" +
		"&code_challenge_method=S256&code_challenge=E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/oauth/authorize?"+q, nil)
	req.AddCookie(f.cookies["alice"])
	f.h.ServeHTTP(rec, req)
	if rec.Code == http.StatusFound {
		t.Fatalf("redirected to an unregistered URI: %s", rec.Header().Get("Location"))
	}
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// The consent screen must not accept a Bearer token as the session: an MCP
// access token that could reach consent could widen its own project set.
func TestSecMCPConsentRefusesBearerSession(t *testing.T) {
	f := newMCPHub(t)
	token := f.connect("alice", f.wiki.ID)

	req := httptest.NewRequest("GET", "/oauth/authorize?client_id=x&redirect_uri=y&response_type=code", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if _, ok := f.srv.SessionUser(req); ok {
		t.Fatal("SessionUser accepted a Bearer token; consent could be driven by an agent")
	}
}

// Folder permissions are one level below projects, and the door has to honor
// them: a hidden folder is invisible and unwritable, and a hidden path reads
// as not-found rather than forbidden (a 403 confirms the file is there).
func TestSecMCPHonorsFolderPermissions(t *testing.T) {
	f := newMCPHub(t)

	for _, p := range []string{"public/open.md", "private/secret.md"} {
		rec := doAs(t, f.h, "PUT",
			"/api/p/"+f.wiki.ID+"/upload/content?path="+p, "content of "+p, f.cookies["alice"])
		if rec.Code != 200 {
			t.Fatalf("seed %s: %d %s", p, rec.Code, rec.Body)
		}
	}
	// Hide private/ from bob.
	rec := doAs(t, f.h, "PUT", "/api/p/"+f.wiki.ID+"/folders",
		map[string]any{"prefix": "private/", "default": PermNone}, f.cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("set folder rule: %d %s", rec.Code, rec.Body)
	}

	cs := f.session(f.connect("bob", f.wiki.ID))

	if out := mustCall(t, cs, "list", map[string]any{"path": "/" + f.wiki.ID, "depth": 5}); strings.Contains(out, "secret.md") {
		t.Fatalf("hidden folder appears in listing:\n%s", out)
	}
	if out := mustCall(t, cs, "grep", map[string]any{"pattern": "content"}); strings.Contains(out, "secret.md") {
		t.Fatalf("grep crossed into a hidden folder:\n%s", out)
	}
	if out := mustCall(t, cs, "glob", map[string]any{"pattern": "**/*.md"}); strings.Contains(out, "secret.md") {
		t.Fatalf("glob listed a hidden file:\n%s", out)
	}
	out, isErr := callTool(t, cs, "read", map[string]any{"path": "/" + f.wiki.ID + "/private/secret.md"})
	if !isErr {
		t.Fatalf("read a hidden file: %s", out)
	}
	if strings.Contains(strings.ToLower(out), "permission") || strings.Contains(out, "403") {
		t.Fatalf("hidden path answered with a permission error, confirming it exists: %s", out)
	}
	if _, isErr := callTool(t, cs, "write", map[string]any{
		"path": "/" + f.wiki.ID + "/private/new.md", "content": "x",
	}); !isErr {
		t.Fatal("wrote into a hidden folder")
	}
}

// An MCP read is an agent read, and its actor must never surface as a device.
// ?by=device is documented as reporting DEVICE ids — something every project
// member already sees in History. A grant id has never appeared there, and
// letting one through would widen that stated exception by accident.
func TestSecMCPReadsAreAgentAndNeverNamedAsDevices(t *testing.T) {
	f := newMCPHub(t)
	ledger, err := OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 0)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.Reads = ledger
	t.Cleanup(func() { ledger.Close() })

	rec := doAs(t, f.h, "PUT",
		"/api/p/"+f.wiki.ID+"/upload/content?path=seen.md", "content", f.cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}

	cs := f.session(f.connect("alice", f.wiki.ID))
	mustCall(t, cs, "read", map[string]any{"path": "/" + f.wiki.ID + "/seen.md"})

	// The read landed, as an agent read.
	if got := ledger.Heat(f.wiki.ID, "", time.Time{}); len(got) == 0 {
		t.Fatal("an MCP read was recorded nowhere")
	} else if e, ok := got["seen.md"]; !ok || e.Agent == 0 {
		t.Fatalf("read was not counted as agent traffic: %+v", got)
	}

	// And ?by=device names nobody, because a grant is not a device.
	rec = doAs(t, f.h, "GET", "/api/p/"+f.wiki.ID+"/heat?by=device", nil, f.cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("heat: %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		Devices []struct {
			ID string `json:"id"`
		} `json:"devices"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	for _, d := range resp.Devices {
		if strings.HasPrefix(d.ID, "mcp") || strings.Contains(d.ID, "mcpg_") {
			t.Fatalf("?by=device reported a grant id: %q", d.ID)
		}
	}
	// The whole response must not carry the account email either.
	if strings.Contains(rec.Body.String(), "alice@x.io") {
		t.Fatalf("heat response leaked an account identity: %s", rec.Body)
	}
}

// A hub with no MCP block has no /mcp at all — not an unauthenticated one.
// The door is off by default because a hub that has not thought about agent
// access should not have agent access.
func TestSecMCPOffByDefault(t *testing.T) {
	h, srv, _, _, _ := permHubAt(t)
	if srv.MCP != nil {
		t.Fatal("MCP is wired without configuration")
	}
	for _, target := range []string{"/mcp", "/oauth/authorize", "/oauth/register", "/oauth/token"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", target, strings.NewReader("{}")))
		// The SPA fallback answers unknown GETs, so the tell is that no
		// handler accepted the POST.
		if rec.Code >= 200 && rec.Code < 300 {
			t.Errorf("%s answered %d on a hub with MCP disabled", target, rec.Code)
		}
	}
}

// A grant is a ceiling on live permission, not a snapshot of it. Removing
// someone's access must take every connection they made with it, at once and
// with nothing to revoke — otherwise offboarding through the UI leaves agent
// tokens holding access the account itself no longer has.
func TestSecMCPGrantFollowsAPermissionChange(t *testing.T) {
	f := newMCPHub(t)
	rec := doAs(t, f.h, "PUT",
		"/api/p/"+f.wiki.ID+"/upload/content?path=doc.md", []byte("visible"), f.cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	cs := f.session(f.connect("bob", f.wiki.ID))
	if out := mustCall(t, cs, "read", map[string]any{"path": "/" + f.wiki.ID + "/doc.md"}); !strings.Contains(out, "visible") {
		t.Fatalf("setup: bob cannot read: %s", out)
	}

	// Downgrade bob to read: his agent loses writing too, immediately.
	rec = doAs(t, f.h, "PUT", "/api/p/"+f.wiki.ID+"/permissions/bob@x.io",
		map[string]string{"level": PermRead}, f.cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("downgrade: %d %s", rec.Code, rec.Body)
	}
	if out, isErr := callTool(t, cs, "write", map[string]any{
		"path": "/" + f.wiki.ID + "/new.md", "content": "x",
	}); !isErr {
		t.Fatalf("a downgraded account's agent still wrote: %s", out)
	}

	// Remove him from the project entirely: the same token now sees nothing.
	rec = doAs(t, f.h, "PUT", "/api/p/"+f.wiki.ID+"/permissions/bob@x.io",
		map[string]string{"level": PermNone}, f.cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("revoke access: %d %s", rec.Code, rec.Body)
	}
	if out, isErr := callTool(t, cs, "read", map[string]any{"path": "/" + f.wiki.ID + "/doc.md"}); !isErr {
		t.Fatalf("a removed account's agent still read the file: %s", out)
	}
	if out := mustCall(t, cs, "list", map[string]any{"path": "/"}); strings.Contains(out, f.wiki.ID) {
		t.Fatalf("a removed account's agent still lists the project: %s", out)
	}
}

// The CLI manages connections with a device token, so list/revoke accept any
// authenticated credential — but an AGENT must never be able to enumerate or
// revoke the connections of the account it acts for. Widening the credential
// is only safe because that rule is stated here rather than inferred from the
// fact that no AuthProvider happens to recognize an MCP token.
func TestSecMCPConnectionsAreNotAgentManageable(t *testing.T) {
	f := newMCPHub(t)
	token := f.connect("alice", f.wiki.ID)
	grants := f.srv.MCP.List("alice@x.io")
	if len(grants) != 1 {
		t.Fatalf("grants = %d", len(grants))
	}
	f.srv.MCP.UseCaller(func(r *http.Request) (User, bool) {
		u := f.srv.RequestUser(r)
		return u, u.Email != ""
	})

	// A browser session manages its own connections.
	rec := doAs(t, f.h, "GET", "/api/mcp/grants", nil, f.cookies["alice"])
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), grants[0].ID) {
		t.Fatalf("session cannot list its own connections: %d %s", rec.Code, rec.Body)
	}

	// An MCP access token replayed at the management API does not authenticate.
	for _, m := range []string{"GET", "DELETE"} {
		target := "/api/mcp/grants"
		if m == "DELETE" {
			target += "/" + grants[0].ID
		}
		req := httptest.NewRequest(m, target, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		if rec.Code < 400 {
			t.Fatalf("%s %s accepted an MCP token: %d %s", m, target, rec.Code, rec.Body)
		}
	}

	// And a request carrying a grant on its context — the shape an internal
	// tool call has — is refused outright, not merely unauthenticated.
	req := httptest.NewRequest("GET", "/api/mcp/grants", nil)
	req = withUser(withGrant(req, MCPGrant{ID: grants[0].ID, Account: "alice@x.io"}),
		User{Email: "alice@x.io"})
	rec = httptest.NewRecorder()
	f.srv.apiMux.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("an agent-context request to the management API = %d, want 403: %s", rec.Code, rec.Body)
	}

	// The connection still exists: nothing above revoked it.
	if got := f.srv.MCP.List("alice@x.io"); len(got) != 1 {
		t.Fatalf("connection count = %d, want 1", len(got))
	}
}
