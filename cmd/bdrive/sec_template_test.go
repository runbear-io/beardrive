package main

// Round 14 — the CLIENT half of the `--template` channel.
//
// `bdrive init --template docs` is the one command in the product that asks a
// hub to author files and then puts them in the user's folder. Two questions
// this file answers with running code:
//
//  1. Can the hub's answer choose what lands on THIS disk? (It cannot — the
//     content comes from the client's own embedded registry, and the hub's
//     `template` field only toggles whether the client writes it. Asserted so
//     it stays that way.)
//  2. Does the hub's answer reach the terminal an onboarding agent is reading?
//     (It does, raw — scoreboard row 21.)
//
// The hub here is a fixture the test controls, for the same reason
// sec_init_test.go's is: `templates.Get` guards the CREATION path on a stock
// hub, so the only way to ask what the client does with a `template` value it
// should never have seen is to hand it one.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode"
)

// ---------------------------------------------------------------- harness

// sec14tOpts is what the fixture hub answers POST /api/projects with.
type sec14tOpts struct {
	template string // the `template` field on the project it hands back
	created  bool   // whether it reports the project as newly created
}

type sec14tEnv struct {
	bin, url, home string
	env            []string

	mu     sync.Mutex
	stored map[string][]byte
}

// sec14tHub starts a fixture hub with auth off, so init runs its whole flow
// without a login dance, and an isolated HOME/BDRIVE_HOME.
func sec14tHub(t *testing.T, opts sec14tOpts) *sec14tEnv {
	t.Helper()
	e := &sec14tEnv{bin: secinitBinary(t), stored: map[string][]byte{}}

	const id = "3f9c11a2-0b64-4e77-9a35-51d7c2ee40b1"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSONTo(w, map[string]any{"mode": "hub", "auth": map[string]any{"enabled": false}})
	})
	project := func(name string) map[string]any {
		return map[string]any{"id": id, "name": name, "template": opts.template}
	}
	mux.HandleFunc("/api/projects", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSONTo(w, map[string]any{"projects": []any{project("listed")}})
			return
		}
		var body struct{ Name string }
		json.NewDecoder(r.Body).Decode(&body)
		writeJSONTo(w, map[string]any{"project": project(body.Name), "created": opts.created})
	})
	mux.HandleFunc("/api/projects/", func(w http.ResponseWriter, r *http.Request) {
		writeJSONTo(w, project(strings.TrimPrefix(r.URL.Path, "/api/projects/")))
	})
	// An empty, writable project: the hub seeds NOTHING, which is exactly the
	// case a client that trusts `template` blindly gets wrong.
	mux.HandleFunc("/api/p/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/store/list"):
			writeJSONTo(w, map[string]any{"objects": []any{}})
		case strings.HasSuffix(r.URL.Path, "/store/exists"):
			writeJSONTo(w, map[string]any{"exists": false})
		case strings.HasSuffix(r.URL.Path, "/store/sign"):
			writeJSONTo(w, map[string]any{"mode": "server"})
		case strings.HasSuffix(r.URL.Path, "/store/object"):
			key := r.URL.Query().Get("key")
			if r.Method == http.MethodGet {
				http.Error(w, "no such object", http.StatusNotFound)
				return
			}
			buf := make([]byte, 0, 1024)
			b := make([]byte, 4096)
			for {
				n, err := r.Body.Read(b)
				buf = append(buf, b[:n]...)
				if err != nil {
					break
				}
			}
			e.mu.Lock()
			e.stored[key] = buf
			e.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "no route", http.StatusNotFound)
		}
	})
	hub := httptest.NewServer(mux)
	t.Cleanup(hub.Close)

	home := t.TempDir()
	e.url, e.home = hub.URL, home
	e.env = append(secinitEnvWithout("HOME", "BDRIVE_HOME", "BDRIVE_TOKEN", "XDG_CONFIG_HOME"),
		"HOME="+home, "BDRIVE_HOME="+filepath.Join(home, ".bdrive"),
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	return e
}

// run executes the real binary in dir.
func (e *sec14tEnv) run(dir string, args ...string) (string, error) {
	cmd := exec.Command(e.bin, args...)
	cmd.Dir = dir
	cmd.Env = e.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// sec14tInit runs the one command the onboarding runbook publishes, in a fresh
// empty folder, and returns the folder plus everything the command printed.
func sec14tInit(t *testing.T, e *sec14tEnv, name string, extra ...string) (string, string, error) {
	t.Helper()
	folder := filepath.Join(t.TempDir(), "notes")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"init", "--server", e.url, "--name", name,
		"--yes", "--no-hooks", "--no-autostart"}, extra...)
	out, err := e.run(folder, args...)
	// init detaches a daemon on success; stop it so it cannot race the
	// assertions or outlive the test's temp HOME.
	t.Cleanup(func() { e.run(folder, "stop") })
	return folder, out, err
}

// ---------------------------------------------------------------------------
// The hub's answer must not choose what is written to this disk.
// ---------------------------------------------------------------------------

// TestSec_Template_AHubsAnswerCannotChooseWhatIsSeededOnDisk.
//
// Assignment question, asked of the code: can a hub name arbitrary content, or
// a template the client has never heard of, and have it written into the
// user's folder by `--template`?
//
// It cannot, and this pins the reason. init resolves the template from the
// FLAG against its own go:embed registry, before any network call:
//
//	if tpl, err = templates.Get(template); err != nil { return err }   // init.go:128
//
// and `seedLocally(folder, tpl)` (init.go:299) writes that value. The hub's
// `template` string is only ever compared (init.go:228, init.go:292) — it
// never selects content and never names a path. So a hub answering with a
// template the client has never heard of gets, at worst, the client's own
// docs skeleton and no hub-chosen byte on disk.
//
// This is the property that keeps a hostile hub's reach over this door limited
// to what it can put in the project's STORAGE — where the ordinary sync guards
// (journal.SafePath, config.ReservedPath, materialize) already apply. Losing
// it would make `--template` a direct file-write primitive.
func TestSec_Template_AHubsAnswerCannotChooseWhatIsSeededOnDisk(t *testing.T) {
	// The hub claims the project was created from a template the client's
	// registry does not contain, and names paths it would like written.
	e := sec14tHub(t, sec14tOpts{created: true, template: "../../../../etc/cron.d/pwned"})
	folder, out, err := sec14tInit(t, e, "notes", "--template", "docs")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	// Control: the client's OWN docs template is what landed.
	if _, statErr := os.Stat(filepath.Join(folder, "AGENTS.md")); statErr != nil {
		t.Fatalf("control: `--template docs` seeded no AGENTS.md (%v)\n%s", statErr, out)
	}
	if _, statErr := os.Stat(filepath.Join(folder, "decisions")); statErr != nil {
		t.Fatalf("control: `--template docs` seeded no decisions/ (%v)\n%s", statErr, out)
	}
	// Nothing the hub named exists, at any depth, anywhere near the folder.
	for _, bad := range []string{
		filepath.Join(folder, "..", "..", "..", "..", "etc", "cron.d", "pwned"),
		filepath.Join(folder, "etc"),
		filepath.Join(e.home, "etc"),
	} {
		if _, statErr := os.Stat(bad); statErr == nil {
			t.Fatalf("the hub's `template` value became a path on disk: %s exists", bad)
		}
	}
	// And it did not silently become PARA either.
	if _, statErr := os.Stat(filepath.Join(folder, "archives")); statErr == nil {
		t.Fatalf("the hub's answer chose the structure written on disk")
	}
}

// TestSec_Template_AHubThatSeedsNothingStillLeavesTheStructureItPromised.
//
// The other side of the same coin, and the reason the client compares at all.
// When the hub reports the template it was asked for, init prints
//
//	"start:   docs template (seeded on the hub)"
//
// and writes nothing locally (init.go:292) — the first cycle is supposed to
// pull it. This fixture hub reports `template: "docs"` and seeds NOTHING, so
// the folder ends up empty while the command reports success.
//
// That is a hub withholding its own content, which no client can prevent; the
// security-relevant part is only that the client must not claim more than it
// knows. The assertion is the honest one: either the structure is there, or
// the output does not tell the user it is.
func TestSec_Template_AHubThatSeedsNothingStillLeavesTheStructureItPromised(t *testing.T) {
	e := sec14tHub(t, sec14tOpts{created: true, template: "docs"})
	folder, out, err := sec14tInit(t, e, "notes", "--template", "docs")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	_, statErr := os.Stat(filepath.Join(folder, "AGENTS.md"))
	if statErr == nil {
		return // the structure is there — nothing to report
	}
	if strings.Contains(out, "seeded on the hub") {
		t.Fatalf("init reported the docs structure was seeded, and the folder is "+
			"empty — the hub said `template: \"docs\"` and stored nothing. The "+
			"client prints the hub's claim as fact (init.go:292) without ever "+
			"looking at what arrived.\nfolder: %v\noutput:\n%s", statErr, out)
	}
}

// ---------------------------------------------------------------------------
// Scoreboard row 21: peer-controlled strings reaching a terminal.
// ---------------------------------------------------------------------------

// TestSec_Template_TheHubsTemplateNameIsNotRenderedRawToTheTerminal.
//
// init.go:228-234, the refusal that fires when `--template` names a project
// that already exists:
//
//	if tpl.Name != "" && !created && p.Template != tpl.Name {
//		from := "an empty project"
//		if p.Template != "" {
//			from = "the " + p.Template + " template"
//		}
//		return fmt.Errorf("project %q already exists and was created from %s\n"+
//			"a template only applies to a new project; ...", p.Name, from)
//	}
//
// `p.Name` reaches the terminal through safeField everywhere else in this
// command (init.go:289). `p.Template` — the same JSON object, the same hub,
// the same trust — is concatenated raw into the error text and printed.
//
// Both values the guard needs are hub-chosen: it answers `created: false` and
// any `template` string it likes. Round 7 established this class as scoreboard
// row 21 and closed it for `bdrive login`, `status`, `log` and
// `restore --list`; this is the same primitive on the front door, which is the
// surface an onboarding agent reads verbatim — INSTALL_FOR_AGENTS.md §5 tells
// it to "read init's output" and summarize it to the user.
//
// The secure behavior asserted is the one the rest of the file already chose:
// no control character, C1 escape or bidi override reaches the terminal.
func TestSec_Template_TheHubsTemplateNameIsNotRenderedRawToTheTerminal(t *testing.T) {
	// Control: an ordinary hub-chosen template name IS printed, so the
	// assertions below are looking at output the command actually produced.
	t.Run("control_ordinary_name", func(t *testing.T) {
		e := sec14tHub(t, sec14tOpts{created: false, template: "para"})
		_, out, err := sec14tInit(t, e, "notes", "--template", "docs")
		if err == nil {
			t.Fatalf("control: init did not refuse a template on an existing "+
				"project:\n%s", out)
		}
		if !strings.Contains(out, "the para template") {
			t.Fatalf("control: the hub's template name never reached the "+
				"terminal:\n%s", out)
		}
	})

	for _, hostile := range sec7Hostile {
		t.Run(sec7Label(hostile), func(t *testing.T) {
			e := sec14tHub(t, sec14tOpts{created: false, template: hostile})
			_, out, err := sec14tInit(t, e, "notes", "--template", "docs")
			if err == nil {
				t.Fatalf("fixture: init did not refuse:\n%s", out)
			}
			if bad, ok := sec14tUnsafe(out); ok {
				t.Fatalf("`bdrive init --template docs` rendered the hub's "+
					"`template` string to the terminal with %s intact.\n"+
					"the hub answered: %q\n"+
					"init printed:     %q\n"+
					"p.Name goes through safeField one screen later "+
					"(init.go:289); p.Template (init.go:231) does not.",
					bad, hostile, out)
			}
		})
	}
}

// sec14tUnsafe reports the first character in s that safeField would have
// stripped: a C0/DEL control, a C1 control, or a Unicode format character.
// Newline and tab are the shell's own, and every other CLI surface in this
// package is measured by the same rule.
func sec14tUnsafe(s string) (string, bool) {
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t' || r == '\r':
			continue
		case r < 0x20, r == 0x7f:
			return fmt.Sprintf("C0 control %#U", r), true
		case r >= 0x80 && r <= 0x9f:
			return fmt.Sprintf("C1 control %#U", r), true
		case unicode.Is(unicode.Cf, r), r >= 0xe0000 && r <= 0xe01ef:
			return fmt.Sprintf("format character %#U", r), true
		}
	}
	return "", false
}
