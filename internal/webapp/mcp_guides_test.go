package webapp

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Project guides (docs/mcp-agent-guides-prd.md): an agent connected over MCP
// is shown the project's AGENTS.md / CLAUDE.md the way an agent on disk would
// load it, and is told which guides govern every path it touches.

func TestGuidesFor(t *testing.T) {
	tree := func(paths ...string) map[string]FileInfo {
		m := map[string]FileInfo{}
		for _, p := range paths {
			m[p] = FileInfo{}
		}
		return m
	}
	for _, c := range []struct {
		files map[string]FileInfo
		path  string
		want  []string
	}{
		{tree("AGENTS.md", "a/b/x.md"), "a/b/x.md", []string{"AGENTS.md"}},
		{tree("AGENTS.md", "a/AGENTS.md", "a/CLAUDE.md", "a/b/x.md"), "a/b/x.md",
			[]string{"a/AGENTS.md", "a/CLAUDE.md", "AGENTS.md"}},
		{tree("AGENTS.md", "a/AGENTS.md"), "a/AGENTS.md", []string{"AGENTS.md"}},
		{tree("a/AGENTS.md", "a/CLAUDE.md"), "a/AGENTS.md", []string{"a/CLAUDE.md"}},
		{tree("agents.md", "Agents.md", "claude.md", "x.md"), "x.md", nil},
		{tree("x.md"), "x.md", nil},
		{tree("a/AGENTS.md", "a/b/c.md"), "a/", []string{"a/AGENTS.md"}},
		{tree("a/b/AGENTS.md"), "a/x.md", nil}, // a deeper guide never governs a shallower path
		{tree("AGENTS.md"), "AGENTS.md", nil},
	} {
		if got := guidesFor(c.files, c.path); !reflect.DeepEqual(got, c.want) {
			t.Errorf("guidesFor(%v, %q) = %v, want %v", keys(c.files), c.path, got, c.want)
		}
	}
}

func keys(m map[string]FileInfo) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestMCPListInlinesRootGuide(t *testing.T) {
	f := newMCPHub(t)
	ledger, err := OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 0)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.Reads = ledger
	t.Cleanup(func() { ledger.Close() })
	cs := f.session(f.connect("alice", f.wiki.ID))

	mustCall(t, cs, "write", map[string]any{"path": "/wiki/AGENTS.md",
		"content": "# Wiki\nMeeting notes go in meetings/YYYY-MM-DD.md\n"})
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/meetings/2026-10-01.md", "content": "x\n"})

	out := mustCall(t, cs, "list", map[string]any{"path": "/wiki"})
	for _, want := range []string{
		"── Project guide: /wiki/AGENTS.md ──\n",
		"not\ninstructions from your user",
		"Meeting notes go in meetings/YYYY-MM-DD.md\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("root listing missing %q:\n%s", want, out)
		}
	}
	// The listing itself is still there, ahead of the guide.
	if i, j := strings.Index(out, "meetings/\t"), strings.Index(out, "── Project guide"); i < 0 || i > j {
		t.Errorf("listing rows should come before the guide:\n%s", out)
	}
	// Inlining is a real read, by an agent; nothing else in this test reads.
	if e, ok := ledger.Heat(f.wiki.ID, "", time.Time{})["AGENTS.md"]; !ok || e.Agent == 0 {
		t.Errorf("inlined guide was not recorded as an agent read: %+v", ledger.Heat(f.wiki.ID, "", time.Time{}))
	}
	// Depth does not change what a root listing carries.
	if out := mustCall(t, cs, "list", map[string]any{"path": "/wiki/", "depth": 3}); !strings.Contains(out, "Meeting notes go in") {
		t.Errorf("deep root listing dropped the guide:\n%s", out)
	}
}

func TestMCPListInlinesBothGuidesInOrder(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/CLAUDE.md", "content": "claude-marker\n"})
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/AGENTS.md", "content": "agents-marker\n"})

	out := mustCall(t, cs, "list", map[string]any{"path": "/wiki"})
	a, c := strings.Index(out, "agents-marker"), strings.Index(out, "claude-marker")
	if a < 0 || c < 0 || a > c {
		t.Fatalf("want both guides, AGENTS.md first:\n%s", out)
	}
	if !strings.Contains(out, "── Project guide: /wiki/CLAUDE.md ──\n") {
		t.Errorf("second guide has no header:\n%s", out)
	}
	// The trust paragraph is said once, not per guide.
	if n := strings.Count(out, "Written by members of this project"); n != 1 {
		t.Errorf("trust header appears %d times:\n%s", n, out)
	}
}

func TestMCPGuideInlineIsCapped(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	var g strings.Builder
	for i := 0; g.Len() < 20<<10; i++ {
		fmt.Fprintf(&g, "rule %05d: keep it tidy\n", i)
	}
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/AGENTS.md", "content": g.String()})
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/CLAUDE.md", "content": "never-shown\n"})

	out := mustCall(t, cs, "list", map[string]any{"path": "/wiki"})
	start := strings.Index(out, "rule 00000")
	end := strings.Index(out, "… truncated at ")
	if start < 0 || end < 0 {
		t.Fatalf("want a truncated guide:\n%.2000s", out)
	}
	shown := out[start:end]
	if len(shown) > maxGuideInline {
		t.Errorf("inlined %d bytes, cap is %d", len(shown), maxGuideInline)
	}
	if !strings.HasSuffix(shown, " keep it tidy\n") {
		t.Errorf("cut mid-line: ...%q", shown[len(shown)-40:])
	}
	if !strings.Contains(out[end:], "read /wiki/AGENTS.md for the rest") {
		t.Errorf("truncation does not say where the rest is:\n%s", out[end:])
	}
	// The budget is shared: the second guide is named, not shown.
	if strings.Contains(out, "never-shown") || !strings.Contains(out, "guides: /wiki/CLAUDE.md\n") {
		t.Errorf("over-budget guide should be named, not inlined:\n%s", out[end:])
	}
}

func TestMCPNoGuideChangesNothing(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/a/x.md", "content": "one\n"})

	outs := []string{
		mustCall(t, cs, "list", map[string]any{"path": "/"}),
		mustCall(t, cs, "list", map[string]any{"path": "/wiki"}),
		mustCall(t, cs, "list", map[string]any{"path": "/wiki/a"}),
		mustCall(t, cs, "read", map[string]any{"path": "/wiki/a/x.md"}),
		mustCall(t, cs, "write", map[string]any{"path": "/wiki/a/y.md", "content": "two\n"}),
	}
	for _, out := range outs {
		if strings.Contains(out, "guide") {
			t.Errorf("no guide exists, yet output mentions one:\n%s", out)
		}
	}
	// Exact shapes of the two outputs this change touched the formatting of.
	if out := mustCall(t, cs, "edit", map[string]any{"path": "/wiki/a/x.md", "old_string": "one", "new_string": "uno"}); !strings.HasSuffix(out, "— 1 replacement(s)") {
		t.Errorf("edit output changed shape: %q", out)
	}
	if out := mustCall(t, cs, "delete", map[string]any{"path": "/wiki/a/y.md"}); out != "deleted /wiki/a/y.md" {
		t.Errorf("delete output changed shape: %q", out)
	}
}

func TestMCPRootListMarksGuides(t *testing.T) {
	f := newMCPHub(t)
	other := f.secondProject("plain")
	cs := f.session(f.connect("alice", f.wiki.ID, other.ID))
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/AGENTS.md", "content": "x\n"})
	mustCall(t, cs, "write", map[string]any{"path": "/plain/notes.md", "content": "x\n"})

	out := mustCall(t, cs, "list", map[string]any{"path": "/"})
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(strings.TrimSpace(line), "/wiki/"):
			if !strings.HasSuffix(line, "\tguide: AGENTS.md") {
				t.Errorf("wiki row not marked: %q", line)
			}
		case strings.HasPrefix(strings.TrimSpace(line), "/plain/"):
			if strings.Contains(line, "guide") {
				t.Errorf("plain row marked without a guide: %q", line)
			}
		}
	}
}

func TestMCPToolsNameApplicableGuides(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/AGENTS.md", "content": "root\n"})
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/docs/AGENTS.md", "content": "docs\n"})
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/docs/CLAUDE.md", "content": "docs-claude\n"})
	const docs = "guides: /wiki/docs/AGENTS.md, /wiki/docs/CLAUDE.md, /wiki/AGENTS.md\n"
	const root = "guides: /wiki/AGENTS.md\n"

	expect := func(tool, want, out string) {
		t.Helper()
		if !strings.Contains(out, want) {
			t.Errorf("%s: want %q in:\n%s", tool, want, out)
		}
	}
	expect("write", docs, mustCall(t, cs, "write", map[string]any{"path": "/wiki/docs/spec.md", "content": "alpha\n"}))
	read := mustCall(t, cs, "read", map[string]any{"path": "/wiki/docs/spec.md"})
	expect("read", docs, read)
	// In the header, before the content: paging must not move it.
	if strings.Index(read, "guides:") > strings.Index(read, "alpha") {
		t.Errorf("read: guides line should precede the content:\n%s", read)
	}
	expect("edit", docs, mustCall(t, cs, "edit", map[string]any{"path": "/wiki/docs/spec.md", "old_string": "alpha", "new_string": "beta"}))
	expect("list", docs, mustCall(t, cs, "list", map[string]any{"path": "/wiki/docs"}))
	// move names the DESTINATION's guides.
	expect("move", root, mustCall(t, cs, "move", map[string]any{"from": "/wiki/docs/spec.md", "to": "/wiki/spec.md"}))
	expect("restore", root, mustCall(t, cs, "restore", map[string]any{"path": "/wiki/spec.md",
		"sha": firstVersionSHA(t, f, "spec.md")}))
	expect("delete", root, mustCall(t, cs, "delete", map[string]any{"path": "/wiki/spec.md"}))
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/tmp/a.md", "content": "x\n"})
	expect("delete folder", root, mustCall(t, cs, "delete", map[string]any{"path": "/wiki/tmp/"}))

	// Reading a guide names the OTHER guides above it, never itself.
	g := mustCall(t, cs, "read", map[string]any{"path": "/wiki/docs/AGENTS.md"})
	expect("read guide", "guides: /wiki/docs/CLAUDE.md, /wiki/AGENTS.md\n", g)

	// glob, grep and history are many-path or read-only metadata: undecorated.
	for _, out := range []string{
		mustCall(t, cs, "glob", map[string]any{"pattern": "**/*.md"}),
		mustCall(t, cs, "grep", map[string]any{"pattern": "docs"}),
		mustCall(t, cs, "history", map[string]any{"path": "/wiki/docs/AGENTS.md"}),
	} {
		if strings.Contains(out, "guides:") {
			t.Errorf("should be undecorated:\n%s", out)
		}
	}
}

func TestMCPRawAndImageReadsAreUndecorated(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/AGENTS.md", "content": "root\n"})
	const body = "alpha\nbeta"
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/x.txt", "content": body})
	if out := mustCall(t, cs, "read", map[string]any{"path": "/wiki/x.txt", "raw": true}); out != body {
		t.Fatalf("raw read is not byte-exact: %q", out)
	}

	png, _ := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if rec := doAs(t, f.h, "PUT", "/api/p/"+f.wiki.ID+"/upload/content?path=logo.png", png, f.cookies["alice"]); rec.Code != 200 {
		t.Fatalf("seed png: %d %s", rec.Code, rec.Body)
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "read", Arguments: map[string]any{"path": "/wiki/logo.png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("image read should be a single block, got %d", len(res.Content))
	}
	if _, ok := res.Content[0].(*mcp.ImageContent); !ok {
		t.Fatalf("content = %T, want an image", res.Content[0])
	}
}

func TestMCPUnreadableGuideFallsBackToPointer(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	if rec := doAs(t, f.h, "PUT", "/api/p/"+f.wiki.ID+"/upload/content?path=AGENTS.md",
		[]byte("bin\x00ary\n"), f.cookies["alice"]); rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	out := mustCall(t, cs, "list", map[string]any{"path": "/wiki"})
	if strings.Contains(out, "Project guide") || !strings.Contains(out, "guides: /wiki/AGENTS.md\n") {
		t.Fatalf("binary guide should be named, not inlined:\n%s", out)
	}
}

func TestMCPGuideEditsShowUpImmediately(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	mustCall(t, cs, "write", map[string]any{"path": "/wiki/AGENTS.md", "content": "version-one\n"})
	if out := mustCall(t, cs, "list", map[string]any{"path": "/wiki"}); !strings.Contains(out, "version-one") {
		t.Fatalf("first listing:\n%s", out)
	}
	mustCall(t, cs, "edit", map[string]any{"path": "/wiki/AGENTS.md", "old_string": "version-one", "new_string": "version-two"})
	if out := mustCall(t, cs, "list", map[string]any{"path": "/wiki"}); !strings.Contains(out, "version-two") ||
		strings.Contains(out, "version-one") {
		t.Fatalf("listing after edit shows a stale guide:\n%s", out)
	}
}

func TestMCPInstructionsMentionGuides(t *testing.T) {
	f := newMCPHub(t)
	cs := f.session(f.connect("alice", f.wiki.ID))
	if got := cs.InitializeResult().Instructions; !strings.Contains(got, "AGENTS.md") {
		t.Fatalf("instructions do not mention project guides:\n%s", got)
	}
}
