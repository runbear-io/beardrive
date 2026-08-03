package config_test

// Round 9, row 17 (internal/config) — replacement tests for guards that
// survived a hand reversion with the WHOLE TestSec suite green. Each one is
// written so that only the guard under test can produce the refusal.
//
// Helpers are prefixed secaud4; secpkgHome and secpkgFolder are reused from
// sec_config_test.go.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
)

// TestSec_Config_EveryPathThatCreatesTheBdriveHomeLeavesItPrivate.
//
// Rounds 4-6 made every FILE under $BDRIVE_HOME 0600, on the stated grounds
// that "every local account could read a private project's path list,
// authorship and signed-in emails". The DIRECTORY those files sit in is the
// other half of that claim and nothing asserted it — SaveSettings' 0700 could
// be widened to 0755 with the entire suite green.
//
// A world-traversable $BDRIVE_HOME hands every other local account the
// directory listing, and the listing IS the metadata: volumes/<mount-id>/
// names every project this device syncs, journal/<device>.jsonl names every
// device in the fleet, and blobs/<aa>/<sha256> is a membership oracle for
// exact file content ("does this machine hold the file whose sha256 is X").
// None of that has to open a single 0600 file.
//
// Both creation orders are exercised because $BDRIVE_HOME has two creators —
// SaveSettings (0700) and LoadDevice (0755) — and MkdirAll does not change the
// mode of a directory that already exists, so whichever runs first on a fresh
// machine decides.
func TestSec_Config_EveryPathThatCreatesTheBdriveHomeLeavesItPrivate(t *testing.T) {
	// A home that does not exist yet, so the mode under test is the one the
	// code chooses rather than the one t.TempDir happened to create.
	fresh := func(t *testing.T) string {
		t.Helper()
		home := filepath.Join(t.TempDir(), "bdrive-home")
		t.Setenv("BDRIVE_HOME", home)
		t.Setenv("BDRIVE_TOKEN", "")
		return home
	}
	check := func(t *testing.T, home string) {
		t.Helper()
		fi, err := os.Stat(home)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("$BDRIVE_HOME is mode %04o — every local account can list the projects "+
				"this device syncs, the devices in the fleet, and the content hashes it holds, "+
				"without opening any of the 0600 files inside", fi.Mode().Perm())
		}
	}
	t.Run("login_creates_it", func(t *testing.T) {
		home := fresh(t)
		if err := config.SaveSettings(config.Settings{Server: "https://hub", Token: "sekret"}); err != nil {
			t.Fatal(err)
		}
		check(t, home)
	})
	t.Run("device_identity_creates_it", func(t *testing.T) {
		home := fresh(t)
		if _, err := config.LoadDevice(); err != nil {
			t.Fatal(err)
		}
		if err := config.SaveSettings(config.Settings{Server: "https://hub", Token: "sekret"}); err != nil {
			t.Fatal(err)
		}
		check(t, home)
	})
}

// TestSec_Config_TheFilesHoldingTheDeviceTokenAreWrittenAtomically.
//
// config.writeJSON is the write path for settings.json (the device token),
// mounts.json (the registry `bdrive resume` walks at every boot) and a
// folder's .bdrive/config.json. It is a temp-file + rename, and both halves of
// that are load-bearing here, yet the whole thing could be replaced with
// os.WriteFile and the entire suite stayed green — while store.WriteFileAtomic,
// which does exactly the same job for the volume store, has had a symlink test
// since round 4.
//
// os.WriteFile follows a symlink at the destination and does not change the
// mode of a file that already exists. So a local process that pre-creates
// $BDRIVE_HOME/settings.json — as a link to a path it can read, or simply as a
// 0644 file — receives this device's hub bearer token the next time
// `bdrive login` or `bdrive logout` writes it.
func TestSec_Config_TheFilesHoldingTheDeviceTokenAreWrittenAtomically(t *testing.T) {
	const token = "bdrive-device-token-SECRET"

	t.Run("symlinked_destination_is_replaced_not_followed", func(t *testing.T) {
		home := secpkgHome(t)
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "harvested.json")
		if err := os.WriteFile(outside, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(home, "settings.json")); err != nil {
			t.Skip("symlinks unavailable")
		}
		if err := config.SaveSettings(config.Settings{Server: "https://hub", Token: token}); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(outside)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), token) {
			t.Errorf("SaveSettings wrote through a symlink planted at settings.json: the device "+
				"token now sits at %s, which the planter chose", outside)
		}
	})

	t.Run("pre_created_mode_does_not_survive", func(t *testing.T) {
		home := secpkgHome(t)
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(home, "settings.json")
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := config.SaveSettings(config.Settings{Server: "https://hub", Token: token}); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("settings.json is mode %04o after SaveSettings — a mode the file already "+
				"had when it was written, so pre-creating it world-readable is enough to read "+
				"the device token", fi.Mode().Perm())
		}
	})
}

// TestSec_Config_LoadProjectRefusesAHostileMountIdItself.
//
// Project.ID is read verbatim from a folder's .bdrive/config.json — a file
// that arrives with the folder (a zip, a clone, a colleague's copy). LoadProject
// checks it and VolumeDir checks it again, and the existing test drives BOTH in
// sequence, so either check alone holds the attack up and neither is actually
// asserted: LoadProject's could be deleted with the whole suite green.
//
// They do not cover the same ground. ResolveMount writes `mounts[p.ID]` into
// the registry BEFORE anything calls VolumeDir, so LoadProject's check is the
// only thing standing between a crafted config and a key of the author's
// choosing in mounts.json — the file `bdrive resume` and the login autostart
// walk at every boot.
func TestSec_Config_LoadProjectRefusesAHostileMountIdItself(t *testing.T) {
	secpkgHome(t)

	// Control: an ordinary id loads, so a refusal below is the guard and not
	// the fixture.
	good := secpkgFolder(t, config.Project{ID: "m-1234abcd", Volume: "wiki"})
	if p, ok, err := config.LoadProject(good); err != nil || !ok || p.ID != "m-1234abcd" {
		t.Fatalf("control: LoadProject = %+v ok=%v err=%v", p, ok, err)
	}

	for _, id := range []string{
		"../../../../tmp/bdrive-pwn",
		"..",
		".",
		"/etc/bdrive-pwn",
		"m-1234abcd/../../../pwn",
		"m-1234abcd/sub",
		`m-1234abcd\..\..\pwn`,
		"m\x00-1234abcd",
		strings.Repeat("m", 65),
		"m 1234abcd/../..",
	} {
		t.Run(id, func(t *testing.T) {
			folder := secpkgFolder(t, config.Project{ID: id, Volume: "wiki"})
			p, ok, err := config.LoadProject(folder)
			if err == nil && ok {
				t.Errorf("LoadProject accepted mount id %q (%+v) — ResolveMount writes it into "+
					"mounts.json as a registry key before any consumer validates it", id, p)
			}
		})
	}
}

// TestSec_Config_ABareIncludeEntryOnlyEverMatchesTheMountRoot.
//
// Project.Include is the legacy scope mechanism, still honored on every load,
// and it decides what leaves this machine. normalizeInclude anchors a bare
// single-segment entry to the mount root; without that anchor the pattern
// matches a directory of that name at ANY depth, so a project scoped to the
// top-level "wiki" folder also uploads private/clients/wiki and every other
// same-named directory in the tree to the whole team. The anchoring could be
// removed with the whole suite green.
func TestSec_Config_ABareIncludeEntryOnlyEverMatchesTheMountRoot(t *testing.T) {
	secpkgHome(t)
	folder := secpkgFolder(t, config.Project{
		ID:      "m-1234abcd",
		Include: []string{"wiki", "docs/", "notes/deep", "build/*"},
	})
	p, ok, err := config.LoadProject(folder)
	if err != nil || !ok {
		t.Fatalf("LoadProject: %v ok=%v", err, ok)
	}
	for _, got := range p.Include {
		bare := strings.TrimSuffix(got, "/")
		if bare == "" || strings.ContainsAny(bare, "/*?[!") {
			continue // multi-segment and glob entries are anchored by compile()
		}
		if !strings.HasPrefix(got, "/") {
			t.Errorf("include entry %q is not anchored to the mount root: it matches a "+
				"directory of that name at any depth, so a scope meant to share one top-level "+
				"folder also uploads every nested folder with the same name (full list: %v)",
				got, p.Include)
		}
	}
}
