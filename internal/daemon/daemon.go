// Package daemon runs one user-session supervisor with an independently locked
// sync worker for each project volume.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
)

// The volume store is keyed by the shared project volume, so exactly one
// daemon runs per volume and its pid/log live in the store dir.
func PidPath(volDir string) string {
	return filepath.Join(volDir, "daemon.pid")
}
func LogPath(volDir string) string {
	return filepath.Join(volDir, "daemon.log")
}

// LockPath is the file a live daemon holds an exclusive flock on for its
// whole lifetime. Liveness is the LOCK, not the pidfile: the kernel drops a
// flock when the holder dies — including at reboot, and including a crash —
// so a leftover daemon.pid can never be mistaken for a running daemon.
//
// The pid alone cannot answer this. `kill(pid, 0)` only asks "does some
// process own this number", and daemon.pid outlives the process (it sits in
// ~/.bdrive, which survives reboots). Any same-user process that later
// recycles the pid used to read as a live daemon — which made `bdrive status`
// lie and, worse, made Start() a silent no-op, so the one documented recovery
// (`bdrive init`) left the folder unsynced.
func LockPath(volDir string) string {
	return filepath.Join(volDir, "daemon.lock")
}

// Running reports the daemon pid for a mount if one is alive. Aliveness is the
// flock on LockPath. The pid comes out of that same locked file — written by
// the holder itself (see announce) — and falls back to daemon.pid only for
// display: nothing binds THAT file's contents to the process holding the lock,
// and a daemon killed with -9 leaves it behind for the next process to inherit
// the number. Stop signals the announced pid only (see lockPid).
func Running(volDir string) (int, bool) {
	pid, ok := lockPid(volDir)
	if !ok {
		return 0, false
	}
	if pid > 0 {
		return pid, true
	}
	data, err := os.ReadFile(PidPath(volDir))
	if err != nil {
		return 0, true
	}
	if p, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && p > 0 {
		return p, true
	}
	return 0, true
}

// RunningCurrent reports workers owned by the current-version global daemon.
// Resume must not mistake a standalone legacy daemon for a current supervisor.
func RunningCurrent(volDir string) (int, bool) {
	pid, running := Running(volDir)
	if !running || pid <= 0 {
		return 0, false
	}
	globalPID, global := GlobalRunning()
	if !global || globalPID != pid {
		return 0, false
	}
	if _, upgrade := globalNeedsUpgrade(); upgrade {
		return 0, false
	}
	return pid, true
}

// lockPid reports whether a daemon holds the mount's lock and, if it announced
// one, its pid. This is the only pid anything ever signals: it is written
// inside the lock and truncated when the lock is released, so it cannot name a
// process that has exited.
func lockPid(volDir string) (int, bool) {
	pid, _, running := readLockInfo(LockPath(volDir))
	return pid, running
}

func readLockInfo(path string) (int, string, bool) {
	f, err := openLock(path)
	if err != nil {
		return 0, "", unopenableLockIsRunning(path)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return 0, "", false
	}
	var buf [8192]byte
	n, _ := f.ReadAt(buf[:], 0)
	lines := strings.SplitN(strings.TrimSpace(string(buf[:n])), "\n", 2)
	pid, _ := strconv.Atoi(lines[0])
	if pid < 0 {
		pid = 0
	}
	fingerprint := ""
	if len(lines) == 2 {
		fingerprint = strings.TrimSpace(lines[1])
	}
	return pid, fingerprint, true
}

// openLock opens the lock file without following a symlink. Following one put
// the flock on whatever the link named, so a single symlink inside
// $BDRIVE_HOME made an unrelated long-lived process's lock read as this
// mount's daemon — Start() a no-op and sync silently never restarting, the
// exact failure the flock design exists to eliminate.
func openLock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
}

// held is the set of lock paths THIS process holds. It is the only evidence
// of a daemon available when the lock file cannot be opened.
var held struct {
	sync.Mutex
	paths map[string]bool
}

func markHeld(path string, on bool) {
	held.Lock()
	defer held.Unlock()
	if held.paths == nil {
		held.paths = map[string]bool{}
	}
	if on {
		held.paths[path] = true
	} else {
		delete(held.paths, path)
	}
}

// unopenableLockIsRunning answers the liveness question when the lock file
// cannot be opened at all.
//
// Round 5 made this fail CLOSED with one carve-out for a symlink, and the
// carve-out was on the wrong axis: a directory at the lock path, a lock file
// nobody can open, and a volume directory that does not exist at all all fail
// to open too, and every one of them then read as a live daemon forever —
// Start a permanent no-op, Stop refusing, `bdrive status` reporting a healthy
// mount that has not synced since. The ENOENT case needs no attacker.
//
// The answer is the reason, not the shape: none of those states is a daemon,
// and reporting one wedges the mount. What makes reporting "no daemon" safe
// here is that holdLock opens the SAME path the same way — a second daemon
// cannot start on a lock it cannot open either, so the "two writers of one
// journal" this guard exists for cannot materialize. The one real daemon that
// can exist while the file is unopenable is this process, and it knows.
func unopenableLockIsRunning(path string) bool {
	held.Lock()
	defer held.Unlock()
	return held.paths[path]
}

// locked reports whether another process holds the lock file.
func locked(path string) bool {
	f, err := openLock(path)
	if err != nil {
		return unopenableLockIsRunning(path)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// holdLock takes the daemon's lifetime lock, returning the open file so the
// holder can announce its pid INSIDE it. Process death releases the flock,
// which is the point.
func holdLock(path string) (*os.File, error) {
	f, err := openLock(path)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another daemon is already running for this mount: %w", err)
	}
	markHeld(path, true)
	return f, nil
}

// release drops the lock and clears the announced pid with it, so no number
// outlives the process that owned it.
func release(f *os.File) {
	markHeld(f.Name(), false)
	f.Truncate(0)
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
}

// hold takes the lock and returns its releaser.
func hold(path string) (func(), error) {
	f, err := holdLock(path)
	if err != nil {
		return nil, err
	}
	return func() { release(f) }, nil
}

// announce writes the holder's pid into the locked file — the only pid Stop
// ever signals. It lives and dies with the lock, so it cannot name a process
// that has exited (and been recycled), which daemon.pid routinely did: a
// daemon killed with -9 leaves that file behind for the next process to
// inherit the number.
func announce(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return err
}

// GlobalLockPath identifies the user-session supervisor's lifetime lock.
func GlobalLockPath() (string, error) {
	home, err := config.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "daemon.lock"), nil
}

func GlobalPidPath() (string, error) {
	home, err := config.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "daemon.pid"), nil
}

func GlobalLogPath() (string, error) {
	home, err := config.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "daemon.log"), nil
}

func globalLockInfo() (int, string, bool) {
	path, err := GlobalLockPath()
	if err != nil {
		return 0, "", false
	}
	return readLockInfo(path)
}

func GlobalRunning() (int, bool) {
	pid, _, running := globalLockInfo()
	return pid, running
}

func currentBinaryFingerprint() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	exe = filepath.Clean(exe)
	info, err := os.Stat(exe)
	if err != nil {
		return ""
	}
	return "fp:" + exe + ":" + strconv.FormatInt(info.Size(), 10) + ":" +
		strconv.FormatInt(info.ModTime().UnixNano(), 10)
}

func announceGlobal(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.WriteAt([]byte(fmt.Sprintf("%d\n%s\n", os.Getpid(), currentBinaryFingerprint())), 0); err != nil {
		return err
	}
	path, err := GlobalPidPath()
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
}

func globalNeedsUpgrade() (int, bool) {
	pid, fingerprint, running := globalLockInfo()
	return pid, running && pid > 0 && pid != os.Getpid() &&
		fingerprint != "" && fingerprint != currentBinaryFingerprint()
}

func WakeSupervisor() bool {
	pid, running := GlobalRunning()
	return running && pid > 0 && syscall.Kill(pid, syscall.SIGHUP) == nil
}

func isTestExecutable(exe string) bool {
	return strings.HasSuffix(exe, ".test") || strings.Contains(exe, "/_test/")
}

func EnsureRunningBestEffort(volDir string, scanInterval, remoteInterval time.Duration) {
	exe, err := os.Executable()
	if err != nil || isTestExecutable(exe) {
		return
	}
	_, _ = Start("", volDir, scanInterval, remoteInterval)
}

// EnsureRunningAsync starts the detached process before a short-lived hook
// exits, without waiting for the supervisor or volume worker to announce.
func EnsureRunningAsync(volDir string, scanInterval, remoteInterval time.Duration) {
	exe, err := os.Executable()
	if err != nil || isTestExecutable(exe) {
		return
	}
	_, fingerprint, global := globalLockInfo()
	if global && fingerprint != "" && fingerprint == currentBinaryFingerprint() {
		if volDir == "" {
			WakeSupervisor()
		} else if _, running := Running(volDir); !running {
			WakeSupervisor()
		}
		return
	}
	if oldPID, upgrade := globalNeedsUpgrade(); upgrade {
		path, err := GlobalLockPath()
		if err != nil {
			return
		}
		if err := terminateLockedProcess(oldPID, path); err != nil {
			return
		}
	}
	globalLog, err := GlobalLogPath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(globalLog), 0o700); err != nil {
		return
	}
	logf, err := os.OpenFile(globalLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer logf.Close()
	cmd := exec.Command(exe, "daemon", "run",
		"--scan-interval", scanInterval.String(),
		"--remote-interval", remoteInterval.String())
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return
	}
	_ = cmd.Process.Release()
}

// Start starts or wakes the global supervisor and waits for the requested worker.
// folder is retained for source compatibility; discovery uses the mount registry.
func Start(folder, volDir string, scanInterval, remoteInterval time.Duration) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	if volDir != "" {
		logf, err := os.OpenFile(LogPath(volDir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return 0, err
		}
		logf.Close()
	}
	// Keep the per-volume log private, but a test binary must never touch the
	// user-session home, inspect running daemons, or re-exec itself.
	if isTestExecutable(exe) {
		return 0, fmt.Errorf("refusing to start a daemon from a test binary (%s)", filepath.Base(exe))
	}
	globalLog, err := GlobalLogPath()
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(globalLog), 0o700); err != nil {
		return 0, err
	}
	logf, err := os.OpenFile(globalLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	if err := reapLegacyDaemonIfNeeded(volDir); err != nil {
		return 0, err
	}
	if pid, upgrade := globalNeedsUpgrade(); upgrade {
		path, err := GlobalLockPath()
		if err != nil {
			return 0, err
		}
		if err := terminateLockedProcess(pid, path); err != nil {
			return 0, err
		}
	}
	if volDir != "" {
		if pid, running := Running(volDir); running && pid > 0 {
			return pid, nil
		}
	}
	deadline := time.Now().Add(startTimeout)
	spawned := false
	for time.Now().Before(deadline) {
		if pid, running := GlobalRunning(); running && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGHUP)
		} else if !spawned {
			cmd := exec.Command(exe, "daemon", "run",
				"--scan-interval", scanInterval.String(),
				"--remote-interval", remoteInterval.String())
			cmd.Stdout, cmd.Stderr = logf, logf
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := cmd.Start(); err != nil {
				return 0, err
			}
			if err := cmd.Process.Release(); err != nil {
				return 0, err
			}
			spawned = true
		}
		time.Sleep(20 * time.Millisecond)
		if volDir != "" {
			if pid, running := Running(volDir); running && pid > 0 {
				return pid, nil
			}
		} else if pid, running := GlobalRunning(); running && pid > 0 {
			return pid, nil
		}
	}
	return 0, fmt.Errorf("daemon did not come up within %s; see %s", startTimeout, globalLog)
}

// startTimeout bounds how long Start waits for the child to take the lock and
// write its pid. Generous on purpose: it covers a cold binary on a loaded
// machine, and the only cost of waiting is a slower `bdrive init`.
const startTimeout = 10 * time.Second

// watchRetry is how long to wait before re-opening the hub's change stream
// after it fails or ends. Long enough that a hub which does not implement the
// route (or is down) costs one request a minute rather than one a tick, short
// enough that a hub restart is invisible: the tick is still running the whole
// time, so this delay only ever costs latency, never a sync.
const watchRetry = time.Minute

// watchedPollFactor is how much the remote poll slows down while the hub's
// change stream is open and healthy.
//
// The stream already announces every peer write, so the poll beside it exists
// for exactly one job: catching a stream that has stopped delivering without
// saying so — a half-open TCP connection whose reads simply never return. That
// is a rare failure with a cheap fix, so it wants a safety net, not a
// heartbeat. At the 10s default this makes the watched cadence 5 minutes.
//
// It is a FACTOR rather than an absolute so the relationship survives both
// ends: an operator who tightens --remote-interval tightens the net with it,
// and a test can run the whole contract in seconds.
//
// The measurement that motivated it: one idle device on one project cost
// 17,280 requests a day at the unwatched cadence — 9.4% of the production
// hub's entire daily traffic — to be told nothing by a poll while an open
// stream told it the same nothing for free (docs/hub-load-prd.md).
const watchedPollFactor = 30

// Stop terminates the daemon for a mount and waits for it to exit. Exit is
// observed by the lock being released, not by the pid disappearing: the pid
// could be recycled while we wait, and the lock cannot.
func Stop(volDir string) (bool, error) {
	pid, running := lockPid(volDir)
	if !running {
		os.Remove(PidPath(volDir))
		return false, nil
	}
	if pid <= 0 {
		return stopLegacyUnannouncedDaemon(volDir)
	}
	otherActive, err := otherSupervisorVolumeRunning(volDir, pid)
	if err != nil {
		return false, err
	}
	globalPID, global := GlobalRunning()
	if (global && globalPID == pid) || otherActive || globalLockIndeterminate() {
		if err := store.SetPaused(volDir, true); err != nil {
			return false, err
		}
		deadline := time.Now().Add(5 * time.Second)
		nextWake := time.Time{}
		for time.Now().Before(deadline) {
			if !locked(LockPath(volDir)) {
				os.Remove(PidPath(volDir))
				return true, nil
			}
			if time.Now().After(nextWake) {
				_ = syscall.Kill(pid, syscall.SIGHUP)
				nextWake = time.Now().Add(200 * time.Millisecond)
			}
			time.Sleep(20 * time.Millisecond)
		}
		otherActive, err := otherSupervisorVolumeRunning(volDir, pid)
		if err != nil {
			return false, err
		}
		if otherActive || globalLockIndeterminate() {
			return false, fmt.Errorf("daemon worker for %s did not stop within 5s", volDir)
		}
	}
	if err := terminateLockedProcess(pid, LockPath(volDir)); err != nil {
		return false, err
	}
	os.Remove(PidPath(volDir))
	return true, nil
}

// globalLockIndeterminate distinguishes an unreadable global lock from an
// unlocked one. In particular, an EACCES response is never permission to kill
// a PID which may own multiple workers in another process.
func globalLockIndeterminate() bool {
	path, err := GlobalLockPath()
	if err != nil {
		return false
	}
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return os.IsPermission(err)
	}
	f.Close()
	return false
}

// A stuck worker must not make a per-project stop terminate other projects.
// Only the last worker may use process-level escalation.
func otherSupervisorVolumeRunning(volDir string, pid int) (bool, error) {
	home, err := config.Home()
	if err != nil {
		return false, err
	}
	root := filepath.Join(home, "volumes")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check other volume workers: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		other := filepath.Join(root, entry.Name())
		if filepath.Clean(other) == filepath.Clean(volDir) {
			continue
		}
		if otherPID, running := lockPid(other); running && otherPID == pid {
			return true, nil
		}
	}
	return false, nil
}

func terminateLockedProcess(pid int, path string) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !locked(path) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !locked(path) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("daemon (pid %d) still holds %s after SIGKILL", pid, path)
}

type mountTarget struct {
	folder string
	proj   config.Project
}

type volumeWorker struct {
	expectedRemote string
	cancel         context.CancelFunc
	wakeCh         chan struct{}
	done           chan struct{}
	cancelling     bool
	err            error
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// loadProjectRegular opens and validates one project config without a
// check-then-reopen race. Nonblocking/no-follow keeps FIFOs and symlinks out
// of the supervisor's discovery path.
func loadProjectRegular(folder string) (config.Project, bool, error) {
	path := filepath.Join(folder, config.ProjectDir, "config.json")
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return config.Project{}, false, nil
		}
		return config.Project{}, false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 1<<20 {
		return config.Project{}, false, err
	}
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return config.Project{}, false, err
	}
	var hdr struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(data, &hdr) == nil && hdr.Kind == config.WorkspaceKind {
		return config.Project{}, false, nil
	}
	var p config.Project
	if err := json.Unmarshal(data, &p); err != nil {
		return config.Project{}, false, err
	}
	if p.ID != "" && !config.ValidMountID(p.ID) {
		return config.Project{}, false, fmt.Errorf("invalid mount id")
	}
	for i, entry := range p.Include {
		trimmed := strings.TrimSuffix(entry, "/")
		if trimmed != "" && !strings.ContainsAny(trimmed, "/*?[!") &&
			!strings.HasPrefix(entry, "/") && !strings.HasPrefix(entry, "!") {
			p.Include[i] = "/" + entry
		}
	}
	return p, true, nil
}

// activeMountsForVolume filters registry entries before inspecting any folder,
// so an unrelated volume's filesystem cannot block this worker's sync loop.
func activeMountsForVolume(volDir, expectedRemote string) []mountTarget {
	mounts, err := config.LoadMounts()
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(mounts))
	for id := range mounts {
		v, err := config.VolumeDir(id)
		if err == nil && v == volDir && !store.PausedMount(volDir, id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	var targets []mountTarget
	for _, id := range ids {
		mi := mounts[id]
		proj, ok, err := loadProjectRegular(mi.Path)
		if err != nil || !ok || proj.ID != id ||
			!config.SameRemote(proj.Remote, mi.Remote) || !config.SameRemote(proj.Remote, expectedRemote) {
			continue
		}
		targets = append(targets, mountTarget{folder: mi.Path, proj: proj})
	}
	return targets
}

// activeVolumes validates each registry entry before exposing any folder to sync.
// A changed folder remote cannot redirect a running worker to an unchosen hub.
func activeVolumes() (map[string][]mountTarget, error) {
	mounts, err := config.LoadMounts()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(mounts))
	for id := range mounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	volumes := make(map[string][]mountTarget)
	for _, id := range ids {
		mi := mounts[id]
		volDir, err := config.VolumeDir(id)
		if err != nil || store.PausedMount(volDir, id) {
			continue
		}
		proj, ok, err := loadProjectRegular(mi.Path)
		if err != nil || !ok || proj.ID != id || !config.SameRemote(proj.Remote, mi.Remote) {
			continue
		}
		volumes[volDir] = append(volumes[volDir], mountTarget{folder: mi.Path, proj: proj})
	}
	return volumes, nil
}

// Run owns the user-session lock and reconciles independently locked workers.
func Run(folder string, scanInterval, remoteInterval time.Duration) error {
	if folder != "" {
		if err := ValidateLegacyRunFolder(folder); err != nil {
			return err
		}
	}
	if scanInterval <= 0 || remoteInterval <= 0 {
		return fmt.Errorf("daemon intervals must be positive")
	}
	path, err := GlobalLockPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lockf, err := holdLock(path)
	if err != nil {
		return err
	}
	defer release(lockf)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	reloadCh := make(chan os.Signal, 1)
	signal.Notify(reloadCh, syscall.SIGHUP)
	defer signal.Stop(reloadCh)
	if err := announceGlobal(lockf); err != nil {
		return err
	}
	pidPath, _ := GlobalPidPath()
	defer os.Remove(pidPath)
	if err := reapAllLegacyDaemons(); err != nil {
		return err
	}
	// First-use identity creation must finish before workers start concurrently.
	if _, err := config.LoadDevice(); err != nil {
		return err
	}
	workers := make(map[string]*volumeWorker)
	backoffUntil := make(map[string]time.Time)
	backoffDelay := make(map[string]time.Duration)
	workerDoneCh := make(chan string, 1)
	defer func() {
		for _, w := range workers {
			w.cancel()
		}
		for _, w := range workers {
			<-w.done
		}
	}()
	ticker := time.NewTicker(scanInterval)
	defer ticker.Stop()
	wake := false
	for {
		volumes, err := activeVolumes()
		if err != nil {
			return err
		}
		for volDir, w := range workers {
			matches := false
			for _, mount := range volumes[volDir] {
				if config.SameRemote(mount.proj.Remote, w.expectedRemote) {
					matches = true
					break
				}
			}
			select {
			case <-w.done:
				w.cancel()
				if w.err != nil && !w.cancelling {
					delay := backoffDelay[volDir]
					if delay <= 0 {
						delay = maxDuration(scanInterval, 50*time.Millisecond)
					} else {
						delay *= 2
					}
					delay = minDuration(delay, maxDuration(remoteInterval, 5*time.Second))
					backoffDelay[volDir] = delay
					backoffUntil[volDir] = time.Now().Add(delay)
				} else {
					delete(backoffDelay, volDir)
					delete(backoffUntil, volDir)
				}
				delete(workers, volDir)
				continue
			default:
			}
			if !matches {
				w.cancel()
				w.cancelling = true
			} else if wake {
				select {
				case w.wakeCh <- struct{}{}:
				default:
				}
			}
		}
		for volDir := range backoffUntil {
			if _, active := volumes[volDir]; !active {
				delete(backoffUntil, volDir)
				delete(backoffDelay, volDir)
			}
		}
		for volDir, mounts := range volumes {
			if _, exists := workers[volDir]; exists {
				continue
			}
			if until, ok := backoffUntil[volDir]; ok && time.Now().Before(until) {
				continue
			}
			if err := reapLegacyDaemonIfNeeded(volDir); err != nil {
				return err
			}
			wctx, cancel := context.WithCancel(ctx)
			w := &volumeWorker{
				expectedRemote: mounts[0].proj.Remote, cancel: cancel,
				wakeCh: make(chan struct{}, 1), done: make(chan struct{}),
			}
			workers[volDir] = w
			go func(volDir string, w *volumeWorker) {
				var runErr error
				defer func() {
					w.err = runErr
					w.cancel()
					close(w.done)
					select {
					case workerDoneCh <- volDir:
					default:
					}
					if runErr != nil {
						log.Printf("volume worker %s exited: %v", volDir, runErr)
					}
				}()
				runErr = runVolumeWorker(wctx, volDir, w.expectedRemote, scanInterval, remoteInterval, w.wakeCh)
			}(volDir, w)
		}
		if len(workers) == 0 && len(volumes) == 0 {
			return nil
		}
		wake = false
		select {
		case <-ctx.Done():
			return nil
		case <-workerDoneCh:
		case <-ticker.C:
		}
	}
}

// workerInitHook is a test seam for observing worker initialization attempts.
var workerInitHook func(string)

// workerCycleHook is a test seam set before starting workers and cleared only
// after all workers finish; production leaves it nil.
var workerCycleHook func(string)

func cycleSafely(ctx context.Context, sess *syncer.Session, volDir string, logger *log.Logger) (res *syncer.Result, err error, panicked bool) {
	defer func() {
		if p := recover(); p != nil {
			logger.Printf("cycle panic recovered: %v\n%s", p, debug.Stack())
			err = fmt.Errorf("cycle panic: %v", p)
			panicked = true
		}
	}()
	if workerCycleHook != nil {
		workerCycleHook(volDir)
	}
	res, err = sess.Cycle(ctx)
	return
}

func runVolumeWorker(ctx context.Context, volDir, expectedRemote string, scanInterval, remoteInterval time.Duration, wakeCh <-chan struct{}) error {
	if workerInitHook != nil {
		workerInitHook(volDir)
	}
	st, err := store.Open(volDir)
	if err != nil {
		return err
	}
	dev, err := config.LoadDevice()
	if err != nil {
		return err
	}
	lockf, err := holdLock(LockPath(volDir))
	if err != nil {
		return err
	}
	defer release(lockf)
	if err := announce(lockf); err != nil {
		return err
	}
	if err := os.WriteFile(PidPath(volDir), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return err
	}
	defer os.Remove(PidPath(volDir))
	logf, err := os.OpenFile(LogPath(volDir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	logger := log.New(logf, "", log.LstdFlags)
	logger.Printf("worker started: volume=%s remote=%q device=%s(%s) scan=%s sync=%s",
		volDir, expectedRemote, dev.Name, dev.ID, scanInterval, remoteInterval)
	var be remote.Backend
	defer func() {
		if be != nil {
			be.Close()
		}
	}()
	var lastRemote time.Time
	var lastToken string
	lastAccess := store.AccessOK
	var watch <-chan struct{}
	var watchCancel context.CancelFunc
	var nextWatch, watchOpened time.Time
	stopWatch := func() {
		if watchCancel != nil {
			watchCancel()
			watchCancel = nil
		}
		watch = nil
	}
	defer stopWatch()
	refreshed := make(map[string]bool)
	for {
		if ctx.Err() != nil || store.Paused(volDir) {
			return nil
		}
		targets := activeMountsForVolume(volDir, expectedRemote)
		if len(targets) == 0 {
			logger.Printf("no active mounts remain; exiting")
			return nil
		}
		proj := targets[0].proj
		for _, target := range targets {
			if !refreshed[target.folder] {
				refreshed[target.folder] = true
				go func(folder string) {
					if err := config.RefreshWorkspaceOf(folder); err != nil {
						logger.Printf("workspace manifest not refreshed: %v", err)
					}
				}(target.folder)
			}
		}
		// Re-read settings each tick too, so a login/logout/account switch
		// after the daemon started is reflected in op authorship — otherwise
		// a long-lived daemon stamps every change with a stale identity. The
		// http backend captures its credential at open, so drop it when the
		// token changes and reconnect with the new one.
		settings, _ := config.LoadSettings()
		if settings.Token != lastToken {
			if be != nil {
				be.Close()
				be = nil
			}
			stopWatch() // the stream carries the old credential
			lastToken = settings.Token
			lastRemote = time.Time{}
		}

		// How long this daemon is willing to go without talking to the hub.
		//
		// With the change stream open, a peer's write arrives by push and
		// clears the gate below, so the timer is only catching a stream that
		// died without telling us. Without one, the timer IS the sync: it has
		// to stay at remoteInterval or nothing would notice a peer at all.
		//
		// Note this reads `watch`, which a failed dial, a token change, an
		// offline cycle and the hub's own streamMaxAge all set back to nil —
		// so losing the stream restores the tight cadence on the very next
		// tick rather than at the end of a long one.
		pollEvery := remoteInterval
		if watch != nil {
			pollEvery = remoteInterval * watchedPollFactor
		}
		doRemote := proj.Remote != "" && time.Since(lastRemote) >= pollEvery
		if doRemote && be == nil {
			b, err := remote.Open(ctx, proj.Remote)
			if err != nil {
				logger.Printf("remote unavailable: %v", err)
				doRemote = false
				lastRemote = time.Now()
			} else {
				be = b
			}
		}
		// Open the change stream once there is a backend to open it on. Only
		// hub (https://) remotes implement Watcher; a bucket never will, and
		// the type assertion failing is the normal case, not an error.
		if be != nil && watch == nil && time.Now().After(nextWatch) {
			if wr, ok := be.(remote.Watcher); ok {
				wctx, cancel := context.WithCancel(ctx)
				ch, err := wr.Watch(wctx)
				if err != nil {
					cancel()
					// Quietly: an older hub answers 404 here forever, and this
					// must not become a log line every 30s for a daemon that
					// is otherwise working perfectly.
					nextWatch = time.Now().Add(watchRetry)
				} else {
					watch, watchCancel, watchOpened = ch, cancel, time.Now()
				}
			} else {
				nextWatch = time.Now().Add(time.Hour) // not a hub; stop asking
			}
		}

		var res syncer.Result
		var cycleErr error
		haveResult := false
		mergeResult := func(r *syncer.Result) {
			if r == nil {
				return
			}
			if !haveResult {
				res = *r
				haveResult = true
				return
			}
			res.LocalOps += r.LocalOps
			res.PulledOps += r.PulledOps
			res.Conflicts += r.Conflicts
			res.Merged += r.Merged
			res.Adopted += r.Adopted
			res.Pruned += r.Pruned
			res.Materialized += r.Materialized
			res.Pushed = res.Pushed || r.Pushed
			res.Backfilling = res.Backfilling || r.Backfilling
			res.Inbound = append(res.Inbound, r.Inbound...)
			res.Reverted = append(res.Reverted, r.Reverted...)
			res.Offline = res.Offline || r.Offline
			if res.OfflineErr == nil {
				res.OfflineErr = r.OfflineErr
			}
			res.ReadOnly = res.ReadOnly || r.ReadOnly
			res.NoAccess = res.NoAccess || r.NoAccess
			if res.AccessErr == nil {
				res.AccessErr = r.AccessErr
			}
		}

		// Run the primary target first, then each sibling. A sibling gets the
		// backend on a remote tick, or when an earlier sibling committed local
		// ops that must be published before this target catches up.
		maxLocalTarget := -1
		cyclePanicked := false
		for i, target := range targets {
			if i > 0 && ctx.Err() != nil {
				break
			}
			sess := &syncer.Session{
				Folder:         target.folder,
				MountID:        target.proj.ID,
				ExpectedRemote: target.proj.Remote,
				Store:          st,
				Device:         dev,
				Account:        settings,
			}
			if doRemote || res.LocalOps > 0 {
				sess.Backend = be
			}
			r, rerr, panicked := cycleSafely(ctx, sess, volDir, logger)
			if panicked {
				cyclePanicked = true
				break
			}
			if rerr != nil && cycleErr == nil {
				cycleErr = rerr
			}
			mergeResult(r)
			if r != nil && r.LocalOps > 0 {
				maxLocalTarget = i
			}
		}

		// A later target may have committed after an earlier target's cycle.
		// Replay the shared journal into those earlier folders immediately,
		// without another remote request, so concurrent worktrees converge in
		// the same daemon tick.
		if !cyclePanicked && maxLocalTarget > 0 && ctx.Err() == nil {
			for i := range maxLocalTarget {
				target := targets[i]
				sess := &syncer.Session{
					Folder:         target.folder,
					MountID:        target.proj.ID,
					ExpectedRemote: target.proj.Remote,
					Store:          st,
					Device:         dev,
					Account:        settings,
				}
				r, rerr, panicked := cycleSafely(ctx, sess, volDir, logger)
				if panicked {
					cyclePanicked = true
					break
				}
				if rerr != nil && cycleErr == nil {
					cycleErr = rerr
				}
				mergeResult(r)
			}
		}
		if cyclePanicked {
			// Even a busy watch or repeated SIGHUP must not turn a panic into
			// a hot retry loop. Other volumes keep running during this backoff.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(scanInterval):
			}
			continue
		}

		switch {
		case ctx.Err() != nil:
			logger.Printf("daemon stopping")
			return nil
		case cycleErr != nil:
			logger.Printf("cycle error: %v", cycleErr)
		case res.NoAccess:
			// The connection is fine, the answer isn't: keep the backend and
			// keep ticking cheaply so a re-grant self-heals. Log the
			// transition only — a paused daemon must stay quiet.
			if lastAccess != store.AccessNone {
				logger.Printf("access revoked for this project; sync paused (%v)", res.AccessErr)
				lastAccess = store.AccessNone
			}
			lastRemote = time.Now()
		case res.ReadOnly:
			if lastAccess != store.AccessReadOnly {
				if reason := res.Reason(); reason != "" {
					logger.Printf("read-only on this project, pulling only; local changes stay on this device (%s)", reason)
				} else {
					logger.Printf("read-only on this project, pulling only; local changes stay on this device")
				}
				lastAccess = store.AccessReadOnly
			}
			lastRemote = time.Now()
		case res.Offline:
			logger.Printf("offline, will retry: %v", res.OfflineErr)
			if be != nil {
				be.Close()
				be = nil
			}
			stopWatch() // its connection is to the host we just failed to reach
			lastRemote = time.Now()
		default:
			// "access restored" is a claim about the hub, and a local-only tick
			// never asked it one. Announcing it there is what made a refused
			// device alternate between this line and the read-only one every few
			// seconds: three cheap scan ticks run between remote passes, each
			// cleared the flag, and the next remote pass set it again.
			if doRemote && lastAccess != store.AccessOK {
				logger.Printf("access restored; syncing normally")
				lastAccess = store.AccessOK
			}
			if res.Activity() {
				logger.Printf("local+%d pulled+%d merged=%d conflicts=%d files~%d pushed=%v",
					res.LocalOps, res.PulledOps, res.Merged, res.Conflicts, res.Materialized, res.Pushed)
			}
			if doRemote {
				lastRemote = time.Now()
			}
			if res.LocalOps > 0 && !doRemote {
				lastRemote = time.Time{} // push local edits on the next tick
			}
			if res.Backfilling {
				lastRemote = time.Time{} // fetch the next batch on the next tick
			}
		}

		select {
		case <-ctx.Done():
			logger.Printf("daemon stopping")
			return nil
		case _, open := <-watch:
			if !open {
				// The stream ended. A stream that ran a long time was healthy
				// and hit the hub's streamMaxAge, so re-dial at once; one that
				// died quickly is a hub that is down or has no such route, and
				// backs off. Without the distinction every client would go
				// blind for watchRetry once an hour, on purpose, forever.
				lived := time.Since(watchOpened)
				stopWatch()
				if lived < watchRetry {
					nextWatch = time.Now().Add(watchRetry)
				}
				break
			}
			// A peer pushed. Clearing the gate makes the cycle below a remote
			// one instead of a cheap local scan.
			lastRemote = time.Time{}
		case <-wakeCh:
			lastRemote = time.Time{}
		case <-time.After(scanInterval):
		}
	}
}
