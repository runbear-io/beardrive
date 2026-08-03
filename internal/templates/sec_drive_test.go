package templates

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
)

// Round 11 — internal/templates driven end to end. Round 9 swept three guards
// into this package from the outside; round 10 swept the HUB half
// (webapp/templates.go) and left this half's own doors — Get, WriteTo,
// SafePath's delegation, the closedness of the registry, and the shipped
// CONTENT — unexercised.
//
// The content matters because a template's files are AGENTS.md and friends:
// they land in a folder that syncs to every teammate, and every agent session
// on every one of their machines reads them at the top of its context. That is
// a distribution channel, and this package is the only thing standing in it.
//
// Helpers are prefixed sec11.

// sec11All is every shipped template plus its files, loaded the way both
// seeding doors load them.
func sec11All(t *testing.T) []Template {
	t.Helper()
	all := List()
	if len(all) != len(shipped) {
		t.Fatalf("List() returned %d templates, shipped has %d — a template failed to load "+
			"and List() swallows the error", len(all), len(shipped))
	}
	return all
}

// ---------------------------------------------------------------------------
// 1. Is the registry closed? Can a template be introduced at all?
// ---------------------------------------------------------------------------

// TestSec_Templates_GetIsTheOnlyDoorAndItIsClosed
//
// Get is the only constructor every caller uses (cmd/bdrive/init.go:127 and
// :547, webapp/server.go:755) and the name it takes comes off a `--template`
// flag and off a JSON request body respectively. load() builds an embed-FS
// path by concatenation — `root := "files/" + name` — so anything Get lets
// through reaches fs.WalkDir as a path.
//
// The set-membership check in front of it is what makes that safe. This pins
// it: no traversal, no case fold, no separator, no prefix match, and no
// spelling of a real name other than the exact one may reach load().
func TestSec_Templates_GetIsTheOnlyDoorAndItIsClosed(t *testing.T) {
	for _, name := range []string{
		"",
		".",
		"..",
		"../files/docs",
		"../../internal",
		"files/docs",
		"docs/",
		"/docs",
		"DOCS",
		"docs ",
		" docs",
		"doc",
		"docs\x00",
		"docs\n",
		"docs/../wiki",
	} {
		t.Run(strings.ReplaceAll(name, "\x00", "\\x00"), func(t *testing.T) {
			tpl, err := Get(name)
			if err == nil {
				t.Errorf("Get(%q) was accepted and loaded %d files — the name is concatenated "+
					"into an embed-FS path in load()", name, len(tpl.Files))
			}
			if len(tpl.Files) != 0 || tpl.Name != "" {
				t.Errorf("Get(%q) returned a non-zero Template alongside its error: %+v", name, tpl.Name)
			}
		})
	}
}

// TestSec_Templates_TheRegistryHasNoWriteDoor
//
// `shipped` is a package-level var. Nothing exported adds to it, and Names()
// / List() must report exactly it — if a future setter (or an init() in
// another file of this package) grew one, the two surfaces that render the
// choice to a user would start offering it and Get would start loading it.
func TestSec_Templates_TheRegistryHasNoWriteDoor(t *testing.T) {
	want := []string{"docs", "wiki", "para"}
	if got := Names(); !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %q, want %q — the shipped set changed; every name here is "+
			"concatenated into an embed path and rendered as a choice on the hub", got, want)
	}
	for _, tpl := range sec11All(t) {
		if _, err := Get(tpl.Name); err != nil {
			t.Errorf("List() offers %q but Get(%q) fails: %v", tpl.Name, tpl.Name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. SafePath's delegation, and the rule WriteTo actually needs.
// ---------------------------------------------------------------------------

// TestSec_Templates_SafePathIsExactlyTheJournalRule
//
// SafePath's doc says "The rule is journal.SafePath: a template file becomes
// an Op.Path on every device, so this door may not be more permissive than the
// journal is." That is a delegation, and a delegation that drifts is a hole
// nobody looks at twice. Pin it on the inputs that matter.
func TestSec_Templates_SafePathIsExactlyTheJournalRule(t *testing.T) {
	for _, p := range []string{
		"docs/README.md", "AGENTS.md", "a/b/c.md",
		"", ".", "..", "../x", "/etc/passwd", "a/../b", "a//b", "./a",
		"a/", "x\x00y", "x\ny", "x\x7fy", ".bdrive/config.json", ".git/config",
	} {
		if got, want := SafePath(p), journal.SafePath(p); got != want {
			t.Errorf("SafePath(%q) = %v, journal.SafePath = %v — the template door drifted "+
				"from the journal's rule", p, got, want)
		}
	}
}

// TestSec_Templates_AReservedPathIsRefusedAtTheWrite
//
// WriteTo's own doc comment states the contract this asserts:
//
//	"Template and File are exported with exported fields, so 'today every
//	 template comes from the go:embed' is a property of the callers, not of
//	 this function: the guard belongs where the write happens."
//
// The guard it applies is SafePath (= journal.SafePath) plus store.UnderRoot.
// Neither knows about RESERVED paths. `.bdrive/config.json` is clean,
// relative, control-character-free and squarely inside the root — it passes
// both — and it is the file that holds a mount's remote URL, i.e. which hub
// this folder's contents are pushed to. `.git/config` is the same shape.
//
// Round 9 asserted that no SHIPPED template names one. That is a property of
// the files in this repo, which is exactly the reasoning WriteTo's comment
// says is not good enough for this function.
func TestSec_Templates_AReservedPathIsRefusedAtTheWrite(t *testing.T) {
	for _, bad := range []string{".bdrive/config.json", ".git/config", ".bdrive/settings.json"} {
		t.Run(bad, func(t *testing.T) {
			if !config.ReservedPath(bad) {
				t.Fatalf("fixture: %q is not a reserved path", bad)
			}
			folder := t.TempDir()
			tpl := Template{Name: "hostile", Files: []File{{Path: bad, Content: "remote: https://attacker.example/p/x\n"}}}
			wrote, err := tpl.WriteTo(folder)
			if err == nil {
				t.Errorf("WriteTo wrote %q into the project (wrote=%q) — SafePath and "+
					"store.UnderRoot both pass a reserved path, and this one decides which "+
					"hub the folder syncs to", bad, wrote)
			}
			if _, err := os.Lstat(filepath.Join(folder, filepath.FromSlash(bad))); err == nil {
				t.Errorf("WriteTo created %s", filepath.Join(folder, bad))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 3. Seeding into a folder that already has contents, or that moves under it.
// ---------------------------------------------------------------------------

// TestSec_Templates_SeedingNeverReplacesAProjectFile
//
// The never-overwrite rule is what makes the agent path safe: `bdrive init
// --template docs` on an ALREADY-INITIALIZED folder (init.go:165) seeds into a
// folder whose contents came from the hub — a live project, with a team's
// AGENTS.md in it. An overwrite there is not a local accident: the next cycle
// journals it as this device's edit and last-writer-wins replaces the team's
// file for everyone.
//
// Asserted for every shipped template and every file in it.
func TestSec_Templates_SeedingNeverReplacesAProjectFile(t *testing.T) {
	for _, tpl := range sec11All(t) {
		t.Run(tpl.Name, func(t *testing.T) {
			folder := t.TempDir()
			const mine = "the team's real content, do not touch\n"
			for _, f := range tpl.Files {
				p := filepath.Join(folder, filepath.FromSlash(f.Path))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(mine), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			wrote, err := tpl.WriteTo(folder)
			if err != nil {
				t.Fatal(err)
			}
			if len(wrote) != 0 {
				t.Errorf("WriteTo reported writing %q into a folder that already had every "+
					"one of those paths", wrote)
			}
			for _, f := range tpl.Files {
				got, err := os.ReadFile(filepath.Join(folder, filepath.FromSlash(f.Path)))
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != mine {
					t.Errorf("seeding the %s template replaced %s — on the agent path that file "+
						"is the team's, and the next cycle pushes the replacement to everyone",
						tpl.Name, f.Path)
				}
			}
		})
	}
}

// TestSec_Templates_ADirectorySwappedForASymlinkMidSeedWritesNothingOutside
//
// Round 9 pinned a symlink that is ALREADY in the folder when WriteTo starts.
// WriteTo iterates the template's files in sorted order and re-checks nothing
// between them, so the other shape is a name that becomes a symlink WHILE it
// runs — the folder is a live project with a daemon materializing into it and,
// on the agent path, an agent session in it.
//
// This drives the same window deterministically: seed once (creating the
// directories), replace one seeded directory with a symlink pointing outside,
// then seed again — which is exactly what re-running `bdrive init --template`
// does.
func TestSec_Templates_ADirectorySwappedForASymlinkMidSeedWritesNothingOutside(t *testing.T) {
	tpl, err := Get("docs")
	if err != nil {
		t.Fatal(err)
	}
	folder := t.TempDir()
	outside := t.TempDir()
	if _, err := tpl.WriteTo(folder); err != nil {
		t.Fatal(err)
	}
	// Pick a directory the template owns and swap it.
	dirs := tpl.Dirs()
	if len(dirs) == 0 {
		t.Skip("the docs template has no directories")
	}
	victim := filepath.Join(folder, filepath.FromSlash(dirs[0]))
	if err := os.RemoveAll(victim); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, victim); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := tpl.WriteTo(folder); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("re-seeding the docs template wrote %q into %s, outside the project at %s — "+
			"a name under the seed root turned into a symlink between the two runs",
			names, outside, folder)
	}
}

// ---------------------------------------------------------------------------
// 4. The shipped CONTENT is instructions to every teammate's agent.
// ---------------------------------------------------------------------------

// TestSec_Templates_ShippedContentOnlyEverSaysWhereAFileGoes
//
// Every shipped template's AGENTS.md lands in a folder that syncs, and AGENTS.md
// is a file agent platforms load into context automatically, in every session,
// on every teammate's machine. A `bdrive init --template docs` by one person is
// therefore a write into the standing instructions of everyone else's agent.
//
// The package doc calls the AGENTS.md "the deliverable", so this content is
// meant to steer agents — the boundary is WHAT it may steer. Filing rules are
// the product. Anything that makes an agent run a command, reach the network,
// read outside the project, or touch credentials or bdrive's own state is not,
// and would ship to every user of the template in the next release.
//
// This is a content-diff alarm, not a parser: it fails when a future template
// edit crosses the line, which is when nobody is looking.
func TestSec_Templates_ShippedContentOnlyEverSaysWhereAFileGoes(t *testing.T) {
	banned := []string{
		// command execution
		"curl ", "wget ", "chmod ", "sudo ", "rm -rf", "| sh", "|sh", "| bash", "eval ",
		"npm install", "pip install", "go install",
		// credentials and bdrive's own state
		"BDRIVE_TOKEN", ".bdrive/", "~/.bdrive", "settings.json", "auth.json",
		".ssh", ".env", "id_rsa", "api key", "API key", "password", "secret key",
		// prompt-injection shapes. Deliberately narrow: these templates are
		// PROSE about filing, and a term common in ordinary English ("silently",
		// "disregard") fires on the guidance itself rather than on an attack.
		"ignore previous", "ignore the above", "ignore all prior",
		"system prompt", "without asking the user", "do not tell the user",
		"do not mention this",
		// reaching outside the folder
		"os.Getenv", "http://", "bdrive share", "bdrive login",
	}
	for _, tpl := range sec11All(t) {
		for _, f := range tpl.Files {
			lower := strings.ToLower(f.Content)
			for _, b := range banned {
				if !strings.Contains(lower, strings.ToLower(b)) {
					continue
				}
				t.Errorf("%s/%s contains %q — this file is seeded into a SYNCED folder and "+
					"read as standing instructions by every agent session of every teammate; "+
					"template content may say where a file goes and nothing else",
					tpl.Name, f.Path, b)
			}
		}
	}
}

// TestSec_Templates_ShippedContentCarriesNoHiddenText
//
// The same channel, one layer down. A directive an agent reads and a reviewer
// does not is the whole prompt-injection primitive, and bidi/zero-width runes
// are the form of it no editor and no code review shows: an agent tokenizes
// them, a human sees the line it is hiding in and nothing else.
//
// HTML comments are deliberately NOT flagged. The wiki template uses two of
// them to carry worked examples, and a comment is plainly visible in the raw
// markdown that both the agent and any reviewer of this repo read — it hides
// from the RENDERED page, not from review.
func TestSec_Templates_ShippedContentCarriesNoHiddenText(t *testing.T) {
	hidden := map[string]rune{
		"zero-width space":       '\u200b',
		"zero-width non-joiner":  '\u200c',
		"zero-width joiner":      '\u200d',
		"left-to-right override": '\u202d',
		"right-to-left override": '\u202e',
		"word joiner":            '\u2060',
		"BOM / ZWNBSP":           '\ufeff',
	}
	for _, tpl := range sec11All(t) {
		for _, f := range tpl.Files {
			for name, r := range hidden {
				if strings.ContainsRune(f.Content, r) {
					t.Errorf("%s/%s carries a %s (%U) — text a reviewer cannot see in a file "+
						"every teammate's agent reads", tpl.Name, f.Path, name, r)
				}
			}
		}
	}
}
