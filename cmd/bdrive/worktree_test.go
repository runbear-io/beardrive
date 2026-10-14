package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
)

const worktreeIgnore = "# custom rules\ncustom_ignored/\n"

func worktreeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	bhome := filepath.Join(home, ".bdrive")
	t.Setenv("BDRIVE_HOME", bhome)
	return bhome
}

func initWorktree(t *testing.T, folder, rawRemote string) config.Project {
	t.Helper()
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	hasShared := config.SharedVolumeHasIgnoreState(rawRemote, "")
	proj, err := config.SaveProject(folder, config.Project{
		Volume: "worktree-project",
		Remote: rawRemote,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasShared {
		ignorePath := filepath.Join(folder, ".bdriveignore")
		if _, err := os.Stat(ignorePath); os.IsNotExist(err) {
			if err := os.WriteFile(ignorePath, []byte(starterIgnore), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, _, err := config.EnrollMount(folder); err != nil {
		t.Fatal(err)
	}
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(vdir); err != nil {
		t.Fatal(err)
	}
	// `bdrive init` clears a prior stop marker as part of re-enrollment.
	if err := store.SetPaused(vdir, false); err != nil {
		t.Fatal(err)
	}
	return proj
}

func cycleWorktree(t *testing.T, folder string) *syncer.Result {
	t.Helper()
	sess, _, err := openSession(t.Context(), folder, true)
	if err != nil {
		t.Fatal(err)
	}
	res, cycleErr := sess.Cycle(t.Context())
	closeSession(sess)
	if cycleErr != nil {
		t.Fatal(cycleErr)
	}
	return res
}

func localOps(t *testing.T, mountID string) []journal.Op {
	t.Helper()
	vdir, err := config.VolumeDir(mountID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := config.LoadDevice()
	if err != nil {
		t.Fatal(err)
	}
	ops, err := st.DeviceOps(dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

func maxSeq(ops []journal.Op) int64 {
	var max int64
	for _, op := range ops {
		if op.Seq > max {
			max = op.Seq
		}
	}
	return max
}

func remoteOps(t *testing.T, rawRemote, device string) []journal.Op {
	t.Helper()
	be, err := remote.Open(t.Context(), rawRemote)
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	body, err := be.Get(t.Context(), "journal/"+device+".jsonl")
	if remote.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		t.Fatal(err)
	}
	ops, err := journal.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

func assertNo409(t *testing.T, res *syncer.Result) {
	t.Helper()
	if res.OfflineErr != nil && strings.Contains(strings.ToLower(res.OfflineErr.Error()), "409") {
		t.Fatalf("sync reported a 409 conflict: %v", res.OfflineErr)
	}
	if res.AccessErr != nil && strings.Contains(strings.ToLower(res.AccessErr.Error()), "409") {
		t.Fatalf("sync reported a 409 conflict: %v", res.AccessErr)
	}
}

func TestWorktree_DeleteAndReinitReusesVolumeAndNever409s(t *testing.T) {
	worktreeHome(t)
	remoteRoot := t.TempDir()
	rawRemote := "file://" + remoteRoot

	wt1 := t.TempDir()
	proj1 := initWorktree(t, wt1, rawRemote)
	if err := os.MkdirAll(filepath.Join(wt1, "projects", "cost-reduction"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "projects", "cost-reduction", "README.md"), []byte("# Cost reduction\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, ".bdriveignore"), []byte(worktreeIgnore), 0o644); err != nil {
		t.Fatal(err)
	}
	first := cycleWorktree(t, wt1)
	if !first.Pushed {
		t.Fatal("initial worktree sync did not push")
	}
	assertNo409(t, first)
	oldVolume, err := config.VolumeDir(proj1.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeSeq := maxSeq(localOps(t, proj1.ID))
	dev, err := config.LoadDevice()
	if err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(wt1); err != nil {
		t.Fatal(err)
	}
	wt2 := t.TempDir()
	proj2 := initWorktree(t, wt2, rawRemote)
	mounts, err := config.LoadMounts()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mounts[proj2.ID]; !ok {
		t.Fatalf("new worktree row %s missing: %#v", proj2.ID, mounts)
	}
	if mounts[proj2.ID].VolumeID != proj1.ID {
		t.Fatalf("new worktree VolumeID = %q, want shared volume %q: %#v", mounts[proj2.ID].VolumeID, proj1.ID, mounts)
	}
	newVolume, err := config.VolumeDir(proj2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newVolume != oldVolume {
		t.Fatalf("reinit selected volume %q, want the old volume %q", newVolume, oldVolume)
	}
	secretPath := filepath.Join(wt2, "custom_ignored", "secret.txt")
	if err := os.MkdirAll(filepath.Dir(secretPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretPath, []byte("must stay local\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	joined := cycleWorktree(t, wt2)
	if joined.Pushed {
		t.Fatal("joining worktree unexpectedly pushed before a local edit")
	}
	if got, err := os.ReadFile(filepath.Join(wt2, "projects", "cost-reduction", "README.md")); err != nil || string(got) != "# Cost reduction\n" {
		t.Fatalf("README.md after reinit = %q (%v), want the existing project file", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(wt2, ".bdriveignore")); err != nil || string(got) != worktreeIgnore {
		t.Fatalf(".bdriveignore after reinit = %q (%v), want the custom rules, not starterIgnore", got, err)
	}
	if _, err := os.Stat(secretPath); err != nil {
		t.Fatalf("custom-ignored file was not left on disk during the first wt2 cycle: %v", err)
	}
	for _, op := range localOps(t, proj2.ID) {
		if op.Path == "custom_ignored/secret.txt" {
			t.Fatal("custom-ignored file was journaled on wt2's first cycle")
		}
	}
	sharedStore, err := store.Open(newVolume)
	if err != nil {
		t.Fatal(err)
	}
	syncState, err := sharedStore.LoadSync()
	if err != nil {
		t.Fatal(err)
	}
	if syncState.IgnoreAccepted != worktreeIgnore {
		t.Fatalf("shared store IgnoreAccepted = %q, want custom .bdriveignore text", syncState.IgnoreAccepted)
	}

	if err := os.WriteFile(filepath.Join(wt2, "projects", "cost-reduction", "dashboard.md"), []byte("# Dashboard\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pushed := cycleWorktree(t, wt2)
	if !pushed.Pushed {
		t.Fatal("new worktree edit was not pushed")
	}
	assertNo409(t, pushed)
	afterSeq := maxSeq(localOps(t, proj2.ID))
	if afterSeq <= beforeSeq {
		t.Fatalf("device journal sequence did not advance: before=%d after=%d", beforeSeq, afterSeq)
	}

	ops := remoteOps(t, rawRemote, dev.ID)
	var readmePut bool
	for _, op := range ops {
		if op.Path == "projects/cost-reduction/README.md" && op.Kind == journal.KindPut {
			readmePut = true
		}
		if op.Path == "projects/cost-reduction/README.md" && op.Kind == journal.KindDelete {
			t.Fatal("reinitializing a deleted worktree pushed a README.md delete to the hub")
		}
	}
	if !readmePut {
		t.Fatal("hub journal no longer contains the original README.md put")
	}
}

func TestWorktree_ConcurrentMountsShareVolumeAndIsolateInbound(t *testing.T) {
	worktreeHome(t)
	rawRemote := "file://" + t.TempDir()
	wt1, wt2 := t.TempDir(), t.TempDir()
	proj1 := initWorktree(t, wt1, rawRemote)
	proj2 := initWorktree(t, wt2, rawRemote)
	v1, err := config.VolumeDir(proj1.ID)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := config.VolumeDir(proj2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v1 != v2 {
		t.Fatalf("concurrent mounts do not share a volume: %q != %q", v1, v2)
	}

	cycleWorktree(t, wt1)
	cycleWorktree(t, wt2)
	st, err := store.Open(v1)
	if err != nil {
		t.Fatal(err)
	}
	initialInbound, err := st.DrainInboundForMount(proj2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(initialInbound) != 1 || initialInbound[0].Path != ".bdriveignore" {
		t.Fatalf("wt2 initial inbound = %#v, want the materialized .bdriveignore", initialInbound)
	}
	if err := os.WriteFile(filepath.Join(wt1, "a.md"), []byte("from wt1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pushed := cycleWorktree(t, wt1)
	if !pushed.Pushed {
		t.Fatal("wt1 edit was not pushed")
	}
	pulled := cycleWorktree(t, wt2)
	if pulled == nil {
		t.Fatal("wt2 sync returned no result")
	}
	if got, err := os.ReadFile(filepath.Join(wt2, "a.md")); err != nil || string(got) != "from wt1\n" {
		t.Fatalf("wt2 a.md = %q (%v), want wt1's materialized edit", got, err)
	}

	got1, err := st.DrainInboundForMount(proj1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got1) != 0 {
		t.Fatalf("wt1 inbound spool unexpectedly contains wt2's materialization: %#v", got1)
	}
	got2, err := st.DrainInboundForMount(proj2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 1 || got2[0].Path != "a.md" {
		t.Fatalf("wt2 inbound spool after materializing a.md = %#v, want only a.md", got2)
	}

	if err := st.LogInboundForMount(proj1.ID, "from-wt2.md", false); err != nil {
		t.Fatal(err)
	}
	if err := st.LogInboundForMount(proj2.ID, "from-wt1.md", false); err != nil {
		t.Fatal(err)
	}
	got1, err = st.DrainInboundForMount(proj1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got1) != 1 || got1[0].Path != "from-wt2.md" {
		t.Fatalf("wt1 inbound drain = %#v, want only wt1's explicit event", got1)
	}
	got2, err = st.DrainInboundForMount(proj2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 1 || got2[0].Path != "from-wt1.md" {
		t.Fatalf("wt2 inbound spool was stolen by wt1: %#v", got2)
	}
}

func TestWorktree_StopForgetThenReinitRecoversVolumeViaSyncStateRemote(t *testing.T) {
	worktreeHome(t)
	rawRemote := "file://" + t.TempDir()
	wt1 := t.TempDir()
	proj1 := initWorktree(t, wt1, rawRemote)
	if err := os.WriteFile(filepath.Join(wt1, "before-stop.md"), []byte("before stop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := cycleWorktree(t, wt1)
	if !first.Pushed {
		t.Fatal("initial sync did not push")
	}
	stoppedVolume, err := config.VolumeDir(proj1.ID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(stoppedVolume)
	if err != nil {
		t.Fatal(err)
	}
	syncState, err := st.LoadSync()
	if err != nil {
		t.Fatal(err)
	}
	if syncState.Remote != rawRemote {
		t.Fatalf("sync.json Remote = %q, want %q", syncState.Remote, rawRemote)
	}
	dev, err := config.LoadDevice()
	if err != nil {
		t.Fatal(err)
	}
	if got := remoteOps(t, rawRemote, dev.ID); len(got) == 0 {
		t.Fatal("initial journal was not written to the hub")
	}

	stop := stopCmd()
	stop.SetArgs([]string{wt1, "--forget"})
	if err := stop.Execute(); err != nil {
		t.Fatal(err)
	}
	mounts, err := config.LoadMounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 0 {
		t.Fatalf("stop --forget left mounts behind: %#v", mounts)
	}
	if err := os.RemoveAll(wt1); err != nil {
		t.Fatal(err)
	}

	wt2 := t.TempDir()
	proj2 := initWorktree(t, wt2, rawRemote)
	newVolume, err := config.VolumeDir(proj2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newVolume != stoppedVolume {
		t.Fatalf("reinit did not rediscover the old volume: %q != %q", newVolume, stoppedVolume)
	}
	if err := os.WriteFile(filepath.Join(wt2, "after-reinit.md"), []byte("after reinit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pushed := cycleWorktree(t, wt2)
	if !pushed.Pushed {
		t.Fatal("reinit edit was not pushed")
	}
	assertNo409(t, pushed)
	if got := remoteOps(t, rawRemote, dev.ID); maxSeq(got) <= 1 {
		t.Fatalf("hub journal did not append after reinit: %#v", got)
	}
}

func TestWorktree_SharedVolumePicksMostProgressedStore(t *testing.T) {
	bhome := worktreeHome(t)
	rawRemote := "file://" + t.TempDir()
	if err := config.SaveMounts(map[string]config.MountInfo{
		"m-zzz": {Remote: rawRemote},
		"m-aaa": {Remote: rawRemote},
	}); err != nil {
		t.Fatal(err)
	}

	states := map[string]store.SyncState{
		"m-zzz": {Remote: rawRemote, PushedOps: 5, Lamport: 10},
		"m-aaa": {Remote: rawRemote, PushedOps: 0, Lamport: 1},
	}
	for id, state := range states {
		dir := filepath.Join(bhome, "volumes", id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := store.WriteJSONAtomic(filepath.Join(dir, "sync.json"), state); err != nil {
			t.Fatal(err)
		}
	}

	if got := config.SharedVolumeIDForRemote(rawRemote, ""); got != "m-zzz" {
		t.Fatalf("SharedVolumeIDForRemote selected %q, want most-progressed m-zzz", got)
	}
}

func TestWorktree_UncycledFirstMountStillSeedsStarterIgnore(t *testing.T) {
	worktreeHome(t)
	rawRemote := "file://" + t.TempDir()
	wt1, wt2 := t.TempDir(), t.TempDir()
	initWorktree(t, wt1, rawRemote)
	proj2 := initWorktree(t, wt2, rawRemote)

	ignorePath := filepath.Join(wt2, ".bdriveignore")
	if got, err := os.ReadFile(ignorePath); err != nil || string(got) != starterIgnore {
		t.Fatalf("wt2 .bdriveignore = %q (%v), want starterIgnore", got, err)
	}
	if err := os.WriteFile(filepath.Join(wt2, ".env"), []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := cycleWorktree(t, wt2); res == nil {
		t.Fatal("wt2 sync returned no result")
	}
	for _, op := range localOps(t, proj2.ID) {
		if op.Path == ".env" {
			t.Fatal("starterIgnore failed to exclude .env on wt2's first cycle")
		}
	}
}

func TestWorktree_MovedMountSelfHealsAfterSiblingInit(t *testing.T) {
	worktreeHome(t)
	rawRemote := "file://" + t.TempDir()
	wt1 := t.TempDir()
	proj1 := initWorktree(t, wt1, rawRemote)
	cycleWorktree(t, wt1)
	movedWT1 := filepath.Join(filepath.Dir(wt1), "moved-worktree")
	if err := os.Rename(wt1, movedWT1); err != nil {
		t.Fatal(err)
	}
	wt2 := t.TempDir()
	initWorktree(t, wt2, rawRemote)

	if _, ok, err := config.ResolveMount(movedWT1); err != nil || !ok {
		t.Fatalf("ResolveMount(movedWT1) = ok=%v, err=%v", ok, err)
	}
	mounts, err := config.LoadMounts()
	if err != nil {
		t.Fatal(err)
	}
	mi, ok := mounts[proj1.ID]
	if !ok {
		t.Fatalf("moved mount row %s missing after sibling init: %#v", proj1.ID, mounts)
	}
	if mi.Path != movedWT1 {
		t.Fatalf("moved mount path = %q, want %q: %#v", mi.Path, movedWT1, mounts)
	}
}

func TestWorktree_BackwardCompatLegacyMountUnchanged(t *testing.T) {
	bhome := worktreeHome(t)
	rawRemote := "file://" + t.TempDir()
	legacyFolder := t.TempDir()
	legacy, err := config.SaveProject(legacyFolder, config.Project{
		ID:     "m-legacy",
		Volume: "legacy",
		Remote: rawRemote,
	})
	if err != nil {
		t.Fatal(err)
	}
	legacyVolume := filepath.Join(bhome, "volumes", "m-legacy")
	if err := os.MkdirAll(legacyVolume, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(legacyVolume, "legacy-marker")
	if err := os.WriteFile(marker, []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveMounts(map[string]config.MountInfo{
		legacy.ID: {Path: legacyFolder, Volume: legacy.Volume, Remote: legacy.Remote},
	}); err != nil {
		t.Fatal(err)
	}
	gotLegacy, err := config.VolumeDir("m-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if gotLegacy != legacyVolume {
		t.Fatalf("legacy VolumeDir = %q, want %q", gotLegacy, legacyVolume)
	}

	secondFolder := t.TempDir()
	second := initWorktree(t, secondFolder, rawRemote)
	mounts, err := config.LoadMounts()
	if err != nil {
		t.Fatal(err)
	}
	if mounts[second.ID].VolumeID != "m-legacy" {
		t.Fatalf("second mount VolumeID = %q, want legacy volume m-legacy", mounts[second.ID].VolumeID)
	}
	gotSecond, err := config.VolumeDir(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotSecond != legacyVolume {
		t.Fatalf("second mount VolumeDir = %q, want %q", gotSecond, legacyVolume)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "keep me\n" {
		t.Fatalf("legacy volume marker changed: %q (%v)", got, err)
	}
	if _, ok := mounts[legacy.ID]; !ok {
		t.Fatal("enrolling a second live worktree removed the legacy mount row")
	}
}
