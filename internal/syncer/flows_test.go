package syncer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
)

// End-to-end scenarios for the init knowledge flows documented in
// plugin/commands/init.md and the beardrive skill ("Connecting knowledge
// tooling"): a shared subfolder carved out of a repo, a teammate connecting
// over pre-existing local content, and a nested mount (e.g. a team knowledge
// folder inside a personal brain that is itself a mount) syncing through its
// own project.

// deviceAt is newDevice with an explicit working folder, for topologies where
// the folder's location matters (nested mounts).
func deviceAt(t *testing.T, name, folder string, backend remote.Backend) *Session {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "volume"))
	if err != nil {
		t.Fatal(err)
	}
	return &Session{
		Folder:  folder,
		Store:   st,
		Device:  config.Device{ID: name, Name: name, Author: name + "@test"},
		Backend: backend,
	}
}

func conflictFiles(t *testing.T, folder string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(folder, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.Contains(d.Name(), ".bdrive-conflict-") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A repo shares only its knowledge subfolder (`bdrive init --shared Wiki`);
// a teammate connects from their own checkout with the same scope. Wiki
// content flows both ways; each side's code never leaves its machine.
func TestSharedSubfolderScopeBothDevices(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	write(t, a.Folder, ".bdrive/config.json", `{"include": ["Wiki/"]}`)
	write(t, a.Folder, "Wiki/home.md", "welcome")
	write(t, a.Folder, "src/code.go", "package main")
	cycle(t, a)

	write(t, b.Folder, ".bdrive/config.json", `{"include": ["Wiki/"]}`)
	write(t, b.Folder, "main.py", "print('local only')")
	res := cycle(t, b)
	if res.LocalOps != 0 {
		t.Fatalf("b journaled %d ops for out-of-scope files, want 0", res.LocalOps)
	}
	if got := read(t, b.Folder, "Wiki/home.md"); got != "welcome" {
		t.Fatalf("Wiki/home.md = %q, want welcome", got)
	}

	// Wiki edits flow back; code never crosses in either direction.
	write(t, b.Folder, "Wiki/home.md", "welcome v2")
	cycle(t, b)
	cycle(t, a)
	if got := read(t, a.Folder, "Wiki/home.md"); got != "welcome v2" {
		t.Fatalf("a Wiki/home.md = %q, want welcome v2", got)
	}
	for folder, absent := range map[string]string{a.Folder: "main.py", b.Folder: "src/code.go"} {
		if _, err := os.Stat(filepath.Join(folder, absent)); !os.IsNotExist(err) {
			t.Fatalf("%s leaked across devices", absent)
		}
	}
}

// The git-handoff teammate story: after the first user connects docs/, a
// teammate's checkout already holds the same files. Connecting must converge
// with zero conflict copies (identical content is adopted, not duplicated).
func TestConnectWithIdenticalLocalContent(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	write(t, a.Folder, "docs/guide.md", "v1")
	write(t, a.Folder, "docs/setup.md", "steps")
	cycle(t, a)

	b := newDevice(t, "devb", be)
	write(t, b.Folder, "docs/guide.md", "v1")
	write(t, b.Folder, "docs/setup.md", "steps")
	cycle(t, b)
	cycle(t, a)
	cycle(t, b)

	for _, s := range []*Session{a, b} {
		if got := read(t, s.Folder, "docs/guide.md"); got != "v1" {
			t.Fatalf("guide.md = %q, want v1", got)
		}
		if c := conflictFiles(t, s.Folder); len(c) != 0 {
			t.Fatalf("identical content produced conflict copies: %v", c)
		}
	}
}

// Same story with a stale divergent copy: nothing is silently lost — the
// devices converge on one version and the other survives as a conflict copy.
func TestConnectWithDivergentLocalContent(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	write(t, a.Folder, "docs/guide.md", "hub version")
	cycle(t, a)

	b := newDevice(t, "devb", be)
	time.Sleep(10 * time.Millisecond)
	write(t, b.Folder, "docs/guide.md", "stale local version")
	cycle(t, b)
	cycle(t, a)
	cycle(t, b)

	av, bv := read(t, a.Folder, "docs/guide.md"), read(t, b.Folder, "docs/guide.md")
	if av != bv {
		t.Fatalf("devices diverged: %q vs %q", av, bv)
	}
	survived := map[string]bool{av: true}
	for _, s := range []*Session{a, b} {
		for _, p := range conflictFiles(t, s.Folder) {
			rel, _ := filepath.Rel(s.Folder, p)
			survived[read(t, s.Folder, rel)] = true
		}
	}
	if !survived["hub version"] || !survived["stale local version"] {
		t.Fatalf("a version was silently lost; surviving: %v", survived)
	}
}

// The follower-brain topology: a personal folder is a mount on one project
// while a team knowledge folder nested inside it is a mount on another.
// Each project sees only its own files, in both directions, even as both
// actively sync.
func TestNestedMountSyncsIndependently(t *testing.T) {
	personal := sharedRemote(t)
	team := sharedRemote(t)

	// Alice: personal brain mount with a nested team mount inside it.
	aliceRoot := t.TempDir()
	alice := deviceAt(t, "alice", aliceRoot, personal)
	aliceTeam := deviceAt(t, "alice-team", filepath.Join(aliceRoot, "team"), team)
	write(t, aliceRoot, "private.md", "my captures")
	write(t, aliceRoot, "team/.bdrive/config.json", `{"mount_id":"m-team"}`)
	write(t, aliceRoot, "team/plan.md", "roadmap v1")
	cycle(t, alice)
	cycle(t, aliceTeam)

	// Bob syncs only the team project; Alice's laptop syncs only personal.
	bobTeam := newDevice(t, "bob-team", team)
	cycle(t, bobTeam)
	if got := read(t, bobTeam.Folder, "plan.md"); got != "roadmap v1" {
		t.Fatalf("team plan.md = %q, want roadmap v1", got)
	}
	if _, err := os.Stat(filepath.Join(bobTeam.Folder, "private.md")); !os.IsNotExist(err) {
		t.Fatal("personal file leaked into the team project")
	}
	laptop := newDevice(t, "alice-laptop", personal)
	cycle(t, laptop)
	if got := read(t, laptop.Folder, "private.md"); got != "my captures" {
		t.Fatalf("private.md = %q, want my captures", got)
	}
	if _, err := os.Stat(filepath.Join(laptop.Folder, "team")); !os.IsNotExist(err) {
		t.Fatal("team folder leaked into the personal project")
	}

	// Bob's team edit reaches Alice's nested mount; her personal project
	// must not journal the change it can see on disk.
	write(t, bobTeam.Folder, "plan.md", "roadmap v2")
	cycle(t, bobTeam)
	cycle(t, aliceTeam)
	if got := read(t, aliceRoot, "team/plan.md"); got != "roadmap v2" {
		t.Fatalf("alice team/plan.md = %q, want roadmap v2", got)
	}
	if res := cycle(t, alice); res.LocalOps != 0 {
		t.Fatalf("personal mount journaled %d ops for nested team content, want 0", res.LocalOps)
	}
}

// The conflict-copy filename must keep matching the glob documented for the
// OKF validation hook (SKILL.md): validate alone can't see conflict copies,
// so agents check `*.bdrive-conflict-*` — renaming the pattern breaks them.
func TestConflictCopyNameMatchesDocumentedGlob(t *testing.T) {
	name := conflictName("Wiki/page.md", "Snow's MacBook", time.Now())
	ok, err := filepath.Match("*.bdrive-conflict-*", filepath.Base(name))
	if err != nil || !ok {
		t.Fatalf("conflict copy %q no longer matches the documented glob *.bdrive-conflict-*", name)
	}
	if strings.ContainsAny(filepath.Base(name), " '") {
		t.Fatalf("conflict copy name %q should sanitize device names", name)
	}
}
