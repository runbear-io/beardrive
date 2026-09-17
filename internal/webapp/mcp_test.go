package webapp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The MCP door is exercised through the REAL protocol — the SDK's own client
// against an httptest server — rather than by calling tool functions. The
// bugs worth catching here live in the wiring (auth, the grant ceiling, the
// re-entrant internal call), and a direct function call skips all of it.

type mcpFixture struct {
	t       *testing.T
	srv     *Server
	ts      *httptest.Server
	h       http.Handler
	cookies map[string]*http.Cookie
	wiki    Project
}

func newMCPHub(t *testing.T) *mcpFixture {
	t.Helper()
	h, srv, cookies, p, _ := permHubAt(t)
	mcpAuth, err := OpenMCPAuth(filepath.Join(t.TempDir(), "mcp.json"), srv.SessionUser, srv.ConnectableProjects)
	if err != nil {
		t.Fatal(err)
	}
	mcpAuth.OrgName = srv.OrgName
	srv.MCP = mcpAuth
	// Handler again so /mcp and the OAuth routes are registered, and so
	// apiMux points at a mux that has them.
	h = srv.Handler()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return &mcpFixture{t: t, srv: srv, ts: ts, h: h, cookies: cookies, wiki: p}
}

// connect runs the whole OAuth dance the way a real MCP client does —
// register, authorize (ticking boxes on the consent screen), exchange — and
// returns the access token.
func (f *mcpFixture) connect(who string, projects ...string) string {
	f.t.Helper()

	// 1. Dynamic client registration (RFC 7591).
	regBody, _ := json.Marshal(map[string]any{
		"client_name":   "Test Agent",
		"redirect_uris": []string{"http://127.0.0.1:9999/callback"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/oauth/register", strings.NewReader(string(regBody)))
	f.h.ServeHTTP(rec, req)
	if rec.Code != 201 {
		f.t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	var reg struct {
		ClientID string `json:"client_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &reg)

	// 2. Consent. PKCE verifier/challenge as a real client would.
	verifier := "verifier-0123456789-0123456789-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	q := url.Values{}
	q.Set("client_id", reg.ClientID)
	q.Set("redirect_uri", "http://127.0.0.1:9999/callback")
	q.Set("response_type", "code")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", "xyz")

	form := url.Values{}
	for _, p := range projects {
		form.Add("project", p)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/oauth/authorize?"+q.Encode(), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(f.cookies[who])
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		f.t.Fatalf("authorize: %d %s", rec.Code, rec.Body)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		f.t.Fatal(err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		f.t.Fatalf("no code in %s", loc)
	}
	if got := loc.Query().Get("state"); got != "xyz" {
		f.t.Fatalf("state = %q, want xyz", got)
	}

	// 3. Exchange.
	tf := url.Values{}
	tf.Set("grant_type", "authorization_code")
	tf.Set("code", code)
	tf.Set("code_verifier", verifier)
	tf.Set("client_id", reg.ClientID)
	tf.Set("redirect_uri", "http://127.0.0.1:9999/callback")
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/oauth/token", strings.NewReader(tf.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	f.h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		f.t.Fatalf("token: %d %s", rec.Code, rec.Body)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	json.Unmarshal(rec.Body.Bytes(), &tok)
	if tok.AccessToken == "" {
		f.t.Fatal("no access token")
	}
	return tok.AccessToken
}

// bearer is an http.RoundTripper that adds the token, the way an MCP client
// holding a grant does.
type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	base := b.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

// session opens a real MCP session over streamable HTTP.
func (f *mcpFixture) session(token string) *mcp.ClientSession {
	f.t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             f.ts.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: bearer{token: token}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		f.t.Fatalf("connect: %v", err)
	}
	f.t.Cleanup(func() { cs.Close() })
	return cs
}

// call runs a tool and returns its text, failing on a protocol error.
func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: transport error: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func mustCall(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	out, isErr := callTool(t, cs, name, args)
	if isErr {
		t.Fatalf("%s(%v) failed: %s", name, args, out)
	}
	return out
}

// ---- the flow works at all ----

func TestMCPConnectAndListTools(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"list": false, "read": false, "glob": false, "grep": false,
		"write": false, "edit": false, "delete": false, "move": false,
		"history": false, "restore": false,
	}
	for _, tl := range tools.Tools {
		if _, ok := want[tl.Name]; !ok {
			t.Errorf("unexpected tool %q", tl.Name)
			continue
		}
		want[tl.Name] = true
		if tl.Description == "" {
			t.Errorf("tool %q has no description", tl.Name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("missing tool %q", name)
		}
	}
}

func TestMCPUnauthenticatedGets401WithChallenge(t *testing.T) {
	f := newMCPHub(t)
	res, err := http.Post(f.ts.URL+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
	// RFC 9728: without this header a client cannot discover where to
	// authenticate and simply reports the server as broken.
	if wa := res.Header.Get("WWW-Authenticate"); !strings.Contains(wa, "resource_metadata") {
		t.Fatalf("WWW-Authenticate = %q, want a resource_metadata pointer", wa)
	}
}

func TestMCPDiscoveryDocuments(t *testing.T) {
	f := newMCPHub(t)
	for _, path := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
		"/.well-known/oauth-authorization-server",
		"/.well-known/oauth-authorization-server/mcp",
	} {
		res, err := http.Get(f.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		json.NewDecoder(res.Body).Decode(&doc)
		res.Body.Close()
		if res.StatusCode != 200 || len(doc) == 0 {
			t.Fatalf("%s: %d %v", path, res.StatusCode, doc)
		}
	}
	// The AS document has to name endpoints that exist and PKCE S256, or a
	// real client refuses before it ever reaches /mcp.
	res, _ := http.Get(f.ts.URL + "/.well-known/oauth-authorization-server")
	var as struct {
		Authorization string   `json:"authorization_endpoint"`
		Token         string   `json:"token_endpoint"`
		Registration  string   `json:"registration_endpoint"`
		Challenge     []string `json:"code_challenge_methods_supported"`
	}
	json.NewDecoder(res.Body).Decode(&as)
	res.Body.Close()
	if as.Authorization == "" || as.Token == "" || as.Registration == "" {
		t.Fatalf("AS metadata is missing endpoints: %+v", as)
	}
	if len(as.Challenge) == 0 || as.Challenge[0] != "S256" {
		t.Fatalf("code_challenge_methods_supported = %v, want [S256]", as.Challenge)
	}
}

// ---- the filesystem ----

func TestMCPFilesystemRoundTrip(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	root := "/" + f.wiki.ID

	// The root lists connected projects.
	if out := mustCall(t, cs, "list", map[string]any{"path": "/"}); !strings.Contains(out, f.wiki.ID) {
		t.Fatalf("root list does not name the project: %s", out)
	}

	// write → read round trip. Note the nested path: there is no mkdir.
	mustCall(t, cs, "write", map[string]any{
		"path": root + "/docs/deep/spec.md", "content": "# Spec\n\nhello world\n",
	})
	out := mustCall(t, cs, "read", map[string]any{"path": root + "/docs/deep/spec.md"})
	if !strings.Contains(out, "hello world") {
		t.Fatalf("read = %s", out)
	}

	// list shows the implicit directory at depth 1, the file deeper.
	if out := mustCall(t, cs, "list", map[string]any{"path": root}); !strings.Contains(out, "docs/") {
		t.Fatalf("list depth 1 = %s, want a docs/ entry", out)
	}
	if out := mustCall(t, cs, "list", map[string]any{"path": root, "depth": 5}); !strings.Contains(out, "spec.md") {
		t.Fatalf("list depth 5 = %s", out)
	}

	// glob and grep find it.
	if out := mustCall(t, cs, "glob", map[string]any{"pattern": "**/*.md"}); !strings.Contains(out, "spec.md") {
		t.Fatalf("glob = %s", out)
	}
	if out := mustCall(t, cs, "grep", map[string]any{"pattern": "hello"}); !strings.Contains(out, "spec.md") {
		t.Fatalf("grep = %s", out)
	}
	if out := mustCall(t, cs, "grep", map[string]any{"pattern": "HELLO", "ignore_case": true}); !strings.Contains(out, "spec.md") {
		t.Fatalf("grep -i = %s", out)
	}
	if out, _ := callTool(t, cs, "grep", map[string]any{"pattern": "nothingmatchesthis"}); !strings.Contains(out, "No matches") {
		t.Fatalf("grep miss = %s", out)
	}

	// edit replaces exact text.
	mustCall(t, cs, "edit", map[string]any{
		"path": root + "/docs/deep/spec.md", "old_string": "hello world", "new_string": "goodbye world",
	})
	if out := mustCall(t, cs, "read", map[string]any{"path": root + "/docs/deep/spec.md"}); !strings.Contains(out, "goodbye world") {
		t.Fatalf("after edit = %s", out)
	}

	// history records both writes, attributed to the human.
	hist := mustCall(t, cs, "history", map[string]any{"path": root + "/docs/deep/spec.md"})
	if !strings.Contains(hist, "alice@x.io") && !strings.Contains(hist, "Alice") {
		t.Fatalf("history does not name the account: %s", hist)
	}

	// move, then delete.
	mustCall(t, cs, "move", map[string]any{
		"from": root + "/docs/deep/spec.md", "to": root + "/docs/spec.md",
	})
	if out, isErr := callTool(t, cs, "read", map[string]any{"path": root + "/docs/deep/spec.md"}); !isErr {
		t.Fatalf("old path still readable after move: %s", out)
	}
	mustCall(t, cs, "read", map[string]any{"path": root + "/docs/spec.md"})
	mustCall(t, cs, "delete", map[string]any{"path": root + "/docs/spec.md"})
	if _, isErr := callTool(t, cs, "read", map[string]any{"path": root + "/docs/spec.md"}); !isErr {
		t.Fatal("file still readable after delete")
	}
}

// edit must refuse when the file moved under it, or an agent silently
// discards a teammate's concurrent change.
func TestMCPEditIsCompareAndSwap(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	root := "/" + f.wiki.ID

	mustCall(t, cs, "write", map[string]any{"path": root + "/a.txt", "content": "one\n"})
	out := mustCall(t, cs, "read", map[string]any{"path": root + "/a.txt"})
	sha := shaFrom(t, out)

	// Someone else changes it.
	mustCall(t, cs, "write", map[string]any{"path": root + "/a.txt", "content": "two\n"})

	got, isErr := callTool(t, cs, "edit", map[string]any{
		"path": root + "/a.txt", "old_string": "two", "new_string": "three", "sha": sha,
	})
	if !isErr || !strings.Contains(got, "stale") {
		t.Fatalf("stale edit was allowed: %q", got)
	}
	// Without a sha the edit proceeds — the agent opted out of the check.
	mustCall(t, cs, "edit", map[string]any{
		"path": root + "/a.txt", "old_string": "two", "new_string": "three",
	})
}

func shaFrom(t *testing.T, readOutput string) string {
	t.Helper()
	i := strings.Index(readOutput, "sha ")
	if i < 0 {
		t.Fatalf("no sha in read output: %s", readOutput)
	}
	rest := readOutput[i+4:]
	if j := strings.IndexAny(rest, ",) \n"); j >= 0 {
		return rest[:j]
	}
	return rest
}

func TestMCPEditRefusesAmbiguousMatch(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	root := "/" + f.wiki.ID
	mustCall(t, cs, "write", map[string]any{"path": root + "/b.txt", "content": "x\nx\n"})

	out, isErr := callTool(t, cs, "edit", map[string]any{
		"path": root + "/b.txt", "old_string": "x", "new_string": "y",
	})
	if !isErr || !strings.Contains(out, "2 times") {
		t.Fatalf("ambiguous edit was allowed: %q", out)
	}
	mustCall(t, cs, "edit", map[string]any{
		"path": root + "/b.txt", "old_string": "x", "new_string": "y", "replace_all": true,
	})
}

func TestMCPRestorePreviousVersion(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	root := "/" + f.wiki.ID

	mustCall(t, cs, "write", map[string]any{"path": root + "/c.txt", "content": "first\n"})
	mustCall(t, cs, "write", map[string]any{"path": root + "/c.txt", "content": "second\n"})

	// Too short to identify anything is refused, and says what to do.
	if out, isErr := callTool(t, cs, "restore", map[string]any{"path": root + "/c.txt", "sha": "abc"}); !isErr ||
		!strings.Contains(out, "too short") {
		t.Fatalf("ambiguous sha accepted: %q", out)
	}
	// A sha that names no version of THIS file is refused too.
	if out, isErr := callTool(t, cs, "restore",
		map[string]any{"path": root + "/c.txt", "sha": "deadbeefcafe"}); !isErr ||
		!strings.Contains(out, "no version") {
		t.Fatalf("unknown sha accepted: %q", out)
	}

	full := firstVersionSHA(t, f, "c.txt")
	mustCall(t, cs, "restore", map[string]any{"path": root + "/c.txt", "sha": full})
	if out := mustCall(t, cs, "read", map[string]any{"path": root + "/c.txt"}); !strings.Contains(out, "first") {
		t.Fatalf("restore did not bring back the first version: %s", out)
	}

	// And the SHORT sha history actually prints must work, or restore is
	// unreachable: history is the only tool that names a version.
	mustCall(t, cs, "write", map[string]any{"path": root + "/c.txt", "content": "third\n"})
	hist := mustCall(t, cs, "history", map[string]any{"path": root + "/c.txt"})
	shortSHA := ""
	for _, line := range strings.Split(hist, "\n") {
		if i := strings.Index(line, "sha:"); i >= 0 {
			shortSHA = strings.TrimSpace(line[i+4:])
		}
	}
	if shortSHA == "" {
		t.Fatalf("history printed no sha: %s", hist)
	}
	if len(shortSHA) >= 64 {
		t.Fatalf("history printed a full sha (%d chars); the short-sha path is untested", len(shortSHA))
	}
	mustCall(t, cs, "restore", map[string]any{"path": root + "/c.txt", "sha": shortSHA})
}

func firstVersionSHA(t *testing.T, f *mcpFixture, path string) string {
	t.Helper()
	rec := doAs(t, f.h, "GET", "/api/p/"+f.wiki.ID+"/history?path="+url.QueryEscape(path), nil, f.cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("history: %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		Entries []struct {
			Blob string `json:"blob"`
		} `json:"entries"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Entries) < 2 {
		t.Fatalf("want 2 versions, got %d", len(resp.Entries))
	}
	return resp.Entries[len(resp.Entries)-1].Blob
}

// A small image comes back as an image; a large one is described instead, so
// one read cannot blow the agent's context with base64.
func TestMCPReadReturnsImagesInline(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	root := "/" + f.wiki.ID

	// A 1x1 PNG, written through the hub's own upload door.
	png, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	// png, not string(png): doAs JSON-marshals anything that is not []byte,
	// which would escape the binary and store a different file.
	rec := doAs(t, f.h, "PUT", "/api/p/"+f.wiki.ID+"/upload/content?path=logo.png",
		png, f.cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("seed png: %d %s", rec.Code, rec.Body)
	}

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "read", Arguments: map[string]any{"path": root + "/logo.png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || len(res.Content) != 1 {
		t.Fatalf("read png: %+v", res.Content)
	}
	img, ok := res.Content[0].(*mcp.ImageContent)
	if !ok {
		t.Fatalf("content = %T, want an image block", res.Content[0])
	}
	if img.MIMEType != "image/png" || len(img.Data) != len(png) {
		t.Fatalf("image = %s, %d bytes (want image/png, %d)", img.MIMEType, len(img.Data), len(png))
	}
}

// Agents cannot tell a file path from a folder path, so they list both. A
// bare "Nothing at X" sends them hunting for a file that is right there.
func TestMCPListOfAFileSaysSo(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	root := "/" + f.wiki.ID
	mustCall(t, cs, "write", map[string]any{"path": root + "/readme.md", "content": "hi\n"})

	out := mustCall(t, cs, "list", map[string]any{"path": root + "/readme.md"})
	if !strings.Contains(out, "is a file") || !strings.Contains(out, "read") {
		t.Fatalf("list of a file = %q, want it to say so and point at read", out)
	}
	if out := mustCall(t, cs, "list", map[string]any{"path": root + "/nope"}); !strings.Contains(out, "Nothing at") {
		t.Fatalf("list of a missing path = %q", out)
	}
}

// ---- regressions from the black-box validation round ----

// move is write-then-delete, so moving a file onto itself wrote the bytes and
// then deleted the path it had just written. "Rename these to match our
// convention" hits this on every file that is already correctly named, and the
// tool reported success.
func TestMCPMoveOntoItselfDoesNotDestroyTheFile(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	p := "/" + f.wiki.ID + "/keep/vital.md"

	mustCall(t, cs, "write", map[string]any{"path": p, "content": "months of work\n"})
	out := mustCall(t, cs, "move", map[string]any{"from": p, "to": p})
	if strings.Contains(out, "moved") {
		t.Errorf("self-move reported a move: %q", out)
	}
	got := mustCall(t, cs, "read", map[string]any{"path": p})
	if !strings.Contains(got, "months of work") {
		t.Fatalf("self-move destroyed the file: %s", got)
	}
}

// move onto an occupied path silently destroyed the file already there. write
// is documented as "create or overwrite"; move is not.
func TestMCPMoveRefusesToClobberDestination(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	root := "/" + f.wiki.ID
	mustCall(t, cs, "write", map[string]any{"path": root + "/mv/keep.txt", "content": "MONTHS OF WORK\n"})
	mustCall(t, cs, "write", map[string]any{"path": root + "/mv/tmp.txt", "content": "scratch\n"})

	out, isErr := callTool(t, cs, "move", map[string]any{
		"from": root + "/mv/tmp.txt", "to": root + "/mv/keep.txt",
	})
	if !isErr {
		t.Fatalf("move clobbered an existing file: %q", out)
	}
	if got := mustCall(t, cs, "read", map[string]any{"path": root + "/mv/keep.txt"}); !strings.Contains(got, "MONTHS OF WORK") {
		t.Fatalf("destination was destroyed: %s", got)
	}
	// The source is untouched too — a refused move must not half-happen.
	mustCall(t, cs, "read", map[string]any{"path": root + "/mv/tmp.txt"})
}

// write is the only way to rewrite a whole file, so it needs the same
// lost-update defence edit has. Without it two agents rewriting one file both
// reported success and one change vanished.
func TestMCPWriteHonorsCompareAndSwap(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	p := "/" + f.wiki.ID + "/shared.md"

	mustCall(t, cs, "write", map[string]any{"path": p, "content": "base\n"})
	sha := shaFrom(t, mustCall(t, cs, "read", map[string]any{"path": p}))
	mustCall(t, cs, "write", map[string]any{"path": p, "content": "someone else\n"})

	out, isErr := callTool(t, cs, "write", map[string]any{"path": p, "content": "mine\n", "sha": sha})
	if !isErr || !strings.Contains(out, "stale") {
		t.Fatalf("stale write was allowed: %q", out)
	}
	if got := mustCall(t, cs, "read", map[string]any{"path": p}); !strings.Contains(got, "someone else") {
		t.Fatalf("the refused write landed anyway: %s", got)
	}
	// No sha is still an unconditional overwrite: creating a file must not
	// require reading one that does not exist.
	mustCall(t, cs, "write", map[string]any{"path": p, "content": "mine\n"})
}

// The decorated read is lossy — line numbers to strip, and a file with and
// without a trailing newline render identically. Copying a file through it
// invented a newline. raw must round-trip byte-exactly.
func TestMCPRawReadRoundTripsExactly(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	root := "/" + f.wiki.ID

	for i, content := range []string{"alpha\nbeta", "alpha\nbeta\n", "", "\n", "  trailing  \n\n"} {
		src := fmt.Sprintf("%s/raw/%d.txt", root, i)
		dst := fmt.Sprintf("%s/copy/%d.txt", root, i)
		mustCall(t, cs, "write", map[string]any{"path": src, "content": content})

		raw := mustCall(t, cs, "read", map[string]any{"path": src, "raw": true})
		if raw != content {
			t.Errorf("raw read of %q = %q", content, raw)
		}
		// The copy an agent would make, and the shas must agree.
		mustCall(t, cs, "write", map[string]any{"path": dst, "content": raw})
		srcSHA := storedSHA(t, f, fmt.Sprintf("raw/%d.txt", i))
		dstSHA := storedSHA(t, f, fmt.Sprintf("copy/%d.txt", i))
		if srcSHA != dstSHA {
			t.Errorf("copy of %q changed the content: %s vs %s", content, short(srcSHA), short(dstSHA))
		}
	}
}

func storedSHA(t *testing.T, f *mcpFixture, path string) string {
	t.Helper()
	rec := doAs(t, f.h, "GET", "/api/p/"+f.wiki.ID+"/history?path="+url.QueryEscape(path), nil, f.cookies["alice"])
	var resp struct {
		Entries []HistoryEntry `json:"entries"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Entries) == 0 {
		t.Fatalf("no history for %s", path)
	}
	return resp.Entries[0].Blob
}

// A malformed glob reported "no files match", which an agent acts on as "this
// project has none". grep already errors; glob must too.
func TestMCPGlobReportsBadPatterns(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	mustCall(t, cs, "write", map[string]any{"path": "/" + f.wiki.ID + "/a.md", "content": "x"})

	for _, pat := range []string{"[", "/**/*.md"} {
		out, isErr := callTool(t, cs, "glob", map[string]any{"pattern": pat})
		if !isErr {
			t.Errorf("glob(%q) = %q, want an error rather than a false negative", pat, out)
		}
	}
	// The corrected form of the leading-slash case still works.
	if out := mustCall(t, cs, "glob", map[string]any{"pattern": "**/*.md"}); !strings.Contains(out, "a.md") {
		t.Fatalf("valid glob broke: %s", out)
	}
}

// A trailing slash is load-bearing in delete, so write must not quietly drop
// it and leave a file and a folder sharing one name.
func TestMCPWriteRefusesFolderPath(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	if out, isErr := callTool(t, cs, "write", map[string]any{
		"path": "/" + f.wiki.ID + "/dir/", "content": "x",
	}); !isErr || !strings.Contains(out, "folder") {
		t.Fatalf("write accepted a folder path: %q", out)
	}
}

// Deleting a folder without the trailing slash used to answer "no such file"
// about a path the caller had just seen listed.
func TestMCPDeleteOfFolderExplainsTheSlash(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	root := "/" + f.wiki.ID
	mustCall(t, cs, "write", map[string]any{"path": root + "/scratch/a.md", "content": "x"})

	out, isErr := callTool(t, cs, "delete", map[string]any{"path": root + "/scratch"})
	if !isErr || !strings.Contains(out, "trailing slash") {
		t.Fatalf("delete of a folder = %q, want a pointer to the slash form", out)
	}
	mustCall(t, cs, "delete", map[string]any{"path": root + "/scratch/"})
}

// list stamps must carry their zone, or an agent relays the wrong day; and a
// delete row has no content, so it must not print an empty sha column.
func TestMCPTimestampsAndEmptyShaColumns(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	root := "/" + f.wiki.ID
	mustCall(t, cs, "write", map[string]any{"path": root + "/t.md", "content": "x"})

	if out := mustCall(t, cs, "list", map[string]any{"path": root}); !strings.Contains(out, "Z\t") {
		t.Fatalf("list timestamps carry no timezone: %s", out)
	}
	mustCall(t, cs, "delete", map[string]any{"path": root + "/t.md"})
	hist := mustCall(t, cs, "history", map[string]any{"path": root + "/t.md"})
	if strings.Contains(hist, "sha:\n") || strings.HasSuffix(strings.TrimRight(hist, "\n"), "sha:") {
		t.Fatalf("history printed a dangling empty sha: %q", hist)
	}
}

// raw exists so a file can be copied byte-for-byte. On binary it must REFUSE:
// falling through to the "is a binary file" description handed the agent a
// sentence it would then write as the file's content.
func TestMCPRawReadRefusesBinary(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	rec := doAs(t, f.h, "PUT", "/api/p/"+f.wiki.ID+"/upload/content?path=blob.dat",
		[]byte{0x00, 0x01, 0x02, 0xff, 0x00}, f.cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	out, isErr := callTool(t, cs, "read", map[string]any{
		"path": "/" + f.wiki.ID + "/blob.dat", "raw": true,
	})
	if !isErr {
		t.Fatalf("raw read of binary returned content an agent would copy: %q", out)
	}
	if !strings.Contains(out, "binary") {
		t.Fatalf("error does not explain why: %q", out)
	}
}

// ---- regressions from validation round 2 ----

// The worst bug this door has had: read paged a 1 MiB PREFIX and reported the
// prefix's line count as the file's. grep would quote line 28500 of a file
// read called 11072 lines long, and "offset is past the end" was
// indistinguishable from real EOF — so an agent summarising an export drew
// conclusions from its first 40% and reported success.
func TestMCPReadPagesTheWholeFileNotAPrefix(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	root := "/" + f.wiki.ID

	// Comfortably over the 1 MiB byte cap: 40k lines of ~40 bytes.
	var big strings.Builder
	const lines = 40000
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(&big, "line %06d padding padding padding\n", i)
	}
	if big.Len() <= maxReadBytes {
		t.Fatalf("fixture is only %d bytes; it must exceed the %d-byte cap", big.Len(), maxReadBytes)
	}
	rec := doAs(t, f.h, "PUT", "/api/p/"+f.wiki.ID+"/upload/content?path=big.log",
		[]byte(big.String()), f.cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}

	// The header must report the REAL total.
	head := mustCall(t, cs, "read", map[string]any{"path": root + "/big.log"})
	if !strings.Contains(head, fmt.Sprintf("%d lines", lines)) {
		t.Fatalf("read reports the wrong length (want %d lines):\n%s", lines, firstLine(head))
	}

	// A line deep past the byte cap must be reachable, and be the right line.
	deep := mustCall(t, cs, "read", map[string]any{"path": root + "/big.log", "offset": 39998, "limit": 3})
	if !strings.Contains(deep, "line 039999") {
		t.Fatalf("a line past the byte cap is unreachable:\n%s", deep)
	}
	if strings.Contains(deep, "past the end") {
		t.Fatalf("read claimed EOF for a line that exists:\n%s", deep)
	}

	// grep and read must agree about the same file.
	g := mustCall(t, cs, "grep", map[string]any{"pattern": "line 039999"})
	if !strings.Contains(g, "big.log") {
		t.Fatalf("grep cannot find what read can: %s", g)
	}

	// Exactly one continuation hint, and the offset it names must work.
	if n := strings.Count(head, "read again with offset="); n != 1 {
		t.Fatalf("want exactly 1 continuation hint, got %d:\n%s", n, tailLines(head, 4))
	}
	next := offsetFrom(t, head)
	cont := mustCall(t, cs, "read", map[string]any{"path": root + "/big.log", "offset": next, "limit": 2})
	if strings.Contains(cont, "past the end") {
		t.Fatalf("the offset read handed out was rejected by read: %s", cont)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func offsetFrom(t *testing.T, out string) int {
	t.Helper()
	i := strings.Index(out, "offset=")
	if i < 0 {
		t.Fatalf("no offset hint in: %s", tailLines(out, 3))
	}
	n := 0
	for _, c := range out[i+7:] {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// ** must mean "any number of levels", anywhere in the pattern. It collapsed
// to a single level, so `glob("tmp/**/*.log")` returned 1 of 7 files — the
// agent deleted it and reported the cleanup done.
func TestMCPGlobDoubleStarIsRecursive(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	root := "/" + f.wiki.ID
	for _, p := range []string{"g/top.log", "g/one/a.log", "g/one/two/b.log", "g/one/two/three/c.log"} {
		mustCall(t, cs, "write", map[string]any{"path": root + "/" + p, "content": "x\n"})
	}

	all := mustCall(t, cs, "glob", map[string]any{"pattern": "g/**/*.log"})
	for _, want := range []string{"a.log", "b.log", "c.log"} {
		if !strings.Contains(all, want) {
			t.Errorf("g/**/*.log missed %s:\n%s", want, all)
		}
	}
	// A single star must still mean a single level, or ** means nothing.
	one := mustCall(t, cs, "glob", map[string]any{"pattern": "g/*/*.log"})
	if strings.Contains(one, "b.log") {
		t.Errorf("g/*/*.log matched two levels down:\n%s", one)
	}
	// Patterns are relative to the search path, as the tool's error says.
	scoped := mustCall(t, cs, "glob", map[string]any{"pattern": "one/**/*.log", "path": root + "/g"})
	if !strings.Contains(scoped, "b.log") {
		t.Errorf("a pattern relative to path matched nothing:\n%s", scoped)
	}
}

// grep's glob FILTER got none of the validation the glob tool got, so a
// malformed filter searched zero files and answered "No matches" — an agent
// concludes the symbol is unused.
func TestMCPGrepValidatesItsGlobFilter(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	root := "/" + f.wiki.ID
	mustCall(t, cs, "write", map[string]any{"path": root + "/a/x.log", "content": "panic here\n"})

	for _, bad := range []string{"[", "/**/*.log"} {
		out, isErr := callTool(t, cs, "grep", map[string]any{"pattern": "panic", "glob": bad})
		if !isErr {
			t.Errorf("grep accepted a malformed glob %q and answered: %s", bad, out)
		}
	}
	// And a valid recursive filter finds the file.
	if out := mustCall(t, cs, "grep", map[string]any{"pattern": "panic", "glob": "**/*.log"}); !strings.Contains(out, "x.log") {
		t.Fatalf("valid glob filter found nothing: %s", out)
	}
}

// move refused a file collision but happily created a file shadowing a folder.
// `move notes.md docs` means "into docs/" to whoever typed it.
func TestMCPMoveRefusesToShadowAFolder(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	root := "/" + f.wiki.ID
	mustCall(t, cs, "write", map[string]any{"path": root + "/docs/readme.md", "content": "x\n"})
	mustCall(t, cs, "write", map[string]any{"path": root + "/notes.md", "content": "y\n"})

	out, isErr := callTool(t, cs, "move", map[string]any{"from": root + "/notes.md", "to": root + "/docs"})
	if !isErr {
		t.Fatalf("move created a file shadowing a folder: %s", out)
	}
	if !strings.Contains(out, "folder") {
		t.Fatalf("error does not explain the collision: %s", out)
	}
}

// read used to answer "no such file" for a folder, and to decide a file was an
// image purely from its extension.
func TestMCPReadFolderAndMisnamedImage(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	root := "/" + f.wiki.ID
	mustCall(t, cs, "write", map[string]any{"path": root + "/app/main.go", "content": "package main\n"})
	mustCall(t, cs, "write", map[string]any{"path": root + "/fake.png", "content": "not really a png\n"})

	out, isErr := callTool(t, cs, "read", map[string]any{"path": root + "/app"})
	if !isErr || !strings.Contains(out, "folder") {
		t.Fatalf("read of a folder = %q, want a pointer to list", out)
	}
	// A text file named .png is text, and raw must work on it.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "read", Arguments: map[string]any{"path": root + "/fake.png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, isImage := res.Content[0].(*mcp.ImageContent); isImage {
		t.Fatal("a text file named .png was returned as an image")
	}
	if got := mustCall(t, cs, "read", map[string]any{"path": root + "/fake.png", "raw": true}); got != "not really a png\n" {
		t.Fatalf("raw read of a mis-named file = %q", got)
	}
}

// An empty file has zero lines, not one blank one.
func TestMCPReadEmptyFileSaysEmpty(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	root := "/" + f.wiki.ID
	mustCall(t, cs, "write", map[string]any{"path": root + "/empty.txt", "content": ""})
	if out := mustCall(t, cs, "read", map[string]any{"path": root + "/empty.txt"}); !strings.Contains(out, "empty") {
		t.Fatalf("read of an empty file = %q, want it to say so", out)
	}
}

// Deleting a whole project looks exactly like the folder form that works.
func TestMCPDeleteWholeProjectIsRefusedClearly(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	out, isErr := callTool(t, cs, "delete", map[string]any{"path": "/" + f.wiki.ID + "/"})
	if !isErr || !strings.Contains(out, "whole project") {
		t.Fatalf("delete of a project = %q, want a specific refusal", out)
	}
}

// The compare-and-swap has to be atomic, not advisory. Eight concurrent
// writers passing one valid sha were ALL accepted and seven updates lost —
// which is precisely the guarantee the parameter advertises.
func TestMCPCompareAndSwapIsAtomic(t *testing.T) {
	f := newMCPHub(t)
	token := f.connect("alice", f.wiki.ID)
	root := "/" + f.wiki.ID

	seed := f.session(token)
	mustCall(t, seed, "write", map[string]any{"path": root + "/race.md", "content": "base\n"})
	sha := shaFrom(t, mustCall(t, seed, "read", map[string]any{"path": root + "/race.md"}))

	// Independent sessions, started together, all claiming the same base sha.
	const writers = 8
	var (
		wg       sync.WaitGroup
		start    = make(chan struct{})
		mu       sync.Mutex
		accepted int
	)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cs := f.session(token)
			<-start
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "write",
				Arguments: map[string]any{
					"path": root + "/race.md", "content": fmt.Sprintf("writer %d\n", i), "sha": sha,
				},
			})
			if err != nil {
				return
			}
			if !res.IsError {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	// Exactly one writer may win: after the first, the sha has moved.
	if accepted != 1 {
		t.Fatalf("%d of %d concurrent writers were accepted against one sha; want 1 "+
			"(the rest silently lost their update)", accepted, writers)
	}
}

// A path may name its project by NAME, not just by 36 characters of UUID.
// Every path an agent writes, quotes, or pastes back carried the id; names
// make a transcript readable and a transposition unlikely.
func TestMCPPathsAcceptProjectNames(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))

	// f.wiki is named "wiki" by permHubAt.
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/by-name.md", "content": "hello\n"})
	// ...and the id still resolves to the same file.
	if out := mustCall(t, cs, "read", map[string]any{"path": "/" + f.wiki.ID + "/by-name.md"}); !strings.Contains(out, "hello") {
		t.Fatalf("name and id disagree: %s", out)
	}
	mustCall(t, cs, "list", map[string]any{"path": "/wiki"})
	mustCall(t, cs, "history", map[string]any{"path": "/wiki/by-name.md"})
	if out := mustCall(t, cs, "grep", map[string]any{"pattern": "hello", "path": "/wiki"}); !strings.Contains(out, "by-name.md") {
		t.Fatalf("grep scoped by name found nothing: %s", out)
	}
	mustCall(t, cs, "move", map[string]any{"from": "/wiki/by-name.md", "to": "/wiki/renamed.md"})

	// An unknown name is refused the same way an unknown id is — the error
	// must not distinguish them, or it becomes an existence oracle.
	out, isErr := callTool(t, cs, "read", map[string]any{"path": "/nosuchproject/x.md"})
	if !isErr || !strings.Contains(out, "no such project") {
		t.Fatalf("unknown project name = %q", out)
	}
}

// Paths an agent reads out of a result must be as quotable as the ones it
// writes in. Accepting names on input while emitting UUIDs on output solves
// half the problem: every grep hit, every write confirmation, every error
// still opened with 36 characters the agent has to carry forward.
func TestMCPOutputPathsUseProjectNames(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))

	out := mustCall(t, cs, "write", map[string]any{"path": "/wiki/notes/a.md", "content": "needle\n"})
	if strings.Contains(withoutURLs(out), f.wiki.ID) {
		t.Errorf("write confirmation names the project by id: %s", out)
	}
	if !strings.Contains(out, "/wiki/notes/a.md") {
		t.Errorf("write confirmation does not name the readable path: %s", out)
	}
	for _, tc := range []struct{ tool, arg string }{{"grep", "needle"}, {"glob", "**/*.md"}} {
		args := map[string]any{"pattern": tc.arg}
		got := mustCall(t, cs, tc.tool, args)
		if strings.Contains(withoutURLs(got), f.wiki.ID) {
			t.Errorf("%s output names the project by id:\n%s", tc.tool, got)
		}
		if !strings.Contains(got, "/wiki/") {
			t.Errorf("%s output does not use the project name:\n%s", tc.tool, got)
		}
	}
	// And a path quoted back from that output is accepted.
	mustCall(t, cs, "read", map[string]any{"path": "/wiki/notes/a.md"})
}

// withoutURLs strips the hub links tool output carries, so an assertion about
// the PATHS it prints is not fooled by the project id inside a link. Links are
// id-based deliberately — see fileURL.
var urlRe = regexp.MustCompile(`https?://\S+`)

func withoutURLs(s string) string { return urlRe.ReplaceAllString(s, "") }

// An answer that names a file should be able to link it, so every place these
// tools name one they print its hub page beside the path.
//
// Emitted, not left to the agent to compose. The two things a formula gets
// wrong are both checked here: the link carries the project ID even though the
// path column carries its NAME (the viewer resolves a name only when it is
// unique among the READER's projects, which is a different set), and every
// path segment is percent-encoded.
func TestMCPOutputCarriesHubLinks(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))

	// A name with the two characters that must not reach a URL raw: a space,
	// and the "#" that would otherwise cut the path off at a fragment.
	const rest = "notes/road map #2.md"
	proj := f.ts.URL + "/" + f.wiki.ID
	want := proj + "/notes/road%20map%20%232.md"

	out := mustCall(t, cs, "write", map[string]any{"path": "/wiki/" + rest, "content": "needle\n"})
	if !strings.Contains(out, "url: "+want+"\n") {
		t.Errorf("write has no link (want %s):\n%s", want, out)
	}
	if out := mustCall(t, cs, "read", map[string]any{"path": "/wiki/" + rest}); !strings.Contains(out, "url: "+want+"\n") {
		t.Errorf("read has no link (want %s):\n%s", want, out)
	}
	if out := mustCall(t, cs, "history", map[string]any{"path": "/wiki/" + rest}); !strings.Contains(out, "url: "+want+"\n") {
		t.Errorf("history has no link (want %s):\n%s", want, out)
	}

	// One row per file: the link is the last column, so every column that was
	// there before keeps its position.
	if out := mustCall(t, cs, "list", map[string]any{"path": "/wiki/notes"}); !strings.Contains(out, "\t"+want+"\n") {
		t.Errorf("list row has no link (want %s):\n%s", want, out)
	}
	if out := mustCall(t, cs, "list", map[string]any{"path": "/wiki"}); !strings.Contains(out, "notes/\t"+proj+"/notes\n") {
		t.Errorf("folder row has no link:\n%s", out)
	}
	if out := mustCall(t, cs, "glob", map[string]any{"pattern": "**/*.md"}); !strings.Contains(out, "\t"+want+"\n") {
		t.Errorf("glob row has no link (want %s):\n%s", want, out)
	}
	if out := mustCall(t, cs, "grep", map[string]any{"pattern": "needle", "files_only": true}); !strings.Contains(out, "\t"+want+"\n") {
		t.Errorf("files_only grep row has no link (want %s):\n%s", want, out)
	}

	// A move links the destination — the source is gone, and a link to it 404s.
	out = mustCall(t, cs, "move", map[string]any{"from": "/wiki/" + rest, "to": "/wiki/notes/plan.md"})
	if !strings.Contains(out, "url: "+proj+"/notes/plan.md\n") {
		t.Errorf("move does not link the destination:\n%s", out)
	}
}

// Reading a past version must link THOSE bytes. The live page is a different
// file by then, and nothing in a bare link says which one the answer was
// written from.
func TestMCPReadOfOldVersionLinksThatVersion(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/v.md", "content": "first\n"})
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/v.md", "content": "second\n"})

	hist := mustCall(t, cs, "history", map[string]any{"path": "/wiki/v.md"})
	shas := regexp.MustCompile(`sha:([0-9a-f]+)`).FindAllStringSubmatch(hist, -1)
	if len(shas) < 2 {
		t.Fatalf("want two versions:\n%s", hist)
	}
	old := shas[len(shas)-1][1]

	out := mustCall(t, cs, "read", map[string]any{"path": "/wiki/v.md", "sha": old})
	if !strings.Contains(out, "first") {
		t.Fatalf("did not read the old version:\n%s", out)
	}
	if !strings.Contains(out, "url: "+f.ts.URL+"/"+f.wiki.ID+"/v.md?v="+old) {
		t.Errorf("link is not pinned to the version read (sha %s):\n%s", old, out)
	}
}

// A file with one enormous line used to be unreachable by EVERY tool at once —
// paged read refused it, raw refused it, grep called it unsearchable, and the
// three errors pointed at each other. It must now be readable, with the cut
// reported so a partial view is never mistaken for the whole file.
func TestMCPReadOfAnUnpageableFile(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	// A realistic shape: ordinary lines with one dumped payload among them.
	body := "first\n" + strings.Repeat("x", maxLineShown*3) + "\nlast\n"
	rec := doAs(t, f.h, "PUT", "/api/p/"+f.wiki.ID+"/upload/content?path=bundle.js",
		[]byte(body), f.cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	out := mustCall(t, cs, "read", map[string]any{"path": "/wiki/bundle.js"})
	if !strings.Contains(out, "3 lines") {
		t.Fatalf("read did not page the file: %s", firstLine(out))
	}
	// The ordinary lines around the huge one are reachable...
	if !strings.Contains(out, "first") || !strings.Contains(out, "last") {
		t.Fatalf("ordinary lines unreachable beside a long one:\n%s", out)
	}
	// ...and the truncation is stated, not silent.
	if !strings.Contains(out, "truncated") {
		t.Fatalf("a cut line was not reported:\n%s", out)
	}
}

// ---- regressions from validation round 3 ----

// The root listing must only ever print an address that resolves back to the
// row it is on. A project whose NAME is another project's ID is unaddressable
// by name — ids win, as they must — and the listing advertised that name
// anyway, so an agent faithfully following the row wrote into a DIFFERENT
// project and the write reported success.
func TestMCPListingNeverAdvertisesAWrongAddress(t *testing.T) {
	f := newMCPHub(t)
	// A second project literally named after the first one's id.
	shadow := f.secondProject(f.wiki.ID)
	cs := f.session(f.connect("alice", f.wiki.ID, shadow.ID))

	out := mustCall(t, cs, "list", map[string]any{"path": "/"})
	rows := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "/") {
			continue
		}
		rows++
		addr := strings.Trim(strings.SplitN(line, "\t", 2)[0], "/")
		// Every row names its id: as the address itself, or in "(id …)".
		id := addr
		if i := strings.Index(line, "(id "); i >= 0 {
			id = strings.TrimSuffix(line[i+4:], ")")
			id = strings.TrimSpace(id)
		}
		probe := "probe-" + id[:8] + ".txt"
		mustCall(t, cs, "write", map[string]any{"path": "/" + addr + "/" + probe, "content": "x"})

		// The write must have landed in the project the row named, and in no
		// other connected project.
		if !existsIn(t, f, id, probe) {
			t.Errorf("row %q addressed a project other than the id it names", line)
		}
		for _, other := range []string{f.wiki.ID, shadow.ID} {
			if other != id && existsIn(t, f, other, probe) {
				t.Errorf("row %q wrote into project %s as well", line, other)
			}
		}
	}
	if rows != 2 {
		t.Fatalf("listed %d rows, want 2:\n%s", rows, out)
	}
}

// existsIn reports whether a path exists in a project, via the hub's own API.
func existsIn(t *testing.T, f *mcpFixture, projectID, path string) bool {
	t.Helper()
	rec := doAs(t, f.h, "GET", "/api/p/"+projectID+"/history?path="+url.QueryEscape(path), nil, f.cookies["alice"])
	if rec.Code != 200 {
		return false
	}
	var resp struct {
		Entries []HistoryEntry `json:"entries"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	return len(resp.Entries) > 0
}

// These paths sync to real filesystems, where a file and a folder cannot share
// a name. write accepted it in both directions, producing a project no
// teammate's device can materialize while MCP reported everything green.
func TestMCPWriteRefusesFileFolderCollision(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	root := "/" + f.wiki.ID

	mustCall(t, cs, "write", map[string]any{"path": root + "/dir/child.txt", "content": "x\n"})
	// A file where a folder already is.
	if out, isErr := callTool(t, cs, "write", map[string]any{"path": root + "/dir", "content": "clobber"}); !isErr {
		t.Errorf("write created a file shadowing a folder: %s", out)
	}
	// And a folder under something that is already a file.
	mustCall(t, cs, "write", map[string]any{"path": root + "/note.md", "content": "y\n"})
	if out, isErr := callTool(t, cs, "write", map[string]any{"path": root + "/note.md/child.md", "content": "z"}); !isErr {
		t.Errorf("write nested a file under a file: %s", out)
	}
	// The original file is untouched by either refusal.
	if got := mustCall(t, cs, "read", map[string]any{"path": root + "/dir/child.txt"}); !strings.Contains(got, "x") {
		t.Fatalf("collision check disturbed the tree: %s", got)
	}
}

// A search tool must never let a long line turn into a silent "not found".
// grep now searches far into such a line and, when it still has to cut one,
// says so — naming the file, so "no matches" is never read as "not here".
func TestMCPGrepNeverSilentlySkipsAFile(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))

	// The needle sits beyond even grep's generous per-line window.
	huge := strings.Repeat("var a=1;", (maxGrepLine/8)+200) + "NEEDLE"
	rec := doAs(t, f.h, "PUT", "/api/p/"+f.wiki.ID+"/upload/content?path=min.js",
		[]byte(huge), f.cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	out := mustCall(t, cs, "grep", map[string]any{"pattern": "NEEDLE"})
	if !strings.Contains(out, "longer than") || !strings.Contains(out, "min.js") {
		t.Fatalf("grep hid that it could not search a whole line:\n%s", out)
	}

	// And a needle INSIDE the searched window is found, not skipped with it.
	rec = doAs(t, f.h, "PUT", "/api/p/"+f.wiki.ID+"/upload/content?path=early.js",
		[]byte("EARLYNEEDLE"+strings.Repeat("y", maxGrepLine*2)), f.cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	if got := mustCall(t, cs, "grep", map[string]any{"pattern": "EARLYNEEDLE"}); !strings.Contains(got, "early.js") {
		t.Fatalf("grep missed a match inside a long line:\n%s", got)
	}
}

// The binary notice has to ride on the NO-MATCH answer too — that is exactly
// when it decides whether "not found" is true.
func TestMCPGrepReportsBinarySkipsOnNoMatch(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	rec := doAs(t, f.h, "PUT", "/api/p/"+f.wiki.ID+"/upload/content?path=blob.bin",
		append([]byte{0, 1, 2}, []byte("SECRETNEEDLE")...), f.cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	out := mustCall(t, cs, "grep", map[string]any{"pattern": "SECRETNEEDLE"})
	if !strings.Contains(out, "skipped") || !strings.Contains(out, "blob.bin") {
		t.Fatalf("no-match answer hides the binary it skipped:\n%s", out)
	}
}

// "How many occurrences of X?" answered 200 for a file with 40,000, because
// the header reported the DISPLAY cap as the total.
func TestMCPGrepHeaderReportsTheTrueTotal(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	var big strings.Builder
	const hits = maxGrepMatches * 3
	for i := 0; i < hits; i++ {
		fmt.Fprintf(&big, "hit %d NEEDLE\n", i)
	}
	rec := doAs(t, f.h, "PUT", "/api/p/"+f.wiki.ID+"/upload/content?path=many.log",
		[]byte(big.String()), f.cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	out := mustCall(t, cs, "grep", map[string]any{"pattern": "NEEDLE"})
	if !strings.Contains(out, fmt.Sprintf("%d match(es)", hits)) {
		t.Fatalf("header does not report the true total (%d):\n%s", hits, firstLine(out))
	}
	if !strings.Contains(out, "showing the first") {
		t.Fatalf("header does not say the body is capped:\n%s", firstLine(out))
	}
}

// grep -C, the affordance every round asked for: it turns "find the panic and
// show me around it" from two calls into one.
func TestMCPGrepContextLines(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	mustCall(t, cs, "write", map[string]any{
		"path": "/wiki/app.log", "content": "one\ntwo\nBOOM\nfour\nfive\n",
	})
	out := mustCall(t, cs, "grep", map[string]any{"pattern": "BOOM", "context": 1})
	if !strings.Contains(out, "two") || !strings.Contains(out, "four") {
		t.Fatalf("context lines missing:\n%s", out)
	}
	// A match is marked with ':' and context with '-', the way grep does it.
	if !strings.Contains(out, ":3:BOOM") || !strings.Contains(out, "-2-two") {
		t.Fatalf("matches and context are not distinguishable:\n%s", out)
	}
}

// Brace expansion is a top-three agent glob idiom that path.Match cannot do.
// Matching nothing is a false negative; say so instead.
func TestMCPGlobRejectsBraceExpansion(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/a.go", "content": "x"})
	out, isErr := callTool(t, cs, "glob", map[string]any{"pattern": "**/*.{go,md}"})
	if !isErr || !strings.Contains(out, "brace") {
		t.Fatalf("brace glob = %q, want an explanatory error", out)
	}
}

// Small message fixes that each cost the last round a wasted call.
func TestMCPErrorMessagesPointSomewhere(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	root := "/" + f.wiki.ID

	// An empty project is a normal state, not a typo.
	if out := mustCall(t, cs, "list", map[string]any{"path": root}); !strings.Contains(out, "empty") {
		t.Errorf("empty project reads like an error: %q", out)
	}
	mustCall(t, cs, "write", map[string]any{"path": root + "/src/a.go", "content": "package a\n"})

	// edit on a folder gets read's good message.
	if out, isErr := callTool(t, cs, "edit", map[string]any{
		"path": root + "/src", "old_string": "a", "new_string": "b",
	}); !isErr || !strings.Contains(out, "folder") {
		t.Errorf("edit of a folder = %q", out)
	}
	// A no-op edit must not burn a version.
	before := mustCall(t, cs, "read", map[string]any{"path": root + "/src/a.go"})
	if out := mustCall(t, cs, "edit", map[string]any{
		"path": root + "/src/a.go", "old_string": "package a", "new_string": "package a",
	}); !strings.Contains(out, "nothing to do") {
		t.Errorf("no-op edit = %q", out)
	}
	if after := mustCall(t, cs, "read", map[string]any{"path": root + "/src/a.go"}); after != before {
		t.Errorf("a no-op edit changed the file's version")
	}
	// restore on a file that never existed must not send the agent to history.
	if out, isErr := callTool(t, cs, "restore", map[string]any{
		"path": root + "/never.md", "sha": "abcdef123456",
	}); !isErr || !strings.Contains(out, "no such file") {
		t.Errorf("restore of a missing file = %q", out)
	}
}

// ---- regressions from validation round 4 ----

// grep context windows that overlap used to re-emit lines already printed, and
// a large context could emit them out of order — so anything reconstructing a
// file view from grep output got a corrupted, longer-than-real file.
func TestMCPGrepContextDoesNotDuplicateLines(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	mustCall(t, cs, "write", map[string]any{
		"path": "/wiki/near.txt", "content": "l1\nl2\nHIT\nl4\nHIT\nl6\n",
	})
	out := mustCall(t, cs, "grep", map[string]any{"pattern": "HIT", "context": 2})

	seen := map[int]bool{}
	last := 0
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if !strings.Contains(line, "near.txt") {
			continue
		}
		n := lineNoOf(t, line)
		if seen[n] {
			t.Errorf("line %d emitted twice:\n%s", n, out)
		}
		if n < last {
			t.Errorf("line numbers run backwards (%d after %d):\n%s", n, last, out)
		}
		seen[n], last = true, n
	}
}

// lineNoOf pulls the line number out of a grep row, which is "path:N:text" for
// a match and "path-N-text" for context.
func lineNoOf(t *testing.T, row string) int {
	t.Helper()
	i := strings.LastIndex(row, "near.txt")
	rest := row[i+len("near.txt")+1:]
	n := 0
	for _, c := range rest {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		t.Fatalf("no line number in %q", row)
	}
	return n
}

// A search scoped to a folder that does not exist answered "no matches", which
// an agent reads as a clean negative — the same silent-wrong-answer family as
// a skipped file. A typo'd directory must be an error.
func TestMCPSearchOfMissingFolderIsAnError(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/real/a.txt", "content": "NEEDLE\n"})

	for _, tool := range []string{"grep", "glob"} {
		args := map[string]any{"path": "/wiki/prrobe"}
		if tool == "grep" {
			args["pattern"] = "NEEDLE"
		} else {
			args["pattern"] = "*.txt"
		}
		out, isErr := callTool(t, cs, tool, args)
		if !isErr || !strings.Contains(out, "no such folder") {
			t.Errorf("%s on a missing folder = %q, want an error", tool, out)
		}
	}
	// The real folder still works, so the check is not just refusing everything.
	if out := mustCall(t, cs, "grep", map[string]any{"pattern": "NEEDLE", "path": "/wiki/real"}); !strings.Contains(out, "a.txt") {
		t.Fatalf("scoped search broke: %s", out)
	}
}

// Inspecting a past version used to require restoring it, reading, and
// restoring back — two writes and two new versions just to look at one.
func TestMCPReadAPastVersionWithoutRestoring(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/v.txt", "content": "first\n"})
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/v.txt", "content": "second\n"})

	hist := mustCall(t, cs, "history", map[string]any{"path": "/wiki/v.txt"})
	var oldSHA string
	for _, line := range strings.Split(hist, "\n") {
		if i := strings.Index(line, "sha:"); i >= 0 {
			oldSHA = strings.TrimSpace(line[i+4:])
		}
	}
	if oldSHA == "" {
		t.Fatalf("no sha in history: %s", hist)
	}
	got := mustCall(t, cs, "read", map[string]any{"path": "/wiki/v.txt", "sha": oldSHA, "raw": true})
	if got != "first\n" {
		t.Fatalf("read by sha = %q, want the first version", got)
	}
	// And the file itself is untouched — no version burned to look.
	if now := mustCall(t, cs, "read", map[string]any{"path": "/wiki/v.txt", "raw": true}); now != "second\n" {
		t.Fatalf("reading a past version changed the file: %q", now)
	}
	if after := mustCall(t, cs, "history", map[string]any{"path": "/wiki/v.txt"}); after != hist {
		t.Fatalf("reading a past version added a version:\n%s", after)
	}
}

// A sha that is not a sha is a caller mistake, not a concurrent edit. Calling
// it "stale" sent the agent into a re-read/retry loop that never converged.
func TestMCPMalformedSHAIsNotReportedAsStale(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/s.txt", "content": "x\n"})

	out, isErr := callTool(t, cs, "write", map[string]any{
		"path": "/wiki/s.txt", "content": "y\n", "sha": "notasha",
	})
	if !isErr {
		t.Fatalf("a malformed sha was accepted: %s", out)
	}
	if strings.Contains(out, "stale") || !strings.Contains(out, "not a sha") {
		t.Fatalf("malformed sha reported as staleness: %q", out)
	}
}

// A name no filesystem can hold must not be journaled: the product's whole job
// is materializing these paths on real disks.
func TestMCPWriteRefusesOversizedNameComponent(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	long := strings.Repeat("z", 300) + ".txt"
	out, isErr := callTool(t, cs, "write", map[string]any{"path": "/wiki/" + long, "content": "x"})
	if !isErr || !strings.Contains(out, "filesystems") {
		t.Fatalf("a 300-byte filename was accepted: %q", out)
	}
}

// An agent should learn a project is read-only from the listing, not from a
// refusal after it has composed a large edit.
func TestMCPListingMarksReadOnlyProjects(t *testing.T) {
	f := newMCPHub(t)
	rec := doAs(t, f.h, "PUT", "/api/p/"+f.wiki.ID+"/permissions/bob@x.io",
		map[string]string{"level": PermRead}, f.cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("set read-only: %d %s", rec.Code, rec.Body)
	}
	cs := f.session(f.connect("bob", f.wiki.ID))
	if out := mustCall(t, cs, "list", map[string]any{"path": "/"}); !strings.Contains(out, "read-only") {
		t.Fatalf("listing does not mark a read-only project:\n%s", out)
	}
}

// ---- regressions from validation round 5 ----

// A folder rule refusing a write used to answer "you have read-only access to
// this project" — false when the caller has write on the project, and it sends
// them to fix permissions they already hold.
func TestMCPFolderDenialSaysFolderNotProject(t *testing.T) {
	f := newMCPHub(t)
	for _, p := range []string{"public/ok.md", "hr/secret.md"} {
		rec := doAs(t, f.h, "PUT", "/api/p/"+f.wiki.ID+"/upload/content?path="+p,
			[]byte("x"), f.cookies["alice"])
		if rec.Code != 200 {
			t.Fatalf("seed %s: %d", p, rec.Code)
		}
	}
	// bob keeps write on the project; only hr/ is closed to him.
	rec := doAs(t, f.h, "PUT", "/api/p/"+f.wiki.ID+"/folders",
		map[string]any{"prefix": "hr/", "default": PermRead}, f.cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("folder rule: %d %s", rec.Code, rec.Body)
	}
	cs := f.session(f.connect("bob", f.wiki.ID))

	// He can write elsewhere in the project...
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/public/proof.md", "content": "y"})
	// ...and the refusal under hr/ must not blame the project.
	out, isErr := callTool(t, cs, "write", map[string]any{"path": "/wiki/hr/proof.md", "content": "y"})
	if !isErr {
		t.Fatalf("wrote into a restricted folder: %s", out)
	}
	if strings.Contains(out, "read-only access to this project") {
		t.Fatalf("folder refusal blames the project: %q", out)
	}
	if !strings.Contains(out, "hr/") {
		t.Fatalf("folder refusal does not name the folder: %q", out)
	}
}

// "Stale" asserts the file changed and says to re-read and retry. For a sha
// that names no version that is false AND unrecoverable — the retry reads,
// gets the same current sha, and fails identically forever.
func TestMCPUnknownSHAIsNotReportedAsStale(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/v.txt", "content": "one\n"})

	for _, tool := range []string{"write", "edit"} {
		args := map[string]any{"path": "/wiki/v.txt", "sha": "deadbeefcafe"}
		if tool == "write" {
			args["content"] = "two\n"
		} else {
			args["old_string"], args["new_string"] = "one", "two"
		}
		out, isErr := callTool(t, cs, tool, args)
		if !isErr {
			t.Fatalf("%s accepted a sha naming no version: %s", tool, out)
		}
		if strings.Contains(out, "stale") {
			t.Errorf("%s called an unknown sha stale: %q", tool, out)
		}
		if !strings.Contains(out, "no version") {
			t.Errorf("%s does not say the sha names nothing: %q", tool, out)
		}
	}
	// A genuinely stale sha still reports staleness.
	sha := shaFrom(t, mustCall(t, cs, "read", map[string]any{"path": "/wiki/v.txt"}))
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/v.txt", "content": "moved on\n"})
	if out, _ := callTool(t, cs, "write", map[string]any{
		"path": "/wiki/v.txt", "content": "mine\n", "sha": sha,
	}); !strings.Contains(out, "stale") {
		t.Fatalf("a real stale sha no longer reports staleness: %q", out)
	}
}

// A grep hit whose matched token is clipped out of the displayed line reads as
// a false positive — the one thing a search result must never look like.
func TestMCPGrepKeepsTheMatchVisible(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	// The token sits well past the display window.
	line := "PREFIX " + strings.Repeat("a", 2000) + " SUFFIX_MARK"
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/long.txt", "content": line + "\n"})

	out := mustCall(t, cs, "grep", map[string]any{"pattern": "SUFFIX_MARK"})
	if !strings.Contains(out, "SUFFIX_MARK") {
		t.Fatalf("the matched token is not in the result, so the hit looks false:\n%s", out)
	}
	if !strings.Contains(out, "…") {
		t.Fatalf("the line was clipped without saying so:\n%s", out)
	}
}

// "No history for X" reads as a claim that X is unversioned. For a folder
// every file under it has history; for a typo the file does not exist.
func TestMCPHistoryValidatesItsPath(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/src/a.go", "content": "x\n"})

	if out, isErr := callTool(t, cs, "history", map[string]any{"path": "/wiki/src"}); !isErr ||
		!strings.Contains(out, "folder") {
		t.Errorf("history of a folder = %q", out)
	}
	if out, isErr := callTool(t, cs, "history", map[string]any{"path": "/wiki/nope.md"}); !isErr ||
		!strings.Contains(out, "no such file") {
		t.Errorf("history of a missing file = %q", out)
	}
	mustCall(t, cs, "history", map[string]any{"path": "/wiki/src/a.go"})
}

// A read-only caller must learn it cannot write BEFORE being coached on how to
// fix its old_string.
func TestMCPEditChecksPermissionBeforeDiagnosis(t *testing.T) {
	f := newMCPHub(t)
	rec := doAs(t, f.h, "PUT", "/api/p/"+f.wiki.ID+"/upload/content?path=dup.md",
		[]byte("x\nx\n"), f.cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("seed: %d", rec.Code)
	}
	rec = doAs(t, f.h, "PUT", "/api/p/"+f.wiki.ID+"/permissions/bob@x.io",
		map[string]string{"level": PermRead}, f.cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("read-only: %d %s", rec.Code, rec.Body)
	}
	cs := f.session(f.connect("bob", f.wiki.ID))

	out, isErr := callTool(t, cs, "edit", map[string]any{
		"path": "/wiki/dup.md", "old_string": "x", "new_string": "y",
	})
	if !isErr {
		t.Fatalf("a read-only caller edited: %s", out)
	}
	if strings.Contains(out, "replace_all") {
		t.Fatalf("coached a caller who cannot write at all: %q", out)
	}
}

// The consent screen must NOT pin form-action. It constrains every hop of the
// redirect chain a submit sets off, and the hub only knows the first: a client
// that registers an API host which forwards to its app host (the normal shape —
// Runbear goes api.runbear.io -> app.runbear.io) has the chain killed at a hop
// this page can never enumerate. Only a browser enforces CSP, so this asserts
// the header; the chain semantics themselves were confirmed in Chromium.
func TestMCPConsentCSPDoesNotPinFormAction(t *testing.T) {
	f := newMCPHub(t)
	csp := f.consentCSP(t, "https://api.example.com/oauth/callback")

	if strings.Contains(csp, "form-action") {
		t.Errorf("CSP pins form-action, which breaks any client whose callback "+
			"redirects onward:\n  %s", csp)
	}
	// The rest of the policy is still doing real work — no script is what makes
	// dropping form-action safe, since the form cannot be repointed without it.
	if !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("default-src 'none' must stay — it is what blocks script: %q", csp)
	}
}

// Where the code may be sent is enforced server-side, by exact match against
// the client's registered redirect_uris. With form-action gone this is the only
// control on the code's destination, so it gets a test of its own.
func TestMCPAuthorizeRejectsUnregisteredRedirect(t *testing.T) {
	f := newMCPHub(t)

	regBody, _ := json.Marshal(map[string]any{
		"client_name": "Test Agent", "redirect_uris": []string{"https://api.example.com/oauth/callback"},
	})
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest("POST", "/oauth/register", strings.NewReader(string(regBody))))
	var reg struct {
		ClientID string `json:"client_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &reg)

	for _, evil := range []string{
		"https://evil.example.com/cb",                 // another origin entirely
		"https://api.example.com.evil.com/cb",         // prefix that a sloppy match would pass
		"https://api.example.com/oauth/callback/../x", // same origin, different path
		"https://api.example.com/oauth/callback?x=1",  // registered path plus a query
	} {
		q := url.Values{}
		q.Set("client_id", reg.ClientID)
		q.Set("redirect_uri", evil)
		q.Set("response_type", "code")
		q.Set("code_challenge", "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM")
		q.Set("code_challenge_method", "S256")

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/oauth/authorize?"+q.Encode(), nil)
		req.AddCookie(f.cookies["alice"])
		f.h.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("redirect_uri %q: got %d, want 400 — an unregistered "+
				"destination must never reach the consent screen", evil, rec.Code)
		}
	}
}

// consentCSP renders the consent screen for a freshly registered client and
// returns its Content-Security-Policy.
func (f *mcpFixture) consentCSP(t *testing.T, redirect string) string {
	t.Helper()
	regBody, _ := json.Marshal(map[string]any{
		"client_name": "Test Agent", "redirect_uris": []string{redirect},
	})
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest("POST", "/oauth/register", strings.NewReader(string(regBody))))
	if rec.Code != 201 {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	var reg struct {
		ClientID string `json:"client_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &reg)

	q := url.Values{}
	q.Set("client_id", reg.ClientID)
	q.Set("redirect_uri", redirect)
	q.Set("response_type", "code")
	q.Set("code_challenge", "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM")
	q.Set("code_challenge_method", "S256")

	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/oauth/authorize?"+q.Encode(), nil)
	req.AddCookie(f.cookies["alice"])
	f.h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("consent page: %d %s", rec.Code, rec.Body)
	}
	return rec.Header().Get("Content-Security-Policy")
}

// The instructions must reach the client in the initialize result, not merely
// exist as a constant: the options argument to mcp.NewServer was nil before
// this, and passing nil again would drop them with nothing else changing.
func TestMCPInstructionsReachTheClient(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))

	got := cs.InitializeResult().Instructions
	if got == "" {
		t.Fatal("no instructions in the initialize result — ServerOptions dropped?")
	}
	// The two facts that cannot be recovered from the tool names and paths:
	// where the bytes live, and what to do when the project is synced locally.
	for _, want := range []string{"NOT the local filesystem", "/<project>/", "conflict copies"} {
		if !strings.Contains(got, want) {
			t.Errorf("instructions missing %q:\n%s", want, got)
		}
	}
}
