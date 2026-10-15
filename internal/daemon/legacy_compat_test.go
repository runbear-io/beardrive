package daemon

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func checkLegacyCompatSunset(now time.Time) error {
	sunset, err := time.Parse("2006-01-02", LegacyCompatSunsetDate)
	if err != nil {
		return fmt.Errorf("invalid legacy compatibility sunset date: %w", err)
	}
	if now.UTC().Before(sunset) {
		return nil
	}
	return fmt.Errorf("legacy daemon compatibility sunset expired on %s: follow the removal checklist in legacy_compat.go: remove the positional daemon run folder form and ValidateLegacyRunFolder call; remove legacy reaping calls from Start, Run, and worker discovery; replace Stop's unannounced-PID fallback with a fail-closed error; delete legacy_compat.go, legacy_compat_test.go, TestHelperLegacyDaemon, and TestStopRecoversLegacyDaemon; remove the BDRIVE_EXTEND_LEGACY_SUNSET documentation; run daemon security, supervisor, CLI/desktop, and upgrade tests. Set BDRIVE_EXTEND_LEGACY_SUNSET only for an explicitly approved extension", LegacyCompatSunsetDate)
}

func TestLegacyCompat_SunsetExpired(t *testing.T) {
	if os.Getenv("BDRIVE_EXTEND_LEGACY_SUNSET") != "" {
		t.Skip("legacy compatibility sunset explicitly extended")
	}
	if err := checkLegacyCompatSunset(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyCompat_SunsetGateBeforeAndAfter(t *testing.T) {
	for _, tc := range []struct {
		date      string
		wantError bool
	}{
		{"2026-10-15", false},
		{"2026-12-15", true},
		{"2026-12-16", true},
	} {
		t.Run(tc.date, func(t *testing.T) {
			now, err := time.Parse("2006-01-02", tc.date)
			if err != nil {
				t.Fatal(err)
			}
			err = checkLegacyCompatSunset(now)
			if (err != nil) != tc.wantError {
				t.Fatalf("sunset gate = %v, wantError=%v", err, tc.wantError)
			}
			if err != nil && (!strings.Contains(err.Error(), "legacy daemon compatibility sunset expired on 2026-12-15") || !strings.Contains(err.Error(), "removal checklist")) {
				t.Fatalf("unexpected sunset error: %v", err)
			}
		})
	}
}

func TestLegacyCompat_ReapsStandalonePerVolumeDaemonOnSupervisorStart(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	mount := supervisorMount(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperLegacyDaemon$", "daemon", "run")
	cmd.Env = append(os.Environ(), "BDRIVE_TEST_LEGACY_VDIR="+mount.volDir, "BDRIVE_TEST_LEGACY_ANNOUNCE=1")
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
		default:
			_ = cmd.Process.Kill()
			<-exited
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for {
		if pid, running := Running(mount.volDir); running && pid == cmd.Process.Pid {
			break
		}
		select {
		case <-exited:
			t.Fatalf("legacy helper exited before announcing: %v\n%s", waitErr, output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("legacy helper did not announce its lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	done := supervisorRun(t, mount)
	worktreeWaitFor(t, "global supervisor taking over the legacy volume", done, func() bool {
		pid, running := Running(mount.volDir)
		return running && pid == os.Getpid()
	})
	select {
	case <-exited:
		if waitErr != nil {
			t.Fatalf("legacy helper was not stopped cleanly: %v\n%s", waitErr, output.String())
		}
	case <-time.After(time.Second):
		t.Fatal("legacy helper did not exit after takeover")
	}
	if _, err := os.Stat(filepath.Join(mount.volDir, "legacy-stopped")); err != nil {
		t.Fatalf("legacy helper did not receive graceful SIGTERM: %v", err)
	}
	writeWorktreeFile(t, mount.folder, "after-upgrade.txt", "global worker\n")
	worktreeWaitFor(t, "sync after legacy takeover", done, func() bool {
		return worktreeRemoteHasOp(mount.remote, "after-upgrade.txt", "put")
	})
}
