package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
)

// TestHookLatencyBenchmark times the turn-start hook, as Claude Code runs it,
// against a large mount, once with no daemon and once beside a running one.
// It is a measurement, not a check, so it only runs on request:
//
//	BDRIVE_HOOK_BENCH_BINS="before=/path/bdrive-old,after=/path/bdrive-new" \
//	BDRIVE_HOOK_BENCH_FILES=78685 go test ./cmd/bdrive -run HookLatencyBenchmark -v -timeout 30m
//
// Each binary is copied under a .test name, which keeps it from starting a
// daemon of its own (daemon.isTestExecutable); "daemon up" is this test
// holding the volume's daemon lock.
func TestHookLatencyBenchmark(t *testing.T) {
	bins := os.Getenv("BDRIVE_HOOK_BENCH_BINS")
	if bins == "" {
		t.Skip("set BDRIVE_HOOK_BENCH_BINS to run")
	}
	files, _ := strconv.Atoi(os.Getenv("BDRIVE_HOOK_BENCH_FILES"))
	if files <= 0 {
		files = 78685
	}
	runs, _ := strconv.Atoi(os.Getenv("BDRIVE_HOOK_BENCH_RUNS"))
	if runs <= 0 {
		runs = 20
	}

	home := t.TempDir()
	t.Setenv("BDRIVE_HOME", home)
	folder, _ := filepath.EvalSymlinks(t.TempDir())
	proj, err := config.SaveProject(folder, config.Project{
		Volume: "team",
		Remote: "https://hub.example.com/p/p-12345678",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(folder); err != nil {
		t.Fatal(err)
	}
	// Shaped like the team volume: most files are small run outputs deep
	// under one project, the rest notes spread across areas.
	pods := files * 61 / 79
	for i := range files {
		var p string
		if i < pods {
			p = fmt.Sprintf("projects/searchd/pods/run-%04d/out-%03d.json", i/100, i%100)
		} else {
			p = fmt.Sprintf("areas/area-%03d/note-%03d.md", i/100, i%100)
		}
		full := filepath.Join(folder, p)
		if i%100 == 0 || i == pods {
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(full, []byte(fmt.Sprintf("{\"i\":%d}\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The first cycle hashes everything; every hook after it pays the
	// steady-state walk.
	sess, _, err := openSession(context.Background(), folder, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}

	measure := func(bin string) (p50, p95 time.Duration) {
		var took []time.Duration
		for range runs {
			c := exec.Command(bin, "sync", ".", "--hook", "claude-code")
			c.Dir = folder
			c.Stdin = strings.NewReader(`{"session_id":"bench"}`)
			start := time.Now()
			out, err := c.Output()
			took = append(took, time.Since(start))
			if err != nil || !strings.Contains(string(out), "hookSpecificOutput") {
				t.Fatalf("%s: hook failed (%v): %s", bin, err, out)
			}
		}
		slices.Sort(took)
		return took[len(took)/2], took[(len(took)*95+99)/100-1]
	}

	type row struct {
		name, state string
		p50, p95    time.Duration
	}
	var rows []row
	var named [][2]string
	for _, b := range strings.Split(bins, ",") {
		name, path, ok := strings.Cut(b, "=")
		if !ok {
			t.Fatalf("BDRIVE_HOOK_BENCH_BINS entry %q is not name=path", b)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		copyPath := filepath.Join(t.TempDir(), "bdrive-"+name+".test")
		if err := os.WriteFile(copyPath, data, 0o755); err != nil {
			t.Fatal(err)
		}
		named = append(named, [2]string{name, copyPath})
	}
	for _, n := range named {
		p50, p95 := measure(n[1])
		rows = append(rows, row{n[0], "no daemon", p50, p95})
	}
	holdDaemonLock(t, proj)
	markPulled(t, proj, 0)
	for _, n := range named {
		p50, p95 := measure(n[1])
		rows = append(rows, row{n[0], "daemon running", p50, p95})
	}

	var b strings.Builder
	fmt.Fprintf(&b, "hook latency, %d files, %d runs each\n", files, runs)
	fmt.Fprintf(&b, "| binary | state | p50 | p95 |\n|---|---|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", r.name, r.state,
			r.p50.Round(time.Millisecond), r.p95.Round(time.Millisecond))
	}
	t.Log("\n" + b.String())
}
