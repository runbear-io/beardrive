package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// A pidfile outlives the process that wrote it — it sits in ~/.bdrive, which
// survives the reboot that killed the daemon. Liveness therefore cannot be
// "some process owns this number": any same-user process that later recycles
// the pid used to read as a live daemon, which made `bdrive status` lie and
// made Start() a silent no-op, so `bdrive init` left the folder unsynced.
//
// os.Getpid() stands in for the recycler: it is alive, same-user, and
// certainly not a bdrive daemon.
func TestRecycledPidIsNotALiveDaemon(t *testing.T) {
	vdir := t.TempDir()
	writePid(t, vdir, os.Getpid())

	if pid, ok := Running(vdir); ok {
		t.Fatalf("Running reports pid %d as a live daemon; only the lock may say that", pid)
	}
}

// Garbage and stale-but-plausible pidfiles are equally not daemons.
func TestPidFileWithoutLockIsNeverRunning(t *testing.T) {
	for _, body := range []string{"", "\n", "not-a-number", "0", "-1", "999999999"} {
		vdir := t.TempDir()
		if err := os.WriteFile(PidPath(vdir), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, ok := Running(vdir); ok {
			t.Errorf("pidfile %q read as running", body)
		}
	}
}

// The lock is the answer: held → running, released → not. This is what makes
// the reboot case correct without asking the OS about processes at all.
func TestLockDecidesLiveness(t *testing.T) {
	vdir := t.TempDir()
	if _, ok := Running(vdir); ok {
		t.Fatal("a fresh volume dir cannot have a running daemon")
	}

	release, err := hold(LockPath(vdir))
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	writePid(t, vdir, 4242)
	pid, ok := Running(vdir)
	if !ok {
		t.Fatal("a held lock means a running daemon")
	}
	if pid != 4242 {
		t.Fatalf("pid = %d, want the pidfile's 4242 (informational only)", pid)
	}

	// A second holder must be refused: two daemons on one mount would both
	// write the same device journal.
	if _, err := hold(LockPath(vdir)); err == nil {
		t.Fatal("hold succeeded twice — a double daemon is possible")
	}

	release()
	if _, ok := Running(vdir); ok {
		t.Fatal("releasing the lock must end the daemon's liveness")
	}
}

// A held lock with an unreadable pid is still a running daemon — we just
// cannot name it. Stop must say so rather than pretending it stopped one.
func TestLockedWithoutPidFile(t *testing.T) {
	vdir := t.TempDir()
	release, err := hold(LockPath(vdir))
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	pid, ok := Running(vdir)
	if !ok || pid != 0 {
		t.Fatalf("Running = (%d, %v), want (0, true)", pid, ok)
	}
	if stopped, err := Stop(vdir); stopped || err == nil {
		t.Fatalf("Stop = (%v, %v), want (false, error) — nothing to signal", stopped, err)
	}
}

// Stopping when nothing runs is success, and it cleans up the stale pidfile.
func TestStopWithNoDaemon(t *testing.T) {
	vdir := t.TempDir()
	writePid(t, vdir, os.Getpid())

	stopped, err := Stop(vdir)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if stopped {
		t.Fatal("Stop reported killing a daemon that was never running")
	}
	if _, err := os.Stat(PidPath(vdir)); !os.IsNotExist(err) {
		t.Fatal("Stop left the stale pidfile behind")
	}
}

func writePid(t *testing.T, vdir string, pid int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(vdir, "daemon.pid"),
		[]byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
