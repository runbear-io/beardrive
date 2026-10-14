package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/store"
)

// runStop runs `bdrive sync <folder> --hook-stop claude-code` with the given
// Stop event on stdin and returns what reached the real os.Stdout — the hook's
// contract is "one JSON object or nothing", so any stray Printf must show.
func runStop(t *testing.T, folder, event string) string {
	t.Helper()
	c := syncCmd()
	c.SetIn(strings.NewReader(event))
	out, err := seccliRun(t, c, []string{folder, "--hook-stop", "claude-code"})
	if err != nil {
		t.Fatalf("--hook-stop must never fail: %v", err)
	}
	return out
}

// stopDecision parses the receipt, failing unless stdout is exactly one block
// object.
func stopDecision(t *testing.T, out string) string {
	t.Helper()
	if strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Fatalf("receipt is not a single JSON object:\n%s", out)
	}
	var d struct {
		Decision, Reason string
	}
	if err := json.Unmarshal([]byte(out), &d); err != nil || d.Decision != "block" {
		t.Fatalf("receipt = %q (%v), want a block decision", out, err)
	}
	return d.Reason
}

// syncNoted is the async push hook: a plain `bdrive sync .`, note only.
func syncNoted(t *testing.T, folder, note string) {
	t.Helper()
	args := []string{folder}
	if note != "" {
		args = append(args, "--note", note)
	}
	if out, err := seccliRun(t, syncCmd(), args); err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
}

func offlineMount(t *testing.T) (string, config.Project) {
	t.Helper()
	t.Setenv("BDRIVE_HOME", t.TempDir())
	root, _ := filepath.EvalSymlinks(t.TempDir())
	proj := mountAt(t, root, "wiki", "https://hub.example.com/p/p-12345678") // unreachable
	return filepath.Join(root, "wiki"), proj
}

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func deviceOps(t *testing.T, proj config.Project) []journal.Op {
	t.Helper()
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := config.LoadDevice()
	if err != nil {
		t.Fatal(err)
	}
	ops, err := st.DeviceOps(dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

func TestHookStopActiveIsSilent(t *testing.T) {
	folder, proj := offlineMount(t)
	writeFile(t, folder, "b.md", "b")
	syncNoted(t, folder, "claude-code session s1")
	writeFile(t, folder, "c.md", "unscanned")
	before := len(deviceOps(t, proj))

	if out := runStop(t, folder, `{"session_id":"s1","stop_hook_active":true}`); out != "" {
		t.Fatalf("stop_hook_active must be silent, got:\n%s", out)
	}
	if after := len(deviceOps(t, proj)); after != before {
		t.Fatalf("stop_hook_active ran a cycle: %d ops -> %d", before, after)
	}
}

// The headline case: the async push hook committed b.md (note only, no
// Op.Session), the hub is unreachable, and the agent is about to say done.
func TestHookStopOfflineBlocks(t *testing.T) {
	folder, _ := offlineMount(t)
	writeFile(t, folder, "b.md", "b")
	syncNoted(t, folder, "claude-code session s1")

	reason := stopDecision(t, runStop(t, folder, `{"session_id":"s1"}`))
	if !strings.Contains(reason, "`b.md`") || !strings.Contains(reason, "offline") {
		t.Fatalf("reason = %q, want b.md named as offline", reason)
	}
}

func TestHookStopOtherSessionNotListed(t *testing.T) {
	folder, _ := offlineMount(t)
	writeFile(t, folder, "theirs.md", "another session")
	syncNoted(t, folder, "claude-code session s2")
	writeFile(t, folder, "human.md", "a hand edit")
	syncNoted(t, folder, "")

	if out := runStop(t, folder, `{"session_id":"s1"}`); out != "" {
		t.Fatalf("another session's / a human's ops were reported:\n%s", out)
	}
}

func TestHookStopPushedIsSilent(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	root, _ := filepath.EvalSymlinks(t.TempDir())
	proj := mountAt(t, root, "wiki", "file://"+t.TempDir()) // reachable
	folder := filepath.Join(root, "wiki")
	writeFile(t, folder, "b.md", "b")

	// The Stop cycle itself commits b.md (stamped) and pushes it.
	if out := runStop(t, folder, `{"session_id":"s1"}`); out != "" {
		t.Fatalf("everything reached the remote, yet:\n%s", out)
	}
	ops := deviceOps(t, proj)
	if len(ops) != 1 || ops[0].Session != "s1" {
		t.Fatalf("ops = %+v, want b.md stamped with the session", ops)
	}
}

func TestHookStopPausedMount(t *testing.T) {
	folder, proj := offlineMount(t)
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPaused(vdir, true); err != nil {
		t.Fatal(err)
	}
	mountsBefore, _ := os.ReadFile(filepath.Join(os.Getenv("BDRIVE_HOME"), "mounts.json"))
	writeFile(t, folder, "x.md", "never scanned")

	reason := stopDecision(t, runStop(t, folder, `{"session_id":"s1"}`))
	if !strings.Contains(reason, "`x.md`") || !strings.Contains(reason, "sync paused") {
		t.Fatalf("reason = %q, want x.md reported as paused", reason)
	}
	if !store.Paused(vdir) {
		t.Fatal("the receipt resumed a paused mount")
	}
	if ops := deviceOps(t, proj); len(ops) != 0 {
		t.Fatalf("the receipt journaled a paused mount: %+v", ops)
	}
	if after, _ := os.ReadFile(filepath.Join(os.Getenv("BDRIVE_HOME"), "mounts.json")); string(after) != string(mountsBefore) {
		t.Fatal("the receipt rewrote mounts.json")
	}
}

func TestHookStopMultipleMounts(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	root, _ := filepath.EvalSymlinks(t.TempDir())
	mountAt(t, root, "projA", "https://hub.example.com/p/p-aaaaaaaa")
	mountAt(t, root, "projB", "https://hub.example.com/p/p-bbbbbbbb")
	writeFile(t, root, "projA/a.md", "a")
	writeFile(t, root, "projB/b.md", "b")

	reason := stopDecision(t, runStop(t, root, `{"session_id":"s1"}`))
	t.Logf("receipt: %s", reason)
	for _, want := range []string{"`projA/a.md`", "`projB/b.md`"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason = %q, missing %s", reason, want)
		}
	}
}

// A push the hub refuses: the receipt carries the hub's own words.
func TestHookStopRefusedPush(t *testing.T) {
	hub, browser := sec8Hub(t)
	target, err := url.Parse(hub.URL)
	if err != nil {
		t.Fatal(err)
	}
	var refuse atomic.Bool
	proxy := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refuse.Load() && r.Method == http.MethodPut &&
			strings.HasSuffix(r.URL.Path, "/store/object") &&
			strings.HasPrefix(r.URL.Query().Get("key"), "journal/") {
			http.Error(w, "this device is not registered to your account on this hub", http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	s, err := config.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	s.Server = front.URL
	if err := config.SaveSettings(s); err != nil {
		t.Fatal(err)
	}
	var made struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	sec8JSON(t, browser, "POST", hub.URL+"/api/projects", map[string]string{"name": "refused"}, &made, "")
	folder := t.TempDir()
	if _, err := config.SaveProject(folder, config.Project{Volume: "refused", Remote: front.URL + "/p/" + made.Project.ID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(folder); err != nil {
		t.Fatal(err)
	}

	refuse.Store(true)
	writeFile(t, folder, "note.md", "a day of work")
	reason := stopDecision(t, runStop(t, folder, `{"session_id":"s1"}`))
	if !strings.Contains(reason, "`note.md`") || !strings.Contains(reason, "not registered to your account") {
		t.Fatalf("reason = %q, want note.md with the hub's refusal", reason)
	}
}
