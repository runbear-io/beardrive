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

// scopeLines renders the managed block for dirs (already cleaned). The folder
// name is a NAME; the line is a PATTERN, so it goes through the dialect's own
// escape — the same one `bdrive forget` uses. Unescaped, `scope add 'a*'`
// un-ignores every sibling the glob happens to match, and `scope add 'a\b'`
// un-ignores `ab/` instead of the folder the user named.
func scopeLines(dirs []string) []string {
	lines := []string{scopeStart, "/*"}
	for _, d := range dirs {
		lines = append(lines, "!/"+syncer.EscapeIgnore(d)+"/")
	}
	return append(lines, scopeEnd)
}

// unescapeIgnore is EscapeIgnore's inverse, so `scope rm` can match what
// `scope add` wrote.
func unescapeIgnore(rule string) string {
	var b strings.Builder
	for i := 0; i < len(rule); i++ {
		if rule[i] == '\\' && i+1 < len(rule) {
			i++
		}
		b.WriteByte(rule[i])
	}
	return b.String()
}

// scopeBlock locates the managed block in lines: the FIRST complete
// start/end pair, returning the index of each marker.
//
// .bdriveignore syncs, so its bytes are chosen by any project member and both
// markers are lines a teammate can write. Treating every occurrence as
// authoritative meant one comment-shaped line turned the next teammate's
// `scope add` into a wipe of every rule below it, and a lone end marker made
// the scope read as "scoped, zero folders". So: an end marker that opens
// nothing is an ordinary line, an unterminated start is an ordinary line, and
// only what lies between the first matched pair is managed. Two complete
// blocks are genuinely ambiguous — which one is in force decides what leaves
// the machine — so that is refused rather than guessed.
func scopeBlock(lines []string) (start, end int, found bool, err error) {
	open := -1
	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case scopeStart:
			if open < 0 {
				open = i
			}
		case scopeEnd:
			if open < 0 {
				continue
			}
			if found {
				return 0, 0, false, fmt.Errorf("%s holds two bdrive scope blocks; delete the one that is not yours and re-run", syncer.IgnoreFile)
			}
			start, end, found, open = open, i, true, -1
		}
	}
	return start, end, found, nil
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
	lines := strings.Split(string(data), "\n")
	start, end, found, err := scopeBlock(lines)
	if err != nil || !found {
		return nil, false, err
	}
	var dirs []string
	for _, line := range lines[start+1 : end] {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "!") {
			dirs = append(dirs, unescapeIgnore(strings.Trim(strings.TrimPrefix(t, "!"), "/")))
		}
	}
	return dirs, true, nil
}

// writeScopeDirs inserts or replaces the managed block. Passing no dirs
// removes it, which widens the mount back to the whole folder.
func writeScopeDirs(folder string, dirs []string) error {
	path := filepath.Join(folder, syncer.IgnoreFile)
	var lines []string
	if data, err := os.ReadFile(path); err == nil {
		lines = strings.Split(string(data), "\n")
	} else if !os.IsNotExist(err) {
		return err
	}
	// Only the managed block is rewritten; every other line is the team's, and
	// a scope edit that drops one pushes whatever it was keeping off the hub.
	start, end, found, err := scopeBlock(lines)
	if err != nil {
		return err
	}
	kept := lines
	if found {
		kept = append(append([]string{}, lines[:start]...), lines[end+1:]...)
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

// mkdirScopeDirs creates the folders a scope names — the one door all three
// scope-writing paths (`scope add`, `init --only`, `init` on a new folder) go
// through. cleanScopeDirs judges the name's SPELLING; MkdirAll follows
// symlinks, so a link already sitting in the folder takes the creation
// wherever it points, and the project boundary is a question about the disk.
func mkdirScopeDirs(folder string, dirs []string) error {
	for _, d := range dirs {
		abs := filepath.Join(folder, filepath.FromSlash(d))
		if !store.UnderRoot(folder, abs) {
			return fmt.Errorf("%q resolves outside this project (a symlink in the folder points elsewhere)", d)
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return err
		}
	}
	return nil
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
