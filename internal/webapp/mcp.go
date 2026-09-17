package webapp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The MCP door: connected projects presented to an agent as one filesystem.
//
// There is no filesystem behind it. "Files" are the journal folded into a
// tree and blobs fetched on demand — the same thing the viewer has always
// shown, addressed as /<project-id>/<path> so that several projects share one
// root and an agent needs no new concepts to search across them.
//
// The design rule for this file: **tools are internal clients of the handlers
// the hub already has.** A write goes out as a PUT to the hub's own upload
// endpoint, a delete as a POST to /remove, and both travel through proj() →
// requirePermOn → projectPermOf on the way in. That is not indirection for
// its own sake: it is what makes folder permissions, quota accounting,
// read telemetry, blobs-before-journal, and every future gate apply here
// without a second copy that can drift. The only natively-implemented tools
// are the ones with no HTTP equivalent — list, glob, grep, read — and they
// compose the same snapshot + pathFilter helpers the viewer routes use.

// MCPConfig turns the door on. Off by default: a hub that has not thought
// about agent access should not have agent access.
type MCPConfig struct {
	Enabled bool `json:"enabled"`
}

// Caps for one grep. A project the hub has never had on a disk is scanned by
// fetching blobs, so an unbounded grep is an unbounded egress bill and an
// unbounded wait. Truncation is always reported — an agent that believes it
// searched everything and did not is worse than one that knows it stopped.
//
// ponytail: on-demand blob scan, no index. If p95 grep stops being fast
// enough, the upgrade path is a per-project text index in the MetaStore,
// invalidated on each op — not a bigger cap here.
const (
	maxGrepFiles   = 2000
	maxGrepBytes   = 64 << 20
	maxGrepMatches = 200
	maxGrepLine    = 1 << 20
	binaryScanHead = 8 << 10 // a NUL in the first 8 KB means "not text"
	maxReadBytes   = 1 << 20
	maxListEntries = 1000
	maxMoveBytes   = 64 << 20
	maxInlineImage = 1 << 20
	// maxReadScan bounds a paged read. Past it a file is grep's problem, and
	// the response says so rather than silently serving a prefix.
	maxReadScan = 64 << 20
	// maxRequestBody bounds one JSON-RPC request. A write's content travels
	// JSON-ENCODED, so the usable content size is smaller than this and varies
	// with escaping — quote the conservative number, never the raw one.
	maxRequestBody = 4 << 20
	// maxLineShown bounds ONE line. Past it the line is cut and the cut is
	// reported — never a reason to fail the whole file. See eachLine.
	maxLineShown = 4 << 10
	// defaultReadLines bounds an unqualified read. Matches what coding agents
	// expect from a Read tool, and the response always says how to continue.
	defaultReadLines = 2000
)

// mcpServer is the tool registry, built once.
//
// Once, not per request: AddTool infers a JSON schema from each input struct
// by reflection, and in stateless mode getServer runs on EVERY call — so
// building it per request meant ten reflective schema derivations per tool
// call, for a value that never varies. Nothing here is per-caller: the tools
// are methods on *Server and read the grant from the request context, which
// is also what keeps one shared instance from leaking one caller's scope into
// another's.
// mcpInstructions rides the initialize result, so a client shows it to the
// model once per session rather than on every tool call.
//
// It earns that space by answering the one question the tool names cannot.
// They are deliberately the names of the local file tools — that is what makes
// the door usable with no instructions at all — and the paths are
// absolute-looking, so nothing in `read(/wiki/spec.md)` says whether the bytes
// are on this disk or on a hub across the network. Most of the way to get that
// wrong fails loudly (a local path has no project named `Users`), but the
// expensive case is silent: a project that is ALSO synced to the machine is
// reachable through both doors at once, and an agent alternating between them
// races the daemon into conflict copies and reads its own stale writes.
const mcpInstructions = `These tools read and write files stored on a BearDrive hub, over the network.

They are NOT the local filesystem. A path here is /<project>/... — the first
segment names one of the projects connected to this session, and it has nothing
to do with paths on the machine you are running on. Call list with no path to
see which projects you have.

If a project is also synced to this machine as a local folder, work on the local
files with your ordinary file tools: they are the same bytes, and editing
through both doors at once makes conflict copies and stale reads. Use these
tools for projects you have not synced, and for history and restore, which a
local folder cannot give you.

Output carries each file's hub page as a url: read prints one under its header,
list, glob and files_only grep rows end with one, and write, move, restore and
history all name one. When you mention a file in prose, append that link on a
link emoji right after the path — the path stays plain text and the hyperlink
goes on the emoji only, like notes.md [🔗](https://hub.example/<id>/notes.md).
Opening one needs hub sign-in and membership of the project, so they are safe
to paste anywhere internal.`

func (s *Server) mcpServer() *mcp.Server {
	s.mcpOnce.Do(func() {
		srv := mcp.NewServer(&mcp.Implementation{
			Name:    "beardrive",
			Title:   "BearDrive",
			Version: "1",
		}, &mcp.ServerOptions{Instructions: mcpInstructions})
		s.addMCPTools(srv)
		s.mcpSrv = srv
	})
	return s.mcpSrv
}

// MCPHandler is the /mcp endpoint: OAuth check, then the streamable HTTP
// transport. Stateless + JSONResponse is the simplest posture that is also
// correct behind more than one hub process — there is no session affinity to
// get wrong because there is no session.
func (s *Server) MCPHandler() http.Handler {
	h := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s.mcpServer() },
		&mcp.StreamableHTTPOptions{
			Stateless: true, JSONResponse: true,
			// Stated rather than inherited from the SDK default, because it is
			// the effective ceiling on a write and the tool description quotes
			// it. A body over this is rejected before JSON-RPC parsing, so it
			// surfaces as a bare HTTP 413 that many clients render as a
			// connection failure — which is why write says so in advance.
			MaxRequestBodyBytes: maxRequestBody,
		},
	)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g, ok := s.mcpAuthorize(w, r)
		if !ok {
			return
		}
		s.MCP.touch(g.ID)
		// The grant and the account it acts as ride the context from here
		// down. Every internal call a tool makes inherits them, which is how
		// capByGrant sees the grant at the far end of a re-entrant request.
		u := User{Email: g.Account, Name: g.Account}
		r = withUser(withGrant(r, g), u)
		// One resolved project list per request, shared by every tool call in
		// it. See grantProjects.
		ctx := context.WithValue(r.Context(), projCacheKey{}, &projCache{})
		// The origin THIS client reached us on, so tool output can hand back
		// links it can actually open. Per request for the same reason issuer()
		// is: one hub answers on a tunnel, a LAN address and a public name,
		// and a link pinned to one of them is dead in the other two.
		ctx = context.WithValue(ctx, mcpBaseKey{}, requestBaseURL(r))
		r = r.WithContext(ctx)
		h.ServeHTTP(w, r)
	})
}

// mcpAuthorize resolves the bearer token, or answers the 401 that starts the
// OAuth dance. The WWW-Authenticate header is not decoration: RFC 9728 says
// it is how a client discovers where to authenticate, and without it an MCP
// client simply reports that the server is broken.
func (s *Server) mcpAuthorize(w http.ResponseWriter, r *http.Request) (MCPGrant, bool) {
	challenge := fmt.Sprintf(`Bearer resource_metadata=%q`, issuer(r)+"/.well-known/oauth-protected-resource")
	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if tok == "" || tok == r.Header.Get("Authorization") {
		w.Header().Set("WWW-Authenticate", challenge)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return MCPGrant{}, false
	}
	g, ok := s.MCP.Grant(tok)
	if !ok {
		w.Header().Set("WWW-Authenticate", challenge+`, error="invalid_token"`)
		http.Error(w, "invalid or expired token", http.StatusUnauthorized)
		return MCPGrant{}, false
	}
	return g, true
}

// ---- virtual paths ----

// resolveProject turns the first path segment into a project id, accepting
// either the id or the project's NAME.
//
// Names are the point. Every path an agent writes, reads back, quotes in a
// summary, or pastes into its next call carried 36 characters of UUID, which
// is unreadable in a transcript and trivially transposable between two
// projects. `/wiki/docs/spec.md` is the same file as
// `/6f4a…-…/docs/spec.md` and an agent can actually keep it straight.
//
// Ids win over names, so a name can never shadow a real id; an ambiguous name
// is an error that lists the ids rather than a guess.
func (s *Server) resolveProject(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", nil
	}
	projects := s.grantProjects(ctx)
	for _, p := range projects {
		if p.ID == token {
			return p.ID, nil
		}
	}
	var hits []Project
	for _, p := range projects {
		if strings.EqualFold(p.Name, token) {
			hits = append(hits, p)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0].ID, nil
	case 0:
		return "", fmt.Errorf("no such project %q", token)
	}
	ids := make([]string, 0, len(hits))
	for _, p := range hits {
		ids = append(ids, p.ID)
	}
	return "", fmt.Errorf("%q names %d connected projects; use one of these ids: %s",
		token, len(hits), strings.Join(ids, ", "))
}

// splitMCPPath turns "/wiki/docs/spec.md" into ("wiki", "docs/spec.md").
// A bare "/" is the root: no project, no path. The first segment may be an id
// or a name — callers pass it through resolveProject.
func splitMCPPath(p string) (project, rest string) {
	p = strings.TrimPrefix(strings.TrimSpace(p), "/")
	if p == "" {
		return "", ""
	}
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i], strings.Trim(p[i+1:], "/")
	}
	return p, ""
}

// splitGranted is splitMCPPath plus the grant check, for the tools that reach
// the hub over HTTP. Without it those tools surface the hub's own 403 text —
// "you have read-only access to this project" — for a project the grant never
// named, which both confirms the project exists and reports a membership the
// caller was not asking about. The natively-implemented tools get the same
// answer from projectReq; this is that rule for the routed ones.
func (s *Server) splitGranted(ctx context.Context, p string) (project, rest string, err error) {
	project, rest = splitMCPPath(p)
	if project == "" || rest == "" {
		return "", "", fmt.Errorf("path must name a file inside a project, e.g. /%s/README.md", exampleProject(s, ctx))
	}
	if project, err = s.resolveProject(ctx, project); err != nil {
		return "", "", err
	}
	if g, ok := grantFrom(ctx); ok && !g.covers(project) {
		return "", "", fmt.Errorf("no such project %q", project)
	}
	return project, rest, nil
}

// projectLabels maps each connected project id to how a path should NAME it:
// the project's own name when that is unambiguous, the id otherwise.
//
// Built once per tool call rather than per path — grep can format 200 of them,
// and this is what keeps the answer readable instead of 200 lines each opening
// with 36 characters of UUID.
func (s *Server) projectLabels(ctx context.Context) map[string]string {
	ps := s.grantProjects(ctx)
	dupe := map[string]int{}
	for _, p := range ps {
		dupe[strings.ToLower(p.Name)]++
	}
	// A name is only a label if resolveProject would hand it back to THIS
	// project: unique, slash-free, and not shadowed by some project's id
	// (ids win on resolution, so such a name never addresses its own row).
	ids := make(map[string]bool, len(ps))
	for _, p := range ps {
		ids[p.ID] = true
	}
	out := make(map[string]string, len(ps))
	for _, p := range ps {
		usable := p.Name != "" &&
			dupe[strings.ToLower(p.Name)] == 1 &&
			!strings.Contains(p.Name, "/") &&
			!ids[p.Name]
		if usable {
			out[p.ID] = p.Name
			continue
		}
		out[p.ID] = p.ID
	}
	return out
}

// labelPath formats a path the way the caller should quote it back.
func labelPath(labels map[string]string, project, rest string) string {
	if l, ok := labels[project]; ok && l != "" {
		project = l
	}
	return joinMCPPath(project, rest)
}

func joinMCPPath(project, rest string) string {
	if rest == "" {
		return "/" + project
	}
	return "/" + project + "/" + rest
}

// mcpBaseKey carries this hub's origin, set once per request by MCPHandler.
type mcpBaseKey struct{}

// fileURL is the viewer page for one project-relative path: the link an agent
// puts in an answer next to the path it is talking about.
//
// It is emitted rather than left to the agent to compose, and that is the
// whole point of it. A formula ("this hub, then the path you were given")
// builds a dead link twice over: the paths these tools print carry the
// project's NAME, and the viewer resolves a name only when it is unique among
// the projects the BROWSER user can see — a bigger set than this grant's, so a
// name unambiguous here can be ambiguous there and the link lands on a project
// list instead of the file. The id always resolves, for every reader. And
// percent-encoding is the second thing to get wrong.
func fileURL(ctx context.Context, project, rest string) string {
	base, _ := ctx.Value(mcpBaseKey{}).(string)
	if base == "" || project == "" {
		return ""
	}
	u := base + "/" + url.PathEscape(project)
	if rest != "" {
		u += "/" + encodeSegments(rest)
	}
	return u
}

// urlLine is the "url: …" line a single-file response carries, and nothing at
// all when the origin is unknown — a tool that is useful without a link must
// not start printing "url: " with nothing after it.
func urlLine(ctx context.Context, project, rest string) string {
	if u := fileURL(ctx, project, rest); u != "" {
		return "url: " + u + "\n"
	}
	return ""
}

// urlCol is the same link as a trailing column, for output with one file per
// row. Appended last so every existing column keeps its position.
func urlCol(ctx context.Context, project, rest string) string {
	if u := fileURL(ctx, project, rest); u != "" {
		return "\t" + u
	}
	return ""
}

// encodeSegments percent-encodes each segment and leaves the "/" separators
// literal, which is how the frontend's router parses a path back out of a URL
// (see encodePath in router.ts — encoded slashes must survive).
func encodeSegments(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// ---- internal dispatch ----

// memWriter is an http.ResponseWriter that keeps the response in memory.
// net/http/httptest would do this, but importing a testing package into the
// server binary to get twenty lines is the wrong trade.
type memWriter struct {
	code int
	hdr  http.Header
	body bytes.Buffer
}

func newMemWriter() *memWriter { return &memWriter{code: 200, hdr: http.Header{}} }

func (m *memWriter) Header() http.Header         { return m.hdr }
func (m *memWriter) WriteHeader(code int)        { m.code = code }
func (m *memWriter) Write(b []byte) (int, error) { return m.body.Write(b) }

// call re-enters the hub's own mux as the grant's account. This is the seam
// that buys every permission and quota check for free; see the file comment.
func (s *Server) call(ctx context.Context, method, target string, body io.Reader) (*memWriter, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if s.apiMux == nil {
		return nil, fmt.Errorf("server handler not built")
	}
	w := newMemWriter()
	s.apiMux.ServeHTTP(w, req)
	return w, nil
}

// callErr turns a non-2xx internal response into the error an agent sees.
// The hub's own message is passed through: it already says "no such file" or
// "write permission required", which is exactly what the agent needs.
func callErr(w *memWriter) error {
	msg := strings.TrimSpace(w.body.String())
	if msg == "" {
		msg = http.StatusText(w.code)
	}
	if len(msg) > 400 {
		msg = msg[:400]
	}
	return fmt.Errorf("%s", msg)
}

// projectReq is a request bound to one project, for the native tools that
// need the volume and the caller's path filter rather than an HTTP round
// trip. It runs the SAME permission resolution proj() does, so a native tool
// cannot be laxer than a routed one.
func (s *Server) projectReq(ctx context.Context, id, level string) (*volume, *http.Request, Project, error) {
	p, v, err := s.projectVolume(id)
	if err != nil {
		return nil, nil, Project{}, fmt.Errorf("no such project %q", id)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "/api/p/"+url.PathEscape(id)+"/tree", nil)
	if err != nil {
		return nil, nil, Project{}, err
	}
	// Carry whatever the MCP entry point put on the context (grant + user)
	// onto this request, then stash the project the way proj() does.
	req = withProject(withProjectID(req, id), p)
	if !atLeast(s.projectPermOf(req, p), level) {
		return nil, nil, Project{}, fmt.Errorf("no such project %q", id)
	}
	return v, req, p, nil
}

// visibleIn is one project's files as this caller may see them.
func (s *Server) visibleIn(ctx context.Context, id, level string) (map[string]FileInfo, *http.Request, error) {
	v, req, _, err := s.projectReq(ctx, id, level)
	if err != nil {
		return nil, nil, err
	}
	snap, err := v.snapshot(ctx)
	if err != nil {
		return nil, nil, err
	}
	return visibleFiles(snap.files, s.visibility(req)), req, nil
}

// grantProjects is the projects this caller may actually reach right now: in
// the grant, still resolvable, and still readable BY THE ACCOUNT.
//
// The permission check is the part that is easy to leave out and wrong to.
// A grant is a ceiling on live permission, not a snapshot of it — so when
// someone is removed from a project, every tool that resolves a path refuses
// (they all go through projectPermOf) but the root listing, which resolves no
// path, went on naming the project and its name to an agent whose account had
// just been offboarded. A project deleted after consent likewise stops
// appearing rather than erroring every listing.
func (s *Server) grantProjects(ctx context.Context) []Project {
	// Memoized per request. Each entry costs a registry lookup (which may
	// re-read the whole registry — projectPermOf's own comment measures that
	// at 24 ms per request at 5k projects), and one tool call asks for this
	// list more than once: resolveProject to turn a name into an id, then
	// projectLabels to turn ids back into names on the way out.
	if c, ok := ctx.Value(projCacheKey{}).(*projCache); ok {
		c.once.Do(func() { c.ps = s.computeGrantProjects(ctx) })
		return c.ps
	}
	return s.computeGrantProjects(ctx)
}

type projCacheKey struct{}

type projCache struct {
	once sync.Once
	ps   []Project
}

func (s *Server) computeGrantProjects(ctx context.Context) []Project {
	g, ok := grantFrom(ctx)
	if !ok {
		return nil
	}
	var out []Project
	for _, id := range g.Projects {
		if _, _, _, err := s.projectReq(ctx, id, PermRead); err != nil {
			continue
		}
		p, _, err := s.projectVolume(id)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ---- tool plumbing ----

// noOut is `any`, not an empty struct. The SDK infers an output schema from
// the Out type unless it is exactly `any` (server.go: reflect.TypeFor[Out]() !=
// reflect.TypeFor[any]()), so struct{} made every tool advertise
// `outputSchema: {"type":"object","additionalProperties":false}` and then
// return `structuredContent: {}` — a client that prefers structured output
// when a schema is present read EVERY result as empty. These tools answer in
// text; say so by declaring no schema at all.
type noOut = any

// mcpContentTool is mcpTool for a tool that can answer with something other
// than text. Only `read` needs it (images); everything else stays on the
// string-returning helper rather than making every tool pay for the one.
func mcpContentTool[In any](srv *mcp.Server, name, desc string, fn func(context.Context, In) ([]mcp.Content, error)) {
	mcp.AddTool(srv, &mcp.Tool{Name: name, Description: desc},
		func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, noOut, error) {
			out, err := fn(ctx, in)
			if err != nil {
				return &mcp.CallToolResult{IsError: true, Content: mcpText("%s", err)}, nil, nil
			}
			return &mcp.CallToolResult{Content: out}, nil, nil
		})
}

// mcpTool registers a tool whose output is plain text. Every tool in this
// file returns text rather than structured content: an agent reads these, and
// a listing it can scan beats a JSON blob it has to re-serialize to quote.
func mcpTool[In any](srv *mcp.Server, name, desc string, fn func(context.Context, In) (string, error)) {
	mcp.AddTool(srv, &mcp.Tool{Name: name, Description: desc},
		func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, noOut, error) {
			out, err := fn(ctx, in)
			if err != nil {
				// An error the agent can act on, not a protocol failure: MCP
				// reserves transport errors for "the call could not happen",
				// and "you cannot write there" is a result.
				return &mcp.CallToolResult{IsError: true, Content: mcpText("%s", err)}, nil, nil
			}
			return &mcp.CallToolResult{Content: mcpText("%s", out)}, nil, nil
		})
}

// ---- tool inputs ----

type listIn struct {
	Path  string `json:"path" jsonschema:"Absolute virtual path, e.g. / or /my-project/docs"`
	Depth int    `json:"depth,omitempty" jsonschema:"How many levels below path to list. Default 1 (like ls). Use a larger number for a tree."`
}

type readIn struct {
	Path   string `json:"path" jsonschema:"Absolute virtual path to a file, e.g. /my-project/README.md"`
	Offset int    `json:"offset,omitempty" jsonschema:"Line number to start from (1-based)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum number of lines to return"`
	Raw    bool   `json:"raw,omitempty" jsonschema:"Return the file's exact bytes with no line numbers or header. Use this when copying a file, so the content round-trips unchanged."`
	SHA    string `json:"sha,omitempty" jsonschema:"Read a past version instead of the current one, using a sha from history. Does not change the file."`
}

type globIn struct {
	Pattern string `json:"pattern" jsonschema:"Glob pattern, e.g. **/*.md"`
	Path    string `json:"path,omitempty" jsonschema:"Directory to search under. Defaults to / (all connected projects)"`
}

type grepIn struct {
	Pattern    string `json:"pattern" jsonschema:"Regular expression to search for"`
	Path       string `json:"path,omitempty" jsonschema:"Directory to search under. Defaults to / (all connected projects)"`
	Glob       string `json:"glob,omitempty" jsonschema:"Only search files matching this glob, e.g. *.go"`
	IgnoreCase bool   `json:"ignore_case,omitempty" jsonschema:"Case-insensitive match"`
	FilesOnly  bool   `json:"files_only,omitempty" jsonschema:"Return only the matching file paths, not the matching lines"`
	Context    int    `json:"context,omitempty" jsonschema:"Lines of surrounding context to show around each match (like grep -C). Default 0."`
}

type writeIn struct {
	Path    string `json:"path" jsonschema:"Absolute virtual path to write, e.g. /my-project/notes.md"`
	Content string `json:"content" jsonschema:"Full new contents of the file"`
	SHA     string `json:"sha,omitempty" jsonschema:"The sha you last read for this file. If set and the file has changed since, the write is refused. Omit when creating a new file."`
}

type editIn struct {
	Path       string `json:"path" jsonschema:"Absolute virtual path to edit"`
	OldString  string `json:"old_string" jsonschema:"Exact text to replace"`
	NewString  string `json:"new_string" jsonschema:"Replacement text"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"Replace every occurrence instead of requiring a unique match"`
	SHA        string `json:"sha,omitempty" jsonschema:"The sha you last read for this file. If set and the file has changed since, the edit is refused."`
}

type pathIn struct {
	Path string `json:"path" jsonschema:"Absolute virtual path"`
}

type moveIn struct {
	From string `json:"from" jsonschema:"Absolute virtual path to move"`
	To   string `json:"to" jsonschema:"Absolute virtual destination path, in the same project"`
}

type historyIn struct {
	Path  string `json:"path" jsonschema:"Absolute virtual path to a file"`
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum number of versions to return (default 20)"`
}

type restoreIn struct {
	Path string `json:"path" jsonschema:"Absolute virtual path to restore"`
	SHA  string `json:"sha" jsonschema:"The sha of the version to restore, from history"`
}

func (s *Server) addMCPTools(srv *mcp.Server) {
	mcpTool(srv, "list",
		"List files and folders at a path. The root (/) lists the connected projects; "+
			"everything below it is an ordinary path inside one, e.g. /my-project/docs. "+
			"A project can be named by its name or its id. Directories are implicit — "+
			"there is no mkdir, writing a nested path creates it.",
		s.mcpList)
	mcpContentTool(srv, "read",
		"Read a file's contents. Returns numbered lines; use offset/limit to page through a large "+
			"file. Pass raw:true to get the exact bytes with no numbering — use that whenever you "+
			"are COPYING a file, because numbered output cannot be turned back into the original. "+
			"Small images are returned as images.",
		s.mcpRead)
	mcpTool(srv, "glob",
		"Find files by name pattern across connected projects, most recently changed first. "+
			"Patterns are relative to the search path — no leading slash. ** spans any number of "+
			"directories (**/*.md); a pattern with no slash at all (*.md) is matched against every "+
			"file's name at any depth. Brace expansion is not supported.",
		s.mcpGlob)
	mcpTool(srv, "grep",
		"Search file contents with a regular expression across connected projects. "+
			"Pass context:N to see N lines around each match (like grep -C), which saves a "+
			"follow-up read. Reports any file it could not search.",
		s.mcpGrep)
	mcpTool(srv, "write",
		"Create or overwrite a file with the given contents. Creates parent folders implicitly. "+
			"When changing a file that already exists, pass the sha you read so the write is "+
			"refused if a teammate changed it in the meantime. Content is limited to roughly "+
			"2 MB (it travels JSON-encoded, so heavily-escaped text hits the limit sooner).",
		s.mcpWrite)
	mcpTool(srv, "edit",
		"Replace exact text in a file. Pass the sha you read to be refused if a teammate "+
			"changed the file in the meantime, rather than silently overwriting them.",
		s.mcpEdit)
	mcpTool(srv, "delete",
		"Delete a file, or every file under a path ending in /.",
		s.mcpDelete)
	mcpTool(srv, "move",
		"Move or rename one file within a project. Refuses if something already exists at the "+
			"destination — delete it first if you mean to replace it.",
		s.mcpMove)
	mcpTool(srv, "history",
		"Show who changed a file, when, and the sha of each version. Pass one of those shas to "+
			"restore to bring that version back.",
		s.mcpHistory)
	mcpTool(srv, "restore",
		"Restore a previous version of a file, using a sha exactly as history printed it.",
		s.mcpRestore)
}

// ---- read-side tools (native) ----

func (s *Server) mcpList(ctx context.Context, in listIn) (string, error) {
	project, rest := splitMCPPath(in.Path)
	if project == "" {
		ps := s.grantProjects(ctx)
		if len(ps) == 0 {
			return "No projects are connected to this client.", nil
		}
		// Lead with the NAME, because a name is a working path now and the id
		// is 36 characters an agent has to carry through every later call.
		// Fall back to the id for a name shared by two connected projects,
		// where the name alone would be ambiguous.
		// The label must be an address that resolves BACK to this row. Printing
		// a name merely because it is unique was not enough: a project whose
		// NAME is another project's ID is unaddressable by name (ids win, as
		// they must), and the listing was still advertising that name as its
		// path — so an agent faithfully following the row it was given wrote
		// into a different project entirely. Ask the resolver instead of
		// re-deriving the rule and getting a different answer.
		labels := s.projectLabels(ctx)
		var b strings.Builder
		fmt.Fprintf(&b, "%d connected project(s):\n", len(ps))
		for _, p := range ps {
			// Say up front when a project is read-only. Learning it from a
			// refusal costs whatever the agent had already composed.
			access := ""
			if _, req, _, err := s.projectReq(ctx, p.ID, PermRead); err == nil {
				if !atLeast(s.projectPermOf(req, p), PermWrite) {
					access = "\t(read-only)"
				}
			}
			if label := labels[p.ID]; label != p.ID {
				fmt.Fprintf(&b, "  /%s/\t(id %s)%s\n", label, p.ID, access)
				continue
			}
			fmt.Fprintf(&b, "  /%s/\tname: %s%s\n", p.ID, p.Name, access)
		}
		return b.String(), nil
	}
	project, err := s.resolveProject(ctx, project)
	if err != nil {
		return "", err
	}
	files, _, err := s.visibleIn(ctx, project, PermRead)
	if err != nil {
		return "", err
	}
	depth := in.Depth
	if depth <= 0 {
		depth = 1
	}
	prefix := ""
	if rest != "" {
		prefix = rest + "/"
	}

	dirs := map[string]bool{}
	type row struct {
		name string
		fi   FileInfo
	}
	var rows []row
	for p, fi := range files {
		if prefix != "" && !strings.HasPrefix(p, prefix) {
			continue
		}
		tail := strings.TrimPrefix(p, prefix)
		parts := strings.Split(tail, "/")
		if len(parts) > depth {
			dirs[strings.Join(parts[:depth], "/")+"/"] = true
			continue
		}
		rows = append(rows, row{tail, fi})
	}
	if len(rows) == 0 && len(dirs) == 0 {
		// Listing a FILE is a thing agents do constantly (they cannot tell
		// from a path which it is). "Nothing at /p/a.md" reads as "it does not
		// exist" and sends the agent looking for it; say what it actually is.
		if fi, ok := files[rest]; ok {
			return fmt.Sprintf("%s is a file (%s, sha %s), not a folder. Use read to see it.",
				in.Path, humanSize(fi.Size), short(fi.Blob)), nil
		}
		// An empty project is a normal state and the FIRST thing an agent
		// sees in a new one; "Nothing at X" made it read like a typo.
		if rest == "" {
			return fmt.Sprintf("%s is empty — no files yet. Write one to get started.", in.Path), nil
		}
		return fmt.Sprintf("Nothing at %s (no such folder, and no file by that name)", in.Path), nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	names := make([]string, 0, len(dirs))
	for d := range dirs {
		names = append(names, d)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, d := range names {
		fmt.Fprintf(&b, "%s%s\n", d, urlCol(ctx, project, prefix+strings.TrimSuffix(d, "/")))
	}
	n := 0
	for _, r := range rows {
		if n >= maxListEntries {
			fmt.Fprintf(&b, "... truncated at %d entries; narrow the path\n", maxListEntries)
			break
		}
		// Z, not a bare local-looking stamp: an agent relaying "2026-09-17
		// 04:53" to a user reports the wrong day for most of the planet, with
		// nothing in the string to signal it. history already says Z.
		fmt.Fprintf(&b, "%s\t%s\t%s\tsha:%s%s\n", r.name, humanSize(r.fi.Size),
			r.fi.Time.UTC().Format("2006-01-02T15:04Z"), short(r.fi.Blob),
			urlCol(ctx, project, prefix+r.name))
		n++
	}
	return b.String(), nil
}

func (s *Server) mcpRead(ctx context.Context, in readIn) ([]mcp.Content, error) {
	project, rest, err := s.splitGranted(ctx, in.Path)
	if err != nil {
		return nil, err
	}
	files, req, err := s.visibleIn(ctx, project, PermRead)
	if err != nil {
		return nil, err
	}
	fi, ok := files[rest]
	if !ok {
		// Same wrong-shape error delete used to give: "no such file" about a
		// path the agent just saw listed as a folder.
		if hasPrefixIn(files, rest+"/") {
			return nil, fmt.Errorf("%s is a folder; use list to see what is in it", in.Path)
		}
		return nil, fmt.Errorf("no such file: %s", in.Path)
	}
	_, v, err := s.projectVolume(project)
	if err != nil {
		return nil, err
	}
	// A past version, read without mutating anything. Inspecting one used to
	// require restoring it, reading, and restoring back — two writes and two
	// new versions just to look.
	if in.SHA != "" {
		full, err := s.resolveSHA(ctx, project, rest, in.SHA)
		if err != nil {
			return nil, err
		}
		fi.Blob, fi.Size = full, 0
	}
	rc, err := v.source.Open(ctx, rest, fi)
	if err != nil {
		return nil, fmt.Errorf("content temporarily unavailable: %v", err)
	}
	defer rc.Close()
	// A bounded HEAD of the file: enough to sniff binary, and enough to be the
	// whole thing for raw/image. The text path streams the rest (see below) —
	// this is deliberately not "the file".
	data, err := io.ReadAll(io.LimitReader(rc, maxReadBytes+1))
	if err != nil {
		return nil, err
	}
	// A read through the agent door is an AGENT read. recordRead would file it
	// as human, which is exactly the distinction the heat map exists to draw:
	// "a person opened this" and "an agent scanned this" mean different things
	// to someone deciding what to fix. The actor is the grant — never the
	// account's email, which must not enter the ledger — and it is prefixed so
	// it cannot be mistaken for a device id in ?by=device.
	s.recordAgentRead(req, rest)

	// Raw first, and BEFORE the image and binary branches. Raw is the
	// byte-exact answer — it exists because the decorated read is lossy
	// ("alpha\nbeta" and "alpha\nbeta\n" render identically, so a copy
	// invented a trailing newline) — and the whole point of it is copying a
	// file. Falling through to the binary branch would have handed the agent
	// the sentence "…is a binary file", which it then writes AS the content.
	// A refusal is the only honest answer for bytes MCP cannot carry as text.
	if in.Raw {
		if len(data) > maxReadBytes {
			// Do NOT say "read it in pages" without checking whether paging
			// works on THIS file. For a minified bundle it does not — the
			// paged read hits the same long line — and the two messages sent
			// the agent around a loop three calls long to discover there was
			// no path at all. Say which case this is.
			if isUnpageable(data) {
				return nil, fmt.Errorf("%s is %s with no line breaks, so it can be neither paged "+
					"nor returned whole (raw is capped at %s). Use grep to find what you need in it",
					in.Path, humanSize(fi.Size), humanSize(maxReadBytes))
			}
			return nil, fmt.Errorf("%s is larger than %s; raw reads are capped. "+
				"Read it in pages instead (offset/limit), or grep it",
				in.Path, humanSize(maxReadBytes))
		}
		if isBinary(data) {
			return nil, fmt.Errorf("%s is binary (%s, sha %s); MCP carries text, so it cannot be "+
				"read or copied byte-exactly through this door. Download it from the viewer instead",
				in.Path, humanSize(fi.Size), short(fi.Blob))
		}
		return mcpText("%s", string(data)), nil
	}

	// An image comes back AS an image when it is small enough to be worth the
	// agent's context. Inline is base64, so a 5 MB screenshot is ~6.7 MB of
	// tokens — returning every image inline would let one read blow the
	// caller's whole window. Past the cap, describe it instead.
	// Sniffed, not guessed from the extension. A 17-byte text file named
	// fake.png came back as an image block, and raw was ignored for it, so a
	// mis-named file was unreadable by any means.
	if ct := contentType(rest); strings.HasPrefix(ct, "image/") && looksLikeImage(data) {
		if fi.Size <= maxInlineImage && int64(len(data)) == fi.Size {
			return []mcp.Content{&mcp.ImageContent{Data: data, MIMEType: ct}}, nil
		}
		return mcpText("%s is a %s image, %s — too large to inline (cap %s). "+
			"Open it in the viewer.\n%s", in.Path, ct, humanSize(fi.Size),
			humanSize(maxInlineImage), urlLine(ctx, project, rest)), nil
	}
	if isBinary(data) {
		return mcpText("%s is a binary file (%s, sha %s). Not shown as text.\n%s",
			in.Path, humanSize(fi.Size), short(fi.Blob), urlLine(ctx, project, rest)), nil
	}
	// Paging STREAMS the whole file rather than slicing a 1 MiB prefix, and
	// that is the whole fix for the worst bug this door has had.
	//
	// Reading a fixed prefix meant a 2.7 MB log reported "has 11072 lines"
	// (the prefix's count, not the file's), answered "offset 28500 is past the
	// end" for a line grep had just quoted from the same file, and handed back
	// a continuation offset the next call rejected. Three failures compounding
	// into the worst possible shape: indistinguishable from real EOF, so an
	// agent summarising an export drew conclusions from its first 40% and
	// reported success.
	//
	// So: count every line, collect only the requested window. The bytes are
	// being fetched from storage anyway; what we refuse to do is buffer them.
	start := in.Offset
	if start < 1 {
		start = 1
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultReadLines
	}
	last := start + limit - 1

	var (
		window      []string
		total       int
		scanned     int64
		capped      bool
		windowBytes int
		cutLines    []int
	)
	err = eachLine(io.MultiReader(bytes.NewReader(data), rc), maxLineShown,
		func(n int, line string, cut bool) bool {
			total = n
			scanned += int64(len(line)) + 1
			if scanned > maxReadScan {
				capped = true
				return false
			}
			if n >= start && n <= last && windowBytes < maxReadBytes {
				if cut {
					line += "… [line truncated]"
					cutLines = append(cutLines, n)
				}
				windowBytes += len(line) + 1
				window = append(window, line)
			}
			return true
		})
	if err != nil {
		return nil, fmt.Errorf("reading %s: %v", in.Path, err)
	}

	var b strings.Builder
	if capped {
		fmt.Fprintf(&b, "%s (sha %s) is larger than %s — too large to page through here. "+
			"Use grep to find what you need in it.\n", in.Path, short(fi.Blob), humanSize(maxReadScan))
		return mcpText("%s", b.String()), nil
	}
	if total == 0 {
		return mcpText("%s (sha %s) is empty.", in.Path, short(fi.Blob)), nil
	}
	if start > total {
		return mcpText("%s has %d lines; offset %d is past the end.", in.Path, total, start), nil
	}
	fmt.Fprintf(&b, "%s (sha %s, %d lines)\n", in.Path, short(fi.Blob), total)
	if u := fileURL(ctx, project, rest); u != "" {
		// Pinned to the version actually returned. Linking the live page after
		// reading a past one hands the reader different bytes than the answer
		// was written from, and nothing in the link says so.
		if in.SHA != "" {
			u += "?v=" + fi.Blob
		}
		fmt.Fprintf(&b, "url: %s\n", u)
	}
	for i, line := range window {
		fmt.Fprintf(&b, "%6d\t%s\n", start+i, line)
	}
	// Exactly ONE continuation hint, and the offset it names is always an
	// offset this tool will accept. Two hints disagreeing about where to
	// resume cost the last round a call just to find out which one was meant.
	if n := len(cutLines); n > 0 {
		fmt.Fprintf(&b, "... %d line(s) were longer than %s and are shown truncated (line %d%s). "+
			"Use grep to search their full contents.\n",
			n, humanSize(maxLineShown), cutLines[0], plural(n))
	}
	if shown := start + len(window) - 1; shown < total {
		if windowBytes >= maxReadBytes {
			// Stopped on BYTES, not on the line limit. Say which, or the agent
			// reads the hint as "I asked for too many lines" and retries with
			// a smaller limit that changes nothing.
			fmt.Fprintf(&b, "... stopped after %s of text; %d more lines. Read again with offset=%d\n",
				humanSize(int64(windowBytes)), total-shown, shown+1)
		} else {
			fmt.Fprintf(&b, "... %d more lines; read again with offset=%d\n", total-shown, shown+1)
		}
	}
	return mcpText("%s", b.String()), nil
}

func (s *Server) mcpGlob(ctx context.Context, in globIn) (string, error) {
	if in.Pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}
	// grep reports a bad pattern; glob used to report one as "no files match",
	// which an agent reads as "this project has none" and acts on. A false
	// negative is worse than an error because nothing about it looks wrong.
	if err := checkGlob(in.Pattern); err != nil {
		return "", err
	}
	type hit struct {
		path string
		at   time.Time
		url  string
	}
	labels := s.projectLabels(ctx)
	var hits []hit
	err := s.eachProject(ctx, in.Path, func(p Project, rest string, files map[string]FileInfo, _ *http.Request) error {
		for f, fi := range files {
			if rest != "" && !strings.HasPrefix(f, rest+"/") && f != rest {
				continue
			}
			// Match against the path RELATIVE to the search path, which is
			// what the tool's own error text promises ("patterns are relative
			// to the search path"). Matching the project-relative path instead
			// meant `path:"/p/g/"` with `one/**/*.log` matched nothing while
			// the results printed as `g/...` — the tool contradicting itself.
			if matchGlob(in.Pattern, relTo(rest, f)) {
				hits = append(hits, hit{labelPath(labels, p.ID, f), fi.Time, urlCol(ctx, p.ID, f)})
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return fmt.Sprintf("No files match %s.", in.Pattern), nil
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].at.After(hits[j].at) })
	var b strings.Builder
	fmt.Fprintf(&b, "%d file(s) match %s:\n", len(hits), in.Pattern)
	for i, h := range hits {
		if i >= maxListEntries {
			fmt.Fprintf(&b, "... truncated at %d entries; narrow with a more specific pattern or path\n", maxListEntries)
			break
		}
		fmt.Fprintf(&b, "%s%s\n", h.path, h.url)
	}
	return b.String(), nil
}

func (s *Server) mcpGrep(ctx context.Context, in grepIn) (string, error) {
	if in.Pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}
	expr := in.Pattern
	if in.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", fmt.Errorf("bad pattern: %v", err)
	}
	// The glob FILTER needs the same validation the glob tool got. Without it
	// a malformed filter searched zero files and answered "No matches", and an
	// agent concluded the symbol was unused.
	if err := checkGlob(in.Glob); err != nil {
		return "", err
	}

	labels := s.projectLabels(ctx)
	// A matching file and its link, together: the FilesOnly listing sorts
	// these, so a parallel slice of URLs would desync from the paths.
	type hitFile struct{ path, url string }
	var (
		out             strings.Builder
		matches         int
		scanned         int
		bytesRead       int64
		truncated       bool
		hitFiles        []hitFile
		skippedBinary   []string
		unreadableFiles []string
		totalMatches    int
	)
	err = s.eachProject(ctx, in.Path, func(p Project, rest string, files map[string]FileInfo, _ *http.Request) error {
		_, v, err := s.projectVolume(p.ID)
		if err != nil {
			return nil
		}
		// Deterministic order so a truncated grep truncates the same way
		// twice — an agent re-running a search and getting a different
		// prefix of the answer has no way to tell progress from noise.
		paths := make([]string, 0, len(files))
		for f := range files {
			paths = append(paths, f)
		}
		sort.Strings(paths)

		for _, f := range paths {
			if truncated {
				return nil
			}
			if rest != "" && !strings.HasPrefix(f, rest+"/") && f != rest {
				continue
			}
			if in.Glob != "" && !matchGlob(in.Glob, relTo(rest, f)) {
				continue
			}
			fi := files[f]
			if scanned >= maxGrepFiles || bytesRead >= maxGrepBytes {
				truncated = true
				return nil
			}
			scanned++
			bytesRead += fi.Size
			rc, err := v.source.Open(ctx, f, fi)
			if err != nil {
				continue // a blob we cannot fetch is skipped, never fatal
			}
			found, isBin, cut := scanBlob(rc, re, in.Context, func(line int, text string, isMatch bool) bool {
				if in.FilesOnly {
					return false // one hit is enough to name the file
				}
				if !isMatch {
					// Context lines use "-" the way grep does, so an agent can
					// tell a hit from its surroundings without counting.
					if matches < maxGrepMatches {
						fmt.Fprintf(&out, "%s-%d-%s\n", labelPath(labels, p.ID, f), line, text)
					}
					return true
				}
				totalMatches++
				if matches < maxGrepMatches {
					fmt.Fprintf(&out, "%s:%d:%s\n", labelPath(labels, p.ID, f), line, text)
					matches++
				}
				return true
			})
			rc.Close()
			if isBin {
				skippedBinary = append(skippedBinary, labelPath(labels, p.ID, f))
			}
			if cut > 0 {
				unreadableFiles = append(unreadableFiles, labelPath(labels, p.ID, f))
			}
			if found {
				hitFiles = append(hitFiles, hitFile{labelPath(labels, p.ID, f), urlCol(ctx, p.ID, f)})
			}
			if matches >= maxGrepMatches {
				truncated = true
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	var b strings.Builder
	if in.FilesOnly {
		if len(hitFiles) == 0 {
			return "No files contain " + in.Pattern + "." + grepCaveats(skippedBinary, unreadableFiles), nil
		}
		sort.Slice(hitFiles, func(i, j int) bool { return hitFiles[i].path < hitFiles[j].path })
		fmt.Fprintf(&b, "%d file(s) contain %s:\n", len(hitFiles), in.Pattern)
		for _, f := range hitFiles {
			fmt.Fprintf(&b, "%s%s\n", f.path, f.url)
		}
	} else {
		if totalMatches == 0 {
			fmt.Fprintf(&b, "No matches for %s in %d file(s) searched.", in.Pattern, scanned-len(skippedBinary))
			b.WriteString(grepCaveats(skippedBinary, unreadableFiles))
			return b.String(), nil
		}
		// The real total, not the number printed. An agent asked "how many
		// occurrences" answered 200 for a file with 40,000.
		if totalMatches > matches {
			fmt.Fprintf(&b, "%d match(es) in %d file(s), showing the first %d:\n",
				totalMatches, len(hitFiles), matches)
		} else {
			fmt.Fprintf(&b, "%d match(es) in %d file(s):\n", totalMatches, len(hitFiles))
		}
		b.WriteString(out.String())
		// No link column on match rows, and no trailing block of them: a row
		// is "path:N:text" and the only other lines grep prints are bracketed
		// notes, so anything reading its output line by line can tell the two
		// apart. A links section is path-shaped lines with no line number in
		// them — it broke the first reader that tried. files_only is the mode
		// that prints one row per file, and that one carries the link.
	}
	b.WriteString(grepCaveats(skippedBinary, unreadableFiles))
	// Always say when the answer is partial. See the caps above.
	if truncated {
		fmt.Fprintf(&b, "\n[stopped after scanning %d file(s) — narrow with path or glob]\n", scanned)
	}
	return b.String(), nil
}

// scanBlob runs re over rc line by line, calling onMatch until it says stop.
// Reports whether anything matched at all.
func scanBlob(rc io.Reader, re *regexp.Regexp, ctx int, onMatch func(line int, text string, isMatch bool) bool) (found, binary bool, cutLines int) {
	br := bufio.NewReaderSize(rc, 64<<10)
	head, _ := br.Peek(binaryScanHead)
	if bytes.IndexByte(head, 0) >= 0 {
		// Binary: the same rule grep itself uses. Reported separately so the
		// caller can say it skipped them — "No matches in 5 files" when the
		// term is sitting in a binary one is a wrong answer, not a true one.
		return false, true, 0
	}

	var before []string
	beforeAt := 0 // line number of before[0]
	after := 0
	lastEmitted := 0 // never emit a line twice, or out of order

	emit := func(n int, text string, isMatch bool) bool {
		// Overlapping context windows used to re-emit lines already printed,
		// and a large context could even print them out of order — so anything
		// reconstructing a file view from grep output got a corrupted, longer
		// than real file.
		if n <= lastEmitted {
			return true
		}
		lastEmitted = n
		text = clipAroundMatch(text, re, isMatch)
		return onMatch(n, text, isMatch)
	}

	stopped := false
	// maxGrepLine, not maxLineShown: matching is cheap and the DISPLAY is
	// truncated to 400 chars anyway, so searching a quarter of a megabyte of
	// one line costs nothing and finds what a 4 KB cut would have missed.
	err := eachLine(br, maxGrepLine, func(n int, line string, cut bool) bool {
		if cut {
			cutLines++
		}
		if !re.MatchString(line) {
			if after > 0 {
				after--
				if !emit(n, line, false) {
					stopped = true
					return false
				}
			}
			if ctx > 0 {
				if len(before) == 0 {
					beforeAt = n
				}
				before = append(before, line)
				if len(before) > ctx {
					before = before[1:]
					beforeAt++
				}
			}
			return true
		}
		found = true
		for i, b := range before {
			if !emit(beforeAt+i, b, false) {
				stopped = true
				return false
			}
		}
		before, beforeAt = before[:0], 0
		after = ctx
		if !emit(n, line, true) {
			stopped = true
			return false
		}
		return true
	})
	if err != nil && !stopped {
		cutLines++ // could not finish this file; count it as unsearched
	}
	return found, false, cutLines
}

// eachProject runs fn over every project the search path selects: one when
// the path names a project, all connected ones at the root.
func (s *Server) eachProject(ctx context.Context, searchPath string,
	fn func(p Project, rest string, files map[string]FileInfo, req *http.Request) error) error {
	project, rest := splitMCPPath(searchPath)
	targets := s.grantProjects(ctx)
	if project != "" {
		id, err := s.resolveProject(ctx, project)
		if err != nil {
			return err
		}
		project = id
		found := false
		for _, p := range targets {
			if p.ID == project {
				targets, found = []Project{p}, true
				break
			}
		}
		if !found {
			return fmt.Errorf("no such project %q", project)
		}
	} else {
		rest = ""
	}
	scoped := 0
	for _, p := range targets {
		files, req, err := s.visibleIn(ctx, p.ID, PermRead)
		if err != nil {
			continue // a project that will not resolve is skipped, not fatal
		}
		if rest != "" {
			if _, isFile := files[rest]; isFile || hasPrefixIn(files, rest+"/") {
				scoped++
			}
		}
		if err := fn(p, rest, files, req); err != nil {
			return err
		}
	}
	// A search scoped to a folder that does not exist answered "no matches",
	// which is indistinguishable from a clean negative — the same silent-wrong
	// -answer family as a skipped file. list already knew; now these do too.
	if rest != "" && scoped == 0 {
		return fmt.Errorf("no such folder: %s (check the path with list)", searchPath)
	}
	return nil
}

// ---- write-side tools (through the hub's own endpoints) ----

func (s *Server) mcpWrite(ctx context.Context, in writeIn) (string, error) {
	// A trailing slash is load-bearing in delete ("everything under this
	// folder"), so silently dropping it here let one path mean a file in one
	// tool and a folder in another — and left a file and a folder sharing a
	// name, which nothing downstream can tell apart.
	if strings.HasSuffix(strings.TrimSpace(in.Path), "/") {
		return "", fmt.Errorf("%s ends in a slash, which names a folder. "+
			"Write to a file path instead — folders are created by writing files in them", in.Path)
	}
	project, rest, err := s.splitGranted(ctx, in.Path)
	if err != nil {
		return "", err
	}
	if err := checkComponents(rest, in.Path); err != nil {
		return "", err
	}
	// A file may not collide with a folder, in either direction. This matters
	// more here than anywhere else in the API: the whole product syncs these
	// paths onto real disks, and no filesystem can hold both `src` the file
	// and `src/` the folder. Every other tool guarded it; write did not, so an
	// agent could report success and leave a project that no teammate's device
	// can materialize — with the only escape hatch (delete without the slash)
	// telling them to wipe the entire subtree.
	if err := s.checkNoCollision(ctx, project, rest, in.Path); err != nil {
		return "", err
	}
	// The same compare-and-swap edit has, for the other half of the write
	// path. "Rewrite this file" has to use write (edit needs exact old text),
	// and without this that whole class of task had no lost-update defence at
	// all: two agents rewriting one file, no warning to either.
	if in.SHA != "" {
		// Lock ACROSS the check and the write, or the check is advisory.
		defer lockPath(project, rest)()
		if err := s.checkStale(ctx, project, rest, in.SHA, in.Path); err != nil {
			return "", err
		}
	}
	return s.putFile(ctx, project, rest, []byte(in.Content), "wrote")
}

// pathLocks serializes compare-and-swap writes per (project, path).
//
// ponytail: an in-process keyed mutex, not a distributed lock. It closes the
// window that made the advertised guarantee false — eight concurrent writers
// passing one sha were all accepted, seven updates lost — for callers coming
// through this door, which is the door that advertises it. Two hub processes
// still race; that needs a conditional write in the storage layer.
var pathLocks sync.Map // "project\x00path" -> *sync.Mutex

func lockPath(project, rest string) func() {
	v, _ := pathLocks.LoadOrStore(project+"\x00"+rest, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// checkStale refuses when a file has moved on since the caller read it.
// Shared by write and edit so the two cannot drift into different contracts
// for one concept.
func (s *Server) checkStale(ctx context.Context, project, rest, want, shown string) error {
	// A sha that is not a sha is a caller mistake, not a concurrent edit.
	// Reporting it as "stale" sent the agent into a re-read/retry loop that
	// could never converge.
	if !shaLike(want) {
		if shaLike(strings.ToLower(want)) {
			return fmt.Errorf("shas are lowercase hex; pass %s exactly as read or history printed it",
				strings.ToLower(want))
		}
		return fmt.Errorf("%q is not a sha; pass one exactly as read or history printed it "+
			"(hex, at least 6 characters)", want)
	}
	files, _, err := s.visibleIn(ctx, project, PermRead)
	if err != nil {
		return err
	}
	fi, ok := files[rest]
	if !ok {
		return fmt.Errorf("no such file: %s (you passed a sha, so this expected an existing file)", shown)
	}
	if !strings.HasPrefix(fi.Blob, strings.TrimSuffix(want, "…")) {
		// "Stale" asserts the file CHANGED, and tells the caller to re-read
		// and retry. For a sha that names no version of this file that is
		// false and unrecoverable: the retry reads, gets the same current sha,
		// and fails again the same way. restore already discriminates; this is
		// the same question, asked before the accusation.
		if _, err := s.resolveSHA(ctx, project, rest, want); err != nil {
			return err
		}
		return fmt.Errorf("stale: %s has changed since you read it (now sha %s). Read it again, then retry",
			shown, short(fi.Blob))
	}
	return nil
}

// putFile is the one write path, shared by write/edit/move/restore-as-copy:
// a single-shot relay upload to the hub's own endpoint. Everything that makes
// a write safe — the read-only check, path validation, folder permissions,
// quota, journaling, cache invalidation, live-change fan-out — happens inside
// that handler, so there is nothing to duplicate and nothing to forget.
func (s *Server) putFile(ctx context.Context, project, rest string, content []byte, verb string) (string, error) {
	target := "/api/p/" + url.PathEscape(project) + "/upload/content?path=" + url.QueryEscape(rest)
	w, err := s.call(ctx, http.MethodPut, target, bytes.NewReader(content))
	if err != nil {
		return "", err
	}
	if w.code < 200 || w.code >= 300 {
		return "", callErr(w)
	}
	return fmt.Sprintf("%s %s (%s)\n%s", verb,
		labelPath(s.projectLabels(ctx), project, rest), humanSize(int64(len(content))),
		urlLine(ctx, project, rest)), nil
}

func (s *Server) mcpEdit(ctx context.Context, in editIn) (string, error) {
	project, rest, err := s.splitGranted(ctx, in.Path)
	if err != nil {
		return "", err
	}
	if in.OldString == "" {
		return "", fmt.Errorf("old_string is required; use write to replace a whole file")
	}
	files, _, err2 := s.visibleIn(ctx, project, PermRead)
	if err2 != nil {
		return "", err2
	}
	fi, ok := files[rest]
	if !ok {
		if hasPrefixIn(files, rest+"/") {
			return "", fmt.Errorf("%s is a folder; edit works on one file. Use list to see what is in it", in.Path)
		}
		return "", fmt.Errorf("no such file: %s", in.Path)
	}
	// Compare-and-swap. The journal is last-writer-wins per path, so an edit
	// that read, substituted and wrote would silently discard a teammate's
	// concurrent change. Refusing on a moved sha turns the lost update into a
	// retry the agent can see.
	// Refuse a caller who cannot write BEFORE diagnosing their old_string.
	// Reversed, a read-only agent was told to "pass replace_all or include
	// more context", acted on it, and only then learned it could not write —
	// two wasted calls and a misleading first answer.
	if _, req, p, perr := s.projectReq(ctx, project, PermRead); perr == nil {
		if !atLeast(s.pathPermOf(req, p, rest), PermWrite) {
			return "", fmt.Errorf("you cannot write to %s", in.Path)
		}
	}
	// edit is a read-modify-write, so the critical section is the WHOLE
	// operation, not just the sha check — two concurrent edits each replacing
	// the one occurrence of a token both reported "1 replacement(s)".
	defer lockPath(project, rest)()
	if in.SHA != "" {
		if err := s.checkStale(ctx, project, rest, in.SHA, in.Path); err != nil {
			return "", err
		}
	}
	_, v, err := s.projectVolume(project)
	if err != nil {
		return "", err
	}
	rc, err := v.source.Open(ctx, rest, fi)
	if err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(rc, maxReadBytes+1))
	rc.Close()
	if err != nil {
		return "", err
	}
	if isBinary(data) {
		return "", fmt.Errorf("%s is a binary file; edit only works on text", in.Path)
	}
	body := string(data)
	n := strings.Count(body, in.OldString)
	switch {
	case n == 0:
		return "", fmt.Errorf("old_string not found in %s", in.Path)
	case n > 1 && !in.ReplaceAll:
		return "", fmt.Errorf("old_string appears %d times in %s; pass replace_all or include more context", n, in.Path)
	}
	if in.OldString == in.NewString {
		// A no-op edit used to journal a new version anyway, invalidating
		// every teammate's held sha for a change that changed nothing.
		return fmt.Sprintf("%s already reads that way; nothing to do", in.Path), nil
	}
	if in.ReplaceAll {
		body = strings.ReplaceAll(body, in.OldString, in.NewString)
	} else {
		body = strings.Replace(body, in.OldString, in.NewString, 1)
	}
	msg, err := s.putFile(ctx, project, rest, []byte(body), "edited")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s — %d replacement(s)", msg, n), nil
}

func (s *Server) mcpDelete(ctx context.Context, in pathIn) (string, error) {
	// "/p/" looks exactly like the folder form that DOES work ("/p/tmp/"), so
	// the generic "path must name a file" hint reads as a syntax complaint
	// about a request that is really being refused on purpose.
	if proj, rest := splitMCPPath(in.Path); proj != "" && rest == "" {
		return "", fmt.Errorf("%s is a whole project; this tool deletes files and folders inside one. "+
			"Delete the project from the hub if that is what you mean", in.Path)
	}
	project, rest, err := s.splitGranted(ctx, in.Path)
	if err != nil {
		return "", err
	}
	var targets []string
	if strings.HasSuffix(in.Path, "/") {
		files, _, err := s.visibleIn(ctx, project, PermRead)
		if err != nil {
			return "", err
		}
		for f := range files {
			if strings.HasPrefix(f, rest+"/") {
				targets = append(targets, f)
			}
		}
		if len(targets) == 0 {
			return "", fmt.Errorf("nothing under %s", in.Path)
		}
		sort.Strings(targets)
	} else {
		// "no such file: /p/scratch" directly contradicts the listing that
		// just showed six files under scratch/. Name the actual fix.
		files, _, err := s.visibleIn(ctx, project, PermRead)
		if err != nil {
			return "", err
		}
		if _, isFile := files[rest]; !isFile && hasPrefixIn(files, rest+"/") {
			return "", fmt.Errorf("%s is a folder. Pass %s/ (with a trailing slash) to delete everything under it",
				in.Path, strings.TrimSuffix(in.Path, "/"))
		}
		targets = []string{rest}
	}
	// ponytail: one /remove call per path rather than a batch endpoint. A
	// folder delete is rare and human-sized; add a batch route if an agent
	// ever removes thousands at once.
	for _, t := range targets {
		body, _ := json.Marshal(map[string]string{"path": t})
		w, err := s.call(ctx, http.MethodPost,
			"/api/p/"+url.PathEscape(project)+"/remove", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		if w.code < 200 || w.code >= 300 {
			return "", fmt.Errorf("could not delete %s: %w", labelPath(s.projectLabels(ctx), project, t), callErr(w))
		}
	}
	// A folder delete always reports what it removed, even when that is one
	// file: "deleted /proj/a/" reads as though a directory went away, and the
	// agent's next move depends on knowing it was c.md inside it.
	if !strings.HasSuffix(in.Path, "/") {
		return "deleted " + in.Path, nil
	}
	labels := s.projectLabels(ctx)
	var b strings.Builder
	fmt.Fprintf(&b, "deleted %d file(s) under %s:\n", len(targets), in.Path)
	for i, t := range targets {
		if i >= 50 {
			fmt.Fprintf(&b, "  ... and %d more\n", len(targets)-i)
			break
		}
		fmt.Fprintf(&b, "  %s\n", labelPath(labels, project, t))
	}
	return b.String(), nil
}

func (s *Server) mcpMove(ctx context.Context, in moveIn) (string, error) {
	fromP, fromR := splitMCPPath(in.From)
	toP, toR := splitMCPPath(in.To)
	if fromP == "" || fromR == "" || toP == "" || toR == "" {
		return "", fmt.Errorf("both from and to must name a file inside a project")
	}
	fromP, err := s.resolveProject(ctx, fromP)
	if err != nil {
		return "", err
	}
	if toP, err = s.resolveProject(ctx, toP); err != nil {
		return "", err
	}
	if fromP != toP {
		return "", fmt.Errorf("move works within one project; copy the content across instead")
	}
	// from == to is a no-op, and it MUST be: move is write-then-delete, so
	// moving a file onto itself wrote the bytes and then deleted the path it
	// had just written — destroying the file and reporting "moved". Normalizing
	// a batch of filenames is a stock agent task, and in any such batch the
	// files already correctly named are exactly the ones this hit.
	if fromR == toR {
		return fmt.Sprintf("%s is already at that path; nothing to do", in.From), nil
	}
	files, _, err2 := s.visibleIn(ctx, fromP, PermRead)
	if err2 != nil {
		return "", err2
	}
	fi, ok := files[fromR]
	if !ok {
		// A folder is a plausible thing to try to move, and "no such file"
		// contradicts the listing the agent just saw.
		if hasPrefixIn(files, fromR+"/") {
			return "", fmt.Errorf("%s is a folder; move handles one file at a time. "+
				"List it and move each file, or write the files at their new paths", in.From)
		}
		return "", fmt.Errorf("no such file: %s", in.From)
	}
	// Moving onto an existing file destroys it. write is documented as
	// "create or overwrite" so clobbering there is the contract; move is
	// documented as "move or rename", which does not imply destroying an
	// unrelated file that happens to be in the way.
	//
	// ponytail: checked against the snapshot, so a file created in the gap
	// between this check and the write is still overwritten. The journal keeps
	// every version either way, and closing the race needs a conditional write
	// the upload endpoint does not offer — add one there if this ever bites.
	// A trailing slash on the destination reads as "move it INTO this folder"
	// to anyone who types it, and silently created a FILE named after the
	// folder. write refuses the same shape; move must agree.
	if strings.HasSuffix(strings.TrimSpace(in.To), "/") {
		return "", fmt.Errorf("%s ends in a slash. Name the destination FILE, e.g. %s%s",
			in.To, in.To, path.Base(fromR))
	}
	// A file named like an existing folder is the same collision write and
	// delete already refuse: `move notes.md docs` means "into docs/" to the
	// agent that typed it, and silently created a junk file called docs.
	if err := s.checkNoCollision(ctx, toP, toR, in.To); err != nil {
		return "", err
	}
	if victim, taken := files[toR]; taken {
		return "", fmt.Errorf("%s already exists (%s, sha %s). "+
			"Delete it first if you mean to replace it, or move to a different path",
			in.To, humanSize(victim.Size), short(victim.Blob))
	}
	_, v, err := s.projectVolume(fromP)
	if err != nil {
		return "", err
	}
	// Bounded: move is a read-and-rewrite (the hub has no rename endpoint), so
	// an unbounded ReadAll here would pull an arbitrarily large blob into the
	// hub's memory on one tool call. Every other reader in this file is
	// capped; this one was not.
	if fi.Size > maxMoveBytes {
		return "", fmt.Errorf("%s is %s; move copies content and is capped at %s. "+
			"Use the viewer or a synced device for files this large",
			in.From, humanSize(fi.Size), humanSize(maxMoveBytes))
	}
	rc, err := v.source.Open(ctx, fromR, fi)
	if err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(rc, maxMoveBytes+1))
	rc.Close()
	if err != nil {
		return "", err
	}
	// ponytail: write-then-delete, not an atomic rename — the hub has no move
	// endpoint, and a put + delete is exactly what one would journal anyway.
	// The window where both paths exist is one cycle; if a crash lands in it,
	// the copy survives and nothing is lost.
	if _, err := s.putFile(ctx, toP, toR, data, "wrote"); err != nil {
		return "", err
	}
	if _, err := s.mcpDelete(ctx, pathIn{Path: in.From}); err != nil {
		return "", fmt.Errorf("copied to %s but could not remove %s: %w", in.To, in.From, err)
	}
	return fmt.Sprintf("moved %s → %s\n%s", in.From, in.To, urlLine(ctx, toP, toR)), nil
}

func (s *Server) mcpHistory(ctx context.Context, in historyIn) (string, error) {
	project, rest, err := s.splitGranted(ctx, in.Path)
	if err != nil {
		return "", err
	}
	w, err := s.call(ctx, http.MethodGet,
		"/api/p/"+url.PathEscape(project)+"/history?path="+url.QueryEscape(rest), nil)
	if err != nil {
		return "", err
	}
	if w.code < 200 || w.code >= 300 {
		return "", callErr(w)
	}
	// HistoryEntry, not a local copy of its fields: a hand-rolled struct here
	// is a second definition of the wire format that silently decodes to zero
	// values the day the real one changes.
	var resp struct {
		Entries []HistoryEntry `json:"entries"`
	}
	if err := json.Unmarshal(w.body.Bytes(), &resp); err != nil {
		return "", fmt.Errorf("could not read history: %v", err)
	}
	if len(resp.Entries) == 0 {
		// "No history for X" reads as a factual claim that X is unversioned.
		// For a folder, every file under it HAS history; for a typo, the file
		// does not exist. Both were answered the same way.
		files, _, ferr := s.visibleIn(ctx, project, PermRead)
		if ferr == nil {
			if hasPrefixIn(files, rest+"/") {
				return "", fmt.Errorf("%s is a folder; history is per file. Use list to see what is in it", in.Path)
			}
			if _, ok := files[rest]; !ok {
				return "", fmt.Errorf("no such file: %s", in.Path)
			}
		}
		return fmt.Sprintf("No history for %s.", in.Path), nil
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	var b strings.Builder
	fmt.Fprintf(&b, "History of %s (newest first):\n", in.Path)
	b.WriteString(urlLine(ctx, project, rest))
	for i, e := range resp.Entries {
		if i >= limit {
			fmt.Fprintf(&b, "... %d older version(s)\n", len(resp.Entries)-i)
			break
		}
		who := e.UserName
		if who == "" {
			who = e.User
		}
		if who == "" {
			who = e.Author
		}
		if who == "" {
			who = "unknown"
		}
		kind := e.Kind
		if kind == "" {
			kind = "put"
		}
		// A delete has no content, so it has no sha. Printing "sha:" with
		// nothing after it is a column that parses as present-but-empty.
		sha := "-"
		if e.Blob != "" {
			sha = "sha:" + short(e.Blob)
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\n", e.Time, kind, who, humanSize(e.Size), sha)
	}
	return b.String(), nil
}

func (s *Server) mcpRestore(ctx context.Context, in restoreIn) (string, error) {
	project, rest, err := s.splitGranted(ctx, in.Path)
	if err != nil {
		return "", err
	}
	// history prints short shas, so demanding 64 chars here made restore
	// unreachable through MCP: the only tool that names a version emitted a
	// value the only tool that consumes one refused. Resolve a prefix against
	// this file's own history, the way git does — and refuse an ambiguous one
	// rather than guessing which version the agent meant.
	sha, err := s.resolveSHA(ctx, project, rest, in.SHA)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]string{"path": rest, "sha": sha})
	w, err := s.call(ctx, http.MethodPost,
		"/api/p/"+url.PathEscape(project)+"/restore", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	if w.code < 200 || w.code >= 300 {
		return "", callErr(w)
	}
	return fmt.Sprintf("restored %s to version %s\n%s", in.Path, short(sha),
		urlLine(ctx, project, rest)), nil
}

// ---- small helpers ----

// matchGlob matches a path against a glob, with ** meaning "any number of
// directory levels" ANYWHERE in the pattern.
//
// The old implementation special-cased a leading "**/" and handed everything
// else to path.Match, where ** collapses to * and stops at a slash. So
// "g/**/*.log" found exactly one level: `glob("tmp/**/*.log")` returned 1 of 7
// files, the agent deleted it and reported the cleanup done. A plausible short
// answer is worse than an error, because nothing about it looks wrong.
func matchGlob(pattern, name string) bool {
	if pattern == "" {
		return true
	}
	if matchParts(strings.Split(pattern, "/"), strings.Split(name, "/")) {
		return true
	}
	// A bare "*.md" should also match "docs/a.md": agents write the pattern
	// they would type in a shell and expect a recursive search.
	if !strings.Contains(pattern, "/") {
		return matchParts([]string{"**", pattern}, strings.Split(name, "/"))
	}
	return false
}

// matchParts matches segment lists, where "**" consumes zero or more segments.
func matchParts(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// Trailing ** matches whatever is left, including nothing.
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchParts(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], name[0]); err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

func isBinary(b []byte) bool {
	if len(b) > binaryScanHead {
		b = b[:binaryScanHead]
	}
	return bytes.IndexByte(b, 0) >= 0
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	}
}

// exampleProject names a real connected project in an error message, so the
// agent's next attempt has a working path in it rather than a placeholder.
func exampleProject(s *Server, ctx context.Context) string {
	if ps := s.grantProjects(ctx); len(ps) > 0 {
		return ps[0].ID
	}
	return "project"
}

// ---- hooks the OAuth consent screen needs from the server ----

// SessionUser resolves a BROWSER session — cookie only. The consent screen
// uses it to decide who is granting access, and it must never accept a Bearer
// token: an MCP access token that could reach the consent screen could widen
// its own project set without a human in the loop.
func (s *Server) SessionUser(r *http.Request) (User, bool) {
	if s.Auth == nil {
		return User{}, false
	}
	if r.Header.Get("Authorization") != "" {
		// Ask the provider about the cookie alone, not the header the caller
		// supplied.
		r = r.Clone(r.Context())
		r.Header.Del("Authorization")
	}
	u, ok := s.Auth.Authenticate(r)
	return u, ok && u.Email != ""
}

// ConnectableProjects is what an account may offer to an MCP client: every
// project it can already read, each carrying the level it holds there.
//
// Read, not write, is the bar: connecting a project you can only view is a
// legitimate thing to want, and the grant inherits the same read ceiling.
func (s *Server) ConnectableProjects(email string) []MCPProject {
	if s.Projects == nil || s.Dir == nil {
		return nil
	}
	email = normEmail(email)
	var out []MCPProject
	for _, p := range s.Projects.List() {
		if p.Org == "" {
			continue
		}
		level := s.projectPermFor(p, email)
		if !atLeast(level, PermRead) {
			continue
		}
		out = append(out, MCPProject{Project: p, Level: level})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// OrgName is an org's display name, for the consent screen's grouping.
func (s *Server) OrgName(id string) string {
	if s.Dir == nil {
		return id
	}
	if o, ok := s.Dir.Get(id); ok && o.Name != "" {
		return o.Name
	}
	return id
}

// recordAgentRead files an MCP read against the grant that made it. Telemetry
// degrades silently: it must never fail a tool call.
func (s *Server) recordAgentRead(r *http.Request, path string) {
	if s.Reads == nil {
		return
	}
	project := projectID(r)
	if project == "" {
		return
	}
	actor := "mcp"
	if g, ok := grantFrom(r.Context()); ok {
		actor = "mcp:" + g.ID
	}
	s.Reads.Record(project, path, ReadKindAgent, actor)
}

// mcpText is the one-line text result every tool but read returns.
func mcpText(format string, args ...any) []mcp.Content {
	return []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}}
}

// hasPrefixIn reports whether any known path starts with prefix — "is this a
// folder", asked of the listing the caller can already see.
func hasPrefixIn(files map[string]FileInfo, prefix string) bool {
	for p := range files {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// resolveSHA expands a short sha to the full one, scoped to one file's own
// history. Scoping is not just convenience: it is what keeps a prefix from
// reaching a blob belonging to a file the caller never asked about.
func (s *Server) resolveSHA(ctx context.Context, project, path, want string) (string, error) {
	want = strings.TrimSpace(want)
	if want == "" {
		return "", fmt.Errorf("sha is required; run history to see the versions of this file")
	}
	if len(want) == 64 {
		return want, nil
	}
	if len(want) < 6 {
		return "", fmt.Errorf("sha %q is too short to identify a version; use at least 6 characters", want)
	}
	if lower := strings.ToLower(want); lower != want {
		want = lower // shas are printed lowercase; accept the shouted form
	}
	w, err := s.call(ctx, http.MethodGet,
		"/api/p/"+url.PathEscape(project)+"/history?path="+url.QueryEscape(path), nil)
	if err != nil {
		return "", err
	}
	if w.code < 200 || w.code >= 300 {
		return "", callErr(w)
	}
	var resp struct {
		Entries []HistoryEntry `json:"entries"`
	}
	if err := json.Unmarshal(w.body.Bytes(), &resp); err != nil {
		return "", err
	}
	if len(resp.Entries) == 0 {
		return "", fmt.Errorf("no such file: %s (it has no history to restore from)",
			labelPath(s.projectLabels(ctx), project, path))
	}
	seen := map[string]bool{}
	for _, e := range resp.Entries {
		if e.Blob != "" && strings.HasPrefix(e.Blob, want) {
			seen[e.Blob] = true
		}
	}
	switch len(seen) {
	case 0:
		return "", fmt.Errorf("no version of %s has a sha starting with %s; run history to see them",
			labelPath(s.projectLabels(ctx), project, path), want)
	case 1:
		for full := range seen {
			return full, nil
		}
	}
	return "", fmt.Errorf("sha %q matches %d versions of that file; use more characters", want, len(seen))
}

// checkGlob rejects a pattern path.Match cannot parse, and the leading slash
// agents reach for because every path in this API is absolute.
func checkGlob(pattern string) error {
	if pattern == "" {
		return nil
	}
	if strings.HasPrefix(pattern, "/") {
		return fmt.Errorf("glob patterns are relative to the search path, so %q cannot match. "+
			"Drop the leading slash (use %q), and pass `path` to scope the search",
			pattern, strings.TrimLeft(pattern, "/"))
	}
	if strings.ContainsAny(pattern, "{}") {
		return fmt.Errorf("glob pattern %s uses brace expansion, which is not supported. "+
			"Run one pattern per extension (e.g. **/*.go then **/*.md), or use grep with a regex", pattern)
	}
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, "probe"); err != nil {
			return fmt.Errorf("bad glob pattern %s: %v", pattern, err)
		}
	}
	return nil
}

// RequestUser is requestUser for callers outside this package (cmd/bdrive
// wires it into MCPAuth so the CLI can manage connections).
func (s *Server) RequestUser(r *http.Request) User { return s.requestUser(r) }

// relTo is path relative to a search prefix — "" when the prefix is the whole
// path, and path unchanged when there is no prefix.
func relTo(prefix, path string) string {
	if prefix == "" {
		return path
	}
	return strings.TrimPrefix(strings.TrimPrefix(path, prefix), "/")
}

// looksLikeImage sniffs the magic bytes of the formats a browser renders, so
// read answers on content rather than on a filename anyone can choose.
func looksLikeImage(b []byte) bool {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return true
	case bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}): // JPEG
		return true
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return true
	case bytes.HasPrefix(b, []byte("RIFF")) && len(b) > 12 && bytes.Equal(b[8:12], []byte("WEBP")):
		return true
	case bytes.HasPrefix(b, []byte("<svg")), bytes.Contains(firstBytes(b, 256), []byte("<svg")):
		return true
	default:
		return false
	}
}

func firstBytes(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

// checkNoCollision refuses a path that would make a file and a folder share a
// name, in either direction. The paths here are synced to real filesystems,
// where that state cannot exist — so accepting it produces a project that
// looks fine through this door and breaks every teammate's device.
func (s *Server) checkNoCollision(ctx context.Context, project, rest, shown string) error {
	files, _, err := s.visibleIn(ctx, project, PermRead)
	if err != nil {
		return err
	}
	if hasPrefixIn(files, rest+"/") {
		return fmt.Errorf("%s is a folder; name a file inside it instead (e.g. %s/<name>)",
			shown, strings.TrimSuffix(shown, "/"))
	}
	// And the reverse: writing under something that is already a file.
	for seg := rest; ; {
		i := strings.LastIndexByte(seg, '/')
		if i < 0 {
			return nil
		}
		seg = seg[:i]
		if _, isFile := files[seg]; isFile {
			labels := s.projectLabels(ctx)
			return fmt.Errorf("cannot write %s: %s is a file, so it cannot also be a folder. "+
				"Delete or rename it first", shown, labelPath(labels, project, seg))
		}
	}
}

// grepCaveats names what the search could NOT read. It rides on every answer,
// including "no matches" — which is precisely when it matters, and precisely
// where it used to be omitted: "No matches in 1 file(s) searched" for a file
// that was skipped entirely is worse than silence, because it claims a search
// that never happened.
func grepCaveats(binary, unreadable []string) string {
	var b strings.Builder
	if n := len(binary); n > 0 {
		fmt.Fprintf(&b, "\n[skipped %d binary file(s): %s]\n", n, sample(binary, 5))
	}
	if n := len(unreadable); n > 0 {
		fmt.Fprintf(&b, "\n[%d file(s) contain a line longer than %s; only the first %s of those "+
			"lines was searched, so they may still contain matches: %s]\n",
			n, humanSize(maxGrepLine), humanSize(maxGrepLine), sample(unreadable, 5))
	}
	return b.String()
}

func sample(list []string, n int) string {
	if len(list) <= n {
		return strings.Join(list, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(list[:n], ", "), len(list)-n)
}

// isUnpageable reports whether a file has no line break the pager could use in
// the stretch we have in hand — a minified bundle, a one-line JSON dump.
func isUnpageable(head []byte) bool {
	return !bytes.Contains(head, []byte("\n"))
}

// eachLine walks a reader line by line, TRUNCATING any line longer than max
// rather than failing the file.
//
// This exists because bufio.Scanner does the opposite: one over-long line
// makes it error, and both read and grep treated that as "this file cannot be
// handled". A 4,002-line log with a single dumped payload in it was therefore
// unreachable by every tool at once — paged read refused it, raw refused it,
// grep called it unsearchable — and the three errors pointed at each other.
// The shape is common: a jq -c dump, a base64 blob in a log line, a minified
// bundle, a CSV written without newlines.
//
// Truncating is the honest trade. The caller is told which lines were cut, so
// a partial answer is never mistaken for a complete one — and a partial answer
// about line 2,002 beats a confident "no matches" about the whole file.
func eachLine(r io.Reader, max int, fn func(n int, line string, truncated bool) bool) error {
	br := bufio.NewReaderSize(r, 64<<10)
	for n := 1; ; n++ {
		var sb strings.Builder
		truncated := false
		for {
			chunk, isPrefix, err := br.ReadLine()
			if err == io.EOF {
				if sb.Len() > 0 {
					fn(n, sb.String(), truncated)
				}
				return nil
			}
			if err != nil {
				return err
			}
			if room := max - sb.Len(); room > 0 {
				if len(chunk) > room {
					sb.Write(chunk[:room])
					truncated = true
				} else {
					sb.Write(chunk)
				}
			} else if len(chunk) > 0 {
				truncated = true
			}
			if !isPrefix {
				break
			}
		}
		if !fn(n, sb.String(), truncated) {
			return nil
		}
	}
}

func plural(n int) string {
	if n > 1 {
		return " and others"
	}
	return ""
}

// shaLike reports whether a string could be a content address at all.
func shaLike(s string) bool {
	if len(s) < 6 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// maxComponent is the longest single path segment any common filesystem will
// hold. Journaling a longer one produces a file that syncs to no device.
const maxComponent = 255

func checkComponents(rest, shown string) error {
	for _, seg := range strings.Split(rest, "/") {
		if len(seg) > maxComponent {
			return fmt.Errorf("a name in %s is %d bytes; filesystems cap one name at %d, "+
				"so this file could never sync to a device", shown, len(seg), maxComponent)
		}
	}
	return nil
}

// clipAroundMatch shortens a long line for display, keeping the MATCH visible.
//
// Taking the first 400 characters could elide the very token that matched, so
// a real hit came back looking like a false positive — the one thing a search
// result must never look like. The cut is always marked, the way read marks
// its own.
func clipAroundMatch(text string, re *regexp.Regexp, isMatch bool) string {
	const window = 400
	if len(text) <= window {
		return text
	}
	start := 0
	if isMatch {
		if loc := re.FindStringIndex(text); loc != nil {
			start = loc[0] - window/3
			if start < 0 {
				start = 0
			}
		}
	}
	end := start + window
	if end > len(text) {
		end = len(text)
	}
	out := text[start:end]
	if start > 0 {
		out = "…[+" + humanSize(int64(start)) + "] " + out
	}
	if end < len(text) {
		out += " …[+" + humanSize(int64(len(text)-end)) + "]"
	}
	return out
}
