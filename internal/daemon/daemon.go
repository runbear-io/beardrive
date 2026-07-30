// Package daemon runs the per-mount background sync loop and manages its
// lifecycle (detached start, pidfile, graceful stop).
//
// The loop scans the working folder every scan-interval (cheap: size+mtime
// against the state cache) and talks to the remote every remote-interval —
// or immediately after local changes, so edits propagate quickly without
// hammering the object store.
package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
)

// The volume store is keyed by the mount id, so exactly one daemon runs per
// mount and its pid/log live in the store dir.
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

// Running reports the daemon pid for a mount if one is alive. The pid is
// informational (for display and for Stop's signal); aliveness comes from
// LockPath — see the comment there.
func Running(volDir string) (int, bool) {
	if !locked(LockPath(volDir)) {
		return 0, false
	}
	data, err := os.ReadFile(PidPath(volDir))
	if err != nil {
		return 0, true // held by a daemon whose pidfile we can't read
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, true
	}
	return pid, true
}

// locked reports whether another process holds the lock file. Taking it
// non-blocking and immediately releasing is the probe: success means nobody
// held it (so: no daemon), failure means someone does.
func locked(path string) bool {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false // can't tell; treat as not running so Start can try
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// hold takes the daemon's lifetime lock. The returned closer releases it;
// process death releases it too, which is the point.
func hold(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another daemon is already running for this mount: %w", err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// Start launches a detached daemon for the folder (no-op if already running).
func Start(folder, volDir string, scanInterval, remoteInterval time.Duration) (int, error) {
	if pid, ok := Running(volDir); ok {
		return pid, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	logf, err := os.OpenFile(LogPath(volDir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "daemon", "run", folder,
		"--scan-interval", scanInterval.String(),
		"--remote-interval", remoteInterval.String())
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	if err := cmd.Process.Release(); err != nil {
		return 0, err
	}

	// The child announces its own pid, and only once it holds the lifetime
	// lock (see Run). The parent must NOT write PidPath: a child that loses
	// the lock race exits without ever being the daemon, and its pid written
	// here would outlive it — leaving Stop signalling a corpse (ESRCH) while
	// the daemon that won keeps syncing, and status printing a phantom pid.
	//
	// So wait for the lock instead of assuming the spawn worked. A caller
	// that gets a pid back can trust that a daemon owns it. In a race the pid
	// is the winner's rather than the child just spawned, which is the honest
	// answer to "which pid is the daemon" — callers that need to distinguish
	// starting from adopting check Running first (see `bdrive resume`).
	deadline := time.Now().Add(startTimeout)
	for {
		if pid, ok := Running(volDir); ok && pid > 0 {
			return pid, nil
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("daemon did not come up within %s; see %s",
				startTimeout, LogPath(volDir))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startTimeout bounds how long Start waits for the child to take the lock and
// write its pid. Generous on purpose: it covers a cold binary on a loaded
// machine, and the only cost of waiting is a slower `bdrive init`.
const startTimeout = 10 * time.Second

// Stop terminates the daemon for a mount and waits for it to exit. Exit is
// observed by the lock being released, not by the pid disappearing: the pid
// could be recycled while we wait, and the lock cannot.
func Stop(volDir string) (bool, error) {
	pid, ok := Running(volDir)
	if !ok {
		os.Remove(PidPath(volDir))
		return false, nil
	}
	if pid <= 0 {
		// Alive (lock held) but no readable pid — nothing to signal.
		return false, fmt.Errorf("a daemon holds %s but %s is unreadable; kill it by hand",
			LockPath(volDir), PidPath(volDir))
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return false, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !locked(LockPath(volDir)) {
			os.Remove(PidPath(volDir))
			return true, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	os.Remove(PidPath(volDir))
	return true, nil
}

// Run is the daemon main loop, executed in the foreground of the (usually
// detached) `bdrive daemon run` process.
func Run(folder string, scanInterval, remoteInterval time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	proj, ok, err := config.ResolveMount(folder)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s is not a beardrive project (run `bdrive init`)", folder)
	}
	volDir, err := config.VolumeDir(proj.ID)
	if err != nil {
		return err
	}
	st, err := store.Open(volDir)
	if err != nil {
		return err
	}
	dev, err := config.LoadDevice()
	if err != nil {
		return err
	}
	// Hold the lifetime lock before announcing the pid: it is what makes
	// "is a daemon running" answerable, and it also makes a double start
	// impossible (two daemons on one mount would write one journal twice).
	release, err := hold(LockPath(volDir))
	if err != nil {
		return err
	}
	defer release()

	if err := os.WriteFile(PidPath(volDir), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		return err
	}
	defer os.Remove(PidPath(volDir))

	log.Printf("daemon started: folder=%s mount=%s volume=%s remote=%q device=%s(%s) scan=%s sync=%s",
		folder, proj.ID, proj.Volume, proj.Remote, dev.Name, dev.ID, scanInterval, remoteInterval)

	var be remote.Backend
	defer func() {
		if be != nil {
			be.Close()
		}
	}()
	var lastRemote time.Time
	var lastToken string
	// Which access state we last logged, so a degraded daemon says it once
	// instead of on every tick.
	lastAccess := store.AccessOK

	for {
		// Re-read the project config each tick: picks up `bdrive remote set`
		// and hand-edits. A vanished config means the folder was moved,
		// renamed, or deleted — exit cleanly (propagating nothing); the next
		// bdrive command in the folder's new location resumes the daemon.
		cur, ok, err := config.LoadProject(folder)
		if err != nil || !ok {
			log.Printf("project config gone (folder moved or deleted); exiting")
			return nil
		}
		if cur.ID != proj.ID {
			log.Printf("mount identity changed; exiting")
			return nil
		}
		// If the registry says this mount now lives elsewhere, a new
		// location has taken over — stand down.
		if m, err := config.LoadMounts(); err == nil {
			if mi, ok := m[proj.ID]; ok && mi.Path != folder {
				log.Printf("mount re-registered at %s; exiting", mi.Path)
				return nil
			}
		}
		if cur.Remote != proj.Remote {
			log.Printf("remote changed: %q -> %q", proj.Remote, cur.Remote)
			if be != nil {
				be.Close()
				be = nil
			}
			lastRemote = time.Time{}
		}
		proj = cur

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
			lastToken = settings.Token
			lastRemote = time.Time{}
		}

		doRemote := proj.Remote != "" && time.Since(lastRemote) >= remoteInterval
		if doRemote && be == nil {
			b, err := remote.Open(ctx, proj.Remote)
			if err != nil {
				log.Printf("remote unavailable: %v", err)
				doRemote = false
				lastRemote = time.Now()
			} else {
				be = b
			}
		}

		sess := &syncer.Session{Folder: folder, MountID: proj.ID, Store: st, Device: dev, Account: settings}
		if doRemote {
			sess.Backend = be
		}
		res, err := sess.Cycle(ctx)
		switch {
		case ctx.Err() != nil:
			log.Printf("daemon stopping")
			return nil
		case err != nil:
			log.Printf("cycle error: %v", err)
		case res.NoAccess:
			// The connection is fine, the answer isn't: keep the backend and
			// keep ticking cheaply so a re-grant self-heals. Log the
			// transition only — a paused daemon must stay quiet.
			if lastAccess != store.AccessNone {
				log.Printf("access revoked for this project; sync paused (%v)", res.AccessErr)
				lastAccess = store.AccessNone
			}
			lastRemote = time.Now()
		case res.ReadOnly:
			if lastAccess != store.AccessReadOnly {
				log.Printf("read-only on this project, pulling only; local changes stay on this device")
				lastAccess = store.AccessReadOnly
			}
			lastRemote = time.Now()
		case res.Offline:
			log.Printf("offline, will retry: %v", res.OfflineErr)
			if be != nil {
				be.Close()
				be = nil
			}
			lastRemote = time.Now()
		default:
			if lastAccess != store.AccessOK {
				log.Printf("access restored; syncing normally")
				lastAccess = store.AccessOK
			}
			if res.Activity() {
				log.Printf("local+%d pulled+%d conflicts=%d files~%d pushed=%v",
					res.LocalOps, res.PulledOps, res.Conflicts, res.Materialized, res.Pushed)
			}
			if doRemote {
				lastRemote = time.Now()
			}
			if res.LocalOps > 0 && !doRemote {
				lastRemote = time.Time{} // push local edits on the next tick
			}
		}

		select {
		case <-ctx.Done():
			log.Printf("daemon stopping")
			return nil
		case <-time.After(scanInterval):
		}
	}
}
