package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
)

// A mount always covers exactly the folder it was initialized in. When only
// some of that folder's subfolders should sync, the narrowing lives in
// .bdriveignore as a managed block of negation rules — the same file, and the
// same syntax, as every other rule the project shares. There is no second
// scope mechanism: `bdrive init --only` and `bdrive scope` both write here.
//
// The block goes at the top of the file so ordinary rules after it still
// apply (matching is last-match-wins), which keeps `node_modules/` excluded
// inside a scoped folder.
const (
	scopeStart = "# bdrive scope — only these folders sync (managed by bdrive; change with `bdrive scope add/rm`)"
	scopeEnd   = "# end bdrive scope"
)

// scopeLines renders the managed block for dirs (already cleaned).
func scopeLines(dirs []string) []string {
	lines := []string{scopeStart, "/*"}
	for _, d := range dirs {
		lines = append(lines, "!/"+d+"/")
	}
	return append(lines, scopeEnd)
}

// readScopeDirs returns the folders named by the managed block, if any.
func readScopeDirs(folder string) ([]string, bool, error) {
	data, err := os.ReadFile(filepath.Join(folder, syncer.IgnoreFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var dirs []string
	in := false
	for _, line := range strings.Split(string(data), "\n") {
		switch t := strings.TrimSpace(line); {
		case t == scopeStart:
			in = true
		case t == scopeEnd:
			return dirs, true, nil
		case in && strings.HasPrefix(t, "!"):
			dirs = append(dirs, strings.Trim(strings.TrimPrefix(t, "!"), "/"))
		}
	}
	return nil, false, nil // an unterminated block is not a block
}

// writeScopeDirs inserts or replaces the managed block. Passing no dirs
// removes it, which widens the mount back to the whole folder.
func writeScopeDirs(folder string, dirs []string) error {
	path := filepath.Join(folder, syncer.IgnoreFile)
	var kept []string
	if data, err := os.ReadFile(path); err == nil {
		in := false
		for _, line := range strings.Split(string(data), "\n") {
			switch t := strings.TrimSpace(line); {
			case t == scopeStart:
				in = true
			case t == scopeEnd:
				in = false
			case !in:
				kept = append(kept, line)
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	// Drop the blank line the old block left behind, then re-join.
	for len(kept) > 0 && strings.TrimSpace(kept[0]) == "" {
		kept = kept[1:]
	}
	var out []string
	if len(dirs) > 0 {
		out = append(scopeLines(dirs), "")
	}
	out = append(out, kept...)
	body := strings.Join(out, "\n")
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return store.WriteFileAtomic(path, []byte(body), 0o644)
}

// cleanScopeDirs normalizes and validates the folder names a scope names.
func cleanScopeDirs(dirs []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, d := range dirs {
		d = strings.Trim(strings.TrimSpace(d), "/")
		if d == "" {
			return nil, fmt.Errorf("empty folder name in the scope list")
		}
		d = filepath.ToSlash(filepath.Clean(d))
		if d == "." || d == ".." || strings.HasPrefix(d, "../") {
			return nil, fmt.Errorf("%q is not a subfolder of this project", d)
		}
		if hasControlRune(d) {
			return nil, fmt.Errorf("folder name contains a control character: %q", d)
		}
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out, nil
}

// hasControlRune reports whether s carries a character that must never end up
// in a .bdriveignore line. One argument is one rule: a newline is a legal byte
// in a unix path, and the file syncs — so an argument carrying one writes
// extra rules on every teammate's device (`*` there ignores the whole
// project), and `bdrive forget` appends outside the managed block, where
// nothing can ever take them out again. Same rule for a scope folder name,
// whose extra lines would land after the block's end marker.
func hasControlRune(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
