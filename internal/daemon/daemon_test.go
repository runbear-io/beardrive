package daemon

import (
	"bytes"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/store"
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

// TestHelperLegacyDaemon is not a test: re-invoked by
// TestStopRecoversLegacyDaemon it plays a daemon from before the pid moved
// into the lock — holds the flock with nothing written inside it, announces
// itself only via daemon.pid, and exits (releasing the flock) on SIGTERM. Its
// argv carries "daemon run" so Stop's command-line check recognizes it.
func TestHelperLegacyDaemon(t *testing.T) {
	vdir := os.Getenv("BDRIVE_TEST_LEGACY_VDIR")
	if vdir == "" {
		return
	}
	f, err := os.OpenFile(LockPath(vdir), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		os.Exit(1)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		os.Exit(1)
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	if os.Getenv("BDRIVE_TEST_LEGACY_ANNOUNCE") != "" {
		if err := announce(f); err != nil {
			os.Exit(1)
		}
	}
	if err := os.WriteFile(PidPath(vdir), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(1)
	}
	select {
	case <-ch:
		_ = os.WriteFile(filepath.Join(vdir, "legacy-stopped"), []byte("SIGTERM\n"), 0o600)
		os.Exit(0) // the kernel releases the flock with us
	case <-time.After(30 * time.Second):
		os.Exit(1)
	}
}

// A pre-upgrade daemon — flock held, lock file empty, pid only in daemon.pid —
// must still be stoppable: refusing made every pre-upgrade mount unstoppable
// short of kill-by-hand. The fallback is gated on the pidfile's process
// verifiably looking like a bdrive daemon (see the sec test for the negative:
// an arbitrary process named by daemon.pid is never signalled).
func TestStopRecoversLegacyDaemon(t *testing.T) {
	vdir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperLegacyDaemon", "daemon", "run")
	cmd.Env = append(os.Environ(), "BDRIVE_TEST_LEGACY_VDIR="+vdir)
	/* Capture the helper's output and its exit error.

	   Without them the only thing this test could ever say was "never took the
	   lock", which is true of a helper that is slow, a helper that exited at
	   once, and a helper that never started — three different bugs behind one
	   sentence. It failed intermittently on macOS CI for months and every
	   report was that sentence, so the first guess (too slow) got a longer
	   deadline and the next failure arrived at exactly the new deadline. That
	   is what an undiagnosable test costs. */
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(exited) }()
	defer func() {
		cmd.Process.Kill()
		<-exited
	}()

	/* Generous, because what is being waited for is a PROCESS SPAWN, not the
	   lock: the helper takes the flock in microseconds once it is running, and
	   everything before that is the OS starting a fresh copy of this test
	   binary. Five seconds was enough on a developer's machine and not on a
	   loaded macOS CI runner, where this failed intermittently for months
	   (main 67e88d02, and twice in a row on #247) always with this message and
	   always in 5.0s — the shape of a deadline, not of a bug.

	   Still bounded well under the helper's own 30s self-destruct, so a helper
	   that genuinely never takes the lock is still a failure and not a hang. */
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, ok := Running(vdir); ok {
			break
		}
		select {
		case <-exited:
			// It is gone. Say WHY, rather than waiting out a deadline to
			// report the one thing we already knew.
			t.Fatalf("the helper exited before taking the lock: %v\noutput: %s",
				waitErr, strings.TrimSpace(out.String()))
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper daemon never took the lock in %v (still running); output: %s",
				20*time.Second, strings.TrimSpace(out.String()))
		}
		time.Sleep(20 * time.Millisecond)
	}

	stopped, err := Stop(vdir)
	if err != nil || !stopped {
		t.Fatalf("Stop = (%v, %v), want (true, nil) for a legacy daemon", stopped, err)
	}
	if _, ok := Running(vdir); ok {
		t.Fatal("legacy daemon still holds the lock after Stop")
	}
}

// A test binary must never be re-exec'd as a daemon: os.Args[0] is then the
// TEST binary, so "daemon run" re-runs the whole suite — which starts another
// daemon, and so on. One such test took a developer's Mac to load 47 before
// anyone noticed. Start refuses instead; tests that need a real daemon build
// the real binary (cli_e2e_test.go).
func TestStartRefusesToForkTheTestBinary(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	vdir := t.TempDir()
	pid, err := Start(t.TempDir(), vdir, time.Second, time.Second)
	if err == nil {
		t.Fatalf("Start returned pid %d from a test binary — that is the fork bomb", pid)
	}
	if !strings.Contains(err.Error(), "test binary") {
		t.Fatalf("error = %v, want it to name the reason", err)
	}
	if _, ok := Running(vdir); ok {
		t.Fatal("nothing may be left holding the lock")
	}
	info, err := os.Stat(LogPath(vdir))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("Start must create a private volume log before refusing the test binary: info=%v err=%v", info, err)
	}
	if _, err := Start("", "", time.Second, time.Second); err == nil || !strings.Contains(err.Error(), "test binary") {
		t.Fatalf("Start without a volume must also refuse the test binary before accessing global state: %v", err)
	}
	entries, err := os.ReadDir(os.Getenv("BDRIVE_HOME"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("refused test binary touched the user-session home: %v", entries)
	}
}

func TestGlobalLockCarriesBinaryFingerprint(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	path, err := GlobalLockPath()
	if err != nil {
		t.Fatal(err)
	}
	lockf, err := holdLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release(lockf)
	if err := announceGlobal(lockf); err != nil {
		t.Fatal(err)
	}
	pidPath, err := GlobalPidPath()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(pidPath)
	pid, fingerprint, running := globalLockInfo()
	if !running || pid != os.Getpid() || fingerprint != currentBinaryFingerprint() || !strings.HasPrefix(fingerprint, "fp:") {
		t.Fatalf("global lock = (%d, %q, %v), want current pid and fingerprint", pid, fingerprint, running)
	}
	info, err := os.Stat(pidPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("global pid file is not private: info=%v err=%v", info, err)
	}
	if _, upgrade := globalNeedsUpgrade(); upgrade {
		t.Fatal("the current process must never upgrade itself")
	}
	if _, err := lockf.WriteAt([]byte("4242\nfp:old-binary\n"), 0); err != nil {
		t.Fatal(err)
	}
	if pid, upgrade := globalNeedsUpgrade(); !upgrade || pid != 4242 {
		t.Fatalf("stale fingerprint = (%d, %v), want upgrade of 4242", pid, upgrade)
	}
	if err := lockf.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := lockf.WriteAt([]byte("4242\n"), 0); err != nil {
		t.Fatal(err)
	}
	if _, upgrade := globalNeedsUpgrade(); upgrade {
		t.Fatal("a legacy lock without a fingerprint is not an upgrade signal")
	}
}

func TestLockPIDAcceptsFingerprintLine(t *testing.T) {
	volDir := t.TempDir()
	f, err := holdLock(LockPath(volDir))
	if err != nil {
		t.Fatal(err)
	}
	defer release(f)
	if _, err := f.WriteAt([]byte("4242\nfp:binary:123:456\n"), 0); err != nil {
		t.Fatal(err)
	}
	if pid, running := lockPid(volDir); !running || pid != 4242 {
		t.Fatalf("lockPid = (%d, %v), want (4242, true)", pid, running)
	}
}

func TestHelperBlockedGlobalSupervisor(t *testing.T) {
	root := os.Getenv("BDRIVE_TEST_BLOCKED_SUPERVISOR")
	if root == "" {
		return
	}
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	signal.Ignore(syscall.SIGHUP)
	globalPath, err := GlobalLockPath()
	if err != nil {
		os.Exit(1)
	}
	holdHelperLock := func(path string) (*os.File, error) {
		deadline := time.Now().Add(time.Second)
		for {
			f, err := holdLock(path)
			if err == nil || time.Now().After(deadline) {
				return f, err
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	global, err := holdHelperLock(globalPath)
	if err != nil {
		os.Exit(1)
	}
	defer release(global)
	if err := announceGlobal(global); err != nil {
		os.Exit(1)
	}
	for _, name := range []string{"vol-a", "vol-b"} {
		f, err := holdHelperLock(LockPath(filepath.Join(root, name)))
		if err != nil {
			os.Exit(1)
		}
		if err := announce(f); err != nil {
			os.Exit(1)
		}
		// Keep the descriptors reachable for the helper's whole lifetime.
		defer release(f)
	}
	select {
	case <-term:
		os.Exit(0)
	case <-time.After(60 * time.Second):
		os.Exit(1)
	}
}

func startBlockedSupervisor(t *testing.T) (string, string, int) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("BDRIVE_HOME", home)
	root := filepath.Join(home, "volumes")
	volA, volB := filepath.Join(root, "vol-a"), filepath.Join(root, "vol-b")
	for _, volDir := range []string{volA, volB} {
		if err := os.MkdirAll(volDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperBlockedGlobalSupervisor$")
	cmd.Env = append(os.Environ(), "BDRIVE_TEST_BLOCKED_SUPERVISOR="+root)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		select {
		case <-exited:
			return
		default:
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for {
		pidA, a := lockPid(volA)
		pidB, b := lockPid(volB)
		pidG, g := GlobalRunning()
		if a && b && g && pidA == cmd.Process.Pid && pidB == pidA && pidG == pidA {
			break
		}
		select {
		case <-exited:
			t.Fatalf("helper exited before holding both workers: %v\n%s", waitErr, output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper did not acquire both worker locks (a=%v pidA=%d, b=%v pidB=%d, g=%v pidG=%d, want=%d): %s", a, pidA, b, pidB, g, pidG, cmd.Process.Pid, strings.TrimSpace(output.String()))
		}
		time.Sleep(20 * time.Millisecond)
	}
	return volA, volB, cmd.Process.Pid
}

func TestSupervisor_BlockedWorkerStopPreservesOtherProjects(t *testing.T) {
	volA, volB, pid := startBlockedSupervisor(t)
	stopped, err := Stop(volA)
	if stopped || err == nil || !strings.Contains(err.Error(), "did not stop within 5s") {
		t.Fatalf("Stop(blocked A) = (%v, %v), want timeout without killing supervisor", stopped, err)
	}
	if !store.Paused(volA) {
		t.Fatal("blocked project A must remain paused after timeout")
	}
	if actualPID, running := lockPid(volB); !running || actualPID != pid {
		t.Fatalf("stopping blocked A interrupted project B: pid=%d running=%v", actualPID, running)
	}
	if actualPID, running := GlobalRunning(); !running || actualPID != pid {
		t.Fatalf("stopping blocked A terminated supervisor: pid=%d running=%v", actualPID, running)
	}
}

func TestSupervisor_UnreadableGlobalLockDoesNotKillSiblingVolumeOnStop(t *testing.T) {
	volA, volB, pid := startBlockedSupervisor(t)
	globalPath, err := GlobalLockPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(globalPath, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(globalPath, 0o600) })
	otherActive, err := otherSupervisorVolumeRunning(volB, pid)
	if err != nil || !otherActive {
		t.Fatalf("sibling volume detection = (%v, %v), want (true, nil)", otherActive, err)
	}
	if _, running := GlobalRunning(); running {
		t.Log("process can read a mode-0000 global lock; exercising sibling-lock protection")
	} else if !globalLockIndeterminate() {
		t.Fatal("permission-denied global lock must be indeterminate, not absent")
	}
	stopped, err := Stop(volB)
	if stopped || err == nil || !strings.Contains(err.Error(), "daemon worker for "+volB+" did not stop within 5s") {
		t.Fatalf("Stop(B with unreadable global lock) = (%v, %v), want worker timeout", stopped, err)
	}
	if !store.Paused(volB) {
		t.Fatal("project B must be marked paused before the stop attempt")
	}
	if actualPID, running := lockPid(volA); !running || actualPID != pid {
		t.Fatalf("stopping B killed sibling A: pid=%d running=%v", actualPID, running)
	}
	if actualPID, running := lockPid(volB); !running || actualPID != pid {
		t.Fatalf("timeout killed the supervisor: pid=%d running=%v", actualPID, running)
	}
}

func TestSupervisor_EnrolledFIFOConfigDoesNotBlockOtherVolumes(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	projA, projB := supervisorMount(t), supervisorMount(t)
	configPath := filepath.Join(projA.folder, ".bdrive", "config.json")
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(configPath, 0o600); err != nil {
		t.Fatal(err)
	}
	done := supervisorRun(t, projA, projB)
	writeWorktreeFile(t, projB.folder, "b-with-fifo-peer.txt", "B keeps syncing\n")
	worktreeWaitFor(t, "project B syncing beside a FIFO project config", done, func() bool {
		return worktreeRemoteHasOp(projB.remote, "b-with-fifo-peer.txt", "put")
	})
	if _, running := Running(projA.volDir); running {
		t.Fatal("a non-regular project config must not start a worker")
	}
	if pid, running := RunningCurrent(projB.volDir); !running || pid != os.Getpid() {
		t.Fatalf("regular project B has no current worker: pid=%d running=%v", pid, running)
	}
}

func TestRunningCurrentRejectsLegacyAndStaleSupervisor(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	volA := t.TempDir()
	globalPath, err := GlobalLockPath()
	if err != nil {
		t.Fatal(err)
	}
	global, err := holdLock(globalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer release(global)
	worker, err := holdLock(LockPath(volA))
	if err != nil {
		t.Fatal(err)
	}
	defer release(worker)
	pid := os.Getpid() + 1
	if _, err := global.WriteAt([]byte(strconv.Itoa(pid)+"\n"+currentBinaryFingerprint()+"\n"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.WriteAt([]byte(strconv.Itoa(pid)+"\n"), 0); err != nil {
		t.Fatal(err)
	}
	if actualPID, current := RunningCurrent(volA); !current || actualPID != pid {
		t.Fatalf("matching supervisor = (%d, %v), want (%d, true)", actualPID, current, pid)
	}
	for _, tc := range []struct {
		name        string
		pid         int
		fingerprint string
	}{
		{"stale fingerprint", pid, "fp:old-binary"},
		{"different global owner", pid + 1, currentBinaryFingerprint()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := strconv.Itoa(tc.pid) + "\n" + tc.fingerprint + "\n"
			if err := os.WriteFile(globalPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if actualPID, current := RunningCurrent(volA); current || actualPID != 0 {
				t.Fatalf("RunningCurrent = (%d, %v), want (0, false)", actualPID, current)
			}
		})
	}
}

func TestRunningCurrentRejectsStandaloneDaemon(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	volDir := t.TempDir()
	f, err := holdLock(LockPath(volDir))
	if err != nil {
		t.Fatal(err)
	}
	defer release(f)
	if err := announce(f); err != nil {
		t.Fatal(err)
	}
	if pid, running := Running(volDir); !running || pid != os.Getpid() {
		t.Fatalf("standalone lock = (%d, %v), want current process", pid, running)
	}
	if pid, current := RunningCurrent(volDir); current || pid != 0 {
		t.Fatalf("standalone daemon is not a current supervisor worker: (%d, %v)", pid, current)
	}
}

func TestSupervisor_CancellingBlockedWorkerDoesNotBlockStartingNewProject(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	projA := supervisorMount(t)
	entered := make(chan struct{})
	unblock := make(chan struct{})
	var once sync.Once
	workerCycleHook = func(volDir string) {
		if volDir != projA.volDir {
			return
		}
		once.Do(func() { close(entered) })
		<-unblock
	}
	t.Cleanup(func() { workerCycleHook = nil })
	done := supervisorRun(t, projA)
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("supervisor exited before blocked worker entered cycle: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("project A worker did not enter its blocked cycle")
	}
	if err := store.SetPausedMount(projA.volDir, projA.mountID, true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPaused(projA.volDir, true); err != nil {
		t.Fatal(err)
	}
	projB := supervisorMount(t)
	t.Cleanup(func() {
		_ = store.SetPausedMount(projB.volDir, projB.mountID, true)
	})
	if !WakeSupervisor() {
		t.Fatal("failed to wake supervisor after enrolling project B")
	}
	writeWorktreeFile(t, projB.folder, "b-while-a-blocked.txt", "B starts independently\n")
	worktreeWaitFor(t, "project B worker while A is blocked", done, func() bool {
		pid, running := Running(projB.volDir)
		return running && pid == os.Getpid() &&
			worktreeRemoteHasOp(projB.remote, "b-while-a-blocked.txt", journal.KindPut)
	})
	close(unblock)
	if err := store.SetPausedMount(projB.volDir, projB.mountID, true); err != nil {
		t.Fatal(err)
	}
	WakeSupervisor()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("supervisor exited with error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not exit after both projects were paused")
	}
}

func TestSupervisor_BacksOffPersistentWorkerInitFailure(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	projA, projB := supervisorMount(t), supervisorMount(t)
	if err := os.RemoveAll(filepath.Join(projA.volDir, "blobs")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projA.volDir, "blobs"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	workerInitHook = func(volDir string) {
		if volDir == projA.volDir {
			attempts.Add(1)
		}
	}
	t.Cleanup(func() { workerInitHook = nil })
	done := supervisorRun(t, projA, projB)
	worktreeWaitFor(t, "project A initialization attempt", done, func() bool {
		return attempts.Load() > 0
	})
	writeWorktreeFile(t, projB.folder, "b-while-a-fails.txt", "B remains healthy\n")
	worktreeWaitFor(t, "project B sync while A initialization fails", done, func() bool {
		return worktreeRemoteHasOp(projB.remote, "b-while-a-fails.txt", journal.KindPut)
	})
	time.Sleep(250 * time.Millisecond)
	if got := attempts.Load(); got > 10 {
		t.Fatalf("persistent worker initialization hot-looped: %d attempts in 250ms", got)
	}
}
