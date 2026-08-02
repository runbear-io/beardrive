package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
)

// Round 5 security tests for internal/daemon: the unattended process, its
// pid/log/lock trio in $BDRIVE_HOME, and the config it re-reads every tick.
//
// Helper prefix: secdmn.

// Start() re-executes this binary as `<exe> daemon run ...`. Without this
// guard the test binary would re-run its own suite (recursively).
func init() {
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		os.Exit(0)
	}
}

// TestSec_Daemon_StopSignalsOnlyItsOwnDaemon
//
// Stop() takes its target from daemon.pid, a plain file whose contents nothing
// binds to the process holding the lock. The package doc is explicit that the
// pidfile outlives its process, and that a recycled pid used to read as a live
// daemon — that hazard was closed for LIVENESS (the flock) and left open for
// SIGNALLING: Stop still sends SIGTERM, and 5s later SIGKILL, to whatever
// number the file names.
//
// Reaching that state needs no attacker at all. A daemon killed with -9 leaves
// its pidfile behind (the deferred Remove never runs); the next daemon writes
// its own pid only AFTER hold() returns, so between those two instants
// Running() reports the old, possibly recycled, pid as the live daemon — and
// Stop kills it. A same-user process that simply writes the file gets the same
// primitive on demand.
//
// The secure behaviour: Stop must never signal a process that is not the
// daemon holding this mount's lock.
func TestSec_Daemon_StopSignalsOnlyItsOwnDaemon(t *testing.T) {
	vdir := t.TempDir()

	// A live, same-user process that is emphatically not a bdrive daemon.
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

	// A daemon holds the lock for this mount (that is what liveness means).
	release, err := hold(LockPath(vdir))
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	defer release()

	// Control: with no pid to read, Stop refuses to signal anything. The only
	// difference below is a number in a file the daemon does not own, so the
	// delta is this package's decision, not the fixture's.
	if stopped, err := Stop(vdir); stopped || err == nil {
		t.Fatalf("control: Stop = (%v, %v), want (false, error)", stopped, err)
	}

	secdmnWritePid(t, vdir, victim.Process.Pid)
	Stop(vdir)

	select {
	case <-exited:
		t.Fatalf("Stop killed pid %d — an unrelated process named by daemon.pid, "+
			"which the lock holder never wrote and nothing verifies", victim.Process.Pid)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestSec_Daemon_UnreadableLockNeverReadsAsNoDaemon
//
// locked() answers "false — no daemon" whenever it cannot open the lock file
// ("can't tell; treat as not running so Start can try"). That is fail-OPEN on
// the one fact the whole lifecycle rests on:
//
//   - `bdrive status` prints "not running" while sync is running;
//   - `bdrive stop` returns success, deletes the pidfile, and stops nothing —
//     breaking "stop still means stay stopped";
//   - `bdrive resume` / Start believe they may spawn a second daemon (two
//     writers of one journal is the thing the lock exists to prevent).
//
// A chmod on a file in the user's own $BDRIVE_HOME is enough to pin the daemon
// into that state permanently.
func TestSec_Daemon_UnreadableLockNeverReadsAsNoDaemon(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	vdir := t.TempDir()
	release, err := hold(LockPath(vdir))
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	defer release()

	if _, ok := Running(vdir); !ok {
		t.Fatal("control: a held lock must read as a running daemon")
	}
	if err := os.Chmod(LockPath(vdir), 0o000); err != nil {
		t.Skipf("cannot chmod the lock file: %v", err)
	}
	defer os.Chmod(LockPath(vdir), 0o600)

	if _, ok := Running(vdir); !ok {
		t.Fatal("Running says no daemon because it could not open daemon.lock; " +
			"an unanswerable liveness question must fail closed, not report the daemon gone")
	}
	if stopped, err := Stop(vdir); !stopped && err == nil {
		t.Fatalf("Stop = (%v, %v): it reported success without stopping the daemon "+
			"that is still holding the lock", stopped, err)
	}
}

// TestSec_Daemon_StateFilesAreNotWorldReadable
//
// Round 4 closed this for the volume store's journals, state-*.json and
// sync.json (TestSec_Store_VolumeJournalsAreNotWorldReadable). The daemon's own
// three files in the same 0755 directory were not part of that change and are
// all created 0644:
//
//   - daemon.log records the mount id, the working folder's absolute path, the
//     remote URL, the device name+id and per-cycle file counts;
//   - daemon.pid and daemon.lock decide what `bdrive stop` signals and whether
//     anything is running at all.
func TestSec_Daemon_StateFilesAreNotWorldReadable(t *testing.T) {
	vdir := t.TempDir()

	// daemon.lock: created by the liveness probe, i.e. by `bdrive status`.
	Running(vdir)
	secdmnAssertPrivate(t, LockPath(vdir))

	// daemon.log: created by Start before it spawns the child. The child is
	// this test binary, neutered by init() above, so Start finds no daemon and
	// times out — the file is what we came for.
	Start(t.TempDir(), vdir, time.Second, time.Second)
	secdmnAssertPrivate(t, LogPath(vdir))

	// daemon.pid: written by the real loop, once it holds the lock.
	m := secdmnMount(t)
	secdmnRun(t, m)
	secdmnWaitFor(t, "the daemon to announce its pid", func() bool {
		_, err := os.Stat(PidPath(m.volDir))
		return err == nil
	})
	secdmnAssertPrivate(t, PidPath(m.volDir))
}

// TestSec_Daemon_LockPathIsNotFollowedThroughASymlink
//
// hold() and locked() open daemon.lock with plain O_CREATE|O_RDWR, which
// follows a symlink. The flock therefore lands on whatever the link names, so a
// single symlink inside $BDRIVE_HOME decides the answer to "is a daemon
// running" for this mount.
//
// Pointed at a file some unrelated long-lived process holds, locked() is true
// forever: `bdrive status` reports a running daemon, Start() returns early as a
// no-op, and `bdrive resume` counts the mount as already running — so sync
// silently never restarts. That is precisely the failure the flock design was
// introduced to eliminate ("Start() a silent no-op, so the one documented
// recovery left the folder unsynced"), reachable again through the link.
func TestSec_Daemon_LockPathIsNotFollowedThroughASymlink(t *testing.T) {
	vdir := t.TempDir()

	// An unrelated file, flocked by something that is not a bdrive daemon.
	elsewhere := filepath.Join(t.TempDir(), "someone-elses.lock")
	if err := os.WriteFile(elsewhere, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	releaseOther, err := hold(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseOther()

	if err := os.Symlink(elsewhere, LockPath(vdir)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if pid, ok := Running(vdir); ok {
		t.Fatalf("Running = (%d, true): the lock probe followed a symlink out of the "+
			"volume dir and reported an unrelated process's flock as this mount's daemon", pid)
	}
}

// TestSec_Daemon_CorruptConfigDoesNotPropagateDeletes
//
// The stated invariant is that a vanished .bdrive/config.json makes the daemon
// "exit cleanly without propagating deletes" — the folder was moved or renamed,
// not emptied. A config that is present but unusable (truncated, empty, JSON
// null, a directory) must take that same door: the alternative is that one
// corrupt local file turns into a delete op per path, replicated to every
// teammate.
func TestSec_Daemon_CorruptConfigDoesNotPropagateDeletes(t *testing.T) {
	for _, tc := range []struct {
		name, body string
	}{
		{"truncated", `{"id":"m-`},
		{"empty", ``},
		{"json null", `null`},
		{"wrong shape", `[1,2,3]`},
		{"id blanked", `{"volume":"v"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := secdmnMount(t)
			done := secdmnRun(t, m)

			secdmnWaitFor(t, "the first push", func() bool {
				return len(secdmnRemoteOps(t, m)) > 0
			})

			if err := os.WriteFile(filepath.Join(m.folder, ".bdrive", "config.json"),
				[]byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("daemon exited with %v, want a clean exit", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("daemon did not exit after its config became unreadable")
			}

			for _, op := range secdmnRemoteOps(t, m) {
				if strings.Contains(op, `"kind":"delete"`) {
					t.Fatalf("a corrupt %s config propagated a delete: %s", tc.name, op)
				}
			}
		})
	}
}

// TestSec_Daemon_MidRunConfigSwapCannotRedirectTheRemote
//
// The loop re-reads .bdrive/config.json every tick and, on a changed `remote`,
// drops the backend and reconnects to whatever URL that file now names — no
// restart, no user action, no re-validation. The comment justifying the re-read
// cites `bdrive remote set`, a command that no longer exists; `bdrive init` is
// now the only thing that is supposed to write a folder's remote.
//
// .bdrive/config.json is untrusted input by this repo's own precedent (round 4
// validated Project.ID out of it and unbound the device token from
// Project.Remote). Anything that can write one file inside the mount — an agent
// session, a dependency's install script — redirects the whole project's
// contents to a remote of its choice on the next 3s tick. A file:// target
// needs no credential at all, and the daemon then PULLS from that remote too.
//
// The secure behaviour: a mid-run edit must not move the project to a new
// remote behind the user's back.
func TestSec_Daemon_MidRunConfigSwapCannotRedirectTheRemote(t *testing.T) {
	m := secdmnMount(t)
	attacker := t.TempDir()
	done := secdmnRun(t, m)

	secdmnWaitFor(t, "the first push to the real remote", func() bool {
		return len(secdmnRemoteOps(t, m)) > 0
	})

	p, _, err := config.LoadProject(m.folder)
	if err != nil {
		t.Fatal(err)
	}
	p.Remote = "file://" + attacker
	if _, err := config.SaveProject(m.folder, p); err != nil {
		t.Fatal(err)
	}
	// Ordinary work continues in the folder after the edit — this is the file
	// that must not leave for a host the user never chose.
	if err := os.WriteFile(filepath.Join(m.folder, "salary.md"),
		[]byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(4 * time.Second)
	for {
		select {
		case <-deadline:
			// Stop the daemon the documented way before reporting.
			os.RemoveAll(filepath.Join(m.folder, ".bdrive"))
			<-done
			return
		case <-time.After(100 * time.Millisecond):
			if n := secdmnCount(t, attacker); n > 0 {
				got := secdmnTree(t, attacker)
				os.RemoveAll(filepath.Join(m.folder, ".bdrive"))
				<-done
				t.Fatalf("the daemon pushed %d objects to a remote that only appeared in "+
					".bdrive/config.json mid-run; the project changed hosts with no user "+
					"action and no credential:\n%s", n, got)
			}
		}
	}
}

// --- helpers ---------------------------------------------------------------

type secdmnFixture struct {
	folder string
	remote string
	volDir string
}

// secdmnMount builds a one-file project with an isolated $BDRIVE_HOME and a
// file:// remote, registered exactly as `bdrive init` would leave it.
func secdmnMount(t *testing.T) secdmnFixture {
	t.Helper()
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder := t.TempDir()
	rem := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "notes.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := config.SaveProject(folder, config.Project{Remote: "file://" + rem})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(folder); err != nil {
		t.Fatal(err)
	}
	vdir, err := config.VolumeDir(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return secdmnFixture{folder: folder, remote: rem, volDir: vdir}
}

// secdmnRun starts the real loop with fast intervals and returns its exit.
// The daemon is stopped by removing .bdrive (the documented clean exit) rather
// than by a signal, so the test process never installs and drops a SIGTERM
// handler around itself.
func secdmnRun(t *testing.T, m secdmnFixture) chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- Run(m.folder, 25*time.Millisecond, 25*time.Millisecond) }()
	t.Cleanup(func() {
		os.RemoveAll(filepath.Join(m.folder, ".bdrive"))
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	return done
}

func secdmnWaitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// secdmnRemoteOps returns every journal line in the mount's real remote.
func secdmnRemoteOps(t *testing.T, m secdmnFixture) []string {
	t.Helper()
	var out []string
	dir := filepath.Join(m.remote, "journal")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line != "" {
				out = append(out, line)
			}
		}
	}
	return out
}

// secdmnCount counts the objects under a file:// store root.
func secdmnCount(t *testing.T, root string) int {
	t.Helper()
	n := 0
	filepath.Walk(root, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi != nil && !fi.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func secdmnWritePid(t *testing.T, vdir string, pid int) {
	t.Helper()
	if err := os.WriteFile(PidPath(vdir), []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func secdmnAssertPrivate(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("%s was never created: %v", filepath.Base(path), err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("%s mode = %v, want owner-only (0600) — it sits in a 0755 $BDRIVE_HOME",
			filepath.Base(path), fi.Mode().Perm())
	}
}

// secdmnTree lists a file:// store root's objects with their contents, so a
// failure names exactly what left the machine.
func secdmnTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi == nil || fi.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		body, _ := os.ReadFile(p)
		if len(body) > 400 {
			body = body[:400]
		}
		b.WriteString("  " + rel + ": " + strings.TrimSpace(string(body)) + "\n")
		return nil
	})
	return b.String()
}
