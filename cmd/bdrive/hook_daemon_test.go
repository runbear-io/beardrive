package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/daemon"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/store"
)

// holdDaemonLock takes the volume's daemon lock for the rest of the test, so
// the CLI sees a running daemon without one being started.
func holdDaemonLock(t *testing.T, proj config.Project) {
	t.Helper()
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(vdir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(daemon.LockPath(vdir), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	})
	// Announce this process inside the lock, as a daemon does.
	if _, err := f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0); err != nil {
		t.Fatal(err)
	}
	if pid, running := daemon.Running(vdir); !running || pid != os.Getpid() {
		t.Fatalf("the held lock reads as pid %d running=%v", pid, running)
	}
}

// markPulled records a daemon pass against the hub, at the given age.
func markPulled(t *testing.T, proj config.Project, age time.Duration) {
	t.Helper()
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPulled(vdir, []string{proj.ID}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-age)
	if err := os.Chtimes(filepath.Join(vdir, "pulled"), at, at); err != nil {
		t.Fatal(err)
	}
}

// committed reads this device's journal for a project, empty when nothing
// was ever committed.
func committed(t *testing.T, proj config.Project) []journal.Op {
	t.Helper()
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(vdir, "journal", thisDevice(t)+".jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	ops, err := journal.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

func storedNote(t *testing.T, proj config.Project) string {
	t.Helper()
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	return st.LoadNoteForMount(proj.ID)
}

// With the daemon running, the turn-start hook must not walk the tree: the
// uncommitted file stays uncommitted (the daemon's job), while everything the
// turn needs still comes out of the store.
func TestHookLeavesTheSyncToARunningDaemon(t *testing.T) {
	folder, proj := stampFixture(t)
	holdDaemonLock(t, proj)
	markPulled(t, proj, 0)
	seedInbound(t, proj, "notes/readme.md")

	got := runHook(t, folder)

	if ops := committed(t, proj); len(ops) != 0 {
		t.Errorf("the hook ran a cycle beside a running daemon: committed %d ops", len(ops))
	}
	for _, want := range []string{
		"https://hub.example.com/p-12345678",
		"`notes/readme.md`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hook output missing %q:\n%s", want, got)
		}
	}
	if note := storedNote(t, proj); note != "claude-code session sess-42" {
		t.Errorf("note = %q, want the session stamped for the daemon's scans", note)
	}
}

// A daemon that holds its lock but has not reached the hub lately (offline,
// reconnecting, wedged) cannot vouch for the files on disk, so the hook pulls
// itself.
func TestHookPullsWhenTheDaemonHasNotPulledLately(t *testing.T) {
	for name, mark := range map[string]func(*testing.T, config.Project){
		"never pulled": func(*testing.T, config.Project) {},
		"pulled long ago": func(t *testing.T, proj config.Project) {
			markPulled(t, proj, 2*hookPullFresh)
		},
		"pulled just now over a stream past its liveness": func(t *testing.T, proj config.Project) {
			vdir, err := config.VolumeDir(proj.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.MarkPulledUntil(vdir, []string{proj.ID}, time.Now().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			folder, proj := stampFixture(t)
			holdDaemonLock(t, proj)
			mark(t, proj)

			runHook(t, folder)

			if ops := committed(t, proj); len(ops) == 0 {
				t.Error("the hook trusted a daemon that has not pulled lately")
			}
		})
	}
}

// An announcement the daemon hears while the hook reads the store withdraws
// the marker: the hook must notice before it answers, pull the push itself,
// and still report what it had already drained.
func TestHookPullsWhenAnAnnouncementArrivesWhileItReads(t *testing.T) {
	folder, proj := stampFixture(t)
	holdDaemonLock(t, proj)
	markPulled(t, proj, 0)
	seedInbound(t, proj, "notes/readme.md")
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	hookReadSeam = func() { store.ClearPulled(vdir) }
	t.Cleanup(func() { hookReadSeam = nil })

	got := runHook(t, folder)

	if ops := committed(t, proj); len(ops) == 0 {
		t.Error("the hook answered on a vouch withdrawn while it read")
	}
	if !strings.Contains(got, "`notes/readme.md`") {
		t.Errorf("the hook lost what it drained before falling back:\n%s", got)
	}
}

// A daemon restarted inside the freshness window has not passed yet: the
// marker its predecessor left must not vouch for it.
func TestHookPullsBesideARestartedDaemon(t *testing.T) {
	folder, proj := stampFixture(t)
	holdDaemonLock(t, proj)
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vdir, "pulled"), []byte(fmt.Sprintf("%d\n%s\n", os.Getpid()+1, proj.ID)), 0o600); err != nil {
		t.Fatal(err)
	}

	runHook(t, folder)

	if ops := committed(t, proj); len(ops) == 0 {
		t.Error("the hook trusted a pass another daemon process recorded")
	}
}

// A marker from a pass that did not include this mount (it was paused, or
// joined the volume since) vouches for its siblings only.
func TestHookPullsWhenTheMarkerDoesNotCoverTheMount(t *testing.T) {
	folder, proj := stampFixture(t)
	holdDaemonLock(t, proj)
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPulled(vdir, []string{"p-sibling"}); err != nil {
		t.Fatal(err)
	}

	runHook(t, folder)

	if ops := committed(t, proj); len(ops) == 0 {
		t.Error("the hook trusted a pass that did not pull its mount")
	}
}

// A daemon cycle in flight when the turn starts is waited out, so the hook
// reads that pass's finished result: what it materialized is in the turn's
// changed list, not deferred to the next turn.
func TestHookWaitsForTheDaemonCycleInFlight(t *testing.T) {
	folder, proj := stampFixture(t)
	holdDaemonLock(t, proj)
	markPulled(t, proj, 0)
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := st.Lock() // the daemon's cycle
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan string, 1)
	go func() { out <- runHook(t, folder) }()
	time.Sleep(200 * time.Millisecond)
	if err := st.LogInboundForMount(proj.ID, "notes/pulled-in.md", false); err != nil {
		t.Fatal(err)
	}
	markPulled(t, proj, 0)
	unlock()

	if got := <-out; !strings.Contains(got, "`notes/pulled-in.md`") {
		t.Errorf("the hook read before the daemon's cycle finished:\n%s", got)
	}
	if ops := committed(t, proj); len(ops) != 0 {
		t.Errorf("the hook cycled beside a current daemon: committed %d ops", len(ops))
	}
}

// A daemon cycle that outlasts the wait cannot vouch for anything yet, so the
// hook runs its own cycle (which waits for the lock like any cycle).
func TestHookCyclesWhenTheDaemonCycleOutlastsTheWait(t *testing.T) {
	folder, proj := stampFixture(t)
	holdDaemonLock(t, proj)
	markPulled(t, proj, 0)
	old := hookCycleWait
	hookCycleWait = 100 * time.Millisecond
	t.Cleanup(func() { hookCycleWait = old })
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := st.Lock()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { runHook(t, folder); close(done) }()
	time.Sleep(400 * time.Millisecond)
	unlock()
	<-done

	if ops := committed(t, proj); len(ops) == 0 {
		t.Error("the hook trusted the marker while the daemon's cycle was still running")
	}
}

// Between the freshness check and the hook's reads no daemon cycle may run:
// the volume lock stays held until the caller releases it.
func TestDaemonCurrentHoldsTheVolumeLock(t *testing.T) {
	_, proj := stampFixture(t)
	holdDaemonLock(t, proj)
	markPulled(t, proj, 0)
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	release, ok := daemonCurrent(st, proj.ID)
	if !ok {
		t.Fatal("daemonCurrent = false beside a current daemon")
	}
	other, _ := store.Open(vdir)
	if unlock, locked := other.LockWithin(50 * time.Millisecond); locked {
		unlock()
		t.Fatal("a cycle could take the volume lock while the hook was reading")
	}
	release()
	unlock, locked := other.LockWithin(time.Second)
	if !locked {
		t.Fatal("the volume lock stayed held after release")
	}
	unlock()
}

// The after-edit hook beside a daemon that has stopped reaching the hub must
// sync the edit itself.
func TestDeferredSyncCyclesBesideAStaleDaemon(t *testing.T) {
	folder, proj := stampFixture(t)
	holdDaemonLock(t, proj)
	markPulled(t, proj, 2*hookPullFresh)

	runDeferredSync(t, folder)

	if ops := committed(t, proj); len(ops) == 0 {
		t.Error("sync --defer trusted a daemon that has not reached the hub lately")
	}
}

// Without a daemon, the hook is the only thing that syncs, so it cycles.
func TestHookSyncsWhenNoDaemonRuns(t *testing.T) {
	folder, proj := stampFixture(t)

	runHook(t, folder)

	ops := committed(t, proj)
	if len(ops) == 0 {
		t.Fatal("no daemon and the hook committed nothing")
	}
	if ops[0].Session != "sess-42" {
		t.Errorf("Session = %q, want sess-42", ops[0].Session)
	}
}

func runDeferredSync(t *testing.T, folder string) {
	t.Helper()
	c := syncCmd()
	c.SetOut(&bytes.Buffer{})
	c.SetArgs([]string{folder, "--defer", "--note", "claude-code session sess-42"})
	if err := c.Execute(); err != nil {
		t.Fatalf("sync --defer: %v", err)
	}
}

// The after-edit hook runs `sync --defer`: beside a running daemon it only
// records the note, which the daemon then stamps on the edit it picks up.
func TestDeferredSyncOnlyRecordsTheNoteBesideADaemon(t *testing.T) {
	folder, proj := stampFixture(t)
	holdDaemonLock(t, proj)
	markPulled(t, proj, 0)

	runDeferredSync(t, folder)

	if ops := committed(t, proj); len(ops) != 0 {
		t.Errorf("sync --defer cycled beside a running daemon: committed %d ops", len(ops))
	}
	if note := storedNote(t, proj); note != "claude-code session sess-42" {
		t.Errorf("note = %q, want it recorded", note)
	}
}

func TestDeferredSyncCyclesWhenNoDaemonRuns(t *testing.T) {
	folder, proj := stampFixture(t)

	runDeferredSync(t, folder)

	ops := committed(t, proj)
	if len(ops) == 0 {
		t.Fatal("no daemon and sync --defer committed nothing")
	}
	if ops[0].Note != "claude-code session sess-42" {
		t.Errorf("Note = %q, want the session note", ops[0].Note)
	}
}

// Pruning needs a cycle, which --defer may skip, so the two are refused
// together rather than reporting a prune that never ran.
func TestDeferredSyncRefusesPrune(t *testing.T) {
	folder, proj := stampFixture(t)
	holdDaemonLock(t, proj)

	c := syncCmd()
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&bytes.Buffer{})
	c.SetArgs([]string{folder, "--defer", "--prune"})
	if err := c.Execute(); err == nil {
		t.Fatal("sync --defer --prune succeeded")
	}
}

// A session above a mount (a home folder above every mount on the device)
// must not walk it on each turn, even with no daemon to vouch: the mount's
// daemon syncs it, and the turn still gets the mount's links.
func TestHookAboveAMountLeavesTheSyncToItsDaemon(t *testing.T) {
	folder, proj := stampFixture(t)

	out := runHook(t, filepath.Dir(folder))

	if ops := committed(t, proj); len(ops) != 0 {
		t.Errorf("the hook above the mount cycled it: committed %d ops", len(ops))
	}
	if !strings.Contains(out, "p-12345678") {
		t.Errorf("the hook above the mount emitted no link for it:\n%s", out)
	}
}

// The after-edit hook above a mount records the note and leaves the edit to
// the mount's daemon, as beside a daemon that vouches.
func TestDeferredSyncAboveAMountOnlyRecordsTheNote(t *testing.T) {
	folder, proj := stampFixture(t)

	runDeferredSync(t, filepath.Dir(folder))

	if ops := committed(t, proj); len(ops) != 0 {
		t.Errorf("sync --defer above the mount cycled it: committed %d ops", len(ops))
	}
	if note := storedNote(t, proj); note != "claude-code session sess-42" {
		t.Errorf("note = %q, want it recorded", note)
	}
}

// Above a mount there is no cycle to skip, so neither hook waits for the
// daemon's cycle in flight: once per mount, below a home folder, those waits
// add up past the hook's timeout.
func TestHooksAboveAMountDoNotWaitForTheDaemonCycle(t *testing.T) {
	folder, proj := stampFixture(t)
	holdDaemonLock(t, proj)
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := st.Lock() // the daemon's cycle, outlasting both hooks
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	for name, run := range map[string]func(){
		"--hook":  func() { runHook(t, filepath.Dir(folder)) },
		"--defer": func() { runDeferredSync(t, filepath.Dir(folder)) },
	} {
		start := time.Now()
		run()
		if took := time.Since(start); took >= hookCycleWait/2 {
			t.Errorf("%s above the mount took %v waiting on the daemon's cycle", name, took)
		}
	}
}

// One supervisor runs every mount's worker, so a hook above several mounts
// starts it only once each of them holds the session note: otherwise the
// first mount's start could scan the second before its note is in place.
func TestHooksAboveMountsSaveEveryNoteBeforeStartingTheDaemon(t *testing.T) {
	for name, run := range map[string]func(t *testing.T, root string){
		"--hook":  func(t *testing.T, root string) { runHook(t, root) },
		"--defer": func(t *testing.T, root string) { runDeferredSync(t, root) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("BDRIVE_HOME", t.TempDir())
			root, _ := filepath.EvalSymlinks(t.TempDir())
			a := mountAt(t, root, "projA", "https://hub.example.com/p/p-aaaaaaaa")
			b := mountAt(t, root, "projB", "https://hub.example.com/p/p-bbbbbbbb")
			starts := 0
			old := ensureDaemon
			ensureDaemon = func(string, time.Duration, time.Duration) {
				starts++
				for _, p := range []config.Project{a, b} {
					if note := storedNote(t, p); note != "claude-code session sess-42" {
						t.Errorf("daemon started before %s held the note (note %q)", p.Volume, note)
					}
				}
			}
			t.Cleanup(func() { ensureDaemon = old })

			run(t, root)

			if starts != 2 {
				t.Errorf("daemon started %d times, want once per mount", starts)
			}
		})
	}
}
