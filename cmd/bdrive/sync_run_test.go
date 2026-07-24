package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/store"
)

// `bdrive stop` must actually stop: it pauses the volume so the agent turn
// hooks and `bdrive sync` no-op (they used to resume a stopped project on
// the very next turn), and after --forget nothing re-enrolls the mount
// behind the user's back.
func TestStopPausesHooksAndForgetSticks(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder := t.TempDir()
	folder, _ = filepath.EvalSymlinks(folder)
	proj, err := config.SaveProject(folder, config.Project{
		Volume: "wiki",
		Remote: "https://hub.example.com/p/p-12345678",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.ResolveMount(folder); err != nil { // enroll, as `bdrive init` would
		t.Fatal(err)
	}
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}

	runHook := func() string {
		c := syncCmd()
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetIn(strings.NewReader(`{"session_id":"s"}`))
		c.SetArgs([]string{folder, "--hook", "claude-code"})
		if err := c.Execute(); err != nil {
			t.Fatalf("hook mode must never fail: %v", err)
		}
		return out.String()
	}

	// Live project: hook syncs and emits the link formula.
	if !strings.Contains(runHook(), `"hookSpecificOutput"`) {
		t.Fatal("enrolled live project: hook did not emit the formula")
	}

	// stop (no daemon running is fine): pause marker set, hook goes quiet.
	c := stopCmd()
	c.SetArgs([]string{folder})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if !store.Paused(vdir) {
		t.Fatal("stop did not set the paused marker")
	}
	if out := runHook(); out != "" {
		t.Fatalf("hook after stop emitted %q, want silence", out)
	}

	// stop --forget: unregistered, and the hook must not self-heal it back.
	c2 := stopCmd()
	c2.SetArgs([]string{folder, "--forget"})
	if err := c2.Execute(); err != nil {
		t.Fatal(err)
	}
	if out := runHook(); out != "" {
		t.Fatalf("hook after --forget emitted %q, want silence", out)
	}
	mounts, err := config.LoadMounts()
	if err != nil {
		t.Fatal(err)
	}
	if _, enrolled := mounts[proj.ID]; enrolled {
		t.Fatal("hook re-enrolled a forgotten mount")
	}
}
