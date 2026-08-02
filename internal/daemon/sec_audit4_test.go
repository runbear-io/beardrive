package daemon

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Round 9 — the coverage audit of scoreboard row 20.
//
// Reverting this package's guards one at a time left two of them with no test
// anywhere in the repo: `locked()`'s answer when the lock file cannot be opened
// (round 5's fail-closed rule, later re-derived from the reason rather than the
// shape), and `release()`'s truncation of the pid the holder announced. Both
// are one line, both were introduced as fixes, and both can be deleted with
// `go test ./... -run TestSec` still green.
//
// Helper prefix: secaud4.

// TestSec_Daemon_ALockThisProcessHoldsStaysHeldWhenItCannotBeOpened
//
// locked() is the only thing Stop() uses to decide the daemon has exited. It
// opens the lock file to ask, and when that open FAILS it has to answer from
// somewhere else — `unopenableLockIsRunning`, which reports whether THIS
// process holds it.
//
// Answering "false — no daemon" there is fail-open on the fact the whole
// lifecycle rests on:
//
//   - Stop()'s wait loop ends at the first poll, so `bdrive stop` deletes the
//     pidfile, returns success, and stops nothing — "stop still means stay
//     stopped" broken, with the daemon still syncing;
//   - the same reasoning lets Start()/`bdrive resume` spawn a second daemon,
//     and two daemons on one mount are two writers of one journal, which is the
//     single thing the flock exists to prevent.
//
// A chmod on a file in the user's own $BDRIVE_HOME reaches that state, and so
// does anything that makes the open fail (an EMFILE storm, a revoked directory
// mode). Round 5's test asserts the same property for Running(); locked() —
// the one Stop() polls — was never asserted.
//
// The controls make this the guard's decision: an unheld lock reads false, and
// the same lock reads true before the chmod.
func TestSec_Daemon_ALockThisProcessHoldsStaysHeldWhenItCannotBeOpened(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	vdir := t.TempDir()

	// Control 1: with nobody holding it, locked() says false — so a `true`
	// below is not this helper answering true for everything.
	if locked(LockPath(t.TempDir())) {
		t.Fatal("control: locked() reported a lock nobody holds as held")
	}

	release, err := hold(LockPath(vdir))
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	defer release()

	// Control 2: readable and held reads as held.
	if !locked(LockPath(vdir)) {
		t.Fatal("control: a held, readable lock must read as held")
	}

	if err := os.Chmod(LockPath(vdir), 0o000); err != nil {
		t.Skipf("cannot chmod the lock file: %v", err)
	}
	defer os.Chmod(LockPath(vdir), 0o600)

	if !locked(LockPath(vdir)) {
		t.Fatal("locked() says the lock is free because it could not OPEN daemon.lock, " +
			"while this very process is holding it; Stop() polls this to decide the daemon " +
			"exited, so `bdrive stop` reports success and stops nothing, and Start() is free " +
			"to spawn a second writer of the same journal")
	}
}

// TestSec_Daemon_APidLeftInTheLockByADeadDaemonIsNeverSignalled
//
// The pid Stop() signals is written INSIDE the lock file (announce) precisely
// so that it cannot outlive its process — release() truncates it as it drops
// the flock. Round 5's test proves Stop ignores daemon.pid; nothing proves the
// number inside the lock dies with the lock.
//
// Without the truncation the file keeps the last daemon's pid forever, and
// lockPid() hands it to Stop the moment ANY holder has the lock without having
// announced yet — the window every daemon passes through between holdLock()
// and announce(), and the state `bdrive stop` sees whenever a start is in
// flight. On a machine that has been up a while that number belongs to some
// other process of the same user, and Stop sends it SIGTERM and then SIGKILL.
//
// The secure behaviour: a pid announced by a daemon that has released the lock
// is never signalled by anything.
func TestSec_Daemon_APidLeftInTheLockByADeadDaemonIsNeverSignalled(t *testing.T) {
	vdir := t.TempDir()

	// A live, same-user process that is emphatically not a bdrive daemon —
	// standing in for whatever recycled the dead daemon's pid.
	victim := exec.Command("sleep", "600")
	if err := victim.Start(); err != nil {
		t.Skipf("cannot start a victim process: %v", err)
	}
	exited := make(chan struct{})
	go func() { victim.Wait(); close(exited) }()
	defer func() {
		victim.Process.Kill()
		<-exited
	}()

	// A daemon holds the mount's lock and announces its pid, exactly as Run
	// does; then it exits (release). The number it announced is the victim's,
	// which is what pid recycling means.
	f, err := holdLock(LockPath(vdir))
	if err != nil {
		t.Fatalf("holdLock: %v", err)
	}
	if err := f.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(victim.Process.Pid)+"\n"), 0); err != nil {
		t.Fatal(err)
	}
	release(f)

	// The announcement must not have survived the lock.
	if data, err := os.ReadFile(LockPath(vdir)); err != nil {
		t.Fatalf("lock file: %v", err)
	} else if strings.TrimSpace(string(data)) != "" {
		t.Errorf("releasing the lock left the announced pid %q in %s; nothing binds it to a "+
			"live process any more", strings.TrimSpace(string(data)), LockPath(vdir))
	}

	// The next daemon takes the lock and has not announced yet — the window
	// every start passes through.
	release2, err := hold(LockPath(vdir))
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	defer release2()

	Stop(vdir)

	select {
	case <-exited:
		t.Fatalf("Stop killed pid %d, an unrelated process, because a DEAD daemon's "+
			"announcement was still sitting in %s", victim.Process.Pid, LockPath(vdir))
	case <-time.After(300 * time.Millisecond):
	}
}
