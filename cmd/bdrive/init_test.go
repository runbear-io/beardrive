package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Init registers agent sync hooks itself (a separate `bdrive hooks install`
// is another permission prompt), and they go in the platform's USER config —
// never inside the project, which would sync them to the whole team.
func TestInstallAgentHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // keep detection and writes off the real home dir
	folder := t.TempDir()
	if err := os.Mkdir(filepath.Join(folder, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	installAgentHooks(folder)

	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("hooks not written to the user config: %v", err)
	}
	if !strings.Contains(string(data), "bdrive sync") {
		t.Fatalf("user settings.json missing bdrive sync hook: %s", data)
	}
	if _, err := os.Stat(filepath.Join(folder, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("init wrote hooks into the project (stat err: %v)", err)
	}
}

// Hooks an older version left in the project are cleaned up on install, so a
// machine never runs both copies (which would double-count agent reads).
func TestInstallAgentHooksMigratesProjectHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	folder := t.TempDir()
	projCfg := filepath.Join(folder, ".claude", "settings.json")
	if err := os.Mkdir(filepath.Join(folder, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := `{"hooks":{"UserPromptSubmit":[` +
		`{"hooks":[{"type":"command","command":"sh -c 'bdrive sync .'"}]},` +
		`{"hooks":[{"type":"command","command":"echo mine"}]}]}}`
	if err := os.WriteFile(projCfg, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	installAgentHooks(folder)

	data, err := os.ReadFile(projCfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "bdrive sync") {
		t.Fatalf("stale project hook survived the migration: %s", data)
	}
	if !strings.Contains(string(data), "echo mine") {
		t.Fatalf("migration removed a hook that was not ours: %s", data)
	}
}

func TestScopeRemove(t *testing.T) {
	for _, tc := range []struct {
		dirs []string
		args []string
		want []string
		err  bool
	}{
		{dirs: []string{"wiki", "docs"}, args: []string{"docs"}, want: []string{"wiki"}},
		{dirs: []string{"wiki", "docs"}, args: []string{"docs/"}, want: []string{"wiki"}},  // normalized match
		{dirs: []string{"wiki", "docs"}, args: []string{"./docs"}, want: []string{"wiki"}}, // as typed
		{dirs: []string{"wiki", "docs", "notes"}, args: []string{"wiki", "docs"}, want: []string{"notes"}},
		{dirs: []string{"wiki", "docs"}, args: []string{"notes"}, err: true}, // not in scope
	} {
		got, err := scopeRemove(tc.dirs, tc.args)
		if tc.err != (err != nil) {
			t.Errorf("scopeRemove(%q, %q) err = %v, want err %v", tc.dirs, tc.args, err, tc.err)
			continue
		}
		if !tc.err && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("scopeRemove(%q, %q) = %q, want %q", tc.dirs, tc.args, got, tc.want)
		}
	}
}

func TestCleanScopeDirs(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want []string
		err  bool
	}{
		{in: nil, want: nil},
		{in: []string{"wiki"}, want: []string{"wiki"}},
		{in: []string{"wiki", "docs"}, want: []string{"wiki", "docs"}},
		{in: []string{" wiki ", "./docs/", "wiki"}, want: []string{"wiki", "docs"}}, // trimmed, cleaned, deduped
		{in: []string{"a/b"}, want: []string{"a/b"}},
		{in: []string{""}, err: true},
		{in: []string{"wiki", ""}, err: true}, // "wiki,,docs" typo must not half-apply
		{in: []string{"."}, err: true},        // would silently mean whole-folder sync
		{in: []string{"../up"}, err: true},
	} {
		got, err := cleanScopeDirs(tc.in)
		if tc.err != (err != nil) {
			t.Errorf("cleanScopeDirs(%q) err = %v, want err %v", tc.in, err, tc.err)
			continue
		}
		if !tc.err && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("cleanScopeDirs(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The managed block is machine-written and machine-edited: it must round-trip
// and must not disturb the ordinary rules around it.
func TestScopeBlockRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".bdriveignore"), []byte("node_modules/\n*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeScopeDirs(dir, []string{"wiki", "docs"}); err != nil {
		t.Fatal(err)
	}
	dirs, scoped, err := readScopeDirs(dir)
	if err != nil || !scoped || !reflect.DeepEqual(dirs, []string{"wiki", "docs"}) {
		t.Fatalf("readScopeDirs = %q, %v, %v", dirs, scoped, err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, ".bdriveignore"))
	for _, want := range []string{"/*", "!/wiki/", "!/docs/", "node_modules/", "*.log"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("ignore file lost %q:\n%s", want, body)
		}
	}
	// Rewriting replaces the block rather than stacking a second one.
	if err := writeScopeDirs(dir, []string{"wiki"}); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(filepath.Join(dir, ".bdriveignore"))
	if n := strings.Count(string(body), scopeStart); n != 1 {
		t.Fatalf("expected exactly one managed block, got %d:\n%s", n, body)
	}
	if strings.Contains(string(body), "!/docs/") {
		t.Fatalf("removed folder still in the block:\n%s", body)
	}
	// Removing it entirely widens back to the whole folder.
	if err := writeScopeDirs(dir, nil); err != nil {
		t.Fatal(err)
	}
	if _, scoped, _ := readScopeDirs(dir); scoped {
		t.Fatal("block survived an empty write")
	}
	body, _ = os.ReadFile(filepath.Join(dir, ".bdriveignore"))
	if !strings.Contains(string(body), "node_modules/") {
		t.Fatalf("ordinary rules lost when the block was removed:\n%s", body)
	}
}

// Agents (and people) type hosts without a scheme. Accepting that is the
// difference between one command and a failed command plus a retry.
func TestNormalizeServer(t *testing.T) {
	for in, want := range map[string]string{
		"https://hub.example.com":    "https://hub.example.com",
		"http://localhost:8993":      "http://localhost:8993",
		"hub.example.com":            "https://hub.example.com",
		"hub.example.com:4173":       "https://hub.example.com:4173",
		"hub.example.com/":           "https://hub.example.com",
		"localhost:8993":             "http://localhost:8993",
		"127.0.0.1:8993":             "http://127.0.0.1:8993",
		" https://hub.example.com/ ": "https://hub.example.com",
	} {
		if got := normalizeServer(in); got != want {
			t.Errorf("normalizeServer(%q) = %q, want %q", in, got, want)
		}
	}
}
