package main

// Round 7: `bdrive init`, driven end to end as the real binary against a live
// hub. init is the front door and it does the most dangerous things in the
// product — it writes .bdrive/config.json, seeds .bdriveignore, registers
// machine-wide agent hooks that run on every tool call of every session,
// installs a login item that runs before any guard, writes the managed scope
// block into the SYNCED .bdriveignore, and starts a daemon. Everything it
// writes it writes from two untrusted sources: the hub's answer (project name
// and id) and the folder path it was pointed at.
//
// The hub in this file is a fixture the test controls, which is the point:
// `trimName` guards the CREATION path, so the only way to ask what init does
// with a name it should never have seen — a project created by an older hub,
// by a different client, or by a hub that is simply lying — is to hand it one.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------- harness

var (
	secinitBuildOnce sync.Once
	secinitBinPath   string
	secinitBuildErr  error
)

// secinitBinary builds the real bdrive binary once for the whole package.
func secinitBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and execs the bdrive binary; skipped with -short")
	}
	secinitBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "secinit-bin")
		if err != nil {
			secinitBuildErr = err
			return
		}
		secinitBinPath = filepath.Join(dir, "bdrive")
		build := exec.Command("go", "build", "-o", secinitBinPath, "github.com/runbear-io/beardrive/cmd/bdrive")
		if out, err := build.CombinedOutput(); err != nil {
			secinitBuildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if secinitBuildErr != nil {
		t.Fatal(secinitBuildErr)
	}
	return secinitBinPath
}

// secinitHubOpts configures what the fixture hub answers.
type secinitHubOpts struct {
	name       string // the project name handed back (defaults to what was asked)
	id         string // the project id handed back (default: a fresh one per name)
	emptyID    bool   // the hub omits the id entirely
	createFail int    // non-zero: POST /api/projects answers this status
	storeFail  int    // non-zero: every /store/* answers this status
}

type secinitEnv struct {
	bin   string
	url   string
	home  string
	bhome string
	env   []string

	mu       sync.Mutex
	uploaded map[string][]byte // every object body the device pushed
	auth     []string          // every Authorization header the device sent
}

// sentAuth returns every Authorization header this hub was given.
func (e *secinitEnv) sentAuth() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.auth...)
}

// pushed returns a copy of everything the device uploaded to the hub.
func (e *secinitEnv) pushed() map[string][]byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string][]byte, len(e.uploaded))
	for k, v := range e.uploaded {
		out[k] = v
	}
	return out
}

// run executes the real binary in dir and returns its combined output.
func (e *secinitEnv) run(dir string, args ...string) (string, error) {
	cmd := exec.Command(e.bin, args...)
	cmd.Dir = dir
	cmd.Env = e.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// secinitNewEnv starts a fixture hub with auth disabled (so init runs its
// whole flow without the login dance) and an isolated HOME/BDRIVE_HOME, so
// the agent hooks and the login item land where the test can read them.
func secinitNewEnv(t *testing.T, opts secinitHubOpts) *secinitEnv {
	t.Helper()
	bin := secinitBinary(t)
	e := &secinitEnv{bin: bin, uploaded: map[string][]byte{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSONTo(w, map[string]any{"mode": "hub", "auth": map[string]any{"enabled": false}})
	})
	// Like a real hub: one id per project name, a fresh one for each new
	// name. A fixture that answers with ONE id for every project makes
	// checkNotAlreadyMounted refuse the second init for a reason that has
	// nothing to do with what is being tested.
	var mu sync.Mutex
	ids := map[string]string{}
	project := func(asked string) map[string]any {
		name := opts.name
		if name == "" {
			name = asked
		}
		mu.Lock()
		defer mu.Unlock()
		id, ok := ids[asked]
		switch {
		case opts.emptyID:
			id = ""
		case opts.id != "":
			id = opts.id
		case !ok:
			id = fmt.Sprintf("7f3a2c91-4d5e-4b8a-9c17-2ad0f6b3e9%02d", len(ids))
			ids[asked] = id
		}
		return map[string]any{"id": id, "name": name, "template": ""}
	}
	mux.HandleFunc("/api/projects", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSONTo(w, map[string]any{"projects": []any{project("listed")}})
			return
		}
		if opts.createFail != 0 {
			http.Error(w, "nope", opts.createFail)
			return
		}
		var body struct{ Name string }
		json.NewDecoder(r.Body).Decode(&body)
		writeJSONTo(w, map[string]any{"project": project(body.Name), "created": true})
	})
	mux.HandleFunc("/api/projects/", func(w http.ResponseWriter, r *http.Request) {
		writeJSONTo(w, project(strings.TrimPrefix(r.URL.Path, "/api/projects/")))
	})
	// The store API the sync cycle drives: an empty, writable project.
	mux.HandleFunc("/api/p/", func(w http.ResponseWriter, r *http.Request) {
		if opts.storeFail != 0 {
			http.Error(w, "nope", opts.storeFail)
			return
		}
		// Objects are namespaced by project, exactly as remote.Prefixed does
		// on a real hub — otherwise two projects share one key space and no
		// cross-project observation in this file means anything.
		ns := strings.TrimPrefix(r.URL.Path, "/api/p/")
		ns = ns[:strings.Index(ns+"/", "/")] + "/"
		switch {
		case strings.HasSuffix(r.URL.Path, "/store/list"):
			prefix := ns + r.URL.Query().Get("prefix")
			objects := []any{}
			e.mu.Lock()
			for k, v := range e.uploaded {
				if strings.HasPrefix(k, prefix) {
					objects = append(objects, map[string]any{"key": strings.TrimPrefix(k, ns), "size": len(v)})
				}
			}
			e.mu.Unlock()
			writeJSONTo(w, map[string]any{"objects": objects})
		case strings.HasSuffix(r.URL.Path, "/store/exists"):
			e.mu.Lock()
			_, ok := e.uploaded[ns+r.URL.Query().Get("key")]
			e.mu.Unlock()
			writeJSONTo(w, map[string]any{"exists": ok})
		case strings.HasSuffix(r.URL.Path, "/store/sign"):
			writeJSONTo(w, map[string]any{"mode": "server"}) // relay, like a file:// hub
		case strings.HasSuffix(r.URL.Path, "/store/object"):
			key := ns + r.URL.Query().Get("key")
			e.mu.Lock()
			body, ok := e.uploaded[key]
			e.mu.Unlock()
			if r.Method == http.MethodGet {
				if !ok {
					http.Error(w, "no such object", http.StatusNotFound)
					return
				}
				w.Write(body)
				return
			}
			data, _ := io.ReadAll(r.Body)
			e.mu.Lock()
			e.uploaded[key] = data
			e.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "no route", http.StatusNotFound)
		}
	})
	record := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "" {
			e.mu.Lock()
			e.auth = append(e.auth, a)
			e.mu.Unlock()
		}
		mux.ServeHTTP(w, r)
	})
	hub := httptest.NewServer(record)
	t.Cleanup(hub.Close)

	home := t.TempDir()
	bhome := filepath.Join(home, ".bdrive")
	e.url, e.home, e.bhome = hub.URL, home, bhome
	e.env = append(secinitEnvWithout("HOME", "BDRIVE_HOME", "BDRIVE_TOKEN", "XDG_CONFIG_HOME"),
		"HOME="+home, "BDRIVE_HOME="+bhome, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	return e
}

func writeJSONTo(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func secinitEnvWithout(names ...string) []string {
	var out []string
Env:
	for _, kv := range os.Environ() {
		for _, n := range names {
			if strings.HasPrefix(kv, n+"=") {
				continue Env
			}
		}
		out = append(out, kv)
	}
	return out
}

// secinitDangerous reports the terminal-control runes in s: exactly the set
// safeField strips, because they are the set that makes a printed row
// something other than what it says.
func secinitDangerous(s string) []string {
	var found []string
	for _, r := range s {
		switch {
		case r == '\n', r == '\t':
			// init's own formatting; harmless on their own.
		case r < 0x20, r == 0x7f:
			found = append(found, fmt.Sprintf("C0 %#U", r))
		case r >= 0x80 && r <= 0x9f:
			found = append(found, fmt.Sprintf("C1 %#U", r))
		case r == 0x061c, r == 0x200e, r == 0x200f,
			r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			found = append(found, fmt.Sprintf("bidi %#U", r))
		}
	}
	return found
}

// secinitHostileName carries every shape safeField exists to remove: an OSC
// window-title/clipboard write, a CSI row repaint, a bare CR, a forged line,
// and the C1 and bidi forms that carry the same meaning with no ESC byte.
const secinitHostileName = "wiki\x1b]0;pwned\x07\x1b[2K\rdeleted 0 files\n" +
	"\u009b31mRED\u009d52;c;cHduZWQK\u0007\u202eemag.exe"

// ------------------------------------------------------- 1. hostile name

// The hub names the project; init prints that name to the operator's terminal
// and writes it into .bdrive/config.json, from where every later command reads
// it. `bdrive status`, `bdrive log` and `bdrive restore --list` all launder
// hub- and peer-chosen strings through safeField before rendering them (rows
// 18 and 21). init is the command that receives the name FIRST and prints it
// on the create path, on the resume path, and in the closing "next steps"
// block — and it renders all of it with a bare %s.
//
// trimName guards the hub's creation path, so this attack is aimed where a
// name that never went through it arrives: a project created by an older hub,
// by another client, or by a hub that is simply answering with whatever it
// likes. The device must not be renderable by the server it syncs with.
func TestSec_Init_HostileProjectNameNeverRendersRawToTheTerminal(t *testing.T) {
	e := secinitNewEnv(t, secinitHubOpts{name: secinitHostileName})

	work := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	defer e.run(work, "stop", work)

	out, err := e.run(work, "init", "--name", "wiki", "--server", e.url, "--yes")
	if err != nil {
		t.Fatalf("init: %v\n%q", err, out)
	}
	if bad := secinitDangerous(out); len(bad) > 0 {
		t.Errorf("init rendered the hub's project name raw to the terminal: %v\noutput: %q", bad, out)
	}

	// The same string comes back on every re-init, off the local config this
	// time ("resuming <folder> (project <name>)").
	out, err = e.run(work, "init", "--yes")
	if err != nil {
		t.Fatalf("re-init: %v\n%q", err, out)
	}
	if !strings.Contains(out, "resuming") {
		t.Fatalf("re-init did not take the resume path:\n%q", out)
	}
	if bad := secinitDangerous(out); len(bad) > 0 {
		t.Errorf("re-init rendered the stored project name raw to the terminal: %v\noutput: %q", bad, out)
	}
}

// The delta that proves the finding is the server's answer and not the
// harness: the identical command against a hub answering with an ordinary
// name prints an ordinary line.
func TestSec_Init_BenignProjectNamePrintsCleanly(t *testing.T) {
	e := secinitNewEnv(t, secinitHubOpts{})

	work := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	defer e.run(work, "stop", work)

	out, err := e.run(work, "init", "--name", "wiki", "--server", e.url, "--yes")
	if err != nil {
		t.Fatalf("init: %v\n%q", err, out)
	}
	if !strings.Contains(out, "project: wiki") {
		t.Fatalf("init did not report the project it created:\n%s", out)
	}
	if bad := secinitDangerous(out); len(bad) > 0 {
		t.Fatalf("a benign run is already dirty, the fixture is wrong: %v\n%q", bad, out)
	}
}

// --------------------------------------------------- 2. hostile hub: id

// The project id the hub hands back becomes the mount's remote URL
// (server + "/p/" + id) in .bdrive/config.json and in the machine's mount
// registry. init writes both — plus the machine-wide agent hooks and the
// login item — BEFORE anything checks that the id names a project at all.
// The only check is remote.Open's projectPathRe, and it runs inside the first
// sync cycle, where a failure degrades to "offline" by design.
//
// So an id the device can never open produces an init that reports SUCCESS,
// starts a daemon, and prints "the daemon now keeps this folder in sync
// automatically" for a folder whose every cycle will silently be a no-op
// forever. The hub is not trusted for object keys (row 19) and must not be
// trusted here either: init must not report a folder as syncing when the
// remote it just wrote cannot be opened.
func TestSec_Init_AProjectIDTheDeviceCannotUseIsNotReportedAsSyncing(t *testing.T) {
	for _, tc := range []struct {
		label string
		opts  secinitHubOpts
	}{
		{"traversal", secinitHubOpts{id: "../../../etc"}},
		{"nested", secinitHubOpts{id: "p-ok/../../other"}},
		{"space", secinitHubOpts{id: "p ok"}},
		{"absent", secinitHubOpts{emptyID: true}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			e := secinitNewEnv(t, tc.opts)
			work := filepath.Join(t.TempDir(), "proj")
			if err := os.MkdirAll(work, 0o755); err != nil {
				t.Fatal(err)
			}
			defer e.run(work, "stop", work)

			out, err := e.run(work, "init", "--name", "wiki", "--server", e.url, "--yes")
			if err != nil {
				t.Logf("refused: %s", out)
				return // refusing is the secure answer
			}
			if strings.Contains(out, "keeps this folder in sync automatically") {
				t.Errorf("init reported an unusable remote as syncing:\n%s", out)
			}
			cfg, _ := os.ReadFile(filepath.Join(work, ".bdrive", "config.json"))
			t.Logf("mount config left behind: %s", cfg)
		})
	}
}

// `bdrive init --server <url>` exists so that connecting to a hub this device
// has never seen takes one command instead of two. It therefore accepts a URL
// from whoever is driving — a README, an onboarding doc, an agent following a
// link — and the target server then chooses the branch init takes:
// ensureLogin asks it whether authentication is on.
//
// A server that answers "auth: disabled" (which every plain-folder viewer
// legitimately does) sends init down a branch that writes settings.Server =
// <that server> and LEAVES THE EXISTING TOKEN IN PLACE. settings.Server is
// the whole of the token binding round 4 added — remote/http.go's
// deviceToken() hands the token to any base with the same origin as
// settings.Server — so the credential minted for the real hub is now bound to
// the attacker's origin and goes out with the very first sync.
//
// The secure behaviour: a device's credential is for the hub that issued it.
// Pointing init at a different server must never send that server the old
// hub's token.
func TestSec_Init_ServerSwitchNeverHandsTheOldHubsTokenToTheNewServer(t *testing.T) {
	e := secinitNewEnv(t, secinitHubOpts{})

	// This device is signed in to a real hub elsewhere.
	if err := os.MkdirAll(e.bhome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.bhome, "settings.json"),
		[]byte(`{"server":"https://hub.example.com","token":"REAL-HUB-TOKEN","email":"a@example.com"}`),
		0o600); err != nil {
		t.Fatal(err)
	}

	work := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	defer e.run(work, "stop", work)

	out, err := e.run(work, "init", "--name", "wiki", "--server", e.url, "--yes")
	if err != nil {
		t.Logf("init refused the server switch: %s", out)
	}
	// Whether or not init succeeded, the other hub's credential must not have
	// reached this server — not during init, and not on the sync that follows.
	e.run(work, "sync")
	t.Logf("Authorization headers this server received: %q", e.sentAuth())
	for _, a := range e.sentAuth() {
		if strings.Contains(a, "REAL-HUB-TOKEN") {
			t.Errorf("the previous hub's device token was sent to %s: %q\ninit output:\n%s", e.url, a, out)
		}
	}
	settings, _ := os.ReadFile(filepath.Join(e.bhome, "settings.json"))
	var s struct{ Server, Token string }
	json.Unmarshal(settings, &s)
	if strings.Contains(s.Server, "127.0.0.1") && s.Token == "REAL-HUB-TOKEN" {
		t.Errorf("init rebound the old hub's token to the new server: %s", settings)
	}
}

// ------------------------------------------------ 3. places init refuses

// $BDRIVE_HOME is where this device's hub credential lives (settings.json
// holds the bearer token `bdrive login` was issued), alongside every
// project's journals and cached blobs. Nothing in it is a project, and
// syncing it uploads the token to the hub and to every teammate's disk.
//
// The reserved-directory rule (.bdrive is never synced) is applied to path
// segments BELOW the mount root, so it cannot see a mount that IS the bdrive
// home: from there, "settings.json" is just a file at the top level.
func TestSec_Init_RefusesToMountTheBdriveHome(t *testing.T) {
	e := secinitNewEnv(t, secinitHubOpts{})

	// A real session first, so the home has a settings.json with a token in it.
	seed := filepath.Join(t.TempDir(), "seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	defer e.run(seed, "stop", seed)
	if out, err := e.run(seed, "init", "--name", "seed", "--server", e.url, "--yes"); err != nil {
		t.Fatalf("seed init: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(e.bhome, "settings.json"),
		[]byte(`{"server":"`+e.url+`","token":"SECRET-DEVICE-TOKEN"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	defer e.run(e.home, "stop", e.bhome)
	out, err := e.run(e.home, "init", e.bhome, "--name", "home", "--server", e.url, "--yes")
	if err != nil {
		t.Logf("refused: %s", out)
	} else {
		t.Errorf("init mounted $BDRIVE_HOME:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(e.bhome, ".bdrive", "config.json")); err == nil {
		t.Errorf("init initialized $BDRIVE_HOME as a project")
	}
	// The decisive assertion: whatever init decided, this device's hub
	// credential must not have been uploaded to the hub as project content.
	for key, body := range e.pushed() {
		if strings.Contains(string(body), "SECRET-DEVICE-TOKEN") {
			t.Errorf("the device token was pushed to the hub as object %s:\n%s", key, body)
		}
	}
}

// A mount inside another mount is two devices' worth of writers over one set
// of paths: the parent syncs the child's files to the parent's project and
// the child syncs them to its own. init is where the boundary is cheap to
// refuse; the syncer's nested-mount handling (rounds 5 and 6) exists because
// it was not refused here.
func TestSec_Init_RefusesAFolderInsideAnExistingMount(t *testing.T) {
	e := secinitNewEnv(t, secinitHubOpts{})

	parent := filepath.Join(t.TempDir(), "parent")
	child := filepath.Join(parent, "inner")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "secret.md"), []byte("inner content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer e.run(parent, "stop", parent)
	if out, err := e.run(parent, "init", "--name", "parent", "--server", e.url, "--yes"); err != nil {
		t.Fatalf("parent init: %v\n%s", err, out)
	}

	defer e.run(child, "stop", child)
	out, err := e.run(child, "init", "--name", "child", "--server", e.url, "--yes")
	if err == nil {
		t.Errorf("init created a mount inside the mount at %s:\n%s", parent, out)
	} else {
		t.Logf("refused: %s", out)
	}
	if _, err := os.Stat(filepath.Join(child, ".bdrive", "config.json")); err == nil {
		t.Errorf("a nested init still initialized %s", child)
	}
	// What the two projects ended up holding, for whoever fixes this.
	for key, body := range e.pushed() {
		if strings.Contains(key, "journal/") {
			t.Logf("journal %s: %s", key, body)
		}
	}
}

// Round 5 found that agenthooks.Install DELETED the hooks it had just written
// when $HOME is a git repository (the migration that moves project-level
// hooks out was reaching the user config through the git-root walk). The
// regression test for it calls Install directly. This one runs the command
// that calls Install — in the shape that broke it, from a $HOME that is a git
// repo — because init is where the hooks a machine actually gets come from,
// and it reports success either way.
func TestSec_Init_FromAGitRepoHomeStillLeavesTheUserHooksInPlace(t *testing.T) {
	for _, tc := range []struct{ name, gitAt string }{
		// The control: the same command, the same layout, no repo at $HOME.
		// It must pass, so a failure of the other arm is init's decision.
		{"plain home", ""},
		{"home is a dotfiles git repo", "."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := secinitNewEnv(t, secinitHubOpts{})
			dirs := []string{".claude"}
			if tc.gitAt != "" {
				dirs = append(dirs, filepath.Join(tc.gitAt, ".git"))
			}
			for _, dir := range dirs {
				if err := os.MkdirAll(filepath.Join(e.home, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			work := filepath.Join(e.home, "wiki")
			if err := os.MkdirAll(work, 0o755); err != nil {
				t.Fatal(err)
			}
			defer e.run(work, "stop", work)

			out, err := e.run(work, "init", "--name", "wiki", "--server", e.url, "--yes")
			if err != nil {
				t.Fatalf("init: %v\n%s", err, out)
			}
			if !strings.Contains(out, "hooks registered") {
				t.Fatalf("init did not claim to register hooks:\n%s", out)
			}
			settings := filepath.Join(e.home, ".claude", "settings.json")
			body, readErr := os.ReadFile(settings)
			if readErr != nil {
				t.Fatalf("init reported success but %s does not exist: %v\n%s", settings, readErr, out)
			}
			if !strings.Contains(string(body), "bdrive sync") {
				t.Fatalf("init said \"hooks registered\" and left the user config empty.\n"+
					"%s is now:\n%s\ninit said:\n%s", settings, body, out)
			}
		})
	}
}

// A folder reached through a symlink. init records the mount at the path it
// was given; nothing it writes may land outside the directory that path
// actually resolves to.
func TestSec_Init_ThroughASymlinkedFolderWritesOnlyInsideTheTarget(t *testing.T) {
	e := secinitNewEnv(t, secinitHubOpts{})

	target := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	defer e.run(link, "stop", link)

	out, err := e.run(link, "init", "--name", "wiki", "--server", e.url, "--yes")
	if err != nil {
		t.Logf("refused: %s", out)
		return
	}
	if _, err := os.Stat(filepath.Join(target, ".bdrive", "config.json")); err != nil {
		t.Fatalf("init through a symlink wrote its config somewhere other than the target: %v\n%s", err, out)
	}
	// The link itself must still be a link, not replaced by a directory.
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("init replaced the symlink at %s (err %v)", link, err)
	}
}

// ------------------------------------- 4. hostile folder path, end to end

// The folder path init is pointed at reaches the machine-wide agent hook
// guard (through the mount registry the guard greps) and the login item. Both
// were hardened in earlier rounds against a specific character — a newline in
// the guard (round 2), an ampersand in the plist (round 5) — but neither was
// ever driven by init, which is what actually writes them.
//
// The guard runs as `sh -c` on every tool call of every session on the
// machine. Outside a mount it must exit without spawning anything, whatever
// the registry holds.
func TestSec_Init_HostileFolderPathNeverExecutesThroughTheAgentHookGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh guard")
	}
	e := secinitNewEnv(t, secinitHubOpts{})

	base := t.TempDir()
	names := []string{
		"a$(touch " + filepath.Join(base, "PWNED-SUBST") + ")b",
		"a`touch " + filepath.Join(base, "PWNED-BQ") + "`b",
		`a"b'c`,
		"a&b;c|d",
		"a\nb",
		"-rf",
		"a\\b",
	}
	initialized := 0
	for i, name := range names {
		dir := filepath.Join(base, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Logf("cannot create %q on this filesystem: %v", name, err)
			continue
		}
		if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		defer e.run(base, "stop", dir)
		// A distinct project per folder: one project for all of them would be
		// refused by checkNotAlreadyMounted, and the folders after the first
		// would never reach the registry at all.
		// "--" so a leading-dash folder is an argument, not a flag.
		out, err := e.run(base, "init", "--name", fmt.Sprintf("p%d", i), "--server", e.url, "--yes", "--", dir)
		if err != nil {
			t.Logf("init %q refused (acceptable): %v\n%s", name, err, out)
			continue
		}
		initialized++
	}
	if initialized < 4 {
		t.Fatalf("only %d hostile folder names reached the registry; the attack never ran", initialized)
	}
	registry, err := os.ReadFile(filepath.Join(e.bhome, "mounts.json"))
	if err != nil {
		t.Fatalf("no mount registry to grep: %v", err)
	}
	t.Logf("registry the guard greps:\n%s", registry)

	settings, err := os.ReadFile(filepath.Join(e.home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("init registered no user-level hooks at all: %v", err)
	}
	var cfg struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string }
		}
	}
	if err := json.Unmarshal(settings, &cfg); err != nil {
		t.Fatalf("init wrote unparseable agent config: %v\n%s", err, settings)
	}

	// Run every registered hook command from a directory that is NOT a mount,
	// with a fake `bdrive` on PATH that records being spawned. Nothing may run.
	outside := t.TempDir()
	binDir := t.TempDir()
	marker := filepath.Join(base, "PWNED-SPAWN")
	if err := os.WriteFile(filepath.Join(binDir, "bdrive"),
		[]byte("#!/bin/sh\necho spawned >> "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	n := 0
	for event, groups := range cfg.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if !strings.Contains(h.Command, "bdrive") {
					continue
				}
				n++
				cmd := exec.Command("sh", "-c", h.Command)
				cmd.Dir = outside
				cmd.Env = append(e.env, "PATH="+binDir+":"+os.Getenv("PATH"))
				cmd.Stdin = strings.NewReader("{}")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Logf("%s hook exited %v: %s", event, err, out)
				}
			}
		}
	}
	if n == 0 {
		t.Fatalf("no bdrive hook commands found to exercise:\n%s", settings)
	}
	for _, m := range []string{"PWNED-SUBST", "PWNED-BQ", "PWNED-SPAWN"} {
		if _, err := os.Stat(filepath.Join(base, m)); err == nil {
			t.Errorf("the hook guard fired %s outside a mount", m)
		}
	}
}

// The login item runs `bdrive resume` at every login, before any of the
// guards above exist in the session. init is what writes it. It must be a
// file the service manager can actually parse, whatever else happened on this
// machine — a plist that does not parse is silently never loaded, which is
// how sync stops coming back after a reboot with init reporting success.
func TestSec_Init_LoginItemStaysParseableAfterAHostileInit(t *testing.T) {
	e := secinitNewEnv(t, secinitHubOpts{name: secinitHostileName})

	work := filepath.Join(t.TempDir(), "an & odd \"name\"")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	defer e.run(work, "stop", work)
	if out, err := e.run(work, "init", "--name", "p", "--server", e.url, "--yes"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	switch runtime.GOOS {
	case "darwin":
		plist := filepath.Join(e.home, "Library", "LaunchAgents", "ai.beardrive.daemon.plist")
		body, err := os.ReadFile(plist)
		if err != nil {
			t.Fatalf("init registered no login item: %v", err)
		}
		if out, err := exec.Command("plutil", "-lint", plist).CombinedOutput(); err != nil {
			t.Fatalf("init wrote an unparseable login item: %v\n%s\n%s", err, out, body)
		}
	case "linux":
		unit := filepath.Join(e.home, ".config", "systemd", "user", "beardrive.service")
		body, err := os.ReadFile(unit)
		if err != nil {
			t.Skipf("no login unit on this box (systemd not booted?): %v", err)
		}
		for _, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(line, "ExecStart=") && strings.Count(line, "resume") != 1 {
				t.Fatalf("login unit ExecStart is not one resume invocation: %q", line)
			}
		}
	default:
		t.Skip("no login item on this platform")
	}
}

// ------------------------------------------------- 5. a failed init's leftovers

// The folder cannot be written. init must fail before it changes anything
// about this MACHINE: the agent hooks it registers run on every tool call of
// every session, and the login item runs at every login — neither belongs on
// a box where the init that asked for them did not complete.
func TestSec_Init_AnUnwritableFolderLeavesNoMachineWideState(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode bits")
	}
	e := secinitNewEnv(t, secinitHubOpts{})

	work := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(work, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(work, 0o755) })
	defer e.run(work, "stop", work)

	out, err := e.run(work, "init", "--name", "wiki", "--server", e.url, "--yes")
	if err == nil {
		t.Fatalf("init reported success on an unwritable folder:\n%s", out)
	}
	for _, leftover := range []string{
		filepath.Join(e.home, ".claude", "settings.json"),
		filepath.Join(e.home, "Library", "LaunchAgents", "ai.beardrive.daemon.plist"),
		filepath.Join(e.home, ".config", "systemd", "user", "beardrive.service"),
	} {
		if _, err := os.Stat(leftover); err == nil {
			t.Errorf("a failed init still registered %s machine-wide", leftover)
		}
	}
	if _, err := os.Stat(filepath.Join(e.bhome, "mounts.json")); err == nil {
		body, _ := os.ReadFile(filepath.Join(e.bhome, "mounts.json"))
		if strings.Contains(string(body), work) {
			t.Errorf("a failed init still enrolled the folder in the registry:\n%s", body)
		}
	}
}

// A hub that answers every store request with 500 is the reachable shape of
// "the first cycle did not work". init must not print the closing block that
// promises the folder is being kept in sync while every cycle fails.
func TestSec_Init_AHubThatRefusesTheStoreIsNotReportedAsSyncing(t *testing.T) {
	e := secinitNewEnv(t, secinitHubOpts{storeFail: http.StatusInternalServerError})

	work := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	defer e.run(work, "stop", work)

	out, err := e.run(work, "init", "--name", "wiki", "--server", e.url, "--yes")
	if err != nil {
		return
	}
	if !strings.Contains(out, "offline") && !strings.Contains(out, "no access") {
		t.Errorf("init hid the first cycle's failure:\n%s", out)
	}
}

// Project creation fails outright: nothing about this machine may change.
func TestSec_Init_AFailedProjectCreationRegistersNoHooksOrLoginItem(t *testing.T) {
	e := secinitNewEnv(t, secinitHubOpts{createFail: http.StatusInternalServerError})

	work := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(filepath.Join(work, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	defer e.run(work, "stop", work)

	out, err := e.run(work, "init", "--name", "wiki", "--server", e.url, "--yes")
	if err == nil {
		t.Fatalf("init succeeded against a hub that refuses to create projects:\n%s", out)
	}
	for _, leftover := range []string{
		filepath.Join(work, ".bdrive", "config.json"),
		filepath.Join(work, ".bdriveignore"),
		filepath.Join(e.home, ".claude", "settings.json"),
		filepath.Join(e.home, "Library", "LaunchAgents", "ai.beardrive.daemon.plist"),
	} {
		if _, err := os.Stat(leftover); err == nil {
			t.Errorf("a failed init still wrote %s", leftover)
		}
	}
}

// ------------------------------------------------------------- 6. --only

// --only writes a managed block of negation rules into .bdriveignore, which
// SYNCS: a rule that lands outside the block's markers can never be removed
// by `bdrive scope rm` and applies to the whole team forever (round 6's
// finding, on the newline). The rest of the value's shape is attacked here.
func TestSec_Init_HostileOnlyValuesNeverEscapeTheManagedScopeBlock(t *testing.T) {
	e := secinitNewEnv(t, secinitHubOpts{})

	for _, only := range []string{
		"../escape",
		"..",
		".",
		"/etc",
		"a\nb",
		"a\rb",
		"wiki\x7fdocs",
		"wiki\u009bdocs",
		"a/../../b",
		"docs,,wiki",
		"   ",
	} {
		t.Run(fmt.Sprintf("%q", only), func(t *testing.T) {
			work := filepath.Join(t.TempDir(), "proj")
			if err := os.MkdirAll(work, 0o755); err != nil {
				t.Fatal(err)
			}
			defer e.run(work, "stop", work)

			out, err := e.run(work, "init", "--name", "wiki", "--server", e.url, "--yes", "--only", only)
			if err != nil {
				return // refused before anything was written: the secure answer
			}
			data, readErr := os.ReadFile(filepath.Join(work, ".bdriveignore"))
			if readErr != nil {
				t.Fatalf("init succeeded but wrote no .bdriveignore: %v\n%s", readErr, out)
			}
			body := string(data)
			// Whatever survived must be entirely inside one well-formed block,
			// and must not have escaped the project.
			start := strings.Index(body, scopeStart)
			end := strings.Index(body, scopeEnd)
			if start < 0 || end < start {
				t.Fatalf("--only %q produced no well-formed managed block:\n%s", only, body)
			}
			for _, line := range strings.Split(body[start:end], "\n") {
				if strings.HasPrefix(line, "!") && strings.Contains(line, "..") {
					t.Errorf("--only %q wrote an escaping rule %q", only, line)
				}
			}
			// Nothing may have been created outside the project folder.
			if _, err := os.Stat(filepath.Join(filepath.Dir(work), "escape")); err == nil {
				t.Errorf("--only %q created a directory outside the mount", only)
			}
		})
	}
}
