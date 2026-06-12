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

	"github.com/runbear-io/sfs/internal/config"
	"github.com/runbear-io/sfs/internal/remote"
	"github.com/runbear-io/sfs/internal/store"
	"github.com/runbear-io/sfs/internal/syncer"
)

// Daemons are per mount, not per volume: one volume may be mounted at
// several folders (each gets its own daemon), so pid/log files are keyed by
// the mount ID.
func PidPath(volDir, mountID string) string {
	return filepath.Join(volDir, "daemon-"+mountID+".pid")
}
func LogPath(volDir, mountID string) string {
	return filepath.Join(volDir, "daemon-"+mountID+".log")
}

// Running reports the daemon pid for a mount if one is alive.
func Running(volDir, mountID string) (int, bool) {
	data, err := os.ReadFile(PidPath(volDir, mountID))
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
	mountID := config.MountID(folder)
	if pid, ok := Running(volDir, mountID); ok {
		return pid, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	logf, err := os.OpenFile(LogPath(volDir, mountID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
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
	if err := os.WriteFile(PidPath(volDir, mountID), []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		return pid, err
	}
	return pid, cmd.Process.Release()
}

// Stop terminates the daemon for a mount and waits for it to exit.
func Stop(volDir, mountID string) (bool, error) {
	pid, ok := Running(volDir, mountID)
	if !ok {
		os.Remove(PidPath(volDir, mountID))
		return false, nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return false, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			os.Remove(PidPath(volDir, mountID))
			return true, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	os.Remove(PidPath(volDir, mountID))
	return true, nil
}

// Run is the daemon main loop, executed in the foreground of the (usually
// detached) `sfs daemon run` process.
func Run(folder string, scanInterval, remoteInterval time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	mounts, err := config.LoadMounts()
	if err != nil {
		return err
	}
	mi, ok := mounts[folder]
	if !ok {
		return fmt.Errorf("%s is not an sfs mount", folder)
	}
	volDir, err := config.VolumeDir(mi.Volume)
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
	mountID := config.MountID(folder)
	if err := os.WriteFile(PidPath(volDir, mountID), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		return err
	}
	defer os.Remove(PidPath(volDir, mountID))

	log.Printf("daemon started: folder=%s volume=%s remote=%q device=%s(%s) scan=%s sync=%s",
		folder, mi.Volume, mi.Remote, dev.Name, dev.ID, scanInterval, remoteInterval)

	var be remote.Backend
	defer func() {
		if be != nil {
			be.Close()
		}
	}()
	var lastRemote time.Time

	for {
		// Pick up `sfs remote set` / `sfs umnt --forget` without restarting.
		if m, err := config.LoadMounts(); err == nil {
			cur, ok := m[folder]
			if !ok {
				log.Printf("mount unregistered; exiting")
				return nil
			}
			if cur.Remote != mi.Remote {
				log.Printf("remote changed: %q -> %q", mi.Remote, cur.Remote)
				if be != nil {
					be.Close()
					be = nil
				}
				lastRemote = time.Time{}
			}
			mi = cur
		}

		doRemote := mi.Remote != "" && time.Since(lastRemote) >= remoteInterval
		if doRemote && be == nil {
			b, err := remote.Open(ctx, mi.Remote)
			if err != nil {
				log.Printf("remote unavailable: %v", err)
				doRemote = false
				lastRemote = time.Now()
			} else {
				be = b
			}
		}

		sess := &syncer.Session{Folder: folder, Store: st, Device: dev}
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
