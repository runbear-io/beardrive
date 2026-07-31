package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/store"
)

// `bdrive sync --hook` must emit the gated-link formula as Claude Code
// hook JSON, stamp the session note, and stay a silent no-op everywhere
// else — a hook must never fail the turn.
func TestSyncHookMode(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder := t.TempDir()
	folder, _ = filepath.EvalSymlinks(folder)
	proj, err := config.SaveProject(folder, config.Project{
		Volume: "wiki",
		Remote: "https://hub.example.com/p/p-12345678", // unreachable: cycle degrades offline, formula still valid
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.ResolveMount(folder); err != nil { // enroll, as `bdrive init` would
		t.Fatal(err)
	}

	c := syncCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetIn(strings.NewReader(`{"session_id":"sess-42","prompt":"hello"}`))
	c.SetArgs([]string{folder, "--hook", "claude-code"})
	if err := c.Execute(); err != nil {
		t.Fatalf("hook mode must never fail: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		`"hookSpecificOutput"`,
		`"hookEventName":"UserPromptSubmit"`,
		"https://hub.example.com/p-12345678", // base URL: remote minus /p
		"[🔗](",                               // the emoji-link convention
		"code blocks",                        // paths in code blocks stay plain
		"PUBLIC",                             // bdrive share stays opt-in
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hook output missing %q:\n%s", want, got)
		}
	}

	// The session note was stamped for the daemon's follow-up scans.
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	if note := st.LoadNote(); note != "claude-code session sess-42" {
		t.Errorf("note = %q, want the stamped session", note)
	}
}

func TestSyncHookModeNoOps(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())

	// Not a mount: silent success, no output.
	c := syncCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetIn(strings.NewReader(`{"session_id":"x"}`))
	c.SetArgs([]string{t.TempDir(), "--hook", "claude-code"})
	if err := c.Execute(); err != nil {
		t.Fatalf("non-mount must be a silent no-op: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("non-mount emitted output: %s", out.String())
	}

	// A config.json that arrived with the folder (git clone, copied dir)
	// but was never enrolled on this device via `bdrive init`: silent no-op,
	// and — crucially — no device enrollment as a side effect.
	folder := t.TempDir()
	folder, _ = filepath.EvalSymlinks(folder)
	proj, err := config.SaveProject(folder, config.Project{
		Volume: "wiki", Remote: "https://hub.example.com/p/p-12345678",
	})
	if err != nil {
		t.Fatal(err)
	}
	c2 := syncCmd()
	out.Reset()
	c2.SetOut(&out)
	c2.SetIn(strings.NewReader(`{"session_id":"x"}`))
	c2.SetArgs([]string{folder, "--hook", "claude-code"})
	if err := c2.Execute(); err != nil {
		t.Fatalf("unenrolled mount must be a silent no-op: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("unenrolled mount emitted output: %s", out.String())
	}
	mounts, err := config.LoadMounts()
	if err != nil {
		t.Fatal(err)
	}
	if _, enrolled := mounts[proj.ID]; enrolled {
		t.Fatal("hook auto-enrolled the mount; only `bdrive init` may do that")
	}

	// Enrolled but paused by `bdrive stop`: silent no-op too.
	if _, _, err := config.ResolveMount(folder); err != nil {
		t.Fatal(err)
	}
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPaused(vdir, true); err != nil {
		t.Fatal(err)
	}
	c3 := syncCmd()
	out.Reset()
	c3.SetOut(&out)
	c3.SetIn(strings.NewReader(`{"session_id":"x"}`))
	c3.SetArgs([]string{folder, "--hook", "claude-code"})
	if err := c3.Execute(); err != nil {
		t.Fatalf("paused mount must be a silent no-op: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("paused mount emitted output: %s", out.String())
	}

	// Garbage stdin on a live mount: still sync, still emit, never fail.
	if err := store.SetPaused(vdir, false); err != nil {
		t.Fatal(err)
	}
	c4 := syncCmd()
	out.Reset()
	c4.SetOut(&out)
	c4.SetIn(strings.NewReader("not json at all"))
	c4.SetArgs([]string{folder, "--hook", "claude-code"})
	if err := c4.Execute(); err != nil {
		t.Fatalf("garbage stdin must not fail: %v", err)
	}
	if !strings.Contains(out.String(), `"hookSpecificOutput"`) {
		t.Fatalf("formula not emitted on garbage stdin: %s", out.String())
	}
}

// mountAt creates a project folder under parent and enrolls it on this
// device, as `bdrive init` would.
func mountAt(t *testing.T, parent, name, remote string) config.Project {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	proj, err := config.SaveProject(dir, config.Project{Volume: name, Remote: remote})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.ResolveMount(dir); err != nil {
		t.Fatal(err)
	}
	return proj
}

func runHook(t *testing.T, folder string) string {
	t.Helper()
	c := syncCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetIn(strings.NewReader(`{"session_id":"sess-42"}`))
	c.SetArgs([]string{folder, "--hook", "claude-code"})
	if err := c.Execute(); err != nil {
		t.Fatalf("hook mode must never fail: %v", err)
	}
	return out.String()
}

// A session at a root whose subfolders are separate projects must get EVERY
// project's URL, each keyed by the prefix the agent sees — emitting only the
// first mount's base made agents hang one project's paths on another
// project's URL.
func TestSyncHookModeMultipleMounts(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	a := mountAt(t, root, "projA", "https://hub.example.com/p/p-aaaaaaaa")
	b := mountAt(t, root, "projB", "https://hub.example.com/p/p-bbbbbbbb")

	got := runHook(t, root)

	// One JSON object: the hook's stdout contract.
	if n := strings.Count(strings.TrimSpace(got), "\n"); n != 0 {
		t.Fatalf("hook emitted %d objects, want 1:\n%s", n+1, got)
	}
	for _, want := range []string{
		"https://hub.example.com/p-aaaaaaaa",
		"https://hub.example.com/p-bbbbbbbb",
		"`projA/`",
		"`projB/`",
		"matches the path longest", // how to pick between them
		"do not link it",           // a path in neither is not synced
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hook output missing %q:\n%s", want, got)
		}
	}

	// stdin is consumed once, so the note must still reach every mount.
	for _, proj := range []config.Project{a, b} {
		vdir, err := config.VolumeDir(proj.ID)
		if err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(vdir)
		if err != nil {
			t.Fatal(err)
		}
		if note := st.LoadNote(); note != "claude-code session sess-42" {
			t.Errorf("%s: note = %q, want the stamped session", proj.Volume, note)
		}
	}
}

// A mount that has no hub URL must not swallow the context for the mounts
// that do — the old "first mount emits" guard could never detect a mount
// that emitted nothing, because hook mode never returns an error.
func TestSyncHookModeSkipsNonHubMount(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	mountAt(t, root, "a-plain", "file://"+t.TempDir()) // sorts first, no hub
	mountAt(t, root, "b-hub", "https://hub.example.com/p/p-bbbbbbbb")

	got := runHook(t, root)
	if !strings.Contains(got, "https://hub.example.com/p-bbbbbbbb") {
		t.Errorf("hub mount lost its link behind a non-hub mount:\n%s", got)
	}
	if !strings.Contains(got, "`b-hub/`") {
		t.Errorf("hub mount missing its prefix:\n%s", got)
	}
	if strings.Contains(got, "a-plain") {
		t.Errorf("non-hub mount has no URL and must not be listed:\n%s", got)
	}
}

// A session started inside a mount writes paths relative to its own
// directory, so that subpath belongs in the base URL — there is no prefix
// for the agent to strip.
func TestSyncHookModeInsideMount(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	mountAt(t, root, "wiki", "https://hub.example.com/p/p-12345678")
	sub := filepath.Join(root, "wiki", "docs", "notes")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	got := runHook(t, sub)
	if !strings.Contains(got, "https://hub.example.com/p-12345678/docs/notes") {
		t.Errorf("base URL missing the session's subpath (and `/` must stay literal):\n%s", got)
	}
}

// Plain `bdrive sync` (the push hook's form, and what users type) refuses
// unenrolled and paused mounts with instructions instead of silently
// enrolling or resuming.
func TestSyncRefusesUnenrolledAndPaused(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder := t.TempDir()
	folder, _ = filepath.EvalSymlinks(folder)
	proj, err := config.SaveProject(folder, config.Project{Volume: "wiki"})
	if err != nil {
		t.Fatal(err)
	}

	c := syncCmd()
	c.SetArgs([]string{folder})
	err = c.Execute()
	if err == nil || !strings.Contains(err.Error(), "bdrive init") {
		t.Fatalf("unenrolled sync error = %v, want a `bdrive init` pointer", err)
	}

	if _, _, err := config.ResolveMount(folder); err != nil {
		t.Fatal(err)
	}
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPaused(vdir, true); err != nil {
		t.Fatal(err)
	}
	c2 := syncCmd()
	c2.SetArgs([]string{folder})
	err = c2.Execute()
	if err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("paused sync error = %v, want a paused message", err)
	}
}
