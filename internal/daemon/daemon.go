// Package daemon runs one user-session supervisor with an independently locked
// sync worker for each project volume.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
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

// watcherRulesFingerprint changes whenever a mount's watcher-pruning inputs
// change. Include is in the project config while .bdriveignore is a synced
// file, so both local edits and materialized peer edits must trigger a rebuild.
func watcherRulesFingerprint(targets []mountTarget) string {
	var b strings.Builder
	for _, target := range targets {
		b.WriteString(target.folder)
		b.WriteByte(0)
		for _, include := range target.proj.Include {
			b.WriteString(include)
			b.WriteByte(0)
		}
		info, err := os.Stat(filepath.Join(target.folder, syncer.IgnoreFile))
		if err != nil {
			b.WriteString("missing:")
			b.WriteString(err.Error())
		} else {
			b.WriteString(strconv.FormatInt(info.Size(), 10))
			b.WriteByte(':')
			b.WriteString(strconv.FormatInt(info.ModTime().UnixNano(), 10))
		}
		b.WriteByte(0)
	}
	return b.String()
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
		case <-reloadCh:
			wake = true
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

// workerRemoteWakeHook is a test seam called after a remote wake is received
// and before pending local watcher events are collected.
var workerRemoteWakeHook func(string)

// A healthy local watcher replaces the tight safety scan with this slow poll.
// The watcher is only an accelerator: losing it immediately restores the
// existing scanInterval polling path.
const (
	localWatchSafetyInterval = 5 * time.Minute
	localWatchRetry          = time.Minute
)

func localWatchENOSPCRetry(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	multiplier := []int{1, 2, 5, 10}[min(failures-1, 3)]
	return time.Duration(multiplier) * localWatchRetry
}

func localWatchFallbackInterval(scanInterval, lastScanDuration time.Duration) time.Duration {
	if lastScanDuration <= 0 {
		return scanInterval
	}
	return maxDuration(scanInterval, minDuration(time.Minute, lastScanDuration*15))
}

// localWatchNow is a clock seam for the independent full-scan deadline.
var localWatchNow = time.Now

type localWatchEvent struct {
	root      string
	path      string
	op        fsnotify.Op
	directory bool
	seq       uint64
}

type localWatchBatch struct {
	events []localWatchEvent
	full   bool
	fence  uint64
}

type localFolderWatcher struct {
	done     <-chan struct{}
	wake     <-chan struct{}
	err      <-chan error
	close    func()
	take     func(string) localWatchBatch
	fence    func() uint64
	suppress func(string, uint64, func(localWatchEvent) bool)
}

const maxLocalWatchEvents = 4096

var localWatchHooksMu sync.RWMutex

// localWatchAddHook is a deterministic seam for the recursive Add race. It is
// nil in production and lets Darwin tests force one child ENOENT.
var localWatchAddHook func(string) error

// localWatchErrorHook is a deterministic seam for watcher.Errors. It is nil
// in production and lets tests inject the same errors the backend reports.
var localWatchErrorHook func() <-chan error

// localWatchWalkHook observes the root of each incremental recursive walk.
// It is nil in production and lets tests ensure directory creates do not
// rescan the mount root.
var localWatchWalkHook func(string)

// localWatchCreatedHook observes every watcher allocated by
// startLocalFolderWatcher. It is nil in production and exists only so tests
// can prove a replacement watcher is closed during a concurrent shutdown.
var localWatchCreatedHook func(rawWatcher)

// localWatchBackendOverride pins the fsnotify backend while test hooks are
// installed. localWatchBackendPin is an explicit test choice and is kept
// separate so clearing hooks cannot undo it.
var localWatchBackendOverride string
var localWatchBackendPin string

func getLocalWatchHooks() (func(string) error, func() <-chan error, func(string)) {
	localWatchHooksMu.RLock()
	defer localWatchHooksMu.RUnlock()
	return localWatchAddHook, localWatchErrorHook, localWatchWalkHook
}

func getLocalWatchCreatedHook() func(rawWatcher) {
	localWatchHooksMu.RLock()
	defer localWatchHooksMu.RUnlock()
	return localWatchCreatedHook
}

func setLocalWatchHooks(add func(string) error, errors func() <-chan error, walk func(string)) {
	localWatchHooksMu.Lock()
	defer localWatchHooksMu.Unlock()
	localWatchAddHook, localWatchErrorHook, localWatchWalkHook = add, errors, walk
	if add != nil || errors != nil || walk != nil || localWatchCreatedHook != nil {
		if localWatchBackendOverride == "" {
			localWatchBackendOverride = "fsnotify"
		}
	} else if localWatchBackendOverride == "fsnotify" {
		localWatchBackendOverride = ""
	}
}

func setLocalWatchCreatedHook(hook func(rawWatcher)) {
	localWatchHooksMu.Lock()
	defer localWatchHooksMu.Unlock()
	localWatchCreatedHook = hook
	if hook != nil {
		if localWatchBackendOverride == "" {
			localWatchBackendOverride = "fsnotify"
		}
	} else if localWatchAddHook == nil && localWatchErrorHook == nil && localWatchWalkHook == nil &&
		localWatchBackendOverride == "fsnotify" {
		localWatchBackendOverride = ""
	}
}

func localWatchCreated(w rawWatcher) {
	if hook := getLocalWatchCreatedHook(); hook != nil {
		hook(w)
	}
}

func setLocalWatchBackendOverride(backend string) {
	localWatchHooksMu.Lock()
	localWatchBackendOverride = backend
	localWatchHooksMu.Unlock()
}

func getLocalWatchBackendOverride() string {
	localWatchHooksMu.RLock()
	defer localWatchHooksMu.RUnlock()
	return localWatchBackendOverride
}

func resolveWatchBackends() []string {
	localWatchHooksMu.RLock()
	pin, override := localWatchBackendPin, localWatchBackendOverride
	localWatchHooksMu.RUnlock()
	kind := pin
	if kind == "" {
		kind = override
	}
	if kind == "" {
		kind = os.Getenv("BDRIVE_WATCH_BACKEND")
	}
	if kind == "" || kind == "auto" {
		switch runtime.GOOS {
		case "linux":
			return []string{"fanotify", "fsnotify"}
		case "darwin":
			return []string{"fsevents", "fsnotify"}
		default:
			return []string{"fsnotify"}
		}
	}
	if kind == "fsnotify" {
		return []string{"fsnotify"}
	}
	return []string{kind}
}

func openWatchBackend(kind string, roots []string) (rawWatcher, error) {
	if recursiveMockEnabledForTest() {
		return newRecursiveMockWatcher(), nil
	}
	switch kind {
	case "fanotify":
		return newFanotifyRawWatcher(roots)
	case "fsevents":
		return newFSEventsRawWatcher(roots)
	case "fsnotify":
		w, err := fsnotify.NewWatcher()
		if err != nil {
			return nil, err
		}
		return &fsnotifyRaw{Watcher: w}, nil
	default:
		return nil, fmt.Errorf("%w: %s", errWatchBackendUnsupported, kind)
	}
}

// localWatchNotExist follows wrapped backend errors as well as direct path
// errors. A missing configured root must still be rejected separately.
func localWatchNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}

func startLocalFolderWatcher(ctx context.Context, roots []string) (*localFolderWatcher, error) {
	var watcherMu sync.Mutex
	rootList := make([]string, len(roots))
	rootSet := make(map[string]bool, len(roots))
	for i, root := range roots {
		rootList[i] = filepath.Clean(root)
		rootSet[rootList[i]] = true
	}
	for _, root := range rootList {
		if _, err := os.Stat(root); err != nil {
			return nil, err
		}
	}
	stickyDisabled := make(map[string]bool)
	var watcher rawWatcher
	closed := false
	seen := make(map[string]string)
	ownerFilters := make(map[string]*syncer.Filter)
	dirVerdicts := make(map[string]map[string]bool)
	loadOwnerFilter := func(owner string) *syncer.Filter {
		proj, ok, err := config.LoadProject(owner)
		if err != nil || !ok {
			return nil
		}
		f, _ := syncer.LoadFilter(owner, proj.Include)
		return f
	}
	closeWatcher := func() {
		watcherMu.Lock()
		if closed {
			watcherMu.Unlock()
			return
		}
		closed = true
		current := watcher
		watcherMu.Unlock()
		if current != nil {
			_ = current.Close()
		}
	}
	addDirTo := func(w rawWatcher, watched map[string]string, path, owner string) error {
		path = filepath.Clean(path)
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		_, _, walkHook := getLocalWatchHooks()
		if walkHook != nil {
			walkHook(path)
		}
		ownerFilter := ownerFilters[owner]
		if ownerFilter == nil {
			ownerFilter = loadOwnerFilter(owner)
			ownerFilters[owner] = ownerFilter
		}
		if w.Recursive() {
			addHook, _, _ := getLocalWatchHooks()
			if addHook != nil {
				if err := addHook(path); err != nil {
					return err
				}
			}
			if !info.IsDir() {
				return nil
			}
			if err := w.Add(owner); err != nil {
				return err
			}
			watched[owner] = owner
			return nil
		}
		return filepath.WalkDir(path, func(walkPath string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !d.IsDir() {
				return nil
			}
			walkPath = filepath.Clean(walkPath)
			if walkPath != owner && !rootSet[walkPath] {
				if config.ReservedDir(d.Name()) || config.IsMount(walkPath) {
					return filepath.SkipDir
				}
				if ownerFilter != nil {
					rel, relErr := filepath.Rel(owner, walkPath)
					if relErr == nil && rel != "." && ownerFilter.PruneDir(filepath.ToSlash(rel)) {
						return filepath.SkipDir
					}
				}
			}
			if _, ok := watched[walkPath]; ok {
				return nil
			}
			addHook, _, _ := getLocalWatchHooks()
			if addHook != nil {
				if err := addHook(walkPath); err != nil {
					return err
				}
			}
			watcherMu.Lock()
			err := w.Add(walkPath)
			watcherMu.Unlock()
			if err != nil {
				return err
			}
			watched[walkPath] = owner
			return nil
		})
	}
	buildWatcher := func(old rawWatcher) (rawWatcher, map[string]string, error) {
		oldClosed := false
		var lastErr error
		for _, kind := range resolveWatchBackends() {
			if stickyDisabled[kind] {
				continue
			}
			for range 2 {
				w, err := openWatchBackend(kind, rootList)
				if err == nil {
					localWatchCreated(w)
					candidateSeen := make(map[string]string)
					for _, root := range rootList {
						if err = addDirTo(w, candidateSeen, root, root); err != nil {
							break
						}
					}
					if err == nil {
						return w, candidateSeen, nil
					}
					_ = w.Close()
				}
				lastErr = err
				if isResourceWatchErr(err) && old != nil && !oldClosed {
					watcherMu.Lock()
					if !closed {
						_ = old.Close()
						oldClosed = true
					}
					watcherMu.Unlock()
					continue
				}
				if isStructuralWatchErr(err) {
					stickyDisabled[kind] = true
					break
				}
				if isResourceWatchErr(err) {
					break
				}
				return nil, nil, err
			}
		}
		if lastErr != nil && !errors.Is(lastErr, errWatchBackendUnsupported) {
			return nil, nil, lastErr
		}
		return nil, nil, fmt.Errorf("%w: no watcher backend", errWatchBackendUnsupported)
	}
	nw, candidateSeen, err := buildWatcher(nil)
	if err != nil {
		return nil, err
	}
	watcher, seen = nw, candidateSeen
	rebuildWatcher := func() error {
		watcherMu.Lock()
		if closed {
			watcherMu.Unlock()
			return nil
		}
		old := watcher
		watcherMu.Unlock()
		nw, candidateSeen, err := buildWatcher(old)
		if err != nil {
			return err
		}
		watcherMu.Lock()
		if closed {
			watcherMu.Unlock()
			_ = nw.Close()
			return nil
		}
		old = watcher
		watcher, seen = nw, candidateSeen
		watcherMu.Unlock()
		if old != nil {
			_ = old.Close()
		}
		return nil
	}
	forgetDir := func(path string, directory bool) {
		path = filepath.Clean(path)
		watcherMu.Lock()
		current := watcher
		if current != nil {
			current.ForgetPrefix(path)
		}
		watcherMu.Unlock()
		if _, ok := seen[path]; !ok && !directory {
			return
		}
		for watched := range seen {
			if cleanWatchPrefix(path, watched) {
				delete(seen, watched)
			}
		}
	}
	rootFor := func(name string) string {
		name = filepath.Clean(name)
		best := ""
		for _, root := range rootList {
			rel, err := filepath.Rel(root, name)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) &&
				(best == "" || len(root) > len(best)) {
				best = root
			}
		}
		return best
	}
	wake := make(chan struct{}, 1)
	errCh := make(chan error, 1)
	notify := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	report := func(err error) {
		select {
		case errCh <- err:
		default:
		}
	}
	var pendingMu sync.Mutex
	pending := make(map[string][]localWatchEvent)
	full := make(map[string]bool)
	var sequence uint64
	addEvent := func(root string, ev localWatchEvent) {
		pendingMu.Lock()
		defer pendingMu.Unlock()
		sequence++
		ev.seq = sequence
		if root == "" {
			for _, r := range rootList {
				full[r], pending[r] = true, nil
			}
			return
		}
		if full[root] {
			return
		}
		for i := range pending[root] {
			if pending[root][i].path == ev.path {
				pending[root][i].op |= ev.op
				pending[root][i].directory = pending[root][i].directory || ev.directory
				pending[root][i].seq = ev.seq
				return
			}
		}
		if len(pending[root]) >= maxLocalWatchEvents {
			full[root], pending[root] = true, nil
			return
		}
		pending[root] = append(pending[root], ev)
	}
	rootsExist := func() error {
		for _, root := range rootList {
			if _, err := os.Stat(root); err != nil {
				if localWatchNotExist(err) {
					return fmt.Errorf("local watcher root vanished: %s", root)
				}
				return err
			}
		}
		return nil
	}
	var injectedErrors <-chan error
	_, errorHook, _ := getLocalWatchHooks()
	if errorHook != nil {
		injectedErrors = errorHook()
	}
	handleWatcherError := func(watchErr error, ok bool) bool {
		if !ok {
			report(errors.New("local filesystem watcher error channel closed"))
			return false
		}
		if watchErr == nil {
			report(errors.New("local filesystem watcher error"))
			return false
		}
		if !localWatchNotExist(watchErr) {
			report(watchErr)
			return false
		}
		if rootErr := rootsExist(); rootErr != nil {
			report(rootErr)
			return false
		}
		if rebuildErr := rebuildWatcher(); rebuildErr != nil {
			if !localWatchNotExist(rebuildErr) {
				report(rebuildErr)
				return false
			}
			if rootErr := rootsExist(); rootErr != nil {
				report(rootErr)
				return false
			}
		}
		addEvent("", localWatchEvent{})
		notify()
		return true
	}
	take := func(root string) localWatchBatch {
		root = filepath.Clean(root)
		pendingMu.Lock()
		defer pendingMu.Unlock()
		b := localWatchBatch{events: pending[root], full: full[root], fence: sequence}
		delete(pending, root)
		delete(full, root)
		return b
	}
	fence := func() uint64 {
		pendingMu.Lock()
		defer pendingMu.Unlock()
		return sequence
	}
	suppress := func(root string, upto uint64, keep func(localWatchEvent) bool) {
		root = filepath.Clean(root)
		pendingMu.Lock()
		var candidates []localWatchEvent
		for _, ev := range pending[root] {
			if ev.seq <= upto {
				candidates = append(candidates, ev)
			}
		}
		pendingMu.Unlock()
		if len(candidates) == 0 {
			return
		}
		drop := make(map[string]uint64)
		for _, ev := range candidates {
			if !keep(ev) {
				drop[ev.path] = ev.seq
			}
		}
		if len(drop) == 0 {
			return
		}
		pendingMu.Lock()
		evs := pending[root]
		kept := evs[:0]
		for _, ev := range evs {
			if dropSeq, ok := drop[ev.path]; ok && ev.seq == dropSeq {
				continue
			}
			kept = append(kept, ev)
		}
		pending[root] = kept
		pendingMu.Unlock()
	}
	filterEvent := func(root, name string, op fsnotify.Op) bool {
		rel, err := filepath.Rel(root, name)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return false
		}
		rel = filepath.ToSlash(rel)
		base := filepath.Base(rel)
		if strings.HasPrefix(base, ".bdrive-tmp-") || config.ReservedPath(rel) {
			return op&fsnotify.Rename != 0
		}
		return true
	}
	isIgnoredOrNestedAncestor := func(root, name string, directory bool) bool {
		rel, err := filepath.Rel(root, name)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return false
		}
		verdicts := dirVerdicts[root]
		if verdicts == nil {
			verdicts = make(map[string]bool)
			dirVerdicts[root] = verdicts
		}
		parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
		prefix := ""
		for i, part := range parts {
			if prefix == "" {
				prefix = part
			} else {
				prefix = filepath.Join(prefix, part)
			}
			abs := filepath.Join(root, prefix)
			blocked, ok := verdicts[abs]
			if !ok {
				blocked = config.ReservedDir(part) || (ownerFilters[root] != nil && ownerFilters[root].PruneDir(filepath.ToSlash(prefix))) ||
					(!rootSet[abs] && config.IsMount(abs))
				if len(verdicts) >= 16384 {
					clear(verdicts)
				}
				verdicts[abs] = blocked
			}
			if blocked {
				return true
			}
			if i == len(parts)-1 && !directory {
				break
			}
		}
		return false
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer closeWatcher()
		defer close(wake)
		defer close(errCh)
		for {
			watcherMu.Lock()
			currentWatcher := watcher
			watcherMu.Unlock()
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-currentWatcher.Events():
				if !ok {
					report(errors.New("local filesystem watcher closed"))
					return
				}
				root := rootFor(ev.Name)
				name := filepath.Clean(ev.Name)
				directory := false
				if watchedRoot, ok := seen[name]; ok {
					directory = true
					if root == "" {
						root = watchedRoot
					}
				} else if info, statErr := os.Stat(name); statErr == nil {
					directory = info.IsDir()
				}
				if ev.Op&opRescanRoot != 0 {
					if ev.Name != "" && ev.Name != root && isIgnoredOrNestedAncestor(root, name, directory) {
						continue
					}
					addEvent("", localWatchEvent{})
					notify()
					continue
				}
				if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
					forgetDir(name, directory)
				}
				if currentWatcher.Recursive() {
					if name == root && ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
						if rootErr := rootsExist(); rootErr != nil {
							report(rootErr)
							return
						}
					}
					if root != "" && filepath.Base(name) == syncer.IgnoreFile {
						ownerFilters[root] = loadOwnerFilter(root)
						delete(dirVerdicts, root)
						addEvent("", localWatchEvent{})
						notify()
						continue
					}
					if root != "" {
						rel, _ := filepath.Rel(root, name)
						slashRel := filepath.ToSlash(rel)
						if strings.Count(slashRel, "/") >= 2 && strings.HasSuffix(slashRel, "/.bdrive/config.json") {
							delete(dirVerdicts, root)
							addEvent("", localWatchEvent{})
							notify()
							continue
						}
					}
					if root != "" && isIgnoredOrNestedAncestor(root, name, directory) {
						continue
					}
				} else {
					if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 && root != "" && name == root {
						if rootErr := rootsExist(); rootErr != nil {
							report(rootErr)
							return
						}
					}
					if root != "" && filepath.Base(name) == syncer.IgnoreFile {
						ownerFilters[root] = loadOwnerFilter(root)
						delete(dirVerdicts, root)
						if addErr := addDirTo(currentWatcher, seen, root, root); addErr != nil {
							if !handleWatcherError(addErr, true) {
								return
							}
							continue
						}
					}
					if root != "" {
						rel, _ := filepath.Rel(root, name)
						slashRel := filepath.ToSlash(rel)
						if strings.Count(slashRel, "/") >= 2 && strings.HasSuffix(slashRel, "/.bdrive/config.json") {
							nestedRoot := filepath.Dir(filepath.Dir(name))
							forgetDir(nestedRoot, true)
							delete(dirVerdicts, root)
							if addErr := addDirTo(currentWatcher, seen, root, root); addErr != nil {
								if !handleWatcherError(addErr, true) {
									return
								}
								continue
							}
							addEvent("", localWatchEvent{})
							notify()
							continue
						}
					}
					if ev.Op&fsnotify.Create != 0 && directory && root != "" {
						if addErr := addDirTo(currentWatcher, seen, name, root); addErr != nil {
							if isStructuralWatchErr(addErr) && runtime.GOOS == "linux" {
								stickyDisabled["fanotify"] = true
								addErr = fmt.Errorf("%w: %v", fs.ErrNotExist, addErr)
							}
							if !handleWatcherError(addErr, true) {
								return
							}
							continue
						}
					}
				}
				if root == "" || !filterEvent(root, name, ev.Op) {
					continue
				}
				if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename|fsnotify.Chmod) == 0 {
					continue
				}
				addEvent(root, localWatchEvent{root: root, path: name, op: ev.Op, directory: directory})
				notify()
			case watchErr, ok := <-currentWatcher.Errors():
				if !handleWatcherError(watchErr, ok) {
					return
				}
			case watchErr, ok := <-injectedErrors:
				if !handleWatcherError(watchErr, ok) {
					return
				}
			}
		}
	}()
	return &localFolderWatcher{
		done: done, wake: wake, err: errCh, close: closeWatcher,
		take: take, fence: fence, suppress: suppress,
	}, nil
}

func scanHintForBatch(batch localWatchBatch, root string) *syncer.ScanHint {
	hint := &syncer.ScanHint{Full: batch.full}
	if batch.full {
		return hint
	}
	for _, ev := range batch.events {
		rel, err := filepath.Rel(root, ev.path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			hint.Full = true
			hint.Events = nil
			return hint
		}
		kind := syncer.ScanWrite
		switch {
		case ev.op&fsnotify.Remove != 0:
			kind = syncer.ScanRemove
		case ev.op&fsnotify.Rename != 0:
			kind = syncer.ScanRename
		case ev.op&fsnotify.Create != 0:
			kind = syncer.ScanCreate
		case ev.op&fsnotify.Chmod != 0:
			kind = syncer.ScanChmod
		}
		hint.Events = append(hint.Events, syncer.ScanEvent{
			Path: filepath.ToSlash(rel), Kind: kind, Directory: ev.directory,
		})
	}
	return hint
}

func suppressMaterializationEchoes(lw *localFolderWatcher, root, folder, mountID string, st *store.Store, upto uint64) {
	if lw == nil || lw.suppress == nil || upto == 0 {
		return
	}
	var cache map[string]store.CachedFile
	var loadErr error
	var loadOnce sync.Once
	getCache := func() map[string]store.CachedFile {
		loadOnce.Do(func() {
			cache, loadErr = st.LoadCacheReadOnly(mountID)
		})
		return cache
	}
	hasCachedDescendant := func(rel string) bool {
		prefix := rel + "/"
		for cachedRel := range cache {
			if strings.HasPrefix(cachedRel, prefix) {
				return true
			}
		}
		return false
	}
	sameCachedFile := func(rel, abs string) bool {
		cached, ok := cache[rel]
		if !ok {
			return false
		}
		info, err := os.Lstat(abs)
		if err != nil {
			return false
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(abs)
			return err == nil && cached.Link == target &&
				cached.Size == info.Size() && cached.MTimeNS == info.ModTime().UnixNano()
		}
		return cached.Link == "" && info.Mode().IsRegular() &&
			info.Size() == cached.Size && info.ModTime().UnixNano() == cached.MTimeNS
	}
	knownDirectory := func(rel, abs string) bool {
		prefix := rel + "/"
		known := false
		found := make(map[string]bool)
		err := filepath.WalkDir(abs, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if path == abs {
				return nil
			}
			childRel, err := filepath.Rel(folder, path)
			if err != nil {
				return err
			}
			childRel = filepath.ToSlash(childRel)
			if d.IsDir() {
				return nil
			}
			if !strings.HasPrefix(childRel, prefix) || !sameCachedFile(childRel, path) {
				return os.ErrInvalid
			}
			known = true
			found[childRel] = true
			return nil
		})
		if err != nil || !known {
			return false
		}
		for cachedRel := range cache {
			if strings.HasPrefix(cachedRel, prefix) && !found[cachedRel] {
				return false
			}
		}
		return true
	}
	lw.suppress(root, upto, func(ev localWatchEvent) bool {
		cache = getCache()
		if loadErr != nil {
			return true
		}
		rel, err := filepath.Rel(folder, ev.path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true // keep: the path cannot be proven to be an echo
		}
		rel = filepath.ToSlash(rel)
		if rel == syncer.IgnoreFile {
			// Rule changes alter what discovery is allowed to see; retain the
			// event so the next cycle takes the full-policy path.
			return true
		}
		abs := filepath.Join(folder, filepath.FromSlash(rel))
		if ev.op&(fsnotify.Remove|fsnotify.Rename) != 0 {
			_, cached := cache[rel]
			_, statErr := os.Lstat(abs)
			// A missing, untracked path is the only remove/rename that can be
			// known to be a materialization echo. A tracked path must remain
			// queued so scan can journal the deletion.
			safe := !cached && !hasCachedDescendant(rel) && os.IsNotExist(statErr)
			return !safe
		}
		if ev.directory {
			// Directory creates from materialization are safe only when every
			// descendant is a tracked file with its cached fingerprint.
			return !knownDirectory(rel, abs)
		}
		// Unknown paths, missing cache entries, and any stat/read mismatch stay
		// queued for the next scan.
		return !sameCachedFile(rel, abs)
	})
}

func cycleSafely(ctx context.Context, sess *syncer.Session, volDir string, logger *log.Logger, hint *syncer.ScanHint) (res *syncer.Result, err error, panicked bool) {
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
	if hint == nil {
		res, err = sess.Cycle(ctx)
	} else {
		res, err = sess.CycleWithScanHint(ctx, hint)
	}
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
	var lastCycle time.Time
	var lastToken string
	lastAccess := store.AccessOK
	var watch <-chan struct{}
	var watchCancel context.CancelFunc
	var nextWatch, watchOpened time.Time
	var localWatch *localFolderWatcher
	var localWatchWake <-chan struct{}
	var localWatchErr <-chan error
	var localWatchRoots string
	var localWatchRetryAt time.Time
	var lastWatcherRulesFP string
	var watchENOSPCFailures int
	var lastWatchErrText string
	localWatchHealthy := false
	nextHints := make(map[string]*syncer.ScanHint)
	retriedOnce := make(map[string]bool)
	forceFull := true
	lastFullScan := time.Time{}
	var lastScanDuration time.Duration
	stopLocalWatch := func() {
		if localWatch != nil {
			localWatch.close()
			<-localWatch.done
			localWatch = nil
		}
		localWatchWake, localWatchErr = nil, nil
		localWatchHealthy = false
		forceFull = true
	}
	collectLocalHints := func() bool {
		if localWatch == nil || localWatch.take == nil {
			return false
		}
		haveLocalWork := false
		for _, target := range activeMountsForVolume(volDir, expectedRemote) {
			// fsnotify can deliver materialization events after Cycle returns,
			// especially while adding a newly created directory. Fence and
			// suppress while they are still pending, immediately before take.
			// A changed fingerprint and every event after this fence survive.
			if !forceFull && localWatch.fence != nil {
				suppressMaterializationEchoes(localWatch, target.folder, target.folder, target.proj.ID, st, localWatch.fence())
			}
			batch := localWatch.take(target.folder)
			hint := scanHintForBatch(batch, target.folder)
			if prior := nextHints[target.folder]; prior == nil || hint.Full {
				nextHints[target.folder] = hint
			} else if !prior.Full {
				prior.Events = append(prior.Events, hint.Events...)
			}
			if batch.full {
				forceFull = true
			}
			haveLocalWork = haveLocalWork || batch.full || len(batch.events) > 0
		}
		return haveLocalWork
	}
	stopWatch := func() {
		if watchCancel != nil {
			watchCancel()
			watchCancel = nil
		}
		watch = nil
	}
	defer stopWatch()
	defer stopLocalWatch()
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
		currentWatcherRulesFP := watcherRulesFingerprint(targets)
		roots := make([]string, 0, len(targets))
		for _, target := range targets {
			roots = append(roots, target.folder)
		}
		sort.Strings(roots)
		rootKey := strings.Join(roots, "\x00")
		if rootKey != localWatchRoots {
			stopLocalWatch()
			localWatchRoots, localWatchRetryAt = rootKey, time.Time{}
			watchENOSPCFailures = 0
			lastWatchErrText = ""
			lastWatcherRulesFP = ""
		}
		if localWatchHealthy && currentWatcherRulesFP != lastWatcherRulesFP {
			stopLocalWatch()
			localWatchRetryAt = time.Time{}
			forceFull = true
		}
		if localWatch == nil && time.Now().After(localWatchRetryAt) {
			lw, watchErr := startLocalFolderWatcher(ctx, roots)
			if watchErr != nil {
				if errors.Is(watchErr, syscall.ENOSPC) {
					watchENOSPCFailures++
					localWatchRetryAt = time.Now().Add(localWatchENOSPCRetry(watchENOSPCFailures))
					if watchErr.Error() != lastWatchErrText {
						logger.Printf("local filesystem watcher unavailable; polling: %v", watchErr)
						lastWatchErrText = watchErr.Error()
					}
				} else {
					localWatchRetryAt = time.Now().Add(localWatchRetry)
					logger.Printf("local filesystem watcher unavailable; polling: %v", watchErr)
				}
			} else {
				localWatch, localWatchWake, localWatchErr = lw, lw.wake, lw.err
				localWatchHealthy = true
				lastWatcherRulesFP = currentWatcherRulesFP
				watchENOSPCFailures = 0
				lastWatchErrText = ""
				forceFull = true
			}
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

		// Keep local safety scans independent from remote polling. A healthy
		// filesystem watcher makes the local scan a slow backstop; failures
		// fall back to the existing scanInterval cadence.
		//
		// The remote timer still gates hub work. With the change stream open,
		// a peer's write arrives by push and clears that gate below, so the
		// timer only catches a stream that died without telling us. Without a
		// stream, it stays at remoteInterval or peers would go unnoticed.
		//
		// Note this reads `watch`, which a failed dial, a token change, an
		// offline cycle and the hub's own streamMaxAge all set back to nil —
		// so losing the stream restores the tight remote cadence immediately.
		fallbackScanEvery := localWatchFallbackInterval(scanInterval, 0)
		if !localWatchHealthy {
			fallbackScanEvery = localWatchFallbackInterval(scanInterval, lastScanDuration)
		}
		pollEvery := scanInterval
		if localWatchHealthy {
			pollEvery = localWatchSafetyInterval
		} else {
			pollEvery = fallbackScanEvery
		}
		// Remote polling remains independent of the local watcher.
		remotePollEvery := remoteInterval
		if watch != nil {
			remotePollEvery = remoteInterval * watchedPollFactor
		}
		if remotePollEvery < pollEvery {
			pollEvery = remotePollEvery
		}
		waitFor := pollEvery
		remoteFailed := false
		now := localWatchNow()
		fallbackDue := !localWatchHealthy &&
			(lastFullScan.IsZero() || now.Sub(lastFullScan) >= fallbackScanEvery || forceFull)
		doRemote := proj.Remote != "" && time.Since(lastRemote) >= remotePollEvery
		if fallbackDue {
			forceFull = true
		} else if localWatchHealthy &&
			(lastFullScan.IsZero() || now.Sub(lastFullScan) >= localWatchSafetyInterval) {
			forceFull = true
		}
		if doRemote && be == nil {
			b, err := remote.Open(ctx, proj.Remote)
			if err != nil {
				logger.Printf("remote unavailable: %v", err)
				doRemote = false
				lastRemote = time.Now()
				remoteFailed = true
				waitFor = min(waitFor, scanInterval)
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
			res.ScanDuration += r.ScanDuration
			res.ScanFull = res.ScanFull || r.ScanFull
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

		// A full discovery consumes the current fence too: those events are
		// represented by the walk, and must not be replayed as a second local
		// batch after materialization.
		if forceFull && localWatch != nil && localWatch.take != nil {
			for _, target := range targets {
				_ = localWatch.take(target.folder)
			}
		}

		// A bounded cycle still has to drain events that raced the remote
		// deadline. This keeps scan-before-pull true for timer cycles and the
		// local-ops follow-up without turning the healthy watcher path into a
		// full-tree scan.
		if !forceFull {
			collectLocalHints()
		}

		// Run the primary target first, then each sibling. A sibling gets the
		// backend on a remote tick, or when an earlier sibling committed local
		// ops that must be published before this target catches up.
		maxLocalTarget := -1
		retryHints := make(map[string]*syncer.ScanHint)
		newRetriedOnce := make(map[string]bool)
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
			hint := nextHints[target.folder]
			if forceFull {
				hint = &syncer.ScanHint{Full: true}
			} else if hint == nil {
				hint = &syncer.ScanHint{}
			}
			r, rerr, panicked := cycleSafely(ctx, sess, volDir, logger, hint)
			if panicked {
				cyclePanicked = true
				break
			}
			if hint.Full || (r != nil && r.ScanFull) {
				lastFullScan = localWatchNow()
			}
			if rerr != nil && cycleErr == nil {
				cycleErr = rerr
			}
			mergeResult(r)
			if r != nil && len(r.RetryPaths) > 0 {
				var retryHint *syncer.ScanHint
				for _, p := range r.RetryPaths {
					key := target.folder + "\x00" + p
					newRetriedOnce[key] = true
					if retriedOnce[key] {
						continue
					}
					if retryHint == nil {
						retryHint = &syncer.ScanHint{}
					}
					info, statErr := os.Stat(filepath.Join(target.folder, filepath.FromSlash(p)))
					retryHint.Events = append(retryHint.Events, syncer.ScanEvent{
						Path: p, Kind: syncer.ScanWrite,
						Directory: statErr == nil && info.IsDir(),
					})
				}
				if retryHint != nil {
					retryHints[target.folder] = retryHint
				}
			}
			if r != nil && r.LocalOps > 0 {
				maxLocalTarget = i
			}
		}
		retriedOnce = newRetriedOnce

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
				r, rerr, panicked := cycleSafely(ctx, sess, volDir, logger, &syncer.ScanHint{})
				if panicked {
					cyclePanicked = true
					break
				}
				if r != nil && r.ScanFull {
					lastFullScan = localWatchNow()
				}
				if rerr != nil && cycleErr == nil {
					cycleErr = rerr
				}
				mergeResult(r)
			}
		}
		if !cyclePanicked && cycleErr == nil && res.ScanFull && res.ScanDuration > 0 {
			lastScanDuration = res.ScanDuration
		}
		if localWatchHealthy {
			latestTargets := activeMountsForVolume(volDir, expectedRemote)
			if watcherRulesFingerprint(latestTargets) != lastWatcherRulesFP {
				stopLocalWatch()
				localWatchRetryAt = time.Time{}
				forceFull = true
			}
		}
		if cyclePanicked {
			// A panic may have consumed a targeted hint without completing its
			// scan. Reset discovery state so the retry cannot replay stale
			// partial work while missing unhinted files.
			forceFull = true
			clear(nextHints)
			// Even a busy watch or repeated SIGHUP must not turn a panic into
			// a hot retry loop. Other volumes keep running during this backoff.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(scanInterval):
			}
			continue
		}
		if cycleErr != nil {
			forceFull = true
			if !localWatchHealthy && !lastFullScan.IsZero() &&
				localWatchNow().Sub(lastFullScan) < fallbackScanEvery {
				forceFull = false
			}
		} else {
			forceFull = false
		}
		nextHints = retryHints
		if len(retryHints) > 0 {
			waitFor = min(waitFor, scanInterval)
		}
		lastCycle = time.Now()

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
			_ = store.MarkPulled(volDir)
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
				_ = store.MarkPulled(volDir)
			}
			if res.LocalOps > 0 && !doRemote && !remoteFailed && proj.Remote != "" {
				lastRemote = time.Time{} // push local edits on the next cycle
				continue
			}
			if res.Backfilling {
				lastRemote = time.Time{} // fetch the next batch on the next tick
				waitFor = min(waitFor, scanInterval)
			}
		}

		if localWatchHealthy && !lastFullScan.IsZero() {
			remaining := lastFullScan.Add(localWatchSafetyInterval).Sub(localWatchNow())
			if remaining < 0 {
				remaining = 0
			}
			if remaining < waitFor {
				waitFor = remaining
			}
		}
		if !localWatchHealthy && !lastFullScan.IsZero() {
			remaining := maxDuration(0, lastFullScan.Add(fallbackScanEvery).Sub(localWatchNow()))
			if remaining < waitFor {
				waitFor = remaining
			}
		}

		waitDeadline := time.Now().Add(waitFor)
	waitForEvent:
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
			// A peer pushed. Clear the remote deadline; the common cycle path
			// drains local events before starting the bounded scan.
			if hook := workerRemoteWakeHook; hook != nil {
				hook(volDir)
			}
			lastRemote = time.Time{}
		case <-wakeCh:
			lastRemote = time.Time{}
			forceFull = true
			nextHints = make(map[string]*syncer.ScanHint)
		case watchErr, ok := <-localWatchErr:
			if !ok {
				watchErr = errors.New("local filesystem watcher channel closed")
			}
			logger.Printf("local filesystem watcher stopped; polling: %v", watchErr)
			stopLocalWatch()
			localWatchRetryAt = time.Now().Add(localWatchRetry)
			forceFull = true
			nextHints = make(map[string]*syncer.ScanHint)
			continue
		case _, ok := <-localWatchWake:
			if !ok {
				stopLocalWatch()
				localWatchRetryAt = time.Now().Add(localWatchRetry)
				continue
			}
			// Leave events pending throughout the debounce window. Draining on
			// each wake would both bypass echo suppression and overwrite hints
			// collected for earlier user edits in the same window.
			remoteWake := false
			wait := time.Until(lastCycle.Add(scanInterval))
			if localWatchHealthy && !lastFullScan.IsZero() {
				remaining := lastFullScan.Add(localWatchSafetyInterval).Sub(localWatchNow())
				if remaining < wait {
					wait = remaining
				}
			}
			if wait > 0 {
				timer := time.NewTimer(wait)
			localWait:
				for {
					select {
					case <-ctx.Done():
						timer.Stop()
						return nil
					case _, open := <-watch:
						remoteWake = true
						if !open {
							lived := time.Since(watchOpened)
							stopWatch()
							if lived < watchRetry {
								nextWatch = time.Now().Add(watchRetry)
							}
						} else {
							if hook := workerRemoteWakeHook; hook != nil {
								hook(volDir)
							}
							lastRemote = time.Time{}
						}
						timer.Stop()
						break localWait
					case <-wakeCh:
						lastRemote = time.Time{}
						forceFull = true
						nextHints = make(map[string]*syncer.ScanHint)
						timer.Stop()
						break localWait
					case watchErr, ok := <-localWatchErr:
						if !ok {
							watchErr = errors.New("local filesystem watcher channel closed")
						}
						logger.Printf("local filesystem watcher stopped; polling: %v", watchErr)
						stopLocalWatch()
						localWatchRetryAt = time.Now().Add(localWatchRetry)
						forceFull = true
						nextHints = make(map[string]*syncer.ScanHint)
						timer.Stop()
						break localWait
					case _, ok := <-localWatchWake:
						if !ok {
							stopLocalWatch()
							localWatchRetryAt = time.Now().Add(localWatchRetry)
							timer.Stop()
							break localWait
						}
					case <-timer.C:
						break localWait
					}
				}
			}
			if localWatchHealthy && !lastFullScan.IsZero() &&
				localWatchNow().Sub(lastFullScan) >= localWatchSafetyInterval {
				forceFull = true
			}
			if !collectLocalHints() && !forceFull && !remoteWake {
				// A wake is only a notification, not proof of local work. Keep
				// the original remote deadline when it contained only echoes.
				goto waitForEvent
			}
			continue
		case <-time.After(time.Until(waitDeadline)):
		}
	}
}
