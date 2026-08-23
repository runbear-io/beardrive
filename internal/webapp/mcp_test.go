package webapp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// mcpHub is permHubAt plus a read ledger and a seeded journal: alice/bob/carol
// are members of the project's org and dave is not, which is what the
// non-member parity test needs.
func mcpHub(t *testing.T) (http.Handler, *Server, Project, map[string]*http.Cookie, *fakeRemote) {
	t.Helper()
	h, srv, cookies, p, root := permHubAt(t)
	var err error
	srv.Reads, err = OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 0)
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.putAs("dev1", "alice@x.io", "Alice", "guide.md", "# guide")
	f.putAs("dev1", "alice@x.io", "Alice", "notes/plan.md", "- step one")
	return h, srv, p, cookies, f
}

// rpc posts one JSON-RPC object and decodes the envelope. The recorder comes
// back too: the non-member test asserts on the raw body, which must not be a
// JSON-RPC error at all.
func rpc(t *testing.T, h http.Handler, p Project, c *http.Cookie, method string, params any) (rpcResponse, *http.Response, string) {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/mcp", body, c)
	var out rpcResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out, rec.Result(), rec.Body.String()
}

// callTool runs one tools/call and unwraps the text content block back into
// the structure the tool returned.
func callTool(t *testing.T, h http.Handler, p Project, c *http.Cookie, name string, args map[string]any) (map[string]any, *rpcError) {
	t.Helper()
	res, _, raw := rpc(t, h, p, c, "tools/call", map[string]any{"name": name, "arguments": args})
	if res.Error != nil {
		return nil, res.Error
	}
	var wrap struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	data, _ := json.Marshal(res.Result)
	if err := json.Unmarshal(data, &wrap); err != nil || len(wrap.Content) != 1 {
		t.Fatalf("tool %s: unreadable result %s", name, raw)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(wrap.Content[0].Text), &out); err != nil {
		t.Fatalf("tool %s: content is not JSON: %v (%s)", name, err, wrap.Content[0].Text)
	}
	return out, nil
}

func heatOf(t *testing.T, h http.Handler, p Project, c *http.Cookie, path string) HeatEntry {
	t.Helper()
	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/heat?days=30", nil, c)
	if rec.Code != 200 {
		t.Fatalf("heat: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Entries map[string]HeatEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Entries[path]
}

// The wire: initialize, exactly three tools, and an unknown method is a
// JSON-RPC error rather than an HTTP one.
func TestMCPProtocol(t *testing.T) {
	h, _, p, c, _ := mcpHub(t)

	res, _, raw := rpc(t, h, p, c["alice"], "initialize", map[string]any{"protocolVersion": mcpProtocolVersion})
	if res.Error != nil {
		t.Fatalf("initialize: %+v", res.Error)
	}
	init, _ := res.Result.(map[string]any)
	if init["protocolVersion"] != mcpProtocolVersion {
		t.Fatalf("initialize: %s", raw)
	}
	if caps, _ := init["capabilities"].(map[string]any); caps["tools"] == nil {
		t.Fatalf("initialize declares no tools capability: %s", raw)
	}

	res, _, raw = rpc(t, h, p, c["alice"], "tools/list", nil)
	if res.Error != nil {
		t.Fatalf("tools/list: %+v", res.Error)
	}
	var list struct {
		Tools []mcpTool `json:"tools"`
	}
	data, _ := json.Marshal(res.Result)
	json.Unmarshal(data, &list)
	var names []string
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
	}
	if got := strings.Join(names, ","); got != "list_files,read_file,file_history" {
		t.Fatalf("tools = %q, want exactly the three read tools (%s)", got, raw)
	}

	if res, _, _ = rpc(t, h, p, c["alice"], "resources/list", nil); res.Error == nil || res.Error.Code != rpcMethodNotFound {
		t.Fatalf("unknown method: %+v, want -32601", res.Error)
	}
	// notifications carry no id and get no body.
	if _, resp, body := rpc(t, h, p, c["alice"], "notifications/initialized", nil); resp.StatusCode != http.StatusAccepted || body != "" {
		t.Fatalf("notifications/initialized: %d %q, want 202 and empty", resp.StatusCode, body)
	}
}

// A non-member learns exactly what they learn from any other project route:
// the same status, and nothing that says an MCP endpoint is there at all.
func TestMCPNonMemberParity(t *testing.T) {
	h, _, p, c, _ := mcpHub(t)

	tree := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, c["dave"])
	mcp := doAs(t, h, "POST", "/api/p/"+p.ID+"/mcp",
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, c["dave"])
	if mcp.Code != tree.Code {
		t.Fatalf("mcp %d vs tree %d for a non-member: statuses must match", mcp.Code, tree.Code)
	}
	if mcp.Code == 200 {
		t.Fatalf("a non-member reached the endpoint: %s", mcp.Body)
	}
	if strings.Contains(mcp.Body.String(), "jsonrpc") || strings.Contains(mcp.Body.String(), "read_file") {
		t.Fatalf("non-member got a JSON-RPC body: %s", mcp.Body)
	}
	// and unauthenticated is 401 from authGate, still with no protocol body
	anon := doAs(t, h, "POST", "/api/p/"+p.ID+"/mcp",
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, nil)
	if anon.Code != http.StatusUnauthorized || strings.Contains(anon.Body.String(), "jsonrpc") {
		t.Fatalf("anonymous: %d %s, want 401 with no JSON-RPC body", anon.Code, anon.Body)
	}
}

// read_file reads a live path, and a path that moved resolves forward to the
// file's new address rather than 404ing at the caller's stale one.
func TestMCPReadFileFollowsRenames(t *testing.T) {
	h, _, p, c, f := mcpHub(t)
	at := time.Now().UTC().Add(-2 * time.Hour)
	f.putAt("dev1", "old.md", "moved body", at)
	f.move("dev1", "old.md", "docs/new.md", "moved body", at.Add(time.Hour))

	out, rerr := callTool(t, h, p, c["alice"], "read_file", map[string]any{"path": "guide.md"})
	if rerr != nil {
		t.Fatalf("read live: %+v", rerr)
	}
	if out["content"] != "# guide" || out["path"] != "guide.md" {
		t.Fatalf("read live: %+v", out)
	}

	out, rerr = callTool(t, h, p, c["alice"], "read_file", map[string]any{"path": "old.md"})
	if rerr != nil {
		t.Fatalf("read moved: %+v", rerr)
	}
	if out["path"] != "docs/new.md" || out["content"] != "moved body" {
		t.Fatalf("stale path did not resolve forward: %+v", out)
	}

	if _, rerr = callTool(t, h, p, c["alice"], "read_file", map[string]any{"path": "nope.md"}); rerr == nil {
		t.Fatal("missing file: want a JSON-RPC error")
	}
}

// The hole this feature would otherwise open: MCP traffic booked as people
// reading files. Assert the KIND, not the total.
func TestMCPReadsBookAsAgent(t *testing.T) {
	h, _, p, c, _ := mcpHub(t)

	if _, rerr := callTool(t, h, p, c["alice"], "read_file", map[string]any{"path": "guide.md"}); rerr != nil {
		t.Fatalf("read_file: %+v", rerr)
	}
	e := heatOf(t, h, p, c["alice"], "guide.md")
	if e.Agent != 1 || e.Human != 0 {
		t.Fatalf("heat = %+v, want agent 1 / human 0", e)
	}

	// The viewer still books humans after the signature change.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/render?path=guide.md", nil, c["bob"]); rec.Code != 200 {
		t.Fatalf("render: %d %s", rec.Code, rec.Body)
	}
	if e = heatOf(t, h, p, c["alice"], "guide.md"); e.Agent != 1 || e.Human != 1 {
		t.Fatalf("heat = %+v, want agent 1 / human 1", e)
	}
}

// /heat?by=device is the one API that reports actor ids. MCP's actor is a
// constant that no device can be registered under, so the row it adds can
// never be joined to a real machine — and no email appears anywhere.
func TestMCPHeatByDeviceStaysIdentityFree(t *testing.T) {
	h, _, p, c, _ := mcpHub(t)
	if _, rerr := callTool(t, h, p, c["alice"], "read_file", map[string]any{"path": "guide.md"}); rerr != nil {
		t.Fatalf("read_file: %+v", rerr)
	}
	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/heat?by=device&days=30", nil, c["alice"])
	if rec.Code != 200 {
		t.Fatalf("heat by device: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "@x.io") {
		t.Fatalf("an account email leaked into ?by=device: %s", rec.Body)
	}
	var out struct {
		Devices []deviceHeat `json:"devices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var row *deviceHeat
	for i := range out.Devices {
		if out.Devices[i].ID == mcpActor {
			row = &out.Devices[i]
		}
	}
	if row == nil {
		t.Fatalf("no %s row: %s", mcpActor, rec.Body)
	}
	// The whole point of the colon: deviceIDPattern cannot match it, so the
	// registry join has nothing to attach and the row stays anonymous.
	if row.Name != "" || row.OS != "" {
		t.Fatalf("%s carries an identity: %+v", mcpActor, *row)
	}
	if validDeviceID(mcpActor) {
		t.Fatalf("%q is a registrable device id — a member could claim it", mcpActor)
	}
}

// Listing and history are not reads. handleTree records nothing today, and
// history/blob views are never reads; MCP must not change that.
func TestMCPListAndHistoryRecordNothing(t *testing.T) {
	h, _, p, c, _ := mcpHub(t)

	out, rerr := callTool(t, h, p, c["alice"], "list_files", nil)
	if rerr != nil {
		t.Fatalf("list_files: %+v", rerr)
	}
	files, _ := out["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("list_files = %v, want 2 files", out)
	}
	if first, _ := files[0].(map[string]any); first["path"] != "guide.md" {
		t.Fatalf("list_files is not sorted by path: %v", files)
	}

	if out, rerr = callTool(t, h, p, c["alice"], "list_files", map[string]any{"prefix": "notes"}); rerr != nil {
		t.Fatalf("list_files prefix: %+v", rerr)
	}
	if files, _ = out["files"].([]any); len(files) != 1 {
		t.Fatalf("prefix=notes = %v, want just notes/plan.md", out)
	}

	if out, rerr = callTool(t, h, p, c["alice"], "file_history", map[string]any{"path": "guide.md"}); rerr != nil {
		t.Fatalf("file_history: %+v", rerr)
	}
	entries, _ := out["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("file_history = %v, want one version", out)
	}

	if e := heatOf(t, h, p, c["alice"], "guide.md"); e.Agent != 0 || e.Human != 0 || e.Share != 0 {
		t.Fatalf("listing and history recorded a read: %+v", e)
	}
}

// Two gates, and the second is the one that matters: fi.Size is client-
// declared, so a journal op that understates it must not turn into an
// unbounded read.
func TestMCPReadFileSizeCap(t *testing.T) {
	h, _, p, c, f := mcpHub(t)
	big := strings.Repeat("x", maxMCPRead+64)

	f.put("dev1", "huge.md", big)
	_, rerr := callTool(t, h, p, c["alice"], "read_file", map[string]any{"path": "huge.md"})
	if rerr == nil {
		t.Fatal("over-cap file: want a JSON-RPC error")
	}
	if !strings.Contains(rerr.Message, "1048576") {
		t.Fatalf("error does not name the limit: %q", rerr.Message)
	}

	// The same content behind an op that lies about its size.
	sum := sha256.Sum256([]byte(big))
	blob := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(f.dir, "blobs", blob), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	f.append("dev1", journal.Op{Kind: journal.KindPut, Path: "liar.md", Blob: blob, Size: 10, Mode: 0o644})
	if _, rerr = callTool(t, h, p, c["alice"], "read_file", map[string]any{"path": "liar.md"}); rerr == nil {
		t.Fatal("understated size: want a JSON-RPC error, not an unbounded read")
	}
	if e := heatOf(t, h, p, c["alice"], "liar.md"); e.Agent != 0 {
		t.Fatalf("a refused read was recorded: %+v", e)
	}
}

// The promise an agent reads before it calls anything: names, never contents.
func TestMCPToolDescriptionsSayNames(t *testing.T) {
	// Denials are the point, so they are removed before the promise scan —
	// otherwise "there is no content search" reads as one.
	denials := strings.NewReplacer(
		"no content search", "",
		"not file contents", "",
	)
	for _, tl := range mcpTools() {
		d := denials.Replace(strings.ToLower(tl.Description))
		if !strings.Contains(d, "name") {
			t.Errorf("%s does not say what it matches: %q", tl.Name, tl.Description)
		}
		for _, promise := range []string{"content search", "search content", "search the contents", "full-text"} {
			if strings.Contains(d, promise) {
				t.Errorf("%s promises content search (%q): %q", tl.Name, promise, tl.Description)
			}
		}
	}
	if !strings.Contains(mcpTools()[0].Description, "no content search") {
		t.Errorf("list_files must say there is no content search: %q", mcpTools()[0].Description)
	}
}
