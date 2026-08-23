package webapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"
)

// MCP is the read path for agents that cannot mount; hooks remain the write
// path.
//
// One endpoint — POST /api/p/{project}/mcp — speaking stateless
// Streamable-HTTP JSON-RPC 2.0, so an agent in CI, a cloud sandbox, or on a
// laptop that never ran `bdrive init` can list, read and inspect the history
// of a project's files with no daemon and no clone. Three read-only tools,
// each an envelope over a handler that already exists: list_files over
// snapshot, read_file over lookup+Open, file_history over handleHistory.
//
// Auth and permission are entirely upstream: authGate 401s an
// unauthenticated /api/ path and proj() answers 404/403 indistinguishably for
// a non-member, both BEFORE handleMCP runs. A second check here would only
// create a way for the two to disagree. Failures at that layer are HTTP
// statuses with no JSON-RPC body — a client that isn't a member never gets
// far enough to see a tool list.
//
// There is no content search, and the tool descriptions say so: matching is
// on file NAMES. Hub-side content search needs an index the storage layer has
// no home for yet (ROADMAP), and doing it without one means an object-store
// GET per file per query.
const (
	mcpProtocolVersion = "2025-06-18"
	mcpServerVersion   = "1"

	// maxMCPBody bounds a tool call; generous for JSON-RPC params.
	maxMCPBody = 64 << 10
	// maxMCPRead bounds read_file. Enforced TWICE — see readMCPFile.
	maxMCPRead = 1 << 20

	mcpListDefault = 500
	mcpListMax     = 5000

	// mcpActor is the read-heat actor for MCP traffic. The colon is
	// deliberate: it is outside deviceIDPattern (devices.go), so no device
	// can ever be registered under this id and the registry join in
	// heatByDevice can never attach a real machine's name and OS to it. A
	// constant carries no identity, which is what keeps /heat identity-free.
	mcpActor = "mcp:remote"
)

// JSON-RPC 2.0 error codes used here.
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternalError  = -32603
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// mcpTool is one entry of tools/list. Descriptions are the contract an agent
// reads before it calls anything, so they say "names" where names is what is
// matched — see TestMCPToolDescriptionsSayNames.
type mcpTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func mcpTools() []mcpTool {
	return []mcpTool{{
		Name: "list_files",
		Description: "List files in the project. Optionally filter by a path prefix — " +
			"this matches file names and folder paths, not file contents. " +
			"There is no content search.",
		InputSchema: obj(map[string]any{
			"prefix": map[string]any{"type": "string", "description": "only files under this folder, by name"},
			"limit":  map[string]any{"type": "integer", "description": "max files to return (default 500)"},
		}),
	}, {
		Name: "read_file",
		Description: "Read the current text content of one file, addressed by its path name. " +
			"A path that was renamed resolves forward to the file's live name.",
		InputSchema: obj(map[string]any{
			"path": map[string]any{"type": "string", "description": "the file's path name"},
		}, "path"),
	}, {
		Name: "file_history",
		Description: "Versions of one file, newest first: who changed it, when, and whether it " +
			"was added, edited or deleted. Addressed by path name.",
		InputSchema: obj(map[string]any{
			"path":  map[string]any{"type": "string", "description": "the file's path name"},
			"limit": map[string]any{"type": "integer", "description": "max versions to return (default 100)"},
		}, "path"),
	}}
}

// handleMCP answers one JSON-RPC object. Transport failures (bad JSON, an
// oversized body) are HTTP statuses; protocol failures are JSON-RPC error
// objects with HTTP 200, which is what an MCP client expects to parse.
func (s *Server) handleMCP(v *volume, w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMCPBody))
	if err != nil {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	if t := bytes.TrimLeft(body, " \t\r\n"); len(t) > 0 && t[0] == '[' {
		// Batches would mean partial success across tools that record reads;
		// nothing in scope needs them.
		writeRPCError(w, nil, rpcInvalidRequest, "batch requests are not supported")
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCError(w, nil, rpcParseError, "invalid JSON")
		return
	}

	switch req.Method {
	case "initialize":
		writeRPCResult(w, req.ID, map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "beardrive", "version": mcpServerVersion},
		})
	case "notifications/initialized", "notifications/cancelled":
		w.WriteHeader(http.StatusAccepted)
	case "ping":
		writeRPCResult(w, req.ID, map[string]any{})
	case "tools/list":
		writeRPCResult(w, req.ID, map[string]any{"tools": mcpTools()})
	case "tools/call":
		s.mcpCall(v, w, r, req)
	default:
		writeRPCError(w, req.ID, rpcMethodNotFound, "unknown method: "+req.Method)
	}
}

func (s *Server) mcpCall(v *volume, w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var call struct {
		Name      string `json:"name"`
		Arguments struct {
			Path   string `json:"path"`
			Prefix string `json:"prefix"`
			Limit  int    `json:"limit"`
		} `json:"arguments"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &call); err != nil {
			writeRPCError(w, req.ID, rpcInvalidParams, "invalid params: "+err.Error())
			return
		}
	}
	a := call.Arguments
	var (
		out any
		err error
	)
	switch call.Name {
	case "list_files":
		out, err = s.mcpListFiles(v, r, a.Prefix, a.Limit)
	case "read_file":
		out, err = s.mcpReadFile(v, r, a.Path)
	case "file_history":
		out, err = s.mcpFileHistory(v, r, a.Path, a.Limit)
	default:
		writeRPCError(w, req.ID, rpcInvalidParams, "unknown tool: "+call.Name)
		return
	}
	if err != nil {
		writeRPCError(w, req.ID, rpcInternalError, err.Error())
		return
	}
	writeRPCResult(w, req.ID, map[string]any{"content": []any{
		map[string]any{"type": "text", "text": jsonText(out)},
	}})
}

// mcpFile is one row of list_files: enough to decide what to read next,
// with the same provenance the viewer shows.
type mcpFile struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Modified string `json:"modified,omitempty"`
	User     string `json:"user,omitempty"`
	UserName string `json:"user_name,omitempty"`
	Author   string `json:"author,omitempty"`
}

// mcpListFiles is a flat listing off the same snapshot the viewer serves —
// not buildTree's nested Node, which an agent would only have to flatten
// again. Records no read, matching handleTree.
func (s *Server) mcpListFiles(v *volume, r *http.Request, prefix string, limit int) (any, error) {
	snap, err := v.snapshot(r.Context())
	if err != nil {
		return nil, fmt.Errorf("content temporarily unavailable")
	}
	switch {
	case limit <= 0:
		limit = mcpListDefault
	case limit > mcpListMax:
		limit = mcpListMax
	}
	// Same folder semantics handleHistory uses (history.go), so the two tools
	// agree on what a prefix means.
	dir := ""
	if prefix != "" {
		dir = strings.TrimSuffix(prefix, "/") + "/"
	}
	files := make([]mcpFile, 0, len(snap.files))
	for p, fi := range snap.files {
		if dir != "" && !strings.HasPrefix(p, dir) {
			continue
		}
		f := mcpFile{Path: p, Size: fi.Size, User: fi.User, UserName: fi.UserName, Author: fi.Author}
		if !fi.Time.IsZero() {
			f.Modified = fi.Time.UTC().Format("2006-01-02T15:04:05Z")
		}
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	out := map[string]any{"total": len(files)}
	if len(files) > limit {
		files = files[:limit]
		out["truncated"] = true
	}
	out["files"] = files
	return out, nil
}

// mcpReadFile resolves through lookup — so a path that moved reads forward to
// the file's live address — and books the read as AGENT traffic against the
// path lookup returned, never the one the caller sent, or heat splits across
// a file's old and new addresses.
func (s *Server) mcpReadFile(v *volume, r *http.Request, path string) (any, error) {
	if path == "" {
		return nil, fmt.Errorf("read_file needs a path")
	}
	r2 := withQuery(r, url.Values{"path": {path}})
	p, fi, _, err := lookup(v, r2)
	if err != nil {
		return nil, err
	}
	// Two gates, not one. fi.Size comes off a journal op — JSON a client
	// pushed — so believing it is how an understated size turns into an
	// unbounded read.
	if fi.Size > maxMCPRead {
		return nil, fmt.Errorf("%s is %d bytes, over the %d byte read limit", p, fi.Size, maxMCPRead)
	}
	rc, err := v.source.Open(r2.Context(), p, fi)
	if err != nil {
		return nil, fmt.Errorf("content temporarily unavailable")
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxMCPRead+1))
	if err != nil {
		return nil, fmt.Errorf("content temporarily unavailable")
	}
	if len(data) > maxMCPRead {
		return nil, fmt.Errorf("%s is over the %d byte read limit", p, maxMCPRead)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%s is binary; this endpoint serves text only", p)
	}
	s.recordRead(r2, p, ReadKindAgent, mcpActor)
	return map[string]any{"path": p, "size": len(data), "content": string(data)}, nil
}

// mcpFileHistory captures handleHistory rather than refactoring it: the walk,
// sort, cursor and JSON write are one function body, and splitting them to
// serve a second caller is a bigger change than a ResponseWriter that keeps
// what it was handed. Records no read — history views never are.
func (s *Server) mcpFileHistory(v *volume, r *http.Request, path string, limit int) (any, error) {
	if path == "" {
		return nil, fmt.Errorf("file_history needs a path")
	}
	q := url.Values{"path": {path}}
	if limit > 0 {
		q.Set("n", fmt.Sprint(limit))
	}
	// The shallow clone keeps the request context, so projectID(r) and
	// deviceVisibleIn still resolve inside handleHistory.
	cap := &capture{code: http.StatusOK, hdr: http.Header{}}
	s.handleHistory(v, cap, withQuery(r, q))
	if cap.code != http.StatusOK {
		// handleHistory and storeSource write plain-text HTTP errors; they
		// must never be emitted as tool OUTPUT, where an agent would read
		// them as the file's history.
		return nil, fmt.Errorf("%s", strings.TrimSpace(cap.buf.String()))
	}
	return json.RawMessage(cap.buf.Bytes()), nil
}

// withQuery shallow-clones a request with a rewritten query string. lookup
// and handleHistory both read their inputs off r.URL.Query(), but JSON-RPC
// params arrive in the body; this is the cheapest bridge that keeps the
// project id in the context.
func withQuery(r *http.Request, q url.Values) *http.Request {
	r2 := r.Clone(r.Context())
	u := *r.URL
	u.RawQuery = q.Encode()
	r2.URL = &u
	return r2
}

// capture is a ResponseWriter that keeps what it was handed, so an existing
// handler can answer a tool call.
type capture struct {
	hdr  http.Header
	code int
	buf  bytes.Buffer
}

func (c *capture) Header() http.Header { return c.hdr }

func (c *capture) Write(b []byte) (int, error) { return c.buf.Write(b) }

func (c *capture) WriteHeader(code int) { c.code = code }

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	writeJSON(w, rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

// jsonText renders a tool result as the text block MCP clients read.
func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
