package webapp

// Regression: two daemon starts close together must not leave daemon.pid
// naming a process that is not the one holding the lock.
//
// Liveness is the flock (see internal/daemon.LockPath), but Start() writes
// daemon.pid from the PARENT, with the pid of a child that has not taken the
// lock yet. The realistic collision is `bdrive init` immediately followed by
// the login agent's `bdrive resume`: Running() still reads false, so resume
// starts a second daemon, the loser exits, and its pid is already in the file.
//
// The damage is not cosmetic. `bdrive stop` signals the pid from that file, so
// it fails with ESRCH while the surviving daemon keeps syncing — sync cannot be
// turned off, and `stop` is consent withdrawal. `bdrive status` also prints the
// phantom pid.
//
// TestCLIOnboardingE2E already asserts `resume` reports "already running 1",
// but only after several intervening assertions have given the child time to
// lock. This test deliberately runs resume with no delay, which is the window a
// real login agent can land in.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestCLIDaemonPidFileNamesTheLockHolder(t *testing.T) {
	e := newCLIEnv(t)
	work := t.TempDir()

	if out, err := e.run(work, "init", "--name", "race-e2e", "--yes"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	// No sleep on purpose: the absence of one IS the test.
	resumeOut, err := e.run(work, "resume")
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, resumeOut)
	}

	pidPath := findDaemonPidFile(t, filepath.Join(e.home, ".bdrive", "volumes"))
	pid := readDaemonPid(t, pidPath)
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGTERM) })

	// resume must recognise the daemon init just started rather than race it.
	if !strings.Contains(resumeOut, "already running 1") {
		t.Errorf("resume did not see the daemon init started, so it started another:\n%s", resumeOut)
	}

	// Whatever daemon.pid names must actually exist: stop and status both
	// trust it, and after a reboot it is all that identifies the daemon.
	if err := syscall.Kill(pid, 0); err != nil {
		t.Errorf("daemon.pid names pid %d, which is not running (%v) — stop and status both trust this file", pid, err)
	}

	// The consequence, asserted directly: stop must work and must leave
	// nothing syncing behind it.
	out, err := e.run(work, "stop")
	if err != nil {
		t.Errorf("stop failed while a daemon was running: %v\n%s", err, out)
	}
	if out, err := e.run(work, "status"); err == nil && strings.Contains(out, "daemon:   running") {
		t.Errorf("a daemon is still running after stop:\n%s", out)
	}
}

// findDaemonPidFile returns the single daemon.pid under root, failing if there
// is not exactly one — more than one would mean the test mounted more than it
// meant to, and the assertions below would be about the wrong daemon.
func findDaemonPidFile(t *testing.T, root string) string {
	t.Helper()
	var found []string
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("no volume dir at %s: %v", root, err)
	}
	for _, e := range entries {
		p := filepath.Join(root, e.Name(), "daemon.pid")
		if _, err := os.Stat(p); err == nil {
			found = append(found, p)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one daemon.pid under %s, found %d: %v", root, len(found), found)
	}
	return found[0]
}

func readDaemonPid(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("%s does not hold a pid: %q", path, data)
	}
	return pid
}
