package webapp

// The E block of the delta-sync goal (.claude/delta-sync-goal.md): real
// binaries over real HTTP. E2 and E3 are the reason this file exists — they
// drive a binary built from the PRE-CHANGE commit, the only honest "old
// client". Every delta change in this branch is uncommitted work on top of
// HEAD, so `git archive HEAD` yields the pre-change tree without touching
// shared git state; if the delta work gets committed, the ref below must
// become the merge-base of the branch.

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// oldBinRef is the commit the "old client" is built from: the last commit
// before delta sync landed, pinned by sha so it stays the pre-manifest binary
// forever — a moving ref (HEAD, a merge-base) resolves to a delta-aware tree
// once the work is merged, and these tests then build the NEW binary as the
// "old client" and pass vacuously.
const oldBinRef = "33ca0caeabacc6c632fb4ec00fda3abe88c2e3e8"

var (
	oldBinOnce sync.Once
	oldBinPath string
	oldBinErr  error
)

// buildOldBinary extracts oldBinRef into a temp tree via `git archive` (no
// worktree registration, no shared state) and builds bdrive there. Cached for
// the package run.
func buildOldBinary(t *testing.T) string {
	t.Helper()
	oldBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "bdrive-oldbin-*")
		if err != nil {
			oldBinErr = err
			return
		}
		rootOut, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
		if err != nil {
			oldBinErr = fmt.Errorf("git rev-parse: %w", err)
			return
		}
		root := strings.TrimSpace(string(rootOut))
		archive := func() ([]byte, error) {
			return exec.Command("git", "-C", root, "archive", "--format=tar", oldBinRef).Output()
		}
		tarBytes, err := archive()
		if err != nil {
			// CI checkouts are shallow (actions/checkout fetch-depth 1), so
			// the pinned pre-delta commit is usually absent there. Fetch just
			// that commit and retry — one object, no workflow change, and a
			// full local clone never takes this path.
			if fout, ferr := exec.Command("git", "-C", root, "fetch", "--depth=1", "origin", oldBinRef).CombinedOutput(); ferr != nil {
				oldBinErr = fmt.Errorf("git archive %s failed and the commit could not be fetched (shallow clone without network?): %v\n%s", oldBinRef, ferr, fout)
				return
			}
			if tarBytes, err = archive(); err != nil {
				oldBinErr = fmt.Errorf("git archive %s after fetch: %w", oldBinRef, err)
				return
			}
		}
		tr := tar.NewReader(bytes.NewReader(tarBytes))
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				oldBinErr = err
				return
			}
			dst := filepath.Join(dir, filepath.FromSlash(hdr.Name))
			switch hdr.Typeflag {
			case tar.TypeDir:
				if err := os.MkdirAll(dst, 0o755); err != nil {
					oldBinErr = err
					return
				}
			case tar.TypeReg:
				if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
					oldBinErr = err
					return
				}
				f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
				if err != nil {
					oldBinErr = err
					return
				}
				if _, err := io.Copy(f, tr); err != nil {
					f.Close()
					oldBinErr = err
					return
				}
				f.Close()
			}
		}
		bin := filepath.Join(dir, "old-bdrive")
		build := exec.Command("go", "build", "-o", bin, "./cmd/bdrive")
		build.Dir = dir
		if out, err := build.CombinedOutput(); err != nil {
			oldBinErr = fmt.Errorf("build old binary: %v\n%s", err, out)
			return
		}
		oldBinPath = bin
	})
	if oldBinErr != nil {
		t.Fatal(oldBinErr)
	}
	return oldBinPath
}

// randContent is deterministic large content; seeded so reruns measure the
// same bytes.
func randContent(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func shaOfBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// initProject runs init and registers a cleanup stop. `bdrive stop` PAUSES a
// mount (sync then refuses to run), so it must only happen at teardown — the
// daemon stays up during the test, exactly like production; explicit `bdrive
// sync` calls drive the assertions and the daemon's own cycles move the same
// bytes.
func initProject(t *testing.T, e cliEnv, dir, name string, connect bool) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// --name has create-or-join semantics (name-scoped per org), so the same
	// spelling both creates the project on the first device and joins it on
	// every later one; the connect flag exists only for the reader.
	_ = connect
	out, err := e.run(dir, "init", "--name", name, "--yes")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	t.Cleanup(func() { e.run(dir, "stop", dir) })
}

func syncNow(t *testing.T, e cliEnv, dir string) {
	t.Helper()
	if out, err := e.run(dir, "sync"); err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
}

// countingHub wraps a test hub's handler and counts HTTP body bytes in each
// direction — the honest wire cost the E1 row asserts.
func countingHub(t *testing.T) (*httptest.Server, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	inner := startTestHub(t)
	var in, out atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := &countingReader{r: r.Body, n: &in}
		r.Body = body
		cw := &countingRW{ResponseWriter: w, n: &out}
		inner.Config.Handler.ServeHTTP(cw, r)
	}))
	t.Cleanup(proxy.Close)
	return proxy, &in, &out
}

type countingReader struct {
	r io.ReadCloser
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}
func (c *countingReader) Close() error { return c.r.Close() }

type countingRW struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (c *countingRW) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// TestDeltaE2E_TwoDevicesLargeFile (row E1): two real bdrive processes, one
// real hub; a 1-byte edit to a 20 MiB file crosses the wire as chunks, not
// the file — pushed and pulled each under 5 MiB.
func TestDeltaE2E_TwoDevicesLargeFile(t *testing.T) {
	hub, in, out := countingHub(t)
	a := newCLIEnvOn(t, hub)
	b := newCLIEnvOn(t, hub)

	dirA := filepath.Join(t.TempDir(), "proj")
	initProject(t, a, dirA, "delta-e2e", false)
	content := randContent(60, 20<<20)
	if err := os.WriteFile(filepath.Join(dirA, "big.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	syncNow(t, a, dirA)

	dirB := filepath.Join(t.TempDir(), "proj")
	initProject(t, b, dirB, "delta-e2e", true)
	syncNow(t, b, dirB)
	got, err := os.ReadFile(filepath.Join(dirB, "big.bin"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("device B did not converge: %v, %d bytes", err, len(got))
	}

	pushMark := in.Load()
	content[10<<20] ^= 0xff
	if err := os.WriteFile(filepath.Join(dirA, "big.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	syncNow(t, a, dirA)
	pushed := in.Load() - pushMark
	t.Logf("E1: edit pushed %d bytes over real HTTP", pushed)
	if pushed > 5<<20 {
		t.Fatalf("edit pushed %d bytes over the wire, want < 5 MiB", pushed)
	}

	pullMark := out.Load()
	syncNow(t, b, dirB)
	pulled := out.Load() - pullMark
	got, err = os.ReadFile(filepath.Join(dirB, "big.bin"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("device B did not converge on the edit: %v", err)
	}
	t.Logf("E1: edit pulled %d bytes over real HTTP", pulled)
	if pulled > 5<<20 {
		t.Fatalf("edit pulled %d bytes over the wire, want < 5 MiB", pulled)
	}
}

// TestDeltaE2E_OldBinaryReadsChunkedStorage (row E2): a binary that has never
// heard of a manifest syncs a project whose storage holds the big file ONLY
// as chunks + manifest, and gets correct bytes — the hub's reassembly is the
// entire compatibility story, exercised for real.
func TestDeltaE2E_OldBinaryReadsChunkedStorage(t *testing.T) {
	hub := startTestHub(t)
	a := newCLIEnvOn(t, hub)
	old := newCLIEnvBin(t, hub, buildOldBinary(t))

	dirA := filepath.Join(t.TempDir(), "proj")
	initProject(t, a, dirA, "delta-e2e-old-read", false)
	content := randContent(61, 12<<20)
	if err := os.WriteFile(filepath.Join(dirA, "big.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirA, "note.md"), []byte("hello old client"), 0o644); err != nil {
		t.Fatal(err)
	}
	syncNow(t, a, dirA)

	dirOld := filepath.Join(t.TempDir(), "proj")
	initProject(t, old, dirOld, "delta-e2e-old-read", true)
	syncNow(t, old, dirOld)
	got, err := os.ReadFile(filepath.Join(dirOld, "big.bin"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("old binary did not converge on chunked content: %v, %d bytes", err, len(got))
	}
	if got, err := os.ReadFile(filepath.Join(dirOld, "note.md")); err != nil || string(got) != "hello old client" {
		t.Fatalf("old binary missed the small file: %v", err)
	}
}

// TestDeltaE2E_MixedFleetConflictAndDelete: the full sync semantics across
// binary versions on one hub, not just fetch. Concurrent edits of the same
// large file by an old and a new client must resolve the way two same-version
// clients would — one winner everywhere, the loser preserved as a conflict
// copy — and a delete journaled by the new client must remove the file from
// the old client's folder (and the reverse edit land back). If replay or
// conflict handling behaved differently across versions, this is where it
// shows.
func TestDeltaE2E_MixedFleetConflictAndDelete(t *testing.T) {
	hub := startTestHub(t)
	old := newCLIEnvBin(t, hub, buildOldBinary(t))
	cur := newCLIEnvOn(t, hub)

	dirOld := filepath.Join(t.TempDir(), "proj")
	initProject(t, old, dirOld, "delta-e2e-mixed", false)
	base := randContent(70, 8<<20)
	if err := os.WriteFile(filepath.Join(dirOld, "big.bin"), base, 0o644); err != nil {
		t.Fatal(err)
	}
	syncNow(t, old, dirOld)

	dirCur := filepath.Join(t.TempDir(), "proj")
	initProject(t, cur, dirCur, "delta-e2e-mixed", true)
	syncNow(t, cur, dirCur)
	if got, err := os.ReadFile(filepath.Join(dirCur, "big.bin")); err != nil || !bytes.Equal(got, base) {
		t.Fatalf("seed did not converge: %v", err)
	}

	// Concurrent edits before either syncs: old edits the head, new the tail.
	editOld := append([]byte{}, base...)
	editOld[0] ^= 0xff
	editCur := append([]byte{}, base...)
	editCur[len(editCur)-1] ^= 0xff
	if err := os.WriteFile(filepath.Join(dirOld, "big.bin"), editOld, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirCur, "big.bin"), editCur, 0o644); err != nil {
		t.Fatal(err)
	}
	syncNow(t, old, dirOld)
	syncNow(t, cur, dirCur)
	syncNow(t, old, dirOld)
	syncNow(t, cur, dirCur)
	syncNow(t, old, dirOld) // one more pass so the loser's conflict copy propagates

	vOld, err := os.ReadFile(filepath.Join(dirOld, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	vCur, err := os.ReadFile(filepath.Join(dirCur, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(vOld, vCur) {
		t.Fatal("old and new clients converged to different winners")
	}
	if !bytes.Equal(vOld, editOld) && !bytes.Equal(vOld, editCur) {
		t.Fatal("winner is neither client's edit")
	}
	loser := editOld
	if bytes.Equal(vOld, editOld) {
		loser = editCur
	}
	foundConflict := false
	for _, dir := range []string{dirOld, dirCur} {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if strings.Contains(e.Name(), ".bdrive-conflict-") {
				if got, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil && bytes.Equal(got, loser) {
					foundConflict = true
				}
			}
		}
	}
	if !foundConflict {
		t.Fatal("losing edit was not preserved as a conflict copy on either client")
	}

	// Delete journaled by the NEW client must leave the OLD client's folder.
	if err := os.Remove(filepath.Join(dirCur, "big.bin")); err != nil {
		t.Fatal(err)
	}
	syncNow(t, cur, dirCur)
	syncNow(t, old, dirOld)
	if _, err := os.Stat(filepath.Join(dirOld, "big.bin")); err == nil {
		t.Fatal("new client's delete did not propagate to the old client")
	}

	// And a fresh large file from the OLD client lands on the new one.
	rebirth := randContent(71, 6<<20)
	if err := os.WriteFile(filepath.Join(dirOld, "again.bin"), rebirth, 0o644); err != nil {
		t.Fatal(err)
	}
	syncNow(t, old, dirOld)
	syncNow(t, cur, dirCur)
	if got, err := os.ReadFile(filepath.Join(dirCur, "again.bin")); err != nil || !bytes.Equal(got, rebirth) {
		t.Fatalf("old client's new file did not land on the new client: %v", err)
	}
}

// TestDeltaE2E_OldBinaryWritesNewReads (row E3): the old binary pushes whole
// blobs; the current binary must converge byte-identically on them.
func TestDeltaE2E_OldBinaryWritesNewReads(t *testing.T) {
	hub := startTestHub(t)
	old := newCLIEnvBin(t, hub, buildOldBinary(t))
	b := newCLIEnvOn(t, hub)

	dirOld := filepath.Join(t.TempDir(), "proj")
	initProject(t, old, dirOld, "delta-e2e-old-write", false)
	content := randContent(62, 12<<20)
	if err := os.WriteFile(filepath.Join(dirOld, "big.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	syncNow(t, old, dirOld)

	dirB := filepath.Join(t.TempDir(), "proj")
	initProject(t, b, dirB, "delta-e2e-old-write", true)
	syncNow(t, b, dirB)
	got, err := os.ReadFile(filepath.Join(dirB, "big.bin"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("current binary did not converge on old-binary content: %v, %d bytes", err, len(got))
	}

	// And the reverse edit: the current binary edits (chunked push), the old
	// one picks it up (hub reassembly) — full round trip in one project.
	content[100] ^= 0xff
	if err := os.WriteFile(filepath.Join(dirB, "big.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	syncNow(t, b, dirB)
	syncNow(t, old, dirOld)
	got, err = os.ReadFile(filepath.Join(dirOld, "big.bin"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("old binary did not converge on the new binary's edit: %v", err)
	}
}

// TestDeltaE2E_AllReadSurfaces (row E5): viewer file API, history blob,
// download, and a share link all serve correct bytes for a chunked-only file
// over real HTTP.
func TestDeltaE2E_AllReadSurfaces(t *testing.T) {
	hub := startTestHub(t)
	a := newCLIEnvOn(t, hub)

	dir := filepath.Join(t.TempDir(), "proj")
	initProject(t, a, dir, "delta-e2e-surfaces", false)
	content := randContent(63, 10<<20)
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	syncNow(t, a, dir)

	// Resolve the project id from the hub.
	resp, err := a.browser.Get(hub.URL + "/api/projects")
	if err != nil {
		t.Fatal(err)
	}
	var pl struct {
		Projects []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"projects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pl); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var pid string
	for _, p := range pl.Projects {
		if p.Name == "delta-e2e-surfaces" {
			pid = p.ID
		}
	}
	if pid == "" {
		t.Fatalf("project not found in %+v", pl.Projects)
	}

	sha := shaOfBytes(content)
	fetch := func(url string) []byte {
		t.Helper()
		resp, err := a.browser.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d", url, resp.StatusCode)
		}
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if got := fetch(hub.URL + "/api/p/" + pid + "/file?path=big.bin"); !bytes.Equal(got, content) {
		t.Fatal("viewer file API served wrong bytes")
	}
	if got := fetch(hub.URL + "/api/p/" + pid + "/blob?sha=" + sha); !bytes.Equal(got, content) {
		t.Fatal("history blob API served wrong bytes")
	}

	// Share link: minted by the CLI, fetched with no cookies at all.
	out, err := a.run(dir, "share", "big.bin")
	if err != nil {
		t.Fatalf("share: %v\n%s", err, out)
	}
	shareURL := ""
	for _, f := range strings.Fields(out) {
		if strings.Contains(f, "/s/") {
			shareURL = f
		}
	}
	if shareURL == "" {
		t.Fatalf("no share URL in output:\n%s", out)
	}
	anon := &http.Client{}
	resp, err = anon.Get(shareURL)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(got, content) {
		t.Fatalf("share link served wrong bytes: %d, %v, %d bytes", resp.StatusCode, err, len(got))
	}
}

// TestDeltaE2E_MigrateRoundTrip (row E4): real `bdrive export` from one hub,
// real `bdrive import` into another, and a third real device syncs the
// imported project into a fresh folder with correct bytes.
func TestDeltaE2E_MigrateRoundTrip(t *testing.T) {
	hubA := startTestHub(t)
	a := newCLIEnvOn(t, hubA)
	dirA := filepath.Join(t.TempDir(), "proj")
	initProject(t, a, dirA, "delta-e2e-migrate", false)
	content := randContent(64, 9<<20)
	if err := os.WriteFile(filepath.Join(dirA, "big.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirA, "small.md"), []byte("survives migration"), 0o644); err != nil {
		t.Fatal(err)
	}
	syncNow(t, a, dirA)

	arch := filepath.Join(dirA, "export.tar.gz")
	if out, err := a.run(dirA, "export", "-o", arch); err != nil {
		t.Fatalf("export: %v\n%s", err, out)
	}

	// Import runs from anywhere on a signed-in device and CREATES the project
	// on the target hub, named from the archive's manifest.
	hubB := startTestHub(t)
	b := newCLIEnvOn(t, hubB)
	if out, err := b.run(t.TempDir(), "import", arch); err != nil {
		t.Fatalf("import: %v\n%s", err, out)
	}

	c := newCLIEnvOn(t, hubB)
	dirC := filepath.Join(t.TempDir(), "proj")
	initProject(t, c, dirC, "delta-e2e-migrate", true)
	syncNow(t, c, dirC)
	got, err := os.ReadFile(filepath.Join(dirC, "big.bin"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("third device did not converge on migrated content: %v, %d bytes", err, len(got))
	}
	if got, err := os.ReadFile(filepath.Join(dirC, "small.md")); err != nil || string(got) != "survives migration" {
		t.Fatalf("small file lost in migration: %v", err)
	}
}

// TestDeltaE2E_DaemonPropagates (row E6): the daemon path, not one-shot sync.
// A chunked edit lands on a peer through its running daemon.
func TestDeltaE2E_DaemonPropagates(t *testing.T) {
	hub := startTestHub(t)
	a := newCLIEnvOn(t, hub)
	b := newCLIEnvOn(t, hub)

	dirA := filepath.Join(t.TempDir(), "proj")
	initProject(t, a, dirA, "delta-e2e-daemon", false)
	content := randContent(65, 8<<20)
	if err := os.WriteFile(filepath.Join(dirA, "big.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	syncNow(t, a, dirA)

	// Device B keeps its daemon RUNNING (init starts it; no stop here).
	dirB := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(dirB, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := b.run(dirB, "init", "--name", "delta-e2e-daemon", "--yes")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	defer b.run(dirB, "stop", dirB)

	deadline := time.Now().Add(90 * time.Second)
	for {
		got, err := os.ReadFile(filepath.Join(dirB, "big.bin"))
		if err == nil && bytes.Equal(got, content) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not materialize the chunked file in 90s (err=%v)", err)
		}
		time.Sleep(2 * time.Second)
	}
}
