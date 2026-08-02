package main

// Round 7 — the CLI commands no TestSec_* has ever driven. Round 6 closed
// `export`, `scope` and `status`; the coverage note names eleven that were
// still untouched: login, logout, stop, forget, share, import, resume,
// autostart, read-log, whoami, daemon.
//
// These attack four of them plus one primitive:
//
//   - `bdrive forget` — the per-path tool that edits the SYNCED .bdriveignore
//     and then runs a prune cycle. Round 6 proved the scope block is a
//     team-wide delete lever and fixed cleanScopeDirs; forget writes into the
//     same file through a different function that was not fixed.
//   - `bdrive resume` — validates the FOLDER's mount id and then builds a path
//     from the REGISTRY KEY, which nothing validates.
//   - `bdrive login` / `bdrive whoami` / `bdrive status` — every string the hub
//     hands back at sign-in reaches a terminal with a bare %s.
//
// Helpers are prefixed sec7.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/syncer"
)

// sec7ControlRunes reports the control characters in s that must never reach a
// terminal: C0, DEL, C1 (U+009B *is* CSI on any xterm-lineage terminal) and
// the bidi format controls that reorder a rendered row (CVE-2021-42574). This
// is the same set round 6 settled on for `bdrive log` and `bdrive status`.
func sec7ControlRunes(s string) []rune {
	var out []rune
	for _, r := range s {
		switch {
		case r == '\n', r == '\t':
			// legitimate row separators in CLI output
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f,
			r == 0x061c, r == 0x200e, r == 0x200f,
			r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			out = append(out, r)
		}
	}
	return out
}

// sec7Hostile is one hub-chosen string carrying an attack a terminal executes.
var sec7Hostile = []string{
	"ada\x1b]0;pwned\x07",            // rewrite the window title
	"ada\x1b]52;c;cHduZWQ=\x07",      // OSC 52: write the operator's clipboard
	"ada\x1b[2K\rsigned in as root",  // repaint the line as something else
	"ada\u009b2K\rsigned in as root", // the same via C1 CSI, no ESC anywhere
	"ada\u202egnp.eciovni\u202c",     // bidi override: the row reads backwards
	"ada\x7f\x7f\x7f\x7f",            // DEL
}

// ---------------------------------------------------------------------------
// bdrive forget
// ---------------------------------------------------------------------------

// TestSec_Forget_APathCannotInjectExtraIgnoreRules
//
// `bdrive forget <path>` turns its argument into a .bdriveignore line:
//
//	rel, err := filepath.Rel(root, abs)                 // forget.go:102
//	...
//	body += rule + "\n"                                 // forget.go:137
//	os.WriteFile(path, []byte(body), 0o644)
//
// and then runs a cycle with Prune set, which removes from the hub — for every
// teammate — everything the resulting rules now exclude.
//
// ignoreRule checks the argument for `..` and for being .bdriveignore itself.
// It does not check it for NEWLINES, and a newline is a legal byte in a unix
// path (and in an argument an agent was told to pass). One argument therefore
// writes as many rules as it likes. `*` is the whole project, so
//
//	bdrive forget $'notes\n*'
//
// adds "ignore everything" to the file that syncs to every device, and the
// prune that runs in the same command then strips the project from the hub for
// the entire team. Nothing in the CLI can take the rule back out again — the
// managed-block remover only knows about the scope block, and `bdrive forget`
// only ever appends.
//
// This is verbatim the hole round 6 closed for the OTHER writer of this file
// (TestSec_CLI_ScopeRuleCannotOutliveTheScopeThatWroteIt → cleanScopeDirs).
// forget was not part of that fix.
//
// The secure behavior asserted: one argument adds at most one rule.
func TestSec_Forget_APathCannotInjectExtraIgnoreRules(t *testing.T) {
	// Control: an ordinary path adds exactly one line, so the harness is
	// known to reach the writer at all.
	t.Run("control_ordinary_path", func(t *testing.T) {
		root := t.TempDir()
		got := sec7ForgetLines(t, root, "notes")
		if len(got) != 1 || got[0] != "notes" {
			t.Fatalf("control: forgetting %q wrote %q, want exactly [notes]", "notes", got)
		}
	})

	for _, arg := range []string{
		"notes\n*",
		"notes\n*/",
		"notes\r\n!/",
		"notes\n\n*",
	} {
		t.Run(strings.NewReplacer("\n", "\\n", "\r", "\\r").Replace(arg), func(t *testing.T) {
			root := t.TempDir()
			got := sec7ForgetLines(t, root, arg)
			if len(got) > 1 {
				t.Errorf("one `bdrive forget %q` wrote %d rules into the synced .bdriveignore: %q\n"+
					"the extra rules apply to every teammate's device, and the prune cycle forget "+
					"runs in the same command removes everything they exclude from the hub",
					arg, len(got), got)
			}
		})
	}
}

// sec7ForgetLines runs the two functions `bdrive forget` uses to turn an
// argument into .bdriveignore content, and returns the non-empty lines the
// file gained. An argument the door refuses returns no lines, which is the
// secure outcome.
func sec7ForgetLines(t *testing.T, root, arg string) []string {
	t.Helper()
	t.Chdir(root)
	rule, err := ignoreRule(root, arg)
	if err != nil {
		return nil // refused at the door
	}
	if _, err := appendIgnoreRules(root, []string{rule}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, syncer.IgnoreFile))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimRight(line, "\r"); strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// bdrive resume
// ---------------------------------------------------------------------------

// TestSec_Resume_ARegistryKeyCannotEscapeTheBdriveHome
//
// `bdrive resume` — the command the login agent runs at every boot — walks the
// mount registry and builds a volume path out of the map KEY:
//
//	for id, mi := range mounts {                       // resume.go:45
//	    if _, ok, err := config.LoadProject(mi.Path); ... // validates the FOLDER
//	    vdir, err := config.VolumeDir(id)              // resume.go:58 — the KEY
//	    ...
//	    daemon.Start(mi.Path, vdir, ...)               // creates <vdir>/daemon.log
//
// Round 4 established that a mount id is untrusted and validated it where it is
// read — but only in LoadProject (config/project.go:134). VolumeDir itself is
// still a bare filepath.Join onto $BDRIVE_HOME, and LoadMounts (config.go:113)
// unmarshals mounts.json into a map without looking at a single key. So the
// folder's id is checked and the registry's id is not, and resume uses the
// registry's.
//
// Round 6 closed exactly this shape one layer over: Store.LoadCache handed back
// whatever key was in state-<mount>.json and both delete passes joined those
// keys onto the working folder (TestSec_Store_CacheKeysCannotNameAPathOutsideTheVolume).
// mounts.json is the same kind of file — plain JSON in $BDRIVE_HOME, written by
// anything running as the user (an agent session, a dependency's install
// script, an older bdrive) — and its keys reach os.OpenFile.
//
// The secure behavior asserted: nothing `bdrive resume` writes lands outside
// $BDRIVE_HOME, whatever the registry says.
func TestSec_Resume_ARegistryKeyCannotEscapeTheBdriveHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BDRIVE_HOME", home)

	// A real, valid project folder — so resume gets past its own
	// LoadProject(mi.Path) gate and reaches VolumeDir.
	folder := t.TempDir()
	if _, err := config.SaveProject(folder, config.Project{Volume: "wiki"}); err != nil {
		t.Fatal(err)
	}

	// The escape target: an existing directory outside $BDRIVE_HOME, named by
	// the registry key alone.
	outside := t.TempDir()
	rel, err := filepath.Rel(filepath.Join(home, "volumes"), outside)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.SaveMounts(map[string]config.MountInfo{
		filepath.ToSlash(rel): {Path: folder, Volume: "wiki"},
	}); err != nil {
		t.Fatal(err)
	}

	// The primitive, first: this is the line resume runs.
	if vdir, err := config.VolumeDir(filepath.ToSlash(rel)); err == nil {
		if r, rerr := filepath.Rel(home, vdir); rerr != nil || r == ".." ||
			strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			t.Errorf("VolumeDir(<registry key>) = %q, outside $BDRIVE_HOME %q — "+
				"LoadMounts validates no key and VolumeDir validates no id", vdir, home)
		}
	}

	// And the reachable caller: resume must not create anything out there.
	before := sec7Entries(t, outside)
	out, err := seccliRun(t, resumeCmd(), []string{"--quiet"})
	t.Logf("resume: err=%v out=%q", err, out)
	for name := range sec7Entries(t, outside) {
		if !before[name] {
			t.Errorf("`bdrive resume` created %q in %q — outside $BDRIVE_HOME, at a path "+
				"chosen by a mounts.json key", name, outside)
		}
	}
}

func sec7Entries(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range ents {
		out[e.Name()] = true
	}
	return out
}

// ---------------------------------------------------------------------------
// bdrive login / whoami / status
// ---------------------------------------------------------------------------

// TestSec_Login_HubChosenAccountStringsAreNotRenderedToTheTerminal
//
// Scoreboard row 21 is "every peer-controlled string that reaches a terminal".
// Round 5 closed it for `bdrive log` and `bdrive restore --list`; round 6 added
// `bdrive status`, but only for the two fields its fixture set — the project
// name and the remote URL (cmds.go:213, 215, both now safeField).
//
// The account line above them was left with a bare %s, and so was every other
// place the sign-in flow prints what the hub said:
//
//	fmt.Printf("device: %s (%s) signed in as %s\n\n", ...)        // cmds.go:191
//	fmt.Printf("account:     %s (from `bdrive login`...)\n", who) // main.go:110
//	fmt.Printf("signed in as %s <%s>\n", u.Name, u.Email)         // login.go:65
//	fmt.Printf("logged in to %s as %s <%s>\n", ...)               // login.go:198
//	fmt.Printf("to finish signing in, open this link...%s\n", ...)// login.go:362
//
// `u.Name` and `u.Email` are JSON fields off /api/auth/me and off the device
// poll; `verify_url` is a string the hub composes. Scoreboard row 19 is the
// device as client of a HOSTILE hub — `bdrive login <url>` is the one command
// whose whole job is to point this device at a hub it has never seen, on a URL
// a user was given. Everything that hub answers with is rendered raw.
//
// The secure behavior asserted is the one round 5 and round 6 already chose for
// the other CLI surfaces: no control character reaches the terminal.
func TestSec_Login_HubChosenAccountStringsAreNotRenderedToTheTerminal(t *testing.T) {
	// Control: an ordinary account name is printed by all three commands, so
	// the assertions below are looking at output they actually produced.
	t.Run("control_ordinary_name", func(t *testing.T) {
		hub := sec7Hub(t, "Ada Lovelace")
		sec7Session(t, hub.URL, "Ada Lovelace")
		for name, cmd := range sec7AccountCmds() {
			out, err := seccliRun(t, cmd(), sec7AccountArgs()[name])
			if err != nil {
				t.Fatalf("%s: %v (%s)", name, err, out)
			}
			if !strings.Contains(out, "Ada Lovelace") {
				t.Fatalf("control: %s did not print the account name:\n%s", name, out)
			}
		}
	})

	for _, hostile := range sec7Hostile {
		t.Run(sec7Label(hostile), func(t *testing.T) {
			hub := sec7Hub(t, hostile)
			sec7Session(t, hub.URL, hostile)
			for name, cmd := range sec7AccountCmds() {
				out, err := seccliRun(t, cmd(), sec7AccountArgs()[name])
				if err != nil {
					t.Logf("%s returned %v", name, err)
				}
				if bad := sec7ControlRunes(out); len(bad) > 0 {
					t.Errorf("`bdrive %s` printed control characters %q that the hub chose:\n%q",
						name, bad, out)
				}
			}
		})
	}

	// The sign-in flow itself: `bdrive login <url>` against a hub this device
	// has never seen. Without a TTY the CLI takes the device-code path, which
	// prints the hub's verify_url and then the hub's account name.
	for _, hostile := range sec7Hostile {
		t.Run("login/"+sec7Label(hostile), func(t *testing.T) {
			hub := sec7Hub(t, hostile)
			t.Setenv("BDRIVE_HOME", t.TempDir())
			out, err := seccliRun(t, loginCmd(), []string{hub.URL})
			if err != nil {
				t.Logf("login returned %v", err)
			}
			if !strings.Contains(out, "finish signing in") {
				t.Fatalf("control: login did not reach the device-code print:\n%s", out)
			}
			if bad := sec7ControlRunes(out); len(bad) > 0 {
				t.Errorf("`bdrive login <hostile-hub>` printed control characters %q the hub chose:\n%q",
					bad, out)
			}
		})
	}
}

// sec7AccountCmds are the commands that render the signed-in account.
func sec7AccountCmds() map[string]func() *cobra.Command {
	return map[string]func() *cobra.Command{
		"whoami":         whoamiCmd,
		"status":         statusCmd,
		"login --status": loginCmd,
	}
}

func sec7AccountArgs() map[string][]string {
	return map[string][]string{
		"whoami":         nil,
		"status":         nil,
		"login --status": {"--status"},
	}
}

// sec7Session writes the settings `bdrive login` would have written after
// signing in to hub, with the hub's chosen display name.
func sec7Session(t *testing.T, server, name string) {
	t.Helper()
	t.Setenv("BDRIVE_HOME", t.TempDir())
	if err := config.SaveSettings(config.Settings{
		Server: server, Token: "device-token", Email: "ada@example.com", Name: name,
	}); err != nil {
		t.Fatal(err)
	}
	// status with no folder argument needs at least one mount, or it returns
	// before printing the account line.
	folder := t.TempDir()
	if _, err := config.SaveProject(folder, config.Project{Volume: "wiki"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(folder); err != nil {
		t.Fatal(err)
	}
}

// sec7Hub is a bdrive hub that answers every sign-in route with a display name
// of its choosing — the hostile-hub half of row 19.
func sec7Hub(t *testing.T, name string) *httptest.Server {
	t.Helper()
	user := map[string]string{"id": "u-1", "email": "ada@example.com", "name": name}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"mode": "hub",
			"auth": map[string]any{"enabled": true, "cli_login": "/auth/cli"},
		})
	})
	mux.HandleFunc("/api/auth/me", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(user)
	})
	mux.HandleFunc("/api/auth/device/start", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"code":       "abcd1234",
			"verify_url": "https://hub.example.com/auth/device/" + name,
			"interval":   1,
		})
	})
	mux.HandleFunc("/api/auth/device/poll", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"token": "device-token", "user": user})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func sec7Label(s string) string {
	return strings.Map(func(r rune) rune {
		if len(sec7ControlRunes(string(r))) > 0 {
			return '.'
		}
		return r
	}, s)
}

// ---------------------------------------------------------------------------
// bdrive share
// ---------------------------------------------------------------------------

// TestSec_Share_TheFolderConfigCannotRedirectTheDeviceToken
//
// Round 4's client critical was TestSec_Config_FolderConfigCannotRedirectTheDeviceToken:
// a folder's .bdrive/config.json chose where this device's hub token was sent,
// plaintext http included, because the remote URL travels with the folder (a
// zip, a clone, a colleague's copy). The fix bound the credential to
// settings.Server's ORIGIN — in remote.deviceToken (remote/http.go:69), which
// is the SYNC backend's door.
//
// `bdrive share` does not go through it. All three of its calls read the
// destination out of the same untrusted file and attach the raw token:
//
//	server, projectID, err := splitHubRemote(proj.Remote)   // share.go:67
//	serverDo(POST, server+"/api/p/"+projectID+"/shares", settings.Token, data)
//	serverDo(GET,  server+"/api/p/"+projectID+"/shares", settings.Token, nil)   // --list
//	serverDo(DELETE, server+"/api/shares/"+token,        settings.Token, nil)   // --revoke
//
// splitHubRemote checks the SHAPE of the URL (https?://host/p/<id>) and
// nothing else, so any host at all satisfies it. Hand someone a folder — the
// documented way to move a project between machines — and the first
// `bdrive share` in it posts their device token, in an Authorization header,
// to a server of your choosing.
//
// The secure behavior asserted is round 4's: the credential goes to the origin
// it was issued for, and nowhere else.
func TestSec_Share_TheFolderConfigCannotRedirectTheDeviceToken(t *testing.T) {
	const token = "SECRET-DEVICE-TOKEN"

	// Control: when the folder's remote IS the server this device signed in
	// to, the token is sent — so the assertions below are looking at a request
	// the command really makes.
	t.Run("control_same_origin", func(t *testing.T) {
		hub := sec7Recorder(t)
		folder := sec7FolderFor(t, hub.URL, hub.URL, token)
		t.Chdir(folder)
		if _, err := seccliRun(t, shareCmd(), []string{"notes.md"}); err != nil {
			t.Fatalf("share: %v", err)
		}
		if !sec7SawToken(hub, token) {
			t.Fatalf("control: the device token never reached its own hub; " +
				"the harness is not exercising the request")
		}
	})

	for _, args := range [][]string{{"notes.md"}, {"--list"}, {"--revoke", "abc123"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			hostile := sec7Recorder(t)
			// Signed in to a different hub entirely; the folder names the
			// attacker's.
			folder := sec7FolderFor(t, hostile.URL, "https://hub.example.com", token)
			t.Chdir(folder)
			out, err := seccliRun(t, shareCmd(), args)
			t.Logf("share %v: err=%v out=%q", args, err, out)
			if sec7SawToken(hostile, token) {
				t.Errorf("`bdrive share %v` sent this device's token to %s — an origin chosen "+
					"by the folder's .bdrive/config.json, not by `bdrive login`", args, hostile.URL)
			}
		})
	}
}

// TestSec_CLI_TheDeviceTokenIsNotFollowedToAnotherOrigin
//
// Round 4 closed this on the sync client: net/http strips Authorization only
// when the HOSTNAME changes, so a hub's 302 to another port, an https→http
// downgrade, or a sibling subdomain kept the bearer token (round 5 hardened it
// further to refusing the redirect outright — remote/http.go's
// refuseOffOriginRedirect, installed on that backend's http.Client).
//
// The CLI has its own client and it got neither fix:
//
//	var initClient = &http.Client{Timeout: 10 * time.Second}   // init.go:584
//
// No CheckRedirect at all. Every command that talks to the hub's JSON API —
// share, share --list, share --revoke, init's create/get/list project, import —
// goes through serverDo on this client, with `Authorization: Bearer <device
// token>` set. One 302 from the hub (or from anything that can answer for it)
// hands the token to a different origin.
//
// The secure behavior asserted: the token reaches the origin it was sent to,
// and no other.
func TestSec_CLI_TheDeviceTokenIsNotFollowedToAnotherOrigin(t *testing.T) {
	const token = "SECRET-DEVICE-TOKEN"
	elsewhere := sec7Recorder(t)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Same hostname, different port: net/http keeps Authorization.
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(hub.Close)

	folder := sec7FolderFor(t, hub.URL, hub.URL, token)
	t.Chdir(folder)
	out, err := seccliRun(t, shareCmd(), []string{"--list"})
	t.Logf("share --list: err=%v out=%q", err, out)
	if len(sec7Seen(elsewhere)) == 0 {
		t.Fatalf("control: the redirect target was never reached; the test proves nothing")
	}
	if sec7SawToken(elsewhere, token) {
		t.Errorf("a hub 302 carried this device's token to %s: initClient has no CheckRedirect, "+
			"and net/http only strips Authorization when the HOSTNAME changes", elsewhere.URL)
	}
}

// sec7Recorder is a stand-in hub that records the Authorization header of
// every request and answers each CLI call with a well-formed body.
func sec7Recorder(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/shares") && r.Method == http.MethodGet {
			fmt.Fprint(w, `{"shares":[]}`)
			return
		}
		fmt.Fprint(w, `{"url":"https://hub.example.com/s/abc","expires":"0001-01-01T00:00:00Z"}`)
	}))
	t.Cleanup(srv.Close)
	sec7Recorded.Store(srv, &sec7Log{mu: &mu, seen: &seen})
	return srv
}

type sec7Log struct {
	mu   *sync.Mutex
	seen *[]string
}

var sec7Recorded sync.Map

func sec7Seen(srv *httptest.Server) []string {
	v, ok := sec7Recorded.Load(srv)
	if !ok {
		return nil
	}
	l := v.(*sec7Log)
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), *l.seen...)
}

func sec7SawToken(srv *httptest.Server, token string) bool {
	for _, h := range sec7Seen(srv) {
		if strings.Contains(h, token) {
			return true
		}
	}
	return false
}

// sec7FolderFor builds an enrolled project folder whose .bdrive/config.json
// points at remoteServer, on a device signed in to loginServer.
func sec7FolderFor(t *testing.T, remoteServer, loginServer, token string) string {
	t.Helper()
	t.Setenv("BDRIVE_HOME", t.TempDir())
	if err := config.SaveSettings(config.Settings{
		Server: loginServer, Token: token, Email: "ada@example.com", Name: "Ada",
	}); err != nil {
		t.Fatal(err)
	}
	folder := t.TempDir()
	if _, err := config.SaveProject(folder, config.Project{
		Volume: "wiki", Remote: remoteServer + "/p/proj1234",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(folder); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "notes.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return folder
}
