package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
)

// Round 9 — the coverage audit of scoreboard row 21.
//
// Row 21 is "every peer- or hub-controlled string that reaches a terminal", and
// it is enforced by one function (safeField) applied at fourteen call sites
// that rounds 5–8 added one at a time. Removing safeField from each site in
// turn and re-running the whole TestSec suite showed twelve of the fourteen are
// asserted somewhere and two are not:
//
//	cmds.go:216   `bdrive status`   "  remote:   %s"        (mi.Remote)
//	login.go:411  `bdrive login`    "and approve code: %s"  (start.Code)
//
// Both print a string the local trust boundary does not own — the remote URL
// comes out of .bdrive/config.json, which travels with a folder (rounds 4, 5
// and 7 all treat that file as untrusted), and the device code comes from
// whatever hub the URL names.
//
// Helper prefix: secaud4. seccliRun and sec7ControlRunes are the existing
// harness.

// secaud4Mount enrolls a folder whose .bdrive/config.json carries the given
// volume and remote — the shape a folder that arrived from somewhere else has.
func secaud4Mount(t *testing.T, volume, remote string) string {
	t.Helper()
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder := t.TempDir()
	if _, err := config.SaveProject(folder, config.Project{Volume: volume, Remote: remote}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(folder); err != nil {
		t.Fatal(err)
	}
	return folder
}

// TestSec_CLI_StatusDoesNotRenderTheFoldersRemoteURLToTheTerminal
//
// Round 5 closed the project-name half of this line pair; the remote URL
// printed two lines below it is the same trust level and the same terminal.
// .bdrive/config.json is untrusted input by this repo's own precedent — round
// 4 unbound the device token from Project.Remote, round 5 refused a mid-run
// remote swap in the daemon, round 7 refused a copied folder's registry row —
// and `bdrive status` renders that exact field with a bare %s.
//
// A folder is the documented way to move a project between machines (a zip, a
// clone, a colleague's copy), so the string is chosen by whoever handed you the
// folder. A terminal executes what it is handed: the row can be repainted, the
// scrollback cleared, the window title set, the clipboard written (OSC 52).
//
// The secure behaviour is the one round 5 chose for every other row: no C0,
// DEL, C1 or bidi-override character reaches the terminal.
func TestSec_CLI_StatusDoesNotRenderTheFoldersRemoteURLToTheTerminal(t *testing.T) {
	// Control: an ordinary remote is printed, so the assertions below are
	// looking at output this command actually produced.
	t.Run("control_ordinary_remote", func(t *testing.T) {
		folder := secaud4Mount(t, "wiki", "file:///tmp/store")
		out, err := seccliRun(t, statusCmd(), []string{folder})
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if !strings.Contains(out, "file:///tmp/store") {
			t.Fatalf("control: status did not print the remote:\n%s", out)
		}
	})

	for _, remote := range []string{
		"file:///tmp/store\x1b[2K\r  remote:   https://hub.trusted.test/p/p-00000000",
		"file:///tmp/store\x1b]0;pwned\x07",
		"file:///tmp/store\x1b]52;c;cHduZWQ=\x07",
		"file:///tmp/store\x7f\x7f\x7f\x7f",
		"file:///tmp/store52;c;cHduZWQ=", // 8-bit OSC, no ESC byte
		"file:///tmp/store‮gnp.evil/p/p-1", // bidi override
	} {
		t.Run(sec7Label(remote), func(t *testing.T) {
			folder := secaud4Mount(t, "wiki", remote)
			out, err := seccliRun(t, statusCmd(), []string{folder})
			if err != nil {
				t.Logf("status returned %v", err)
			}
			if !strings.Contains(out, "remote:") {
				t.Fatalf("control: status did not reach the remote line:\n%q", out)
			}
			if bad := sec7ControlRunes(out); len(bad) > 0 {
				t.Errorf("`bdrive status` printed control characters %q that came out of "+
					".bdrive/config.json's remote field:\n%q", bad, out)
			}
		})
	}
}

// TestSec_Login_TheDeviceCodeFromAnOlderHubIsNotRenderedRawToTheTerminal
//
// deviceCodeLogin has two print branches. Every existing test takes the
// verify_url one, because the fixture hub always answers with a verify_url.
// The other branch — kept for pre-0.13 hubs, which hand back a short code to
// type into /auth/device — prints the hub's `code` string, and `bdrive login
// <url>` is by definition run against a server this device has never talked to
// before. There is no account, no token and no trust at that point: the hub is
// simply whatever answered the URL.
//
// The secure behaviour: the same as every other hub-chosen string in this
// command — it reaches the terminal as text, not as escape sequences.
func TestSec_Login_TheDeviceCodeFromAnOlderHubIsNotRenderedRawToTheTerminal(t *testing.T) {
	// Control: a benign code takes this branch and is printed.
	t.Run("control_ordinary_code", func(t *testing.T) {
		hub := secaud4OldHub(t, "ABCD-1234")
		t.Setenv("BDRIVE_HOME", t.TempDir())
		out, err := seccliRun(t, loginCmd(), []string{hub.URL})
		if err != nil {
			t.Fatalf("login: %v (%s)", err, out)
		}
		if !strings.Contains(out, "approve code: ABCD-1234") {
			t.Fatalf("control: login did not take the device-code branch:\n%q", out)
		}
	})

	for _, code := range []string{
		"ABCD\x1b[2K\rand approve code: 0000-0000",
		"ABCD\x1b]0;pwned\x07",
		"ABCD\x1b]52;c;cHduZWQ=\x07",
		"ABCD52;c;cHduZWQ=",
	} {
		t.Run(sec7Label(code), func(t *testing.T) {
			hub := secaud4OldHub(t, code)
			t.Setenv("BDRIVE_HOME", t.TempDir())
			out, err := seccliRun(t, loginCmd(), []string{hub.URL})
			if err != nil {
				t.Logf("login returned %v", err)
			}
			if !strings.Contains(out, "approve code:") {
				t.Fatalf("control: login did not reach the device-code print:\n%q", out)
			}
			if bad := sec7ControlRunes(out); len(bad) > 0 {
				t.Errorf("`bdrive login <hub>` printed control characters %q that the hub "+
					"chose as its device code:\n%q", bad, out)
			}
		})
	}
}

// secaud4OldHub is a hub of the pre-0.13 shape: its device-start answer has no
// verify_url, so the CLI prints the code instead of a link.
func secaud4OldHub(t *testing.T, code string) *httptest.Server {
	t.Helper()
	user := map[string]string{"id": "u-1", "email": "ada@example.com", "name": "Ada Lovelace"}
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
		json.NewEncoder(w).Encode(map[string]any{"code": code, "interval": 1})
	})
	mux.HandleFunc("/api/auth/device/poll", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"token": "device-token", "user": user})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}
