// LEGACY COMPATIBILITY SUNSET: 2026-12-15 (two months after 2026-10-15).
// This file migrates v0.17.x standalone per-volume daemons to the global
// supervisor. Do not extend its lifetime without an explicit migration decision.
// Removal checklist, in order:
//  1. Remove the legacy `daemon run <folder>` CLI form and its validation call
//     from Run; retain the argument-free global supervisor command.
//  2. Remove reapLegacyDaemonIfNeeded calls from Start and worker discovery,
//     and remove the reapAllLegacyDaemons call from supervisor startup.
//  3. Replace Stop's unannounced-PID fallback with a fail-closed error; never
//     signal a PID obtained only from daemon.pid.
//  4. Delete this file and legacy_compat_test.go, remove TestHelperLegacyDaemon
//     and TestStopRecoversLegacyDaemon, and remove the sunset override from docs.
//  5. Run the daemon security, supervisor, CLI/desktop, and upgrade test suites.
package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
)

const LegacyCompatSunsetDate = "2026-12-15"

// ValidateLegacyRunFolder preserves the old command's project validation while
// the positional-folder spelling remains supported during migration.
func ValidateLegacyRunFolder(folder string) error {
	_, ok, err := config.ResolveMount(folder)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s is not a beardrive project (run `bdrive init`)", folder)
	}
	return nil
}

// stopLegacyUnannouncedDaemon is the only pidfile fallback. Command-line proof
// is required, and it never escalates to SIGKILL on a pre-invariant PID.
func stopLegacyUnannouncedDaemon(volDir string) (bool, error) {
	data, readErr := os.ReadFile(PidPath(volDir))
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if readErr != nil || parseErr != nil || pid <= 0 ||
		pid == os.Getpid() || !strings.Contains(cmdline(pid), "daemon run") {
		return false, fmt.Errorf("a daemon holds %s but it announced no pid; kill it by hand", LockPath(volDir))
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
		time.Sleep(20 * time.Millisecond)
	}
	return false, fmt.Errorf("daemon (pid %d) did not exit after SIGTERM; kill it by hand", pid)
}

// cmdline fails closed when the process's command line cannot be established.
func cmdline(pid int) string {
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		return strings.ReplaceAll(string(data), "\x00", " ")
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func reapLegacyDaemonIfNeeded(volDir string) error {
	if volDir == "" {
		return nil
	}
	pid, running := Running(volDir)
	globalPID, global := GlobalRunning()
	if !running || pid == os.Getpid() || (global && pid > 0 && pid == globalPID) {
		return nil
	}
	if _, err := Stop(volDir); err != nil {
		return fmt.Errorf("stop legacy daemon for %s: %w", volDir, err)
	}
	return nil
}

func reapAllLegacyDaemons() error {
	home, err := config.Home()
	if err != nil {
		return err
	}
	root := filepath.Join(home, "volumes")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := reapLegacyDaemonIfNeeded(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
