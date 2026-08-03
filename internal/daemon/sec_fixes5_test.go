package daemon

import (
	"os"
	"testing"
)

// Round 6, row 20 — attacking round 5's own fix.
//
// Round 5 changed locked() from fail-OPEN to fail-CLOSED
// (unopenableLockIsRunning: "an unanswerable 'is it running' must not read as
// 'it is gone'"), and carved out exactly one exception: a symlink at the lock
// path, because reporting a daemon there wedged Start() forever.
//
// The exception is drawn on the wrong axis. A symlink is not the only thing
// that can sit at the lock path and not be a daemon's lock file; it is only
// the shape round 5's own hacker happened to plant. Anything that makes
// openLock fail for a reason that is not "someone holds a lock" now reads as a
// live daemon, permanently:
//
//   - a directory at daemon.lock (O_CREATE|O_RDWR on a directory is EISDIR)
//   - a lock file the user cannot open (mode 0000)
//   - a volume directory that does not exist at all (ENOENT with O_CREATE)
//
// The consequence is the exact one the symlink carve-out names: Running()
// reports a daemon, so Start() returns immediately without spawning one, Stop
// refuses ("kill it by hand" — there is nothing to kill), and `bdrive status`
// prints "daemon: running" for a mount that has not synced since. Sync stops
// silently and the operator is told it is healthy — which for a shared project
// means every teammate's changes stop arriving with no signal at all.
//
// Same threat model round 5 used for .bdrive/config.json: anything with write
// access as this user (an agent session, a dependency's install script).
//
// Helper prefix: secfx5.

func TestSec_Daemon_SomethingThatIsNotALockIsNotADaemon(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, vdir string)
	}{
		{"a directory at the lock path", func(t *testing.T, vdir string) {
			os.Remove(LockPath(vdir)) // the liveness probe itself creates it
			if err := os.Mkdir(LockPath(vdir), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"a lock file nobody can open", func(t *testing.T, vdir string) {
			if os.Geteuid() == 0 {
				t.Skip("root opens a 0000 file")
			}
			if err := os.WriteFile(LockPath(vdir), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(LockPath(vdir), 0o000); err != nil {
				t.Fatal(err)
			}
		}},
		{"no volume directory at all", func(t *testing.T, vdir string) {
			if err := os.RemoveAll(vdir); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vdir := t.TempDir()

			// Control: an untouched volume dir has no daemon.
			if pid, ok := Running(vdir); ok {
				t.Fatalf("control failed: Running reports pid %d before anything is planted", pid)
			}

			tc.plant(t, vdir)

			if pid, ok := Running(vdir); ok {
				t.Errorf("Running = (%d, true) with no daemon process anywhere: Start() is now a "+
					"permanent no-op for this mount, Stop() refuses, and `bdrive status` prints "+
					"\"daemon: running\" while sync never runs again", pid)
			}
			// Start must still be able to try. It short-circuits on Running,
			// so a false positive there is the wedge, not a separate bug — but
			// it is the thing that actually breaks, so assert it directly.
			if pid, err := Start(t.TempDir(), vdir, 0, 0); err == nil && pid == 0 {
				t.Errorf("Start returned (0, nil) — it adopted a daemon that does not exist " +
					"and spawned nothing; sync for this mount is off until a human notices")
			}
		})
	}
}
