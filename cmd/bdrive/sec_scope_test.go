package main

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
)

// Round 11 — `bdrive scope`, the surface round 10 judged "already covered by
// rounds 4/6" and skipped, and then flagged its own judgement as an
// overstatement risk.
//
// It writes into the same SYNCED .bdriveignore that `bdrive forget` writes
// into, through a different code path: a bdrive-managed block delimited by two
// marker comments (cmd/bdrive/scopefile.go). Round 10 closed forget's escaping
// hole by routing its argument through syncer.EscapeIgnore. scopeLines never
// got that treatment, and the block's markers are themselves lines in a file
// every teammate can write.
//
// Threat model, unchanged from round 10's forget work: .bdriveignore syncs, so
// its bytes are chosen by any project member, and its rules decide what leaves
// each teammate's machine.
//
// Helpers are prefixed sec11.

// sec11Project builds an isolated BDRIVE_HOME with one enrolled project folder
// and a file:// remote — the same fixture shape sec10Project uses.
func sec11Project(t *testing.T) string {
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

// sec11Scope writes a scope exactly as `bdrive init --only` and `bdrive scope
// add` do: clean the names, mkdir them, render the managed block.
func sec11Scope(t *testing.T, folder string, names ...string) []string {
	t.Helper()
	dirs, err := cleanScopeDirs(names)
	if err != nil {
		t.Skipf("cleanScopeDirs refused %q at the door: %v", names, err)
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(folder, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeScopeDirs(folder, dirs); err != nil {
		t.Fatal(err)
	}
	return dirs
}

// sec11Filter loads the shared rules exactly as a sync cycle does.
func sec11Filter(t *testing.T, folder string) *syncer.Filter {
	t.Helper()
	f, err := syncer.LoadFilter(folder, nil)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// sec11Ignore returns the synced rules file's lines, markers included.
func sec11Ignore(t *testing.T, folder string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(folder, syncer.IgnoreFile))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 1. A folder name is a NAME. scopeLines renders it as a PATTERN.
// ---------------------------------------------------------------------------

// TestSec_Scope_AFolderNameCannotWidenTheScopeIntoAGlob
//
// scopeLines (scopefile.go:28) is:
//
//	lines = append(lines, "!/"+d+"/")
//
// d verbatim, into a gitignore-dialect rule file. That is the identical defect
// round 10 found in `bdrive forget` and fixed with syncer.EscapeIgnore — whose
// own doc comment says a caller that spells the escaping itself is "a second
// definition of the dialect". scope IS that second definition, and it was left
// unescaped.
//
// The direction of the damage is the opposite of forget's, and worse. forget
// writes an EXCLUSION, so an over-broad rule deletes too much. scope writes a
// NEGATION (`!/<dir>/`) under a blanket `/*`, so an over-broad rule INCLUDES
// too much: every sibling folder the pattern happens to match starts syncing
// to the hub and out to the whole team, from a command whose entire purpose is
// to keep the rest of the folder off the network. `bdrive scope --explain`
// exists because users are told to "verify what leaves this machine instead of
// taking it on trust".
//
// A `*` or `?` in a directory name is legal on every unix filesystem, `scope
// add` mkdirs whatever name it is handed, and `chooseScope` (init.go:609) takes
// the names as free text off a survey prompt and splits them on whitespace —
// no validation between the user's keystrokes and the rule.
//
// The delta that makes this the code's decision, not the fixture's: the folder
// that WAS named syncs either way. What changes is the siblings.
func TestSec_Scope_AFolderNameCannotWidenTheScopeIntoAGlob(t *testing.T) {
	for _, tc := range []struct {
		name      string // the folder the user scopes
		bystander string // a sibling the user did NOT scope
	}{
		{"a*", "alpha"},
		{"a?c", "abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			folder := sec11Project(t)
			for _, d := range []string{tc.name, tc.bystander} {
				if err := os.MkdirAll(filepath.Join(folder, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			sec11Scope(t, folder, tc.name)
			f := sec11Filter(t, folder)

			// Control: the named folder syncs. If this fails the fixture is
			// wrong, not the code.
			if f.Skip(tc.name + "/notes.md") {
				t.Fatalf("fixture: scoping %q does not even sync %q", tc.name, tc.name)
			}
			if !f.Skip(tc.bystander + "/secret.md") {
				t.Errorf("`bdrive scope add %q` wrote %q, which also un-ignores %s/ — "+
					"a folder the user never named now syncs to the hub and to every "+
					"teammate, from the command that exists to keep it off the network",
					tc.name, "!/"+tc.name+"/", tc.bystander)
			}
		})
	}
}

// TestSec_Scope_AFolderNameCannotBecomeAnEscapeSequence
//
// The other half of the same missing escape. `\` is gitignore's escape
// character (see compile() in internal/syncer/ignore.go, which consumes it and
// takes the next byte literally) and it is a legal byte in a unix directory
// name.
//
// A folder named `a\b` scoped as `!/a\b/` compiles to the pattern for `ab`.
// Both ends are wrong at once and in opposite directions:
//
//   - the folder the user asked to sync does NOT sync — silently, with the
//     command reporting "syncing only: ./a\b";
//   - a DIFFERENT folder, `ab/`, starts syncing to the whole team.
//
// The second is the exfiltration; the first is why nobody notices.
func TestSec_Scope_AFolderNameCannotBecomeAnEscapeSequence(t *testing.T) {
	folder := sec11Project(t)
	for _, d := range []string{`a\b`, "ab"} {
		if err := os.MkdirAll(filepath.Join(folder, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sec11Scope(t, folder, `a\b`)
	f := sec11Filter(t, folder)

	if f.Skip(`a\b/notes.md`) {
		t.Errorf(`scoping "a\\b" wrote the rule "!/a\\b/", which compiles to the pattern for "ab" — ` +
			`the folder the user named does not sync at all, and the command reported success`)
	}
	if !f.Skip("ab/secret.md") {
		t.Errorf(`scoping "a\\b" un-ignored the unrelated folder "ab/", which now syncs to the hub ` +
			`and to every teammate`)
	}
}

// ---------------------------------------------------------------------------
// 2. The block's markers are lines in a file the whole team writes.
// ---------------------------------------------------------------------------

// TestSec_Scope_AMarkerInTheSharedRulesCannotSwallowTheRulesBelowIt
//
// writeScopeDirs (scopefile.go:62) rebuilds .bdriveignore by walking it and
// dropping every line between scopeStart and scopeEnd. It looks for those
// markers ANYWHERE in the file and treats every occurrence as authoritative.
//
// .bdriveignore syncs. Its bytes are chosen by any project member. A second
// scopeStart line with no matching end — one line, indistinguishable from the
// ordinary comment it looks like — makes writeScopeDirs treat everything from
// there to EOF as block content and drop it.
//
// So the next teammate who runs `bdrive scope add <anything>`, `bdrive scope
// rm <anything>` or `bdrive init . --only <anything>` silently deletes every
// exclusion rule below that line from the SHARED rules file, and pushes the
// deletion to everyone. The rules that vanish are the ones people write to
// keep material off the hub — `secrets/`, `*.pem`, `.env`. On the next cycle
// those paths sync, for the whole team, and nothing reported anything.
//
// Secure behavior asserted: editing the scope changes the managed block and
// nothing else. Every rule the user did not write stays in the file.
func TestSec_Scope_AMarkerInTheSharedRulesCannotSwallowTheRulesBelowIt(t *testing.T) {
	folder := sec11Project(t)
	sec11Scope(t, folder, "docs")

	// What a teammate's synced .bdriveignore looks like after they append one
	// comment line that happens to be the marker. The exclusions below it are
	// ordinary keep-this-off-the-hub rules, written where they still bite
	// inside the scoped folder (the block's own `/*` masks anything outside
	// it, so only rules that narrow WITHIN the scope are observable at all).
	body := strings.Join(sec11Ignore(t, folder), "\n") + "\n" +
		"\nnode_modules/\n" +
		scopeStart + "\n" + // <- the injected line, arriving over sync
		"secrets/\n" +
		"*.pem\n"
	path := filepath.Join(folder, syncer.IgnoreFile)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	private := []string{"docs/secrets/keys.txt", "docs/id.pem"}
	before := sec11Filter(t, folder)
	for _, p := range private {
		if !before.Skip(p) {
			t.Fatalf("fixture: %s was not excluded before the scope edit", p)
		}
	}
	kept := sec11Ignore(t, folder)

	// One ordinary scope edit — `bdrive scope add wiki`.
	dirs, _, err := readScopeDirs(folder)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeScopeDirs(folder, append(dirs, "wiki")); err != nil {
		t.Fatal(err)
	}

	now := strings.Join(sec11Ignore(t, folder), "\n")
	for _, line := range kept {
		if line == scopeStart || line == scopeEnd || line == "/*" || strings.HasPrefix(line, "!/") {
			continue // the managed block is what a scope edit is allowed to rewrite
		}
		if !strings.Contains(now, line) {
			t.Errorf("`bdrive scope add wiki` deleted the rule %q from the shared %s — "+
				"one comment-shaped line in a file every teammate can write turns the next "+
				"scope edit into a rule wipe\nfile now:\n%s", line, syncer.IgnoreFile, now)
		}
	}
	after := sec11Filter(t, folder)
	for _, p := range private {
		if !after.Skip(p) {
			t.Errorf("after `bdrive scope add wiki`, %s is no longer excluded — the rule that "+
				"kept it off the hub was swallowed, and the next cycle pushes it to the whole "+
				"team\nfile now:\n%s", p, now)
		}
	}
}

// TestSec_Scope_AStrayEndMarkerCannotEmptyTheScopeInForce
//
// readScopeDirs (scopefile.go:37) returns at the FIRST scopeEnd it sees,
// whether or not a start marker opened anything. A lone end marker above the
// real block therefore reports "scoped, with zero folders": the caller is told
// a scope is in force and that it names nothing.
//
// `bdrive scope` prints that as "syncing only:" followed by an empty list —
// the audit surface reporting that nothing leaves this machine while the real
// block below still scopes docs/ and wiki/. And `bdrive scope add pub` then
// writes a block containing only pub/, dropping docs/ and wiki/ out of the
// team's scope without a word.
//
// Secure behavior asserted: what readScopeDirs reports is the scope that is
// actually in force.
func TestSec_Scope_AStrayEndMarkerCannotEmptyTheScopeInForce(t *testing.T) {
	folder := sec11Project(t)
	sec11Scope(t, folder, "docs", "wiki")

	path := filepath.Join(folder, syncer.IgnoreFile)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// One line, arriving over sync, above the real block.
	if err := os.WriteFile(path, append([]byte(scopeEnd+"\n"), body...), 0o644); err != nil {
		t.Fatal(err)
	}

	// The rules in force are unchanged: docs/ and wiki/ still sync.
	f := sec11Filter(t, folder)
	for _, d := range []string{"docs", "wiki"} {
		if f.Skip(d + "/x.md") {
			t.Fatalf("fixture: %s/ stopped syncing, so there is no scope to misreport", d)
		}
	}

	dirs, scoped, err := readScopeDirs(folder)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(dirs)
	if !scoped || len(dirs) != 2 || dirs[0] != "docs" || dirs[1] != "wiki" {
		t.Errorf("readScopeDirs reported scoped=%v dirs=%q while docs/ and wiki/ are the scope "+
			"actually in force — `bdrive scope` prints that list as the answer to \"what leaves "+
			"this machine\", and `bdrive scope add` rewrites the block from it, so the next edit "+
			"silently drops every folder the report left out", scoped, dirs)
	}
}

// ---------------------------------------------------------------------------
// 3. scope add creates the folders it scopes.
// ---------------------------------------------------------------------------

// TestSec_Scope_AddCannotCreateADirectoryOutsideTheProject
//
// All three scope-writing doors (`scope add`, `init --only`, `init` on a new
// folder) mkdir the names they are handed. cleanScopeDirs rejects `..`,
// absolute paths and control characters, so the STRING is inside the project.
// MkdirAll follows symlinks, so the BYTES need not be: a symlinked directory
// already sitting in the folder takes the creation wherever it points. Same
// lexical-vs-on-disk gap round 4 closed in the syncer and the file:// backend,
// and round 6 closed in templates.WriteTo with store.UnderRoot.
//
// A symlink gets into the folder the way a folder gets anything: it arrives
// with the folder, or it is pulled from the hub, or an agent session made it.
//
// REWRITTEN BY THE CISO IN ROUND 11, with the coordinator's grant, and
// disclosed in .claude/security-goal.md. The original called cleanScopeDirs,
// then called os.MkdirAll ITSELF, then asserted store.UnderRoot on the result —
// no production code sat between the setup and the assertion, so it measured
// os.MkdirAll and filepath.EvalSymlinks and no change in this repo could move
// it. It now drives mkdirScopeDirs, the single door round 11 routed all three
// call sites through, which is the thing the original prose describes. Verified
// to go RED when the store.UnderRoot call in mkdirScopeDirs is removed.
func TestSec_Scope_AddCannotCreateADirectoryOutsideTheProject(t *testing.T) {
	folder := sec11Project(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(folder, "shared")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// Control: an ordinary subfolder is still created, so a refusal below is
	// about the symlink and not about the door refusing everything.
	if err := mkdirScopeDirs(folder, []string{"docs"}); err != nil {
		t.Fatalf("control: a plain subfolder was refused: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(folder, "docs")); err != nil || !fi.IsDir() {
		t.Fatalf("control: docs/ was not created: %v", err)
	}

	// The attack: the name is spelled inside the project and resolves outside.
	dirs, err := cleanScopeDirs([]string{"shared/loot"})
	if err != nil {
		t.Fatalf("cleanScopeDirs refused a plain relative name: %v", err)
	}
	mkErr := mkdirScopeDirs(folder, dirs)
	abs := filepath.Join(folder, filepath.FromSlash(dirs[0]))
	landed := filepath.Join(outside, "loot")

	if mkErr == nil {
		t.Errorf("`bdrive scope add shared/loot` was accepted; %s resolves to %s — outside the "+
			"project root %s. cleanScopeDirs judges the name's SPELLING; MkdirAll follows the "+
			"symlink already in the folder, so a scope edit writes into a directory the project "+
			"boundary does not cover", abs, landed, folder)
	}
	// The refusal has to be a refusal, not a message: nothing may exist outside.
	if _, err := os.Lstat(landed); err == nil {
		t.Errorf("the door reported %v but created %s anyway", mkErr, landed)
	}
	if store.UnderRoot(folder, abs) {
		t.Fatalf("fixture: %s was expected to resolve outside %s", abs, folder)
	}
}

// ---------------------------------------------------------------------------
// 4. The legacy include list and the managed block, both in force.
// ---------------------------------------------------------------------------

// TestSec_Scope_ReportsEveryScopeMechanismInForce
//
// Two scope mechanisms exist: the managed .bdriveignore block, and the legacy
// Include list in .bdrive/config.json ("still honored, never written").
// syncer.LoadFilter applies BOTH — Skip() is `ignored OR not-included`.
//
// printScope (scope.go:229) returns as soon as it finds an Include list,
// before it ever looks at the block. `bdrive scope` on a project carrying both
// therefore names one of the two sets of rules that decide what leaves the
// machine and never mentions the other. It is the command documented as the
// way to "verify what leaves this machine instead of taking it on trust".
func TestSec_Scope_ReportsEveryScopeMechanismInForce(t *testing.T) {
	folder := sec11Project(t)
	sec11Scope(t, folder, "docs")
	proj, _, err := config.LoadProject(folder)
	if err != nil {
		t.Fatal(err)
	}
	proj.Include = []string{"wiki"}
	if _, err := config.SaveProject(folder, proj); err != nil {
		t.Fatal(err)
	}

	out := secloginCapture(t, func() {
		if err := printScope(folder, proj); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "wiki") {
		t.Fatalf("fixture: the legacy include list was not reported at all\n%s", out)
	}
	if !strings.Contains(out, "docs") {
		t.Errorf("`bdrive scope` on a project with BOTH a legacy include list and a managed "+
			"block reported only the include list — the block scoping ./docs is in force in "+
			"every sync cycle and is absent from the audit output:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// 5. `bdrive hooks install --agent` — the CLI layer above agenthooks.Install.
//
// Round 10 pinned Uninstall's refusal of an unknown agent. Install's is
// unpinned, and hooksCmd is the layer neither round has driven: it turns one
// flag string into the []string agenthooks.Install loops over
// (strings.Split(agentsFlag, ","), hooks.go:62).
//
// What Install writes is not a preference file. It is an inline shell command
// in ~/.claude/settings.json and friends that the platform runs on EVERY turn
// of EVERY session on the machine, in every folder — the repo's own invariant
// list calls it out ("the agent hook guard stays pure shell ... it runs on
// every session and every tool call on the machine"). Registering one the user
// did not ask for is a machine-wide change.
// ---------------------------------------------------------------------------

// sec11Hooks isolates $HOME and runs `bdrive hooks install` with the given
// --agent value, exactly as the CLI parses it.
func sec11Hooks(t *testing.T, args ...string) (string, error) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	folder := t.TempDir()

	c := hooksCmd()
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	c.SilenceUsage, c.SilenceErrors = true, true
	c.SetArgs(append(append([]string{"install"}, args...), folder))

	var err error
	secloginCapture(t, func() { err = c.Execute() })
	return home, err
}

// sec11Registrations lists every file under home that names bdrive — i.e.
// every platform config this machine will now run bdrive from.
func sec11Registrations(t *testing.T, home string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(home, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), "bdrive") {
			rel, _ := filepath.Rel(home, p)
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// TestSec_HooksInstall_ARefusedAgentListRegistersNothing
//
// agenthooks.Install loops the names in order and returns on the first unknown
// one — after installing every name before it, and after returning `out`, the
// partial result. hooksCmd propagates the error, so the command exits non-zero
// and prints a failure, while ~/.claude/settings.json has already been
// rewritten with a hook that fires on every turn of every Claude session on
// the machine.
//
// The same command tree already establishes the opposite posture one file over
// — `bdrive forget` resolves every argument first, because "an argument
// outside the project is an error that writes nothing at all" (forget.go:57).
//
// The delta that makes this the code's decision: put the bad name FIRST and
// nothing is written; put it second and the machine is changed by a command
// that reported failure.
func TestSec_HooksInstall_ARefusedAgentListRegistersNothing(t *testing.T) {
	// Control: unknown name first — refused before any write.
	home, err := sec11Hooks(t, "--agent", "bogus,claude")
	if err == nil {
		t.Fatal("`hooks install --agent bogus,claude` was accepted")
	}
	if got := sec11Registrations(t, home); len(got) != 0 {
		t.Fatalf("fixture: the control case already wrote %q", got)
	}

	// Same list, order swapped.
	home, err = sec11Hooks(t, "--agent", "claude,bogus")
	if err == nil {
		t.Fatal("`hooks install --agent claude,bogus` was accepted")
	}
	if got := sec11Registrations(t, home); len(got) != 0 {
		t.Errorf("`bdrive hooks install --agent claude,bogus` failed with %q and still left %q "+
			"registered under $HOME — the same list with the names in the other order writes "+
			"nothing, so whether a failed command changes the machine depends on argument order. "+
			"What it wrote is an inline shell command the platform runs on every turn of every "+
			"session in every folder", err, got)
	}
}

// TestSec_HooksInstall_AnEmptyAgentValueRegistersNothing
//
// An empty --agent is not "auto": it is a user (or a script, or an agent
// building the command line) naming an empty set. hooks.go:59 tests
// `agentsFlag != ""`, so an empty value falls through to the same branch as
// the default and agenthooks.Install turns a nil list into Detect(folder) —
// every platform with a config directory in the project or in $HOME.
//
// So an unexpanded shell variable registers hooks machine-wide for every
// platform the user has ever installed, silently and with a zero exit code.
// The flag that names which platforms to touch is the only thing standing
// between "one" and "all of them".
func TestSec_HooksInstall_AnEmptyAgentValueRegistersNothing(t *testing.T) {
	// A machine that has Claude Code and Gemini CLI set up: the ordinary case.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	for _, d := range []string{".claude", ".gemini"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	folder := t.TempDir()

	c := hooksCmd()
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	c.SilenceUsage, c.SilenceErrors = true, true
	c.SetArgs([]string{"install", "--agent", "", folder})
	var err error
	secloginCapture(t, func() { err = c.Execute() })

	got := sec11Registrations(t, home)
	if err == nil && len(got) > 0 {
		t.Errorf("`bdrive hooks install --agent ''` succeeded and registered %q — an empty "+
			"value names no platform, and it reached agenthooks.Install as the same nil list "+
			"the default \"auto\" produces, so every detected platform on the machine now runs "+
			"bdrive on every turn of every session", got)
	}
}

// TestSec_HooksInstall_OnlyTheFourKnownPlatformsCanNameAConfigPath
//
// The refusals that DO hold, pinned so they keep holding. agenthooks.ConfigPath
// returns "" for an unknown agent, which is a relative path — a write to the
// process's working directory, which for `bdrive hooks install` is a user's
// project folder, and therefore a SYNCED one.
func TestSec_HooksInstall_OnlyTheFourKnownPlatformsCanNameAConfigPath(t *testing.T) {
	for _, name := range []string{
		"../../etc", "..", "/etc/claude", "CLAUDE", "claude/../codex",
		"claude ", " claude", "claude\x00", "auto,claude", ".claude",
	} {
		t.Run(strings.ReplaceAll(name, "\x00", "\\x00"), func(t *testing.T) {
			home, err := sec11Hooks(t, "--agent", name)
			if err == nil {
				t.Errorf("`hooks install --agent %q` was accepted", name)
			}
			if got := sec11Registrations(t, home); len(got) != 0 {
				t.Errorf("`hooks install --agent %q` wrote %q under $HOME", name, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 6. The other client this device sends its credential with.
// ---------------------------------------------------------------------------

// TestSec_CLI_TheInitClientCarriesAWholeRequestDeadline
//
// internal/remote's sync backend is not the only http.Client the device uses:
// initClient (init.go:636) is the one `bdrive init`, `bdrive login` and
// `bdrive logout` talk to the hub with, carrying the device's bearer token.
// A zero Timeout there is a hub that can hang `bdrive init` forever — and
// `bdrive init` is the command the onboarding runbook tells every agent to run
// first. Round 10 found this client had no CheckRedirect because nobody had
// read its construction; this pins both fields, alongside
// internal/remote/sec_slow_test.go for the sync backend's.
func TestSec_CLI_TheInitClientCarriesAWholeRequestDeadline(t *testing.T) {
	if initClient.Timeout <= 0 {
		t.Errorf("initClient.Timeout = %v — a hub that accepts the request and never answers "+
			"hangs `bdrive init` and `bdrive login` indefinitely", initClient.Timeout)
	}
	if initClient.CheckRedirect == nil {
		t.Error("initClient has no CheckRedirect — a hub's 3xx carries this device's bearer token")
	}
}
