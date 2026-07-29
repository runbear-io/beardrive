package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
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
	// Nothing is stale in an empty project: the watchlist block is absent.
	if strings.Contains(got, "possibly out-of-date") {
		t.Errorf("stale watchlist emitted for a project with no ops:\n%s", got)
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

// The stale-file watchlist rides along on the same additionalContext: files
// that haven't changed in 90d but sit in a directory people are still editing.
// Ages come from journal ops, never mtimes — the ops are seeded under a PEER
// device (scan compares against the materialization cache, not replayed state,
// so a file written to disk first would get a fresh put and the test would
// silently assert nothing).
func TestSyncHookStaleWatchlist(t *testing.T) {
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
	if _, _, err := config.ResolveMount(folder); err != nil {
		t.Fatal(err)
	}
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	shaOld, _, err := st.PutBlobBytes([]byte("old")) // the blob must exist or materialize skips the file
	if err != nil {
		t.Fatal(err)
	}
	shaNew, _, err := st.PutBlobBytes([]byte("new"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.AppendOps("devPeer", []journal.Op{
		{Seq: 1, Lamport: 1, Device: "devPeer", Kind: journal.KindPut, Path: "a/old.md",
			Blob: shaOld, Size: 3, Mode: 0o644, Time: now.Add(-200 * 24 * time.Hour), User: "ken@acme.co"},
		{Seq: 2, Lamport: 2, Device: "devPeer", Kind: journal.KindPut, Path: "a/recent.md",
			Blob: shaNew, Size: 3, Mode: 0o644, Time: now.Add(-24 * time.Hour)},
		{Seq: 3, Lamport: 3, Device: "devPeer", Kind: journal.KindPut, Path: "a/gone.md",
			Blob: shaOld, Size: 3, Mode: 0o644, Time: now.Add(-300 * 24 * time.Hour)},
		{Seq: 4, Lamport: 4, Device: "devPeer", Kind: journal.KindDelete, Path: "a/gone.md",
			Time: now.Add(-10 * 24 * time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}

	c := syncCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetIn(strings.NewReader(`{"session_id":"sess-42"}`))
	c.SetArgs([]string{folder, "--hook", "claude-code"})
	if err := c.Execute(); err != nil {
		t.Fatalf("hook mode must never fail: %v", err)
	}
	var got struct {
		Hook struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
		t.Fatalf("hook output is not one JSON object: %v\n%s", err, out.String())
	}
	ctx := got.Hook.AdditionalContext
	for _, want := range []string{
		"possibly out-of-date files",
		"a/old.md",
		"200d ago",
		"ken@acme.co",
		"1 files near it changed",
		"[🔗](", // the link formula is still there: appended, not replaced
		"PUBLIC",
	} {
		if !strings.Contains(ctx, want) {
			t.Errorf("additionalContext missing %q:\n%s", want, ctx)
		}
	}
	if strings.Contains(ctx, "a/gone.md") {
		t.Errorf("deleted path listed as stale:\n%s", ctx)
	}
	if strings.Contains(ctx, "a/recent.md —") {
		t.Errorf("file inside the 90d bar listed as stale:\n%s", ctx)
	}

	// The cycle materialized a/old.md with today's mtime, and the reported
	// age is still 200d — ages come from the journal, not the filesystem.
	if _, err := os.Stat(filepath.Join(folder, "a", "old.md")); err != nil {
		t.Fatalf("old.md was never materialized, so the mtime case is untested: %v", err)
	}
}

// Ranking and the cap, straight against the helper: at most 5 entries,
// ordered by age × churn descending.
func TestStaleLinesRankAndCap(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	var ops []journal.Op
	seq := int64(0)
	add := func(p string, ageDays int) {
		seq++
		ops = append(ops, journal.Op{Seq: seq, Lamport: seq, Device: "devPeer", Kind: journal.KindPut,
			Path: p, Blob: "deadbeef", Size: 1, Time: now.Add(-time.Duration(ageDays) * 24 * time.Hour)})
	}
	// Seven stale files across two directories; d2 churns twice as hard, so a
	// younger d2 file can outrank an older d1 one.
	for i, age := range []int{100, 120, 140, 160, 180, 200, 220} {
		add(fmt.Sprintf("d1/old%d.md", i), age)
	}
	add("d2/old.md", 95)
	add("d1/recent.md", 1)  // churn 1 for d1
	add("d2/recent1.md", 1) // churn 2 for d2
	add("d2/recent2.md", 2)
	if err := st.AppendOps("devPeer", ops); err != nil {
		t.Fatal(err)
	}
	filter, err := syncer.LoadFilter(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	lines := staleLines(st, filter, now)
	if len(lines) != maxStale {
		t.Fatalf("got %d lines, want the %d cap:\n%s", len(lines), maxStale, strings.Join(lines, "\n"))
	}
	// age × churn: 220, 200, then d2/old.md at 95 × 2 = 190 jumping the
	// 180d-old d1 file, then 180, 160. The two youngest d1 files fall off.
	for i, want := range []string{"d1/old6.md", "d1/old5.md", "d2/old.md", "d1/old4.md", "d1/old3.md"} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d = %q, want %s", i, lines[i], want)
		}
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
