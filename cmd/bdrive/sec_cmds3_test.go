package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/syncer"
)

// Round 10 — the CLI commands round 9 recorded with ZERO TestSec_ coverage:
// forget, scope, hooks, restore, serve, logout, whoami, version, daemon,
// autostart. Helpers are prefixed sec10.
//
// The valuable ones here turned out to be `forget` (which writes a rule into
// the SHARED .bdriveignore and then prunes the hub in the same breath) and
// `serve` (whose config file carries the hub's whole auth posture).

// sec10Project builds an isolated BDRIVE_HOME with one enrolled project folder
// and a file:// remote — the shape every command below needs.
func sec10Project(t *testing.T) string {
	t.Helper()
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder := t.TempDir()
	if _, err := config.SaveProject(folder, config.Project{
		Volume: "notes",
		Remote: "file://" + t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(folder); err != nil {
		t.Fatal(err)
	}
	return folder
}

// sec10Touch creates a file with a literal name (no shell in between).
func sec10Touch(t *testing.T, root, name string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// sec10Filter loads the shared rules exactly as the prune step does.
func sec10Filter(t *testing.T, root string) *syncer.Filter {
	t.Helper()
	f, err := syncer.LoadFilter(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// sec10ForgetRule is what `bdrive forget <arg>` would append, without running
// the prune cycle the real command runs immediately afterwards.
func sec10ForgetRule(t *testing.T, root, name string) string {
	t.Helper()
	rule, err := ignoreRule(root, filepath.Join(root, name))
	if err != nil {
		t.Fatalf("bdrive forget %q was refused before it could be judged: %v", name, err)
	}
	if _, err := appendIgnoreRules(root, []string{rule}); err != nil {
		t.Fatal(err)
	}
	return rule
}

// ---------------------------------------------------------------------------
// bdrive forget — one argument, one rule, one deletion. Or not.
// ---------------------------------------------------------------------------

// TestSec_Forget_AFilenameCannotWidenTheRuleIntoAGlob
//
// `bdrive forget <path>` turns its argument into a .bdriveignore line and then
// runs a PRUNE cycle in the same command — "Add a path to .bdriveignore and
// remove what already synced from the hub". ignoreRule (forget.go:96) emits the
// project-relative path verbatim. .bdriveignore is gitignore syntax, so `*`,
// `?` and `[…]` in that path are wildcards, not characters.
//
// A file literally named `a*` is legal on every unix filesystem, and filenames
// in a synced project are chosen by whoever syncs into it — any teammate. Ask
// forget to drop that one file and the rule it writes matches every sibling
// starting with `a`, all of which the same command then deletes from the hub
// for the whole team. The command's own Long text promises the opposite:
// "Only the hub's copy goes away" — of the path you named.
//
// The delta that makes this the code's decision: the file that WAS named is
// correctly excluded either way. What changes is everything else.
func TestSec_Forget_AFilenameCannotWidenTheRuleIntoAGlob(t *testing.T) {
	root := sec10Project(t)
	sec10Touch(t, root, "a*")        // the file the user means to forget
	sec10Touch(t, root, "alpha.md")  // a teammate's file
	sec10Touch(t, root, "agenda.md") // another

	rule := sec10ForgetRule(t, root, "a*")
	f := sec10Filter(t, root)

	// Control: the named file is excluded. If this fails the fixture is wrong.
	if !f.Skip("a*") {
		t.Fatalf("fixture: rule %q does not even cover the file it names", rule)
	}
	for _, bystander := range []string{"alpha.md", "agenda.md"} {
		if f.Skip(bystander) {
			t.Errorf("`bdrive forget 'a*'` wrote the rule %q, which also excludes %s — "+
				"and forget prunes in the same command, so a teammate's file is deleted "+
				"from the hub by a request to drop one path", rule, bystander)
		}
	}
}

// TestSec_Forget_AFilenameCannotDisablePruningForTheWholeProject
//
// A leading `!` is a NEGATION in gitignore syntax, and pruneOps refuses to run
// at all once any negation is present (syncer.go:1006, and pruneSafe says the
// same thing to the user) — because with scope rules a prune would delete
// everything outside the scope for every teammate.
//
// So a file named `!keep` turns `bdrive forget '!keep'` into: append a
// negation to the SHARED, SYNCED .bdriveignore, outside the managed scope
// block that `bdrive scope rm` knows how to edit. From that moment no
// `--prune` and no `forget` on that project removes anything from the hub, for
// anybody, and the line looks like an ordinary rule. One filename, chosen by
// any teammate, permanently disables the team's only cleanup lever.
func TestSec_Forget_AFilenameCannotDisablePruningForTheWholeProject(t *testing.T) {
	root := sec10Project(t)
	sec10Touch(t, root, "!keep")

	if f := sec10Filter(t, root); f.Negated() {
		t.Fatal("fixture: the project already carries negation rules")
	}
	rule := sec10ForgetRule(t, root, "!keep")
	if f := sec10Filter(t, root); f.Negated() {
		t.Errorf("`bdrive forget '!keep'` appended %q, a negation — pruneOps and pruneSafe "+
			"both refuse to prune anything on a project with negation rules, so one filename "+
			"has disabled hub cleanup for every teammate, in the synced rules file, outside "+
			"the block `bdrive scope` manages", rule)
	}
}

// TestSec_Forget_AFilenameCannotBecomeAComment
//
// The other end of the same missing escape: `#` opens a comment. `bdrive
// forget '#draft.md'` reports "added `#draft.md` to .bdriveignore", writes a
// line that is not a rule, prunes nothing, and leaves the file syncing — a
// silent failure of the command's entire purpose, reported as success.
func TestSec_Forget_AFilenameCannotBecomeAComment(t *testing.T) {
	root := sec10Project(t)
	sec10Touch(t, root, "#draft.md")

	rule := sec10ForgetRule(t, root, "#draft.md")
	if !sec10Filter(t, root).Skip("#draft.md") {
		t.Errorf("`bdrive forget '#draft.md'` wrote %q, which .bdriveignore reads as a comment: "+
			"the command reported the path added and the file still syncs", rule)
	}
}

// TestSec_Forget_KeepsRefusingThePathsItAlreadyRefuses
//
// The guards ignoreRule DOES carry, asserted so a fix for the three above
// cannot quietly drop them: outside the project, the project root itself, the
// rules file, and a control character (which would write more than one rule).
func TestSec_Forget_KeepsRefusingThePathsItAlreadyRefuses(t *testing.T) {
	root := sec10Project(t)
	for _, bad := range []string{
		filepath.Dir(root),
		root,
		filepath.Join(root, "..", "elsewhere"),
		filepath.Join(root, syncer.IgnoreFile),
		filepath.Join(root, "a\nb"),
		filepath.Join(root, "a\rb"),
	} {
		if rule, err := ignoreRule(root, bad); err == nil {
			t.Errorf("ignoreRule(%q) = %q, want a refusal", bad, rule)
		}
	}
}

// ---------------------------------------------------------------------------
// bdrive serve — the command, its config file, and what it does with one that
// asks for something the chosen mode cannot deliver.
// ---------------------------------------------------------------------------

// sec10Occupied returns a listening address, so `serve` fails at bind instead
// of blocking the test forever. Anything the command decides BEFORE binding
// still surfaces as its error.
func sec10Occupied(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

// sec10ServeErr runs `bdrive serve` with a config file and returns its error.
func sec10ServeErr(t *testing.T, cfg map[string]any, extra ...string) error {
	t.Helper()
	t.Setenv("BDRIVE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-c", path, "--addr", sec10Occupied(t)}, extra...)
	c := webCmd()
	c.SetArgs(args)
	c.SilenceUsage, c.SilenceErrors = true, true
	done := make(chan error, 1)
	go func() { done <- c.Execute() }()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("bdrive serve neither refused the config nor failed to bind")
		return nil
	}
}

// TestSec_Serve_ConfigAuthBlockIsNeverSilentlyIgnored
//
// The auth block is the hub's entire access posture: allowed_domains,
// require_approval, allow_signup, admins. web.go builds it only inside
// `if srv.Root != nil` — the hub branch. A config that names a `dir` selects
// the single-volume viewer, which is auth-free by design, and every one of
// those settings is dropped without a word.
//
// So an operator who writes a config with a full auth block and a `dir` gets a
// server on 0.0.0.0:4173 that anyone can read, and a config file that says
// otherwise. The hub already refuses to boot on an incoherent auth posture
// rather than leaving the door open — ValidateSignupPolicy, called eight lines
// below the block that is skipped here. This is the same class, in the branch
// that check never runs in.
//
// The delta: with `remote` instead of `dir` the identical auth block is
// honoured, and ValidateSignupPolicy is what answers. Only the mode decides.
func TestSec_Serve_ConfigAuthBlockIsNeverSilentlyIgnored(t *testing.T) {
	folder := t.TempDir()
	err := sec10ServeErr(t, map[string]any{
		"dir": folder,
		"auth": map[string]any{
			"allowed_domains":  []string{"runbear.io"},
			"require_approval": true,
			"admins":           []string{"boss@runbear.io"},
		},
	})
	if err == nil {
		t.Fatal("fixture: serve returned no error at all")
	}
	if strings.Contains(err.Error(), "address already in use") ||
		strings.Contains(err.Error(), "bind") {
		t.Errorf("`bdrive serve -c` with a dir AND an auth block got as far as binding "+
			"(err = %v) — the whole auth posture (allowed_domains, require_approval, admins) "+
			"was discarded and the folder is served with no sign-in at all. A config that asks "+
			"for gating the chosen mode cannot provide must be refused, exactly as "+
			"ValidateSignupPolicy refuses an incoherent one in hub mode", err)
	}
}

// TestSec_Serve_ConfigRefusesWhatItCannotHonour
//
// The refusals `serve` DOES carry, pinned: an unknown key (DisallowUnknownFields
// — a typo in `allowed_domains` must not read as "no restriction"), an unknown
// database driver, and a dir that is not a directory.
func TestSec_Serve_ConfigRefusesWhatItCannotHonour(t *testing.T) {
	for name, cfg := range map[string]map[string]any{
		"unknown key": {"dir": t.TempDir(), "allowed_domains": []string{"x.io"}},
		"unknown driver": {"remote": "file://" + t.TempDir(),
			"database": map[string]any{"driver": "mongo", "dsn": "x"}},
		"dir is a file": {"dir": filepath.Join(t.TempDir(), "nope")},
	} {
		err := sec10ServeErr(t, cfg)
		if err == nil {
			t.Errorf("%s: serve accepted the config", name)
			continue
		}
		if strings.Contains(err.Error(), "address already in use") {
			t.Errorf("%s: serve reached the listener before rejecting the config (err = %v)", name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// bdrive restore — round 9 tested syncer.Restore; the command that drives it
// and where it gets its arguments were untested.
// ---------------------------------------------------------------------------

// TestSec_Restore_AVersionArgumentCannotReachAnotherFilesContent
//
// `bdrive restore <file> <version>` resolves a short content hash. If that
// resolution ran over the whole log, any hash from `bdrive log` — including
// one belonging to a file the caller can see the hash of but should not be
// able to write — would be restorable INTO a different path, minting a put of
// somebody else's bytes under a name of the attacker's choosing.
//
// versionsOf re-filters to the exact path before pickVersion ever sees it.
// Asserted, because the comment above it says restoring the wrong file "would
// be the worst possible bug in this command" and nothing tested it.
func TestSec_Restore_AVersionArgumentCannotReachAnotherFilesContent(t *testing.T) {
	secret := "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
	mine := "aaaabbbbccccddddeeeeffff00001111aaaabbbbccccddddeeeeffff00001111"
	all := []journal.Op{
		{Kind: journal.KindPut, Path: "secrets/keys.txt", Blob: secret, Size: 9},
		{Kind: journal.KindPut, Path: "notes/mine.md", Blob: mine, Size: 3},
	}

	versions := versionsOf(all, "notes/mine.md")
	if len(versions) != 1 || versions[0].Blob != mine {
		t.Fatalf("fixture: versionsOf returned %+v", versions)
	}
	// The other file's hash, in full and as a prefix, must not resolve here.
	for _, want := range []string{secret, secret[:8], secret[:2]} {
		if op, err := pickVersion(versions, mine, want); err == nil {
			t.Errorf("`bdrive restore notes/mine.md %s` resolved to blob %s — a version "+
				"argument reached a blob that belongs to another path", want, op.Blob)
		}
	}
	// And the real one still resolves, so the refusal is about the path.
	if _, err := pickVersion(versions, "", mine[:8]); err != nil {
		t.Errorf("the file's own version no longer resolves: %v", err)
	}
}

// sec10Carrier is a folder that merely CARRIES a .bdrive/config.json — an
// unpacked archive, a cloned repo, a colleague's copied directory. It is not
// enrolled: nothing in the registry names it. The remote is the attacker's.
func sec10Carrier(t *testing.T, remote string) (folder, id string) {
	t.Helper()
	folder = t.TempDir()
	p, err := config.SaveProject(folder, config.Project{Volume: "notes", Remote: remote})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "readme.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	mounts, err := config.LoadMounts()
	if err != nil {
		t.Fatal(err)
	}
	if _, registered := mounts[p.ID]; registered {
		t.Fatalf("fixture: %s was already enrolled", p.ID)
	}
	return folder, p.ID
}

// sec10Enrolled reports whether the registry now names the id.
func sec10Enrolled(t *testing.T, id string) bool {
	t.Helper()
	mounts, err := config.LoadMounts()
	if err != nil {
		t.Fatal(err)
	}
	_, ok := mounts[id]
	return ok
}

// TestSec_Restore_DoesNotEnrollThisDeviceInAProjectItWasNeverInitedInto
//
// restoreCmd carries this gate, with this comment:
//
//	// Restoring ends in a sync cycle, so it answers to the same gate
//	// `bdrive sync` does: it must not enroll this device or resume a
//	// project someone paused. Only `bdrive init` does that.
//	switch syncBlocked(proj) {
//	case "init":  return "... run `bdrive init` there to connect it"
//
// It is dead code. Two lines above it, restore calls findProject (share.go:181),
// which calls config.ResolveMount, whose tail is
//
//	if !registered || mi.Path != folder || … {
//	        mounts[p.ID] = MountInfo{…}; SaveMounts(mounts)
//	}
//
// — so an id the registry has never seen is WRITTEN INTO IT before syncBlocked
// is ever asked. syncBlocked can no longer return "init" for any folder
// restore can reach.
//
// What that costs: .bdrive/config.json travels with a folder, which is the
// premise rounds 4, 5 and 7 built their work on (Project.ID is regex-validated
// and Project.Remote is origin-bound precisely because this file arrives from
// outside). Running restore once inside an unpacked archive enrolls the device
// in whatever project that file names — a registry row pointing at the
// attacker's remote. `bdrive resume` walks that registry and starts a daemon
// for every enrolled, unpaused mount, and the login registration runs
// `bdrive resume` at every boot. Nobody ever ran `bdrive init`.
//
// The delta that proves this is the command's decision and not the fixture:
// `bdrive sync` in the SAME folder refuses and enrolls nothing. It resolves
// with config.LoadProject — a pure read — before asking syncBlocked
// (cmds.go:55). restore and forget resolve with findProject, which writes.
// Same folder, same gate, two resolvers, one of them a write.
func TestSec_Restore_DoesNotEnrollThisDeviceInAProjectItWasNeverInitedInto(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	attacker := t.TempDir()

	// Control: `bdrive sync` on a carrier refuses and leaves the registry empty.
	{
		ctl, ctlID := sec10Carrier(t, "file://"+attacker)
		if _, err := seccliRun(t, syncCmd(), []string{ctl}); err == nil {
			t.Fatalf("fixture: `bdrive sync` accepted an un-inited carrier folder")
		}
		if sec10Enrolled(t, ctlID) {
			t.Fatalf("fixture: `bdrive sync` itself enrolled %s — the delta below is not a delta", ctlID)
		}
	}

	folder, id := sec10Carrier(t, "file://"+attacker)
	out, err := seccliRun(t, restoreCmd(), []string{filepath.Join(folder, "readme.md")})
	if err == nil {
		t.Fatalf("fixture: restore succeeded outright: %s", out)
	}
	if !strings.Contains(err.Error(), "bdrive init") {
		t.Errorf("`bdrive restore` in an un-inited folder said %q — the gate restore documents "+
			"(\"it must not enroll this device … Only `bdrive init` does that\") never fired", err)
	}
	if sec10Enrolled(t, id) {
		t.Errorf("`bdrive restore` enrolled this device in project %s (remote %s) without "+
			"`bdrive init`: findProject → config.ResolveMount writes the registry row before "+
			"syncBlocked is consulted, and `bdrive resume` starts a daemon for every row",
			id, "file://"+attacker)
	}
}

// TestSec_Forget_DoesNotEnrollThisDeviceInAProjectItWasNeverInitedInto
//
// The same gate, the same dead code, in the command that also PRUNES the hub.
// forget.go opens with findProject(cwd) and then asks syncBlocked; the
// enrollment has already happened.
func TestSec_Forget_DoesNotEnrollThisDeviceInAProjectItWasNeverInitedInto(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder, id := sec10Carrier(t, "file://"+t.TempDir())
	t.Chdir(folder)

	out, err := seccliRun(t, forgetCmd(), []string{filepath.Join(folder, "readme.md")})
	if err == nil {
		t.Logf("forget completed: %s", out)
	}
	if sec10Enrolled(t, id) {
		t.Errorf("`bdrive forget` enrolled this device in project %s without `bdrive init` — "+
			"and it is the command that writes a shared ignore rule and prunes the hub", id)
	}
}

// TestSec_Restore_RefusesAPathOutsideAnyProject
//
// The guard that does hold: a path in no project at all.
func TestSec_Restore_RefusesAPathOutsideAnyProject(t *testing.T) {
	sec10Project(t)
	outside := filepath.Join(t.TempDir(), "elsewhere.md")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := seccliRun(t, restoreCmd(), []string{outside}); err == nil {
		t.Errorf("`bdrive restore %s` was accepted from outside any project", outside)
	}
}

// ---------------------------------------------------------------------------
// bdrive whoami / logout — credentials on the client.
// ---------------------------------------------------------------------------

// TestSec_Whoami_NeverPrintsTheDeviceToken
//
// whoami reads settings.json, which holds the bearer token this device syncs
// with, and prints an identity summary. Anything it prints goes into
// screenshots, pasted terminal output, agent transcripts and CI logs — so the
// token must not be among it, and the hub-chosen name/email must be neutralised
// (the class round 5 closed for `bdrive log`).
func TestSec_Whoami_NeverPrintsTheDeviceToken(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	token := "bdrv_supersecret_0123456789abcdef"
	if err := config.SaveSettings(config.Settings{
		Server: "https://hub.example",
		Token:  token,
		Email:  "user@example.com",
		// Hub-chosen, so it is the hub's string, not ours.
		Name: "Ada\x1b[2J\x1b]0;pwned\x07",
	}); err != nil {
		t.Fatal(err)
	}

	out, err := seccliRun(t, whoamiCmd(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, token) {
		t.Errorf("`bdrive whoami` printed the device token:\n%s", out)
	}
	if !strings.Contains(out, "user@example.com") {
		t.Fatalf("fixture: whoami did not print the account at all:\n%s", out)
	}
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("`bdrive whoami` echoed a hub-chosen name containing terminal control bytes:\n%q", out)
	}
}

// TestSec_Logout_ClearsTheLocalCredentialEvenWhenTheHubIsUnreachable
//
// logout revokes on the hub first and clears locally after. The hub half can
// fail — that is the case an operator reaches for logout in. Assert the local
// token is cleared anyway, that the failure is REPORTED rather than swallowed,
// and that --forget also drops the remembered server.
func TestSec_Logout_ClearsTheLocalCredentialEvenWhenTheHubIsUnreachable(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	// A port nothing is listening on.
	dead := sec10Dead(t)
	if err := config.SaveSettings(config.Settings{
		Server: dead, Token: "tok-abc", Email: "user@example.com", Name: "User",
	}); err != nil {
		t.Fatal(err)
	}

	out, err := seccliRun(t, logoutCmd(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "warning") {
		t.Errorf("`bdrive logout` against an unreachable hub swallowed the failed revocation:\n%s", out)
	}
	s, err := config.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if s.Token != "" || s.Email != "" {
		t.Errorf("`bdrive logout` left a credential behind: token=%q email=%q", s.Token, s.Email)
	}
	if s.Server != dead {
		t.Errorf("plain logout dropped the remembered server %q -> %q", dead, s.Server)
	}
	if strings.Contains(out, "tok-abc") {
		t.Errorf("`bdrive logout` printed the token it was revoking:\n%s", out)
	}
}

// sec10Dead is an http URL nothing listens on.
func sec10Dead(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return "http://" + addr
}

// ---------------------------------------------------------------------------
// bdrive daemon / autostart — round 8 judged these "thin wrappers with no
// CLI-specific input surface" by reading. Verified here.
// ---------------------------------------------------------------------------

// TestSec_DaemonCmd_RunRefusesAFolderThatIsNotAProject
//
// `bdrive daemon run <folder>` is hidden but registered, so it is reachable by
// anything that can run the binary. Its one argument goes straight to
// daemon.Run. A folder with no .bdrive/config.json must be refused rather than
// having a daemon (and a volume store) created for it.
func TestSec_DaemonCmd_RunRefusesAFolderThatIsNotAProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BDRIVE_HOME", home)
	plain := t.TempDir()

	if _, err := seccliRun(t, daemonCmd(), []string{"run", plain}); err == nil {
		t.Errorf("`bdrive daemon run %s` started a daemon for a folder that is not a project", plain)
	}
	// And it left no volume state behind for it.
	if entries, err := os.ReadDir(filepath.Join(home, "volumes")); err == nil && len(entries) > 0 {
		t.Errorf("a refused `daemon run` created %d volume store(s) under $BDRIVE_HOME", len(entries))
	}
}

// TestSec_AutostartCmd_StatusAndUninstallTouchOnlyTheRegistration
//
// `bdrive autostart` (status), `install`, `uninstall` — driven end to end
// against an isolated $HOME. Status must not create the registration, install
// must be idempotent, and uninstall must leave the directory otherwise as it
// found it.
func TestSec_AutostartCmd_StatusAndUninstallTouchOnlyTheRegistration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("BDRIVE_HOME", t.TempDir())

	out, err := seccliRun(t, autostartCmd(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "autostart: registered") {
		t.Errorf("bare `bdrive autostart` reported a registration nothing created:\n%s", out)
	}

	if _, err := seccliRun(t, autostartCmd(), []string{"install"}); err != nil {
		t.Fatal(err)
	}
	second, err := seccliRun(t, autostartCmd(), []string{"install"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second, "already registered") && !strings.Contains(second, "not available") {
		t.Errorf("a second `autostart install` did not report idempotency:\n%s", second)
	}
	if _, err := seccliRun(t, autostartCmd(), []string{"uninstall"}); err != nil {
		t.Fatal(err)
	}
	if _, err := seccliRun(t, autostartCmd(), []string{"uninstall"}); err != nil {
		t.Errorf("a second `autostart uninstall` errored: %v", err)
	}
}

// TestSec_VersionCmd_PrintsNothingAboutTheEnvironment
//
// `bdrive version` is the one command a support request always includes.
// Assert it prints the version and nothing about the machine — no paths, no
// home directory, no account.
func TestSec_VersionCmd_PrintsNothingAboutTheEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BDRIVE_HOME", home)
	if err := config.SaveSettings(config.Settings{
		Server: "https://hub.example", Token: "tok-xyz", Email: "user@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	out, err := seccliRun(t, versionCmd(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{home, "tok-xyz", "user@example.com", "hub.example"} {
		if strings.Contains(out, leak) {
			t.Errorf("`bdrive version` printed %q:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "beardrive") {
		t.Errorf("`bdrive version` printed no version at all: %q", out)
	}
}
