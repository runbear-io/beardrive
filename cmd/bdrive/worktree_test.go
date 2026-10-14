package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
	if err := store.SetPausedMount(vdir, proj.ID, false); err != nil {
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

func TestWorktree_SiblingForgetReloadsIgnoreBeforeMaterializeDelete(t *testing.T) {
	worktreeHome(t)
	rawRemote := "file://" + t.TempDir()
	wt1, wt2 := t.TempDir(), t.TempDir()
	initWorktree(t, wt1, rawRemote)
	initWorktree(t, wt2, rawRemote)
	if err := os.WriteFile(filepath.Join(wt1, "keep.txt"), []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !cycleWorktree(t, wt1).Pushed {
		t.Fatal("initial wt1 sync did not push")
	}
	cycleWorktree(t, wt2)
	if _, err := os.Stat(filepath.Join(wt2, "keep.txt")); err != nil {
		t.Fatalf("keep.txt did not materialize in wt2: %v", err)
	}

	if err := os.WriteFile(filepath.Join(wt1, ".bdriveignore"), []byte("keep.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sess, _, err := openSession(t.Context(), wt1, true)
	if err != nil {
		t.Fatal(err)
	}
	sess.Prune = true
	pruned, cycleErr := sess.Cycle(t.Context())
	closeSession(sess)
	if cycleErr != nil {
		t.Fatal(cycleErr)
	}
	if pruned == nil {
		t.Fatal("wt1 prune returned no result")
	}

	pulled := cycleWorktree(t, wt2)
	if pulled.PulledOps != 0 {
		t.Fatalf("wt2 pulled %d ops from the hub, want shared-local-journal replay", pulled.PulledOps)
	}
	if got, err := os.ReadFile(filepath.Join(wt2, ".bdriveignore")); err != nil || string(got) != "keep.txt\n" {
		t.Fatalf("wt2 .bdriveignore = %q (%v), want keep.txt rule", got, err)
	}
	if _, err := os.Stat(filepath.Join(wt2, "keep.txt")); err != nil {
		t.Fatalf("wt2 keep.txt was deleted while applying the new ignore rule: %v", err)
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
	syncState.Remote = ""
	if err := st.SaveSync(syncState); err != nil {
		t.Fatal(err)
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

func TestWorktree_StopOneWorktreeDoesNotPauseSibling(t *testing.T) {
	worktreeHome(t)
	rawRemote := "file://" + t.TempDir()
	wt1, wt2 := t.TempDir(), t.TempDir()
	proj1 := initWorktree(t, wt1, rawRemote)
	proj2 := initWorktree(t, wt2, rawRemote)
	if !cycleWorktree(t, wt1).Pushed {
		t.Fatal("wt1 initial sync did not push")
	}
	cycleWorktree(t, wt2)

	out, err := seccliRun(t, stopCmd(), []string{wt1})
	if err != nil {
		t.Fatalf("stop wt1: %v\n%s", err, out)
	}
	if got := syncBlocked(proj1); got != "paused" {
		t.Fatalf("syncBlocked(proj1) = %q, want paused", got)
	}
	if got := syncBlocked(proj2); got != "" {
		t.Fatalf("syncBlocked(proj2) = %q, want runnable sibling", got)
	}
	if err := os.WriteFile(filepath.Join(wt2, "sibling.md"), []byte("still live\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := seccliRun(t, syncCmd(), []string{wt2}); err != nil {
		t.Fatalf("sync wt2 after stopping wt1: %v", err)
	}
}

func TestWorktree_LegacyInboundSpoolNotConsumedBySecondaryMount(t *testing.T) {
	worktreeHome(t)
	rawRemote := "file://" + t.TempDir()
	wt1, wt2 := t.TempDir(), t.TempDir()
	proj1 := initWorktree(t, wt1, rawRemote)
	proj2 := initWorktree(t, wt2, rawRemote)
	vdir, err := config.VolumeDir(proj1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(vdir) != proj1.ID {
		t.Fatalf("primary volume dir base = %q, want %q", filepath.Base(vdir), proj1.ID)
	}
	if got, err := config.VolumeDir(proj2.ID); err != nil || got != vdir {
		t.Fatalf("secondary volume dir = %q (%v), want primary %q", got, err, vdir)
	}
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LogInbound("legacy-event.md", false); err != nil {
		t.Fatal(err)
	}
	gotSecondary, err := st.DrainInboundForMount(proj2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotSecondary) != 0 {
		t.Fatalf("secondary mount consumed legacy inbound spool: %#v", gotSecondary)
	}
	gotPrimary, err := st.DrainInboundForMount(proj1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotPrimary) != 1 || gotPrimary[0].Path != "legacy-event.md" {
		t.Fatalf("primary mount legacy inbound = %#v, want legacy-event.md", gotPrimary)
	}
}

func TestWorktree_ReinitEnrolledLegacyMountKeepsOwnAuthoritativeVolume(t *testing.T) {
	bhome := worktreeHome(t)
	rawRemote := "file://" + t.TempDir()
	wt1 := t.TempDir()
	proj1 := initWorktree(t, wt1, rawRemote)
	ownVolume, err := config.VolumeDir(proj1.ID)
	if err != nil {
		t.Fatal(err)
	}
	ownStore, err := store.Open(ownVolume)
	if err != nil {
		t.Fatal(err)
	}
	if err := ownStore.SaveSync(store.SyncState{Remote: rawRemote, PushedOps: 5, Lamport: 10}); err != nil {
		t.Fatal(err)
	}
	staleVolume := filepath.Join(bhome, "volumes", "m-00000000")
	if err := os.MkdirAll(staleVolume, 0o755); err != nil {
		t.Fatal(err)
	}
	staleStore, err := store.Open(staleVolume)
	if err != nil {
		t.Fatal(err)
	}
	if err := staleStore.SaveSync(store.SyncState{Remote: rawRemote}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := config.EnrollMount(wt1); err != nil {
		t.Fatal(err)
	}
	reloadedVolume, err := config.VolumeDir(proj1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(reloadedVolume) != proj1.ID {
		t.Fatalf("re-enrolled legacy mount selected volume %q, want own volume %q", reloadedVolume, proj1.ID)
	}
}

func TestWorktree_SiblingResumeDoesNotClearPrePRLegacyPause(t *testing.T) {
	worktreeHome(t)
	rawRemote := "file://" + t.TempDir()
	wt1 := t.TempDir()
	proj1 := initWorktree(t, wt1, rawRemote)
	cycleWorktree(t, wt1)
	wtUncycled := t.TempDir()
	projUncycled := initWorktree(t, wtUncycled, rawRemote)
	vdir, err := config.VolumeDir(proj1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(vdir, "state-"+projUncycled.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("uncycled mount already has a state cache (stat err: %v)", err)
	}
	if err := store.SetPaused(vdir, true); err != nil {
		t.Fatal(err)
	}

	wt2 := t.TempDir()
	proj2 := initWorktree(t, wt2, rawRemote)
	if err := store.SetPausedMount(vdir, proj2.ID, false); err != nil {
		t.Fatal(err)
	}
	if !store.PausedMount(vdir, proj1.ID) {
		t.Fatal("pre-PR legacy pause was cleared for the primary mount")
	}
	if !store.PausedMount(vdir, projUncycled.ID) {
		t.Fatal("pre-PR legacy pause was cleared for the uncycled mount")
	}
	if store.PausedMount(vdir, proj2.ID) {
		t.Fatal("sibling mount remains paused after resume")
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

func TestWorktree_DesktopControlsIsolateMountsAndSkipStaleSiblingOnProjectSync(t *testing.T) {
	worktreeHome(t)
	remoteServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "remote unavailable", http.StatusBadGateway)
	}))
	defer remoteServer.Close()
	hubRemote := remoteServer.URL + "/p/p-12345678"

	setup := func(folder, id string) config.Project {
		t.Helper()
		proj, err := config.SaveProject(folder, config.Project{
			ID: id, Volume: "desktop-project", Remote: hubRemote,
		})
		if err != nil {
			t.Fatal(err)
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
		if err := store.SetPausedMount(vdir, proj.ID, false); err != nil {
			t.Fatal(err)
		}
		return proj
	}
	wt1, wt2 := t.TempDir(), t.TempDir()
	proj1 := setup(wt1, "m-aaa")
	proj2 := setup(wt2, "m-bbb")
	app := httptest.NewServer(desktopHandler())
	defer app.Close()

	pauseReq, err := http.NewRequest(http.MethodPost, app.URL+"/api/desktop/p/"+proj1.ID+"/pause", nil)
	if err != nil {
		t.Fatal(err)
	}
	pauseReq.Header.Set("X-Bdrive-Desktop", "1")
	pauseResp, err := http.DefaultClient.Do(pauseReq)
	if err != nil {
		t.Fatal(err)
	}
	pauseResp.Body.Close()
	if pauseResp.StatusCode != http.StatusOK {
		t.Fatalf("pause wt1 status = %d, want 200", pauseResp.StatusCode)
	}

	statusResp, err := http.Get(app.URL + "/api/desktop/status")
	if err != nil {
		t.Fatal(err)
	}
	defer statusResp.Body.Close()
	var status struct {
		Mounts []struct {
			ID     string `json:"id"`
			Paused bool   `json:"paused"`
		} `json:"mounts"`
	}
	if err := json.NewDecoder(statusResp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	seen1, seen2 := false, false
	for _, mount := range status.Mounts {
		switch mount.ID {
		case proj1.ID:
			seen1 = true
			if !mount.Paused {
				t.Fatal("desktop pause did not pause wt1")
			}
		case proj2.ID:
			seen2 = true
			if mount.Paused {
				t.Fatal("desktop pause incorrectly paused wt2")
			}
		}
	}
	if !seen1 || !seen2 {
		t.Fatalf("desktop status omitted mounts: %#v", status.Mounts)
	}

	if err := os.RemoveAll(wt1); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt2, "desktop-sync.md"), []byte("still live\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	syncReq, err := http.NewRequest(http.MethodPost, app.URL+"/api/desktop/p/p-12345678/sync", nil)
	if err != nil {
		t.Fatal(err)
	}
	syncReq.Header.Set("X-Bdrive-Desktop", "1")
	syncResp, err := http.DefaultClient.Do(syncReq)
	if err != nil {
		t.Fatal(err)
	}
	defer syncResp.Body.Close()
	if syncResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(syncResp.Body)
		t.Fatalf("desktop project sync status = %d, want 200: %s", syncResp.StatusCode, body)
	}
	vdir2, err := config.VolumeDir(proj2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(vdir2, "state-"+proj2.ID+".json")); err != nil {
		t.Fatalf("desktop sync did not cycle live wt2 (state cache missing): %v", err)
	}
}

func TestWorktree_SharedVolumeRejectsMismatchedSiblingRemote(t *testing.T) {
	worktreeHome(t)
	remote1 := "file://" + t.TempDir()
	remote2 := "file://" + t.TempDir()
	wt1, wt2 := t.TempDir(), t.TempDir()
	proj1 := initWorktree(t, wt1, remote1)
	proj2 := initWorktree(t, wt2, remote1)
	if !cycleWorktree(t, wt1).Pushed {
		t.Fatal("wt1 initial sync did not push")
	}
	cycleWorktree(t, wt2)
	beforeOps := localOps(t, proj1.ID)

	proj2.Remote = remote2
	if _, err := config.SaveProject(wt2, proj2); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt2, "wrong-remote.md"), []byte("must not journal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sess, _, err := openSession(t.Context(), wt2, true)
	if err != nil {
		t.Fatal(err)
	}
	_, cycleErr := sess.Cycle(t.Context())
	closeSession(sess)
	if cycleErr == nil {
		t.Fatal("sync with a sibling remote mismatch succeeded")
	}
	errText := strings.ToLower(cycleErr.Error())
	if !strings.Contains(errText, "remote") || !strings.Contains(errText, "mismatch") {
		t.Fatalf("sync mismatch error = %q, want a volume remote mismatch", cycleErr)
	}
	afterOps := localOps(t, proj1.ID)
	if len(afterOps) != len(beforeOps) {
		t.Fatalf("mismatched sibling appended to shared journal: before=%d after=%d", len(beforeOps), len(afterOps))
	}
	dev, err := config.LoadDevice()
	if err != nil {
		t.Fatal(err)
	}
	if got := remoteOps(t, remote2, dev.ID); len(got) != 0 {
		t.Fatalf("mismatched sibling pushed wt1 journal to remote2: %#v", got)
	}
}

func TestWorktree_RememberVolumeRemoteSerializesWithStoreLock(t *testing.T) {
	worktreeHome(t)
	rawRemote := "file://" + t.TempDir()
	folder := t.TempDir()
	proj := initWorktree(t, folder, rawRemote)
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSync(store.SyncState{Lamport: 1, PushedOps: 1}); err != nil {
		t.Fatal(err)
	}
	unlock, err := st.Lock()
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- config.RememberVolumeRemote(proj.ID, proj.Remote)
	}()
	<-started
	time.Sleep(20 * time.Millisecond)
	if err := st.SaveSync(store.SyncState{
		Lamport: 42, PushedOps: 10, IgnoreAccepted: "# rules\n",
	}); err != nil {
		t.Fatal(err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := st.LoadSync()
	if err != nil {
		t.Fatal(err)
	}
	if got.Lamport != 42 || got.PushedOps != 10 || got.IgnoreAccepted != "# rules\n" {
		t.Fatalf("RememberVolumeRemote overwrote newer sync state: %#v", got)
	}
	if got.Remote != proj.Remote {
		t.Fatalf("RememberVolumeRemote Remote = %q, want %q", got.Remote, proj.Remote)
	}
}

func TestWorktree_ConcurrentEnrollMountPreservesAllRowsAndSharesVolume(t *testing.T) {
	worktreeHome(t)
	rawRemote := "file://" + t.TempDir()
	wt1, wt2 := t.TempDir(), t.TempDir()
	proj1, err := config.SaveProject(wt1, config.Project{Remote: rawRemote})
	if err != nil {
		t.Fatal(err)
	}
	proj2, err := config.SaveProject(wt2, config.Project{Remote: rawRemote})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, folder := range []string{wt1, wt2} {
		wg.Add(1)
		go func(folder string) {
			defer wg.Done()
			_, _, err := config.EnrollMount(folder)
			errs <- err
		}(folder)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	mounts, err := config.LoadMounts()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mounts[proj1.ID]; !ok {
		t.Fatalf("concurrent enrollment lost wt1 row %s: %#v", proj1.ID, mounts)
	}
	if _, ok := mounts[proj2.ID]; !ok {
		t.Fatalf("concurrent enrollment lost wt2 row %s: %#v", proj2.ID, mounts)
	}
	v1, err := config.VolumeDir(proj1.ID)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := config.VolumeDir(proj2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v1 != v2 {
		t.Fatalf("concurrent enrollments selected different volumes: %q != %q", v1, v2)
	}
}

func TestWorktree_CrashAfterAppendOpsBeforeSaveSyncStillWinsVolumeRank(t *testing.T) {
	bhome := worktreeHome(t)
	rawRemote := "file://" + t.TempDir()
	dev, err := config.LoadDevice()
	if err != nil {
		t.Fatal(err)
	}
	if err := config.SaveMounts(map[string]config.MountInfo{
		"m-zzz": {Remote: rawRemote},
		"m-aaa": {Remote: rawRemote},
	}); err != nil {
		t.Fatal(err)
	}
	writeCandidate := func(id string, pushed, lamport int64, count int) {
		t.Helper()
		dir := filepath.Join(bhome, "volumes", id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SaveSync(store.SyncState{Remote: rawRemote, PushedOps: pushed, Lamport: lamport}); err != nil {
			t.Fatal(err)
		}
		ops := make([]journal.Op, count)
		for i := range ops {
			seq := int64(i + 1)
			ops[i] = journal.Op{
				Seq: seq, Lamport: seq, Device: dev.ID,
				Kind: journal.KindPut, Path: id + "-" + string(rune('a'+i)) + ".md",
			}
		}
		if err := st.AppendOps(dev.ID, ops); err != nil {
			t.Fatal(err)
		}
	}
	writeCandidate("m-zzz", 1, 1, 4)
	writeCandidate("m-aaa", 2, 2, 2)

	if got := config.SharedVolumeIDForRemote(rawRemote, ""); got != "m-zzz" {
		t.Fatalf("SharedVolumeIDForRemote selected %q, want crash-recovered m-zzz", got)
	}
}

func TestWorktree_StopForgetPreservesLegacyInboundSpoolForReenrolledMount(t *testing.T) {
	worktreeHome(t)
	rawRemote := "file://" + t.TempDir()
	wt1 := t.TempDir()
	proj1 := initWorktree(t, wt1, rawRemote)
	vdir, err := config.VolumeDir(proj1.ID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LogInbound("queued-before-forget.md", false); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSync(store.SyncState{Remote: rawRemote}); err != nil {
		t.Fatal(err)
	}
	stop := stopCmd()
	stop.SetArgs([]string{wt1, "--forget"})
	if err := stop.Execute(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(wt1); err != nil {
		t.Fatal(err)
	}
	wt2 := t.TempDir()
	proj2 := initWorktree(t, wt2, rawRemote)
	if proj2.ID == proj1.ID {
		t.Fatal("reenrolled worktree unexpectedly reused the deleted mount ID")
	}
	vdir2, err := config.VolumeDir(proj2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if vdir2 != vdir {
		t.Fatalf("reenrolled worktree selected volume %q, want %q", vdir2, vdir)
	}
	got, err := st.DrainInboundForMount(proj2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "queued-before-forget.md" {
		t.Fatalf("reenrolled mount inbound = %#v, want queued-before-forget.md", got)
	}
}

func TestWorktree_OwnContiguousJournalOutranksPeerHeavyTruncatedStore(t *testing.T) {
	bhome := worktreeHome(t)
	rawRemote := "file://" + t.TempDir()
	dev, err := config.LoadDevice()
	if err != nil {
		t.Fatal(err)
	}
	if err := config.SaveMounts(map[string]config.MountInfo{
		"m-zzz": {Remote: rawRemote},
		"m-aaa": {Remote: rawRemote},
	}); err != nil {
		t.Fatal(err)
	}
	appendOps := func(id, device string, count int) {
		t.Helper()
		dir := filepath.Join(bhome, "volumes", id)
		st, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		ops := make([]journal.Op, count)
		for i := range ops {
			seq := int64(i + 1)
			ops[i] = journal.Op{
				Seq: seq, Lamport: seq, Device: device,
				Kind: journal.KindPut, Path: id + "-" + device + "-" + string(rune('a'+i)) + ".md",
			}
		}
		if err := st.AppendOps(device, ops); err != nil {
			t.Fatal(err)
		}
	}
	appendOps("m-zzz", dev.ID, 5)
	appendOps("m-zzz", "peer12345678", 1)
	appendOps("m-aaa", dev.ID, 1)
	appendOps("m-aaa", "peer12345678", 10)
	if got := config.SharedVolumeIDForRemote(rawRemote, ""); got != "m-zzz" {
		t.Fatalf("SharedVolumeIDForRemote selected %q, want own-contiguous m-zzz", got)
	}
}

func TestWorktree_CrashBeforeSaveSyncStillBootstrapsSharedIgnore(t *testing.T) {
	worktreeHome(t)
	rawRemote := "file://" + t.TempDir()
	wt1 := t.TempDir()
	proj1 := initWorktree(t, wt1, rawRemote)
	vdir, err := config.VolumeDir(proj1.ID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	ignoreText := "custom_ignored/\n"
	blob, size, err := st.PutBlobBytes([]byte(ignoreText))
	if err != nil {
		t.Fatal(err)
	}
	dev, err := config.LoadDevice()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendOps(dev.ID, []journal.Op{{
		Seq: 1, Lamport: 1, Device: dev.ID, Kind: journal.KindPut,
		Path: ".bdriveignore", Blob: blob, Size: size,
	}}); err != nil {
		t.Fatal(err)
	}
	wt2 := t.TempDir()
	proj2 := initWorktree(t, wt2, rawRemote)
	secret := filepath.Join(wt2, "custom_ignored", "secret.txt")
	if err := os.MkdirAll(filepath.Dir(secret), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("must remain local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := cycleWorktree(t, wt2); res == nil {
		t.Fatal("wt2 cycle returned no result")
	}
	if got, err := os.ReadFile(filepath.Join(wt2, ".bdriveignore")); err != nil || string(got) != ignoreText {
		t.Fatalf("wt2 .bdriveignore = %q (%v), want crash-recovered rules", got, err)
	}
	for _, op := range localOps(t, proj2.ID) {
		if op.Path == "custom_ignored/secret.txt" {
			t.Fatal("custom-ignored secret was journaled before crash-recovered ignore materialized")
		}
	}
}

func TestWorktree_ReinitWithoutForgetDrainsLegacyInboundAndIsolatesMountNotes(t *testing.T) {
	worktreeHome(t)
	rawRemote := "file://" + t.TempDir()
	wt1, wt2 := t.TempDir(), t.TempDir()
	proj1 := initWorktree(t, wt1, rawRemote)
	proj2 := initWorktree(t, wt2, rawRemote)
	vdir, err := config.VolumeDir(proj1.ID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveNoteForMount(proj1.ID, "wt1-note", time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := st.LoadNoteForMount(proj1.ID); got != "wt1-note" {
		t.Fatalf("wt1 mount note = %q, want wt1-note", got)
	}
	if got := st.LoadNoteForMount(proj2.ID); got != "" {
		t.Fatalf("wt2 mount note = %q, want empty", got)
	}
	if err := st.ClearNoteForMount(proj2.ID); err != nil {
		t.Fatal(err)
	}
	if got := st.LoadNoteForMount(proj1.ID); got != "wt1-note" {
		t.Fatalf("clearing wt2 note cleared wt1 note: %q", got)
	}
	if err := st.LogInbound("before-reinit.md", false); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(wt1); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(wt2); err != nil {
		t.Fatal(err)
	}
	wt2 = t.TempDir()
	proj2 = initWorktree(t, wt2, rawRemote)
	if proj2.ID == proj1.ID {
		t.Fatal("reinitialized worktree unexpectedly reused deleted mount ID")
	}
	got, err := st.DrainInboundForMount(proj2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "before-reinit.md" {
		t.Fatalf("reinitialized mount inbound = %#v, want before-reinit.md", got)
	}
}

func TestWorktree_StopForgetDoesNotClobberConcurrentEnrollment(t *testing.T) {
	worktreeHome(t)
	rawRemote := "file://" + t.TempDir()
	wt1, wt2 := t.TempDir(), t.TempDir()
	proj1 := initWorktree(t, wt1, rawRemote)
	proj2, err := config.SaveProject(wt2, config.Project{Remote: rawRemote})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		stop := stopCmd()
		stop.SetArgs([]string{wt1, "--forget"})
		errs <- stop.Execute()
	}()
	go func() {
		defer wg.Done()
		_, _, err := config.EnrollMount(wt2)
		errs <- err
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	mounts, err := config.LoadMounts()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mounts[proj1.ID]; ok {
		t.Fatalf("stop --forget left proj1 row behind: %#v", mounts)
	}
	if _, ok := mounts[proj2.ID]; !ok {
		t.Fatalf("concurrent enrollment lost proj2 row: %#v", mounts)
	}
}

func TestWorktree_PeerJournalCompletenessOutranksSingleOpStaleStore(t *testing.T) {
	bhome := worktreeHome(t)
	rawRemote := "file://" + t.TempDir()
	dev, err := config.LoadDevice()
	if err != nil {
		t.Fatal(err)
	}
	if err := config.SaveMounts(map[string]config.MountInfo{
		"m-zzz": {Remote: rawRemote},
		"m-aaa": {Remote: rawRemote},
	}); err != nil {
		t.Fatal(err)
	}
	peerOps := make([]journal.Op, 5)
	for i := range peerOps {
		seq := int64(i + 1)
		peerOps[i] = journal.Op{
			Seq: seq, Lamport: seq, Device: "peer12345678",
			Kind: journal.KindPut, Path: "peer-" + string(rune('a'+i)) + ".md",
		}
	}
	zzzDir := filepath.Join(bhome, "volumes", "m-zzz")
	zzzStore, err := store.Open(zzzDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := zzzStore.SaveSync(store.SyncState{Remote: rawRemote}); err != nil {
		t.Fatal(err)
	}
	if err := zzzStore.AppendOps("peer12345678", peerOps); err != nil {
		t.Fatal(err)
	}

	aaaDir := filepath.Join(bhome, "volumes", "m-aaa")
	aaaStore, err := store.Open(aaaDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := aaaStore.SaveSync(store.SyncState{Remote: rawRemote, PushedOps: 1, Lamport: 1}); err != nil {
		t.Fatal(err)
	}
	if err := aaaStore.AppendOps(dev.ID, []journal.Op{{
		Seq: 1, Lamport: 1, Device: dev.ID, Kind: journal.KindPut, Path: "own.md",
	}}); err != nil {
		t.Fatal(err)
	}

	if got := config.SharedVolumeIDForRemote(rawRemote, ""); got != "m-zzz" {
		t.Fatalf("SharedVolumeIDForRemote selected %q, want complete peer-journal m-zzz", got)
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
