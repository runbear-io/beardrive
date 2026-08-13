package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/webapp"
)

// Round 8 — the five CLI commands with zero TestSec_* coverage: import, stop,
// read-log, autostart, daemon. `import` is the one that matters, and it is the
// only command in the CLI whose TARGET is chosen by an untrusted file.
//
// Helpers here are prefixed sec8.

// ---------------------------------------------------------------------------
// a real hub, because the finding is about a real route's semantics
// ---------------------------------------------------------------------------

const (
	sec8Email = "owner@example.com"
	sec8Pass  = "password1"
)

// sec8Hub starts a hub wired the way cmd/bdrive/web.go wires one (auth + orgs
// + projects + the /store proxy over a file:// root) and signs this device in
// through the real device-code flow, leaving a usable settings.json in an
// isolated BDRIVE_HOME. Returns the hub and a signed-in browser client.
//
// A stubbed hub would decide the outcome of the import test by itself, and
// create-or-join-by-name is exactly the semantic under attack — so this uses
// the actual ProjectDB and the actual POST /api/projects handler.
func sec8Hub(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	state := t.TempDir()
	be, err := remote.Open(context.Background(), "file://"+filepath.Join(state, "storage"))
	if err != nil {
		t.Fatal(err)
	}
	projects, err := webapp.OpenProjectDB(filepath.Join(state, "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := webapp.OpenBuiltinAuth(filepath.Join(state, "auth.json"), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	orgs, err := webapp.OpenOrgDB(filepath.Join(state, "orgs.json"))
	if err != nil {
		t.Fatal(err)
	}
	devices, err := webapp.OpenDeviceRegistry(filepath.Join(state, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &webapp.Server{
		Root: be, Projects: projects, Auth: auth, Devices: devices,
		Dir:    webapp.LocalDirectory{OrgDB: orgs},
		Device: webapp.Identity{ID: "hubdev", Name: "hub", Author: "hub@test"},
		Upload: webapp.UploadConfig{Enabled: true},
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	resp, err := browser.PostForm(ts.URL+"/auth/signup", url.Values{
		"email": {sec8Email}, "name": {"Owner"}, "password": {sec8Pass},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(jar.Cookies(sec8URL(t, ts.URL))) == 0 {
		t.Fatal("signup left no session cookie")
	}
	if _, err := orgs.Create("default", sec8Email); err != nil {
		t.Fatal(err)
	}

	// The real headless device flow: start (anonymous) → approve in the
	// browser → poll. This is how a CLI gets a token, and it leaves a token
	// the hub genuinely accepts.
	var start struct{ Code string }
	sec8JSON(t, browser, "POST", ts.URL+"/api/auth/device/start",
		map[string]string{"device": "sec8-device", "os": "test"}, &start, "")
	if start.Code == "" {
		t.Fatal("device/start returned no code")
	}
	if resp, err := browser.PostForm(ts.URL+"/auth/device/"+start.Code, nil); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	var poll struct{ Token string }
	sec8JSON(t, browser, "POST", ts.URL+"/api/auth/device/poll",
		map[string]string{"code": start.Code, "device": "sec8-device"}, &poll, "")
	if poll.Token == "" {
		t.Fatal("device/poll returned no token")
	}

	t.Setenv("BDRIVE_HOME", t.TempDir())
	s, err := config.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	s.Server, s.Token, s.Email, s.Name = ts.URL, poll.Token, sec8Email, "Owner"
	if err := config.SaveSettings(s); err != nil {
		t.Fatal(err)
	}
	return ts, browser
}

func sec8URL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func sec8JSON(t *testing.T, c *http.Client, method, u string, body, out any, token string) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("%s %s: %d %s", method, u, resp.StatusCode, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: bad body %s: %v", method, u, data, err)
		}
	}
}

// sec8Archive builds a bdrive export archive whose manifest names `project`
// and which carries one journal and one honest blob. blobBody, when non-nil,
// replaces the blob's tar member entirely (for the size attack).
func sec8Archive(t *testing.T, project string, blobSize int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hostile-export.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	man, _ := json.Marshal(map[string]any{
		"project": project, "remote": "https://attacker.example/p/x",
		"exported_at": time.Now().UTC(),
	})
	sec8Member(t, tw, "beardrive-export.json", man)

	content := []byte("planted by the archive\n")
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])
	op, _ := json.Marshal(map[string]any{
		"seq": 1, "lamport": 1, "time": time.Now().UTC().Format(time.RFC3339Nano),
		"kind": "put", "path": "PLANTED.md", "blob": sha, "size": len(content),
		"device": "attacker", "user": "attacker@evil.example",
	})
	sec8Member(t, tw, "journal/attacker.jsonl", append(op, '\n'))

	if blobSize > 0 {
		// A gzip bomb: the member declares blobSize bytes of zeros, which
		// compress to almost nothing on the wire.
		if err := tw.WriteHeader(&tar.Header{
			Name: "blobs/" + strings.Repeat("a", 64), Mode: 0o644, Size: blobSize,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.CopyN(tw, sec8Zeros{}, blobSize); err != nil {
			t.Fatal(err)
		}
	} else {
		sec8Member(t, tw, "blobs/"+sha, content)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

type sec8Zeros struct{}

func (sec8Zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func sec8Member(t *testing.T, tw *tar.Writer, name string, body []byte) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// 1. bdrive import — the archive picks the project
// ---------------------------------------------------------------------------

// importCmd's whole target selection is three lines:
//
//	man, first, err := readManifest(tr)          // manifest is INSIDE the archive
//	if name == "" { name = man.Project }
//	p, created, err := createProject(server, token, name, "")
//
// POST /api/projects is create-or-JOIN-by-name (that is its documented
// contract, and `created` in the response is how the caller learns which
// happened — import prints "joined" and carries on). The "must be empty"
// guard runs AFTER:
//
//	if existing, err := be.List(ctx, "journal/"); ... len(existing) > 0 { refuse }
//
// So the sequence for an archive a user was handed — the docs say to pass
// these around, that is the whole anti-lock-in story — is:
//
//  1. the archive names one of the user's OWN projects
//  2. import joins it without asking
//  3. the emptiness check passes if that project has never synced
//  4. the attacker's journals, blobs and fabricated authorship land in it
//
// Step 4 is not cosmetic: every device that syncs that project pulls those
// ops, so files appear in teammates' folders with History crediting an
// attacker-chosen account, and the manifest's own field decided which project
// on which hub. The user typed a filename; the file chose the destination.
//
// Secure behaviour: import writes into a project the USER named (--name, or a
// project it created for this archive). An untrusted manifest may propose a
// name; it may not select an existing project.
func TestSec_Import_AHostileArchiveCannotLandInAProjectTheUserNeverNamed(t *testing.T) {
	hub, browser := sec8Hub(t)

	// The victim already has a project. It is empty — never synced — which is
	// the ordinary state of a project created in the UI and not yet mounted.
	var made struct {
		Project struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"project"`
		Created bool `json:"created"`
	}
	sec8JSON(t, browser, "POST", hub.URL+"/api/projects", map[string]string{"name": "quarterly-plans"}, &made, "")
	if !made.Created || made.Project.ID == "" {
		t.Fatalf("setup: victim project not created: %+v", made)
	}

	// An archive that names it. Nothing else about the archive is unusual.
	archive := sec8Archive(t, "quarterly-plans", 0)

	t.Chdir(t.TempDir())
	out, err := seccliRun(t, importCmd(), []string{archive})
	t.Logf("import: err=%v out=%q", err, out)

	// Whatever import decided, the victim's existing project must not now hold
	// the archive's content.
	var tree struct {
		Entries []struct {
			Path string `json:"path"`
		} `json:"entries"`
	}
	sec8JSON(t, browser, "GET", hub.URL+"/api/p/"+made.Project.ID+"/tree", nil, &tree, "")
	for _, e := range tree.Entries {
		if strings.Contains(e.Path, "PLANTED") {
			t.Fatalf("an archive's own manifest chose the project it landed in: %q now holds %q\n"+
				"(importCmd takes `name` from man.Project and posts it to POST /api/projects, which is\n"+
				"create-or-join-by-name; the \"must be empty\" guard runs after the join)",
				made.Project.Name, e.Path)
		}
	}
	if strings.Contains(out, "joined") {
		t.Fatalf("import joined an existing project the user never named: %s", out)
	}
}

// ---------------------------------------------------------------------------
// 2. bdrive import — a bounded archive must not become an unbounded local write
// ---------------------------------------------------------------------------

// spoolBlob is io.Copy into os.CreateTemp with no cap:
//
//	n, err := io.Copy(io.MultiWriter(tmp, h), r)
//
// The reader is a tar member inside a GZIP stream, and the member's declared
// size is the attacker's number too. Zeros compress about a thousand to one,
// so a 1 MB file that looks exactly like a bdrive export writes a gigabyte to
// the importer's temp filesystem before the first check runs — the sha
// comparison that would reject it happens after the copy, by construction.
//
// This is row 18's own threat model ("a hostile archive"), on the one path
// round 4 did not measure: the archive IS the file the docs tell users to pass
// around, and `bdrive import <file>` is what they are told to run on it.
//
// Every other reader in this codebase that consumes a remote party's bytes is
// bounded — journalOps and every JSON decoder use io.LimitReader, round 7
// capped httpBackend.List at 8 MiB. This one is not.
//
// Secure behaviour asserted, deliberately fix-agnostic and very generous: an
// archive of N bytes must not spool more than 1000×N to local disk. Any real
// cap (a flag, a fixed ceiling, a ratio) satisfies it; no cap does not.
func TestSec_Import_ABoundedArchiveCannotSpoolUnboundedBytesToDisk(t *testing.T) {
	const bomb = 512 << 20 // 512 MiB of zeros in the archive's blob member

	archive := sec8Archive(t, "anything", bomb)
	fi, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("archive on disk: %d bytes, declared blob member: %d bytes (%.0fx)",
		fi.Size(), int64(bomb), float64(bomb)/float64(fi.Size()))

	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	if _, _, err := readManifest(tr); err != nil {
		t.Fatal(err)
	}

	spooled := &sec8CountingBackend{}
	before := sec8TempBytes(t)
	stop, watched := make(chan struct{}), make(chan struct{})
	var peak atomic.Int64
	go func() {
		defer close(watched)
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			if n := sec8TempBytes(t) - before; n > peak.Load() {
				peak.Store(n)
			}
		}
	}()
	_, _, _, ierr := importStore(context.Background(), spooled, tr, nil, false)
	close(stop)
	<-watched
	peakBytes := peak.Load()
	t.Logf("importStore: err=%v, peak temp bytes: %d", ierr, peakBytes)

	limit := fi.Size() * 1000
	if peakBytes > limit {
		t.Fatalf("a %d-byte archive spooled %d bytes to the local temp filesystem (%.0fx)\n"+
			"(spoolBlob is an unbounded io.Copy into os.CreateTemp; the sha check that would reject\n"+
			"these bytes runs after the copy, and the archive is the file the docs say to pass around)",
			fi.Size(), peakBytes, float64(peakBytes)/float64(fi.Size()))
	}
}

// sec8TempBytes is the total size of this process's bdrive-import spool files.
func sec8TempBytes(t *testing.T) int64 {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "bdrive-import-*"))
	var total int64
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil {
			total += fi.Size()
		}
	}
	return total
}

// sec8CountingBackend accepts every Put and stores nothing: the attack is what
// happens before Put is ever reached.
type sec8CountingBackend struct{ puts int }

func (b *sec8CountingBackend) Put(_ context.Context, _ string, r io.Reader, _ int64) error {
	b.puts++
	_, err := io.Copy(io.Discard, r)
	return err
}
func (b *sec8CountingBackend) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("no such object")
}
func (b *sec8CountingBackend) List(context.Context, string) ([]remote.Object, error) {
	return nil, nil
}
func (b *sec8CountingBackend) Exists(context.Context, string) (bool, error) { return false, nil }
func (b *sec8CountingBackend) Close() error                                 { return nil }

// ---------------------------------------------------------------------------
// 3. bdrive stop — a folder's config chooses which project is paused
// ---------------------------------------------------------------------------

// .bdrive/config.json travels with the folder — a zip, a clone, a colleague's
// copy — which is why syncBlocked exists and says, in as many words, that its
// presence alone is not consent to sync: only `bdrive init` enrolls a device.
//
// `bdrive stop` does not consult it. It resolves the folder with mustProject
// (ResolveMount, which self-heals the registry) and then pauses whatever mount
// id that file names:
//
//	vdir, _ := config.VolumeDir(proj.ID)
//	daemon.Stop(vdir)
//	store.SetPaused(vdir, true)
//
// So a folder that arrives on the machine — a repo someone cloned, an archive
// they unpacked — can carry the id of a project this device really does sync,
// and one `bdrive stop` in that folder stops and PAUSES the real one. The
// pause outlives the daemon by design ("cleared by bdrive init"), so the
// victim's project silently stops syncing until someone re-inits it. Silent
// staleness is the failure mode this product exists to prevent.
//
// Secure behaviour: stop pauses the project the user is standing in, and a
// folder this device never enrolled is not one of them.
func TestSec_Stop_AClonedFolderCannotPauseAProjectItOnlyNames(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())

	// The real, enrolled mount.
	real := t.TempDir()
	proj, err := config.SaveProject(real, config.Project{Volume: "wiki", Remote: "file://" + t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(real); err != nil {
		t.Fatal(err)
	}
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}

	// A folder that merely arrived, carrying a config that names the same id.
	clone := t.TempDir()
	if err := os.MkdirAll(filepath.Join(clone, ".bdrive"), 0o755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(proj)
	if err := os.WriteFile(filepath.Join(clone, ".bdrive", "config.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := seccliRun(t, stopCmd(), []string{clone})
	t.Logf("stop in the cloned folder: err=%v out=%q", err, out)

	if sec8Paused(t, vdir) {
		t.Fatalf("`bdrive stop` in a folder this device never enrolled paused the real project %q\n"+
			"(mustProject/ResolveMount trusts .bdrive/config.json, which travels with the folder;\n"+
			"syncBlocked exists precisely because its presence is not consent, and stop never asks it)",
			proj.ID)
	}
}

func sec8Paused(t *testing.T, vdir string) bool {
	t.Helper()
	return store.Paused(vdir) // the same question syncBlocked asks
}

// The same untrusted file, one layer deeper, and this is the sharper form.
//
// ResolveMount — which every folder-taking command calls — re-points the
// registry unconditionally:
//
//	mi, registered := mounts[p.ID]
//	if !registered || mi.Path != folder || mi.Volume != p.Volume || mi.Remote != p.Remote {
//		mounts[p.ID] = MountInfo{Path: folder, Volume: p.Volume, Remote: p.Remote}
//
// That is the "renames and moves are free" self-heal, and it cannot tell a
// move from a COPY: the mount id lives in a file that travels with the folder.
// Rounds 4 and 5 validated the SHAPE of Project.ID (it is joined onto
// $BDRIVE_HOME) and the ORIGIN of Project.Remote (it chose where the token
// went). Nothing validated its AUTHORITY — that this device ever agreed this
// folder is that mount.
//
// So one `bdrive stop` (or status, sync, log, share, export — any of them) in
// a folder that merely arrived rewrites the registry row for a real project:
// its Path becomes the arriving folder, and its Volume and Remote become that
// folder's too. `bdrive resume` and the login autostart both read that row, so
// at the next login the daemon for the user's real project runs on the
// arriving folder — the real folder stops syncing entirely, and the arriving
// folder's contents are what gets pushed to the project.
//
// Secure behaviour: the self-heal follows a mount that MOVED. A registry row
// whose recorded path still holds that mount's own config is not stale, and a
// second folder claiming the same id does not get to take it.
func TestSec_Stop_AnArrivingFolderCannotStealAnEnrolledMountsRegistryRow(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())

	real := t.TempDir()
	proj, err := config.SaveProject(real, config.Project{Volume: "wiki", Remote: "file://" + t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(real); err != nil {
		t.Fatal(err)
	}

	clone := t.TempDir()
	if err := os.MkdirAll(filepath.Join(clone, ".bdrive"), 0o755); err != nil {
		t.Fatal(err)
	}
	hostile := proj
	hostile.Volume = "wiki"
	hostile.Remote = "file://" + t.TempDir() // a remote of the arriving folder's choosing
	body, _ := json.Marshal(hostile)
	if err := os.WriteFile(filepath.Join(clone, ".bdrive", "config.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := seccliRun(t, stopCmd(), []string{clone})
	t.Logf("stop in the arriving folder: err=%v out=%q", err, out)

	mounts, err := config.LoadMounts()
	if err != nil {
		t.Fatal(err)
	}
	mi := mounts[proj.ID]
	if mi.Path != real {
		t.Fatalf("mount %s now points at the folder that merely arrived\n  was: %s\n  now: %s\n"+
			"(ResolveMount re-points the registry row for any folder carrying the id; `bdrive resume`\n"+
			"and the login autostart both start the daemon from this row)", proj.ID, real, mi.Path)
	}
	if mi.Remote != proj.Remote {
		t.Fatalf("mount %s now syncs to a remote the arriving folder chose: %s (was %s)",
			proj.ID, mi.Remote, proj.Remote)
	}
}

// ---------------------------------------------------------------------------
// 4. bdrive read-log — what a hook event may claim was read
// ---------------------------------------------------------------------------

// sec8ReadMount enrols a folder for read-log (LoadProject + a registered,
// unpaused mount, which is what logReads requires) and returns the folder and
// its volume dir.
func sec8ReadMount(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder := t.TempDir()
	proj, err := config.SaveProject(folder, config.Project{Volume: "wiki", Remote: "file://" + t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(folder); err != nil {
		t.Fatal(err)
	}
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	return folder, vdir
}

// sec8Spooled drains the volume's read spool.
func sec8Spooled(t *testing.T, vdir string) []string {
	t.Helper()
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := st.PendingReads()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		out = append(out, e.Path)
	}
	return out
}

func sec8ReadLog(t *testing.T, folder string, event any) {
	t.Helper()
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	cmd := readLogCmd()
	cmd.SetIn(bytes.NewReader(body))
	cmd.SetArgs([]string{folder})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("read-log: %v", err)
	}
}

// matchCandidates turns a search tool's output into read paths by taking each
// line whole AND up to its first colon:
//
//	out = append(out, line)
//	if i := strings.IndexByte(line, ':'); i > 0 { out = append(out, line[:i]) }
//
// "path:12:matched text" is the shape it is written for. But a colon is a
// perfectly legal character in a synced filename — journal.SafePath allows
// every byte from 0x20 up except DEL — so a file named "CLAUDE.md:notes"
// produces the grep line
//
//	CLAUDE.md:notes:12:needle
//
// and the first-colon split names CLAUDE.md, a DIFFERENT file that was never
// opened. statFiles then confirms it exists, which is the guard that is
// supposed to turn a heuristic into a trustworthy read, and it agrees.
//
// The file that does this travels: it is an ordinary file in a shared project,
// so any member can put it there and it lands in every teammate's folder. From
// then on every agent search that matches it reports reads of a path of the
// planter's choosing, under the VICTIM's device id, into the project's read
// heat — which is the signal the Dashboard's staleness quadrant and the folder
// heat dots are drawn from.
//
// Row 10 spent three rounds making sure a member cannot report reads under a
// peer's device id. This is the same forgery from the other end: the device is
// genuine and the read is not.
//
// Secure behaviour: a mined candidate is a read only if it is the file the
// match actually came from.
func TestSec_ReadLog_AFilenameCannotChargeItsReadsToAnotherFile(t *testing.T) {
	folder, vdir := sec8ReadMount(t)
	victim := "CLAUDE.md"
	planted := "CLAUDE.md:notes"
	for _, name := range []string{victim, planted} {
		if err := os.WriteFile(filepath.Join(folder, name), []byte("needle\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Control: an ordinary grep hit records the file it matched in, so the
	// harness is known to reach the spool at all.
	sec8ReadLog(t, folder, map[string]any{
		"tool_name":     "Grep",
		"tool_response": planted + ":12:needle",
	})
	got := sec8Spooled(t, vdir)
	if len(got) == 0 {
		t.Fatalf("control: nothing was spooled at all; the test proves nothing")
	}
	for _, p := range got {
		if p == victim {
			t.Fatalf("a grep that matched %q recorded a read of %q\n"+
				"(matchCandidates splits every response line at its first colon, and a colon is a legal\n"+
				"character in a synced filename — so a file any project member can plant charges its\n"+
				"reads to a path of their choosing, under the reader's own device id)", planted, victim)
		}
	}
}

// The containment property read-log must hold whatever the event says: nothing
// outside the mount is ever spooled. The event is written by the agent
// platform and its strings are ultimately file content and command lines, so
// every field here is attacker-reachable.
func TestSec_ReadLog_NoEventShapeSpoolsAPathOutsideTheMount(t *testing.T) {
	folder, vdir := sec8ReadMount(t)
	outside := filepath.Join(t.TempDir(), "secrets.txt")
	if err := os.WriteFile(outside, []byte("private\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "inside.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	events := []any{
		map[string]any{"tool_name": "Read", "tool_input": map[string]any{"file_path": outside}},
		map[string]any{"tool_name": "Read", "tool_input": map[string]any{"file_path": "../" + filepath.Base(filepath.Dir(outside)) + "/secrets.txt"}},
		map[string]any{"tool_name": "Read", "tool_input": map[string]any{"path": "/etc/hosts"}},
		map[string]any{"tool_name": "Read", "tool_input": map[string]any{"paths": []string{outside, "/etc/hosts"}}},
		map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "cat " + outside}},
		map[string]any{"tool_name": "Grep", "tool_response": outside + ":1:private"},
		map[string]any{"tool_name": "Read", "tool_input": map[string]any{"file_path": folder + "/../" + filepath.Base(folder) + "/inside.md"}},
	}
	for _, e := range events {
		sec8ReadLog(t, folder, e)
	}
	for _, p := range sec8Spooled(t, vdir) {
		if p == "inside.md" {
			continue
		}
		if strings.HasPrefix(p, "..") || strings.HasPrefix(p, "/") || strings.Contains(p, "secrets.txt") || strings.Contains(p, "hosts") {
			t.Errorf("read-log spooled %q, which is not inside the mount", p)
		}
	}
}
