package webapp

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestCLILazyE2E drives the real binary on two devices against a throwaway
// hub: A syncs everything; B joins with --lazy. B must not download the large
// files, must read and search them through cat/ls/grep and the local stdio
// MCP, and must never delete them for A.
func TestCLILazyE2E(t *testing.T) {
	a := newCLIEnv(t)
	b := newCLIEnvOn(t, a.hub)

	aDir := filepath.Join(t.TempDir(), "proj")
	bigLine := func(tag string, n int) string {
		return tag + "\n" + strings.Repeat("filler line of text for size\n", n)
	}
	files := map[string]string{
		"small.md":        "# small\nneedle-small\n",
		"big.html":        bigLine("needle-big", 8000),
		"docs/huge.json":  bigLine("needle-huge", 12000),
		"keep/pinned.bin": bigLine("pinned", 5000),
		"docs/note.md":    "a note\n",
		"old/big2.html":   bigLine("old", 6000),
	}
	for rel, body := range files {
		p := filepath.Join(aDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustRun := func(e cliEnv, dir string, args ...string) string {
		t.Helper()
		out, err := e.run(dir, args...)
		if err != nil {
			t.Fatalf("bdrive %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	mustRun(a, aDir, "init", "--name", "lazyproj", "--yes", "--no-autostart")
	defer a.run(aDir, "stop", aDir)
	mustRun(a, aDir, "sync")
	id := projectIDByName(t, a.browser, a.hub.URL, "lazyproj")

	bDir := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(bDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustRun(b, bDir, "init", "--project", id, "--lazy", "--yes", "--no-autostart")
	defer b.run(bDir, "stop", bDir)
	mustRun(b, bDir, "sync")

	present := func(dir, rel string) bool {
		_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
		return err == nil
	}
	for rel, want := range map[string]bool{"old/big2.html": false, "small.md": true, "docs/note.md": true, "big.html": false, "docs/huge.json": false, "keep/pinned.bin": false} {
		if got := present(bDir, rel); got != want {
			t.Fatalf("B %s on disk = %v, want %v", rel, got, want)
		}
	}
	if out := mustRun(b, bDir, "ls"); !strings.Contains(out, "big.html") || !strings.Contains(out, "(on hub)") {
		t.Fatalf("ls should list the lazy file as on hub:\n%s", out)
	}
	// grep: a lazy file nobody read yet is searched by the hub.
	if out := mustRun(b, bDir, "grep", "needle-huge"); !strings.Contains(out, "docs/huge.json:1:") {
		t.Fatalf("grep did not find the hub-only file:\n%s", out)
	}
	if out := mustRun(b, bDir, "cat", "big.html"); out != files["big.html"] {
		t.Fatalf("cat returned %d bytes, want %d", len(out), len(files["big.html"]))
	}
	if out := mustRun(b, bDir, "grep", "-l", "needle"); !strings.Contains(out, "big.html") || !strings.Contains(out, "small.md") || !strings.Contains(out, "docs/huge.json") {
		t.Fatalf("combined grep missed a file:\n%s", out)
	}
	if out := mustRun(b, bDir, "lazy"); !strings.Contains(out, "on the hub only: 4 files") {
		t.Fatalf("lazy status:\n%s", out)
	}

	// The local MCP server, over stdio, with a real MCP client.
	ctx := context.Background()
	cmd := exec.Command(b.bin, "mcp", "--folder", bDir)
	cmd.Env = append(envWithout("HOME", "BDRIVE_HOME"), "HOME="+b.home, "BDRIVE_HOME="+filepath.Join(b.home, ".bdrive"))
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e"}, nil)
	sess, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var sb strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				sb.WriteString(tc.Text)
			}
		}
		return sb.String(), res.IsError
	}
	out, _ := call("read", map[string]any{"path": "/docs/huge.json", "limit": 2})
	m := regexp.MustCompile(`sha:([0-9a-f]{64})`).FindStringSubmatch(out)
	if m == nil || !strings.Contains(out, "needle-huge") || !strings.Contains(out, "from hub") {
		t.Fatalf("mcp read:\n%s", out)
	}
	if out, _ := call("read", map[string]any{"path": "docs/huge.json", "limit": 1}); !strings.Contains(out, "from cache") {
		t.Fatalf("second mcp read should hit the cache:\n%s", out)
	}
	if out, _ := call("grep", map[string]any{"pattern": "needle-(big|huge|small)", "files_only": true}); !strings.Contains(out, "3 file(s)") {
		t.Fatalf("mcp grep:\n%s", out)
	}
	if out, _ := call("list", map[string]any{"path": "/"}); !strings.Contains(out, "big.html") || !strings.Contains(out, "(on hub)") {
		t.Fatalf("mcp list:\n%s", out)
	}
	if out, isErr := call("edit", map[string]any{"path": "docs/huge.json", "old_string": "needle-huge", "new_string": "needle-edited", "sha": "0000000000"}); !isErr || !strings.Contains(out, "changed since") {
		t.Fatalf("stale sha must be refused:\n%s", out)
	}
	if out, isErr := call("edit", map[string]any{"path": "docs/huge.json", "old_string": "needle-huge", "new_string": "needle-edited", "sha": m[1]}); isErr {
		t.Fatalf("mcp edit:\n%s", out)
	}
	if out, isErr := call("delete", map[string]any{"path": "big.html"}); !isErr || !strings.Contains(out, "kept on the hub") {
		t.Fatalf("deleting a lazy file must explain itself:\n%s", out)
	}
	if out, isErr := call("write", map[string]any{"path": "node_modules/x.js", "content": "x"}); !isErr || !strings.Contains(out, "excluded") {
		t.Fatalf("a write that would never sync must be refused:\n%s", out)
	}
	// Write a lazy path, then delete it before any scan: the delete must
	// reach the hub, not just this disk (bot review P1).
	out2, _ := call("read", map[string]any{"path": "old/big2.html", "limit": 1})
	m2 := regexp.MustCompile(`sha:([0-9a-f]{64})`).FindStringSubmatch(out2)
	if m2 == nil {
		t.Fatalf("read old/big2.html:\n%s", out2)
	}
	if out, isErr := call("write", map[string]any{"path": "old/big2.html", "content": "rewritten\n", "sha": m2[1]}); isErr {
		t.Fatalf("write lazy path:\n%s", out)
	}
	if out, isErr := call("delete", map[string]any{"path": "old/big2.html"}); isErr {
		t.Fatalf("delete right after write:\n%s", out)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(bDir, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, isErr := call("write", map[string]any{"path": "escape/sub/x.md", "content": "x"}); !isErr {
		t.Fatal("a write through a symlink out of the project must be refused")
	}
	if _, err := os.Stat(filepath.Join(outside, "sub")); err == nil {
		t.Fatal("refused write still created a folder outside the project")
	}
	os.Remove(filepath.Join(bDir, "escape"))
	sess.Close()

	// Pin a folder: it downloads.
	mustRun(b, bDir, "pin", "keep")
	mustRun(b, bDir, "sync")
	if !present(bDir, "keep/pinned.bin") {
		t.Fatal("pinned folder did not download")
	}

	// Several more cycles on B, then A: nothing B keeps on the hub was
	// deleted, and B's edit through MCP reached A.
	for i := 0; i < 3; i++ {
		mustRun(b, bDir, "sync")
	}
	mustRun(a, aDir, "sync")
	if present(aDir, "old/big2.html") {
		t.Fatal("a write-then-delete through MCP never reached the hub")
	}
	if !present(aDir, "big.html") {
		t.Fatal("A lost big.html: a lazy file became a delete")
	}
	if got := readFile(t, filepath.Join(aDir, "docs/huge.json")); !strings.HasPrefix(got, "needle-edited\n") {
		t.Fatalf("A did not get B's MCP edit: %q", got[:min(40, len(got))])
	}
	t.Logf("disk: A folder+store %s, B (lazy) folder+store %s", human(du(t, aDir)+du(t, filepath.Join(a.home, ".bdrive", "volumes"))),
		human(du(t, bDir)+du(t, filepath.Join(b.home, ".bdrive", "volumes"))))
}

func du(t *testing.T, root string) int64 {
	var n int64
	filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

func human(n int64) string { return humanSize(n) }
