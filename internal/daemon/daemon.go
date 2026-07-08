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

// Running reports the daemon pid for a mount if one is alive.
func Running(volDir string) (int, bool) {
	data, err := os.ReadFile(PidPath(volDir))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return 0, false
	}
	return pid, true
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
	pid := cmd.Process.Pid
	if err := os.WriteFile(PidPath(volDir), []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		return pid, err
	}
	return pid, cmd.Process.Release()
}

// Stop terminates the daemon for a mount and waits for it to exit.
func Stop(volDir string) (bool, error) {
	pid, ok := Running(volDir)
	if !ok {
		os.Remove(PidPath(volDir))
		return false, nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return false, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
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
	if err := os.WriteFile(PidPath(volDir), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		return err
	}
	defer os.Remove(PidPath(volDir))

	settings, _ := config.LoadSettings()

	log.Printf("daemon started: folder=%s mount=%s volume=%s remote=%q device=%s(%s) scan=%s sync=%s",
		folder, proj.ID, proj.Volume, proj.Remote, dev.Name, dev.ID, scanInterval, remoteInterval)

	var be remote.Backend
	defer func() {
		if be != nil {
			be.Close()
		}
	}()
	var lastRemote time.Time

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
		case res.Offline:
			log.Printf("offline, will retry: %v", res.OfflineErr)
			if be != nil {
				be.Close()
				be = nil
			}
			lastRemote = time.Now()
		default:
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
