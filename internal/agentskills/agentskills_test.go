package agentskills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/plugin"
)

func TestDetect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	folder := t.TempDir()

	if got := Detect(folder); len(got) != 0 {
		t.Fatalf("nothing configured, detected %v", got)
	}
	os.MkdirAll(filepath.Join(folder, ".codex"), 0o755) // project-level
	os.MkdirAll(filepath.Join(home, ".hermes"), 0o755)  // user-level
	if got := strings.Join(Detect(folder), ","); got != "codex,hermes" {
		t.Fatalf("detected %q, want codex,hermes", got)
	}
}

func TestInstallWritesSkillPerAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	results, err := Install(t.TempDir(), []string{"codex", "hermes"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	for _, r := range results {
		if !r.Changed {
			t.Fatalf("%s: fresh install reported unchanged", r.Agent)
		}
		data, err := os.ReadFile(r.Path)
		if err != nil {
			t.Fatalf("%s: %v", r.Agent, err)
		}
		if string(data) != plugin.SkillMD {
			t.Fatalf("%s: written skill differs from the embedded one", r.Agent)
		}
		// Frontmatter is what makes a SKILL.md discoverable on every platform.
		if !strings.HasPrefix(string(data), "---\nname: beardrive\n") {
			t.Fatalf("%s: skill lacks name frontmatter", r.Agent)
		}
	}
	if got := results[0].Path; got != filepath.Join(home, ".codex", "skills", "beardrive", "SKILL.md") {
		t.Fatalf("codex path = %s", got)
	}

	// Re-running is idempotent...
	results, err = Install(t.TempDir(), []string{"codex"})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Changed {
		t.Fatal("second install reported a change")
	}
	if !Installed("codex") {
		t.Fatal("Installed() false right after installing")
	}

	// ...but a stale copy is refreshed to the binary's own.
	os.WriteFile(results[0].Path, []byte("--- old skill ---\n"), 0o644)
	if Installed("codex") {
		t.Fatal("Installed() true for a stale copy")
	}
	results, _ = Install(t.TempDir(), []string{"codex"})
	if !results[0].Changed {
		t.Fatal("stale copy was not refreshed")
	}
}

func TestInstallAutoDetects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)

	results, err := Install(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Agent != "claude" {
		t.Fatalf("auto-detect installed %v", results)
	}
}

func TestInstallUnknownAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := Install(t.TempDir(), []string{"cursor"}); err == nil {
		t.Fatal("unknown agent accepted")
	}
}
