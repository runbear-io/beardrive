package main

import (
	"bytes"
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
	if !daemonSyncs(vdir) {
		t.Fatal("the held lock does not read as a running daemon")
	}
}

// markPulled records a daemon pass against the hub, at the given age.
func markPulled(t *testing.T, proj config.Project, age time.Duration) {
	t.Helper()
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPulled(vdir); err != nil {
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
