package config_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/remote"
)

// Security tests for the client's own state. Two files meet here with very
// different trust levels: $BDRIVE_HOME/settings.json, which holds the device
// token, and <folder>/.bdrive/config.json, which does NOT — it travels with
// the folder (a zip, a clone, a colleague's copy, an import) and is read
// verbatim on every command run in that folder.
//
// This file is package config_test so it can drive remote.Open, which imports
// config.

// secpkgHome points $BDRIVE_HOME at a fresh dir for the duration of a test.
func secpkgHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("BDRIVE_HOME", home)
	t.Setenv("BDRIVE_TOKEN", "")
	return home
}

// secpkgFolder writes a folder carrying the given .bdrive/config.json.
func secpkgFolder(t *testing.T, p config.Project) string {
	t.Helper()
	folder := t.TempDir()
	dir := filepath.Join(folder, config.ProjectDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return folder
}

// TestSec_Config_FolderConfigCannotRedirectTheDeviceToken: Project.Remote is
// read verbatim from a folder's .bdrive/config.json and handed to remote.Open,
// which attaches this device's bearer token to every request against whatever
// host the string names. A folder someone shares with you therefore chooses
// where your hub credential is sent — and may downgrade it to plaintext http.
func TestSec_Config_FolderConfigCannotRedirectTheDeviceToken(t *testing.T) {
	secpkgHome(t)

	seen := make(chan string, 8)
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case seen <- r.Header.Get("Authorization"):
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"objects":[]}`))
	}))
	defer attacker.Close()

	const token = "bdrive-device-token-ISSUED-FOR-THE-REAL-HUB"
	if err := config.SaveSettings(config.Settings{
		Server: "https://hub.acme.example",
		Token:  token,
		Email:  "alice@acme.example",
	}); err != nil {
		t.Fatal(err)
	}

	folder := secpkgFolder(t, config.Project{
		ID:     "m-1234abcd",
		Volume: "wiki",
		Remote: attacker.URL + "/p/p-00000000", // NOT settings.Server
	})

	p, ok, err := config.LoadProject(folder)
	if err != nil || !ok {
		t.Fatalf("LoadProject: %v ok=%v", err, ok)
	}
	be, err := remote.Open(context.Background(), p.Remote)
	if err != nil {
		return // refusing a remote that isn't the logged-in hub is the secure outcome
	}
	defer be.Close()
	if _, err := be.List(context.Background(), "journal/"); err != nil {
		t.Fatalf("List against the planted remote: %v", err)
	}
	select {
	case got := <-seen:
		if strings.Contains(got, token) {
			t.Errorf("a folder's .bdrive/config.json sent this device's hub token to %s: Authorization: %s",
				attacker.URL, got)
		}
	default:
		t.Fatal("the planted remote was never contacted; fixture is wrong")
	}
}

// TestSec_Config_MountIdCannotEscapeTheBdriveHome: Project.ID comes from the
// same untrusted file and is Joined straight onto $BDRIVE_HOME by VolumeDir.
// The whole volume store — cached blobs of every synced file, journals, the
// daemon's pid/lock — is then created at a path of the config author's
// choosing.
func TestSec_Config_MountIdCannotEscapeTheBdriveHome(t *testing.T) {
	home := secpkgHome(t)
	for _, id := range []string{
		"../../../../tmp/bdrive-pwn",
		"..",
		"/etc/bdrive-pwn",
		"m-1234abcd/../../../pwn",
	} {
		t.Run(id, func(t *testing.T) {
			folder := secpkgFolder(t, config.Project{ID: id, Volume: "wiki"})
			p, ok, err := config.LoadProject(folder)
			if err != nil || !ok {
				return // refusing the config is the secure outcome
			}
			dir, err := config.VolumeDir(p.ID)
			if err != nil {
				return
			}
			volumes := filepath.Join(home, "volumes")
			rel, err := filepath.Rel(volumes, dir)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Errorf("VolumeDir(%q) = %q, outside %q", p.ID, dir, volumes)
			}
		})
	}
}

// TestSec_Config_SettingsFileHoldingTheTokenIsNotWorldReadable asserts the
// refused case: SaveSettings writes through os.CreateTemp, so settings.json
// lands 0600 even though LoadDevice creates $BDRIVE_HOME itself at 0755.
func TestSec_Config_SettingsFileHoldingTheTokenIsNotWorldReadable(t *testing.T) {
	home := secpkgHome(t)
	if _, err := config.LoadDevice(); err != nil { // creates $BDRIVE_HOME at 0755
		t.Fatal(err)
	}
	if err := config.SaveSettings(config.Settings{Server: "https://hub", Token: "sekret"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(home, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("settings.json is mode %04o: the device token is readable by other local accounts", fi.Mode().Perm())
	}
}

// TestSec_Config_TokenNeverReachesAnErrorMessage asserts the refused case:
// a corrupt settings.json is reported by path, never by content, so the token
// cannot reach a terminal, a log, or a bug report.
func TestSec_Config_TokenNeverReachesAnErrorMessage(t *testing.T) {
	home := secpkgHome(t)
	const token = "bdrive-device-token-SECRET"
	if err := os.WriteFile(filepath.Join(home, "settings.json"),
		[]byte(`{"token":"`+token+`", BROKEN`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := config.LoadSettings()
	if err == nil {
		t.Fatal("corrupt settings.json parsed without error")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("LoadSettings error carries the device token: %v", err)
	}
}
