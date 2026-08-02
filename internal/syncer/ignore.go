package syncer

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/runbear-io/beardrive/internal/config"
)

// IgnoreFile is the per-folder opt-out list at the mount root. It uses a
// gitignore-style syntax and, unlike the .bdrive settings file, syncs like any
// other file so every device shares the same rules.
const IgnoreFile = ".bdriveignore"

// Filter decides which paths sync. A path syncs when it is not ignored and,
// if an include list is set, matches at least one include pattern.
//
// Pattern syntax (a practical gitignore subset): one pattern per line,
// blank lines and #-comments skipped, `!` re-includes, a trailing `/`
// matches directories only, a `/` anywhere else anchors the pattern to the
// mount root (otherwise it matches at any depth), `*` matches within a path
// segment, `**` across segments, `?` a single character.
type Filter struct {
	ignore  []pattern
	include []pattern
	negated bool // any `!` rules → directory pruning is unsafe

	// nested holds subdirectories that are BearDrive mounts of their own
	// (they contain .bdrive/config.json), discovered during the scan walk.
	// A nested mount syncs through its own project: the parent never scans
	// into it, never materializes over it, and drops cached paths under it
	// without a delete op (same posture as newly ignored paths).
	nested []string
	// root is the mount folder, so the boundary can be resolved ON DISK
	// rather than only from what the scan walk happened to discover. The
	// walk stops at a pruned directory, so a nested mount inside one was
	// never discovered — and one pulled .bdriveignore that un-ignores that
	// directory then let this project write into another project's folder,
	// whose own daemon pushes the file on to ITS members. A project boundary
	// cannot depend on the ignore rules that were in force during the scan.
	root string
	// nestedMiss memoizes directories already checked and found not to be
	// mounts, so the disk check costs one stat per directory per cycle.
	nestedMiss map[string]bool
}

// addNestedMount records a nested mount root (slash-relative to the parent
// mount) so Skip excludes everything under it for the rest of the cycle.
func (f *Filter) addNestedMount(rel string) {
	f.nested = append(f.nested, rel+"/")
}

func (f *Filter) underNestedMount(rel string) bool {
	for _, root := range f.nested {
		if strings.HasPrefix(rel, root) {
			return true
		}
	}
	return f.underMountOnDisk(rel)
}

// underMountOnDisk asks the filesystem whether any ancestor directory of rel
// is a mount of its own. This is the authoritative form of the question: the
// nested list is only what a walk under one particular set of rules found.
func (f *Filter) underMountOnDisk(rel string) bool {
	if f.root == "" {
		return false
	}
	dir := path.Dir(rel)
	for dir != "." && dir != "/" && dir != "" {
		if f.nestedMiss[dir] {
			dir = path.Dir(dir)
			continue
		}
		if config.IsMount(filepath.Join(f.root, filepath.FromSlash(dir))) {
			f.addNestedMount(dir) // remembered for the rest of the cycle
			return true
		}
		if f.nestedMiss == nil {
			f.nestedMiss = map[string]bool{}
		}
		f.nestedMiss[dir] = true
		dir = path.Dir(dir)
	}
	return false
}

type pattern struct {
	re     *regexp.Regexp
	negate bool
}

// LoadFilter builds the filter for a folder from its .bdriveignore (if any)
// plus the include list from the .bdrive settings file — the exact rules the
// sync cycle applies, for callers outside the cycle (e.g. `bdrive read-log`
// deciding whether an agent-read path is part of the project).
func LoadFilter(folder string, include []string) (*Filter, error) {
	return loadFilter(folder, include)
}

// loadFilter builds the filter for a folder from its .bdriveignore (if any)
// plus the include list from the .bdrive settings file.
func loadFilter(folder string, include []string) (*Filter, error) {
	f := &Filter{root: folder}
	for _, line := range include {
		if p, ok := compile(line); ok {
			f.include = append(f.include, p)
		}
	}
	data, err := os.ReadFile(filepath.Join(folder, IgnoreFile))
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		p, ok := compile(line)
		if !ok {
			continue
		}
		f.ignore = append(f.ignore, p)
		if p.negate {
			f.negated = true
		}
	}
	return f, nil
}

// EscapeIgnore turns a literal project-relative path into a rule line that
// matches that path and nothing else. It is compile's inverse and lives beside
// it on purpose: a caller that spells the escaping itself is a second
// definition of the dialect, which is how `bdrive forget <name>` came to write
// a glob, a negation or a comment depending on what a teammate had named a
// file.
func EscapeIgnore(rel string) string {
	var b strings.Builder
	for i := 0; i < len(rel); i++ {
		switch c := rel[i]; c {
		case '\\', '*', '?':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '!', '#':
			// Only meaningful at the start of a line, but escaping them
			// everywhere costs nothing and needs no positional reasoning.
			b.WriteByte('\\')
			b.WriteByte(c)
		case ' ', '\t', '\r', '\n', '\v', '\f':
			// Whitespace is a metacharacter here too: compile trims it off
			// both ends, so "notes/a " unescaped is the rule for "notes/a" —
			// a DIFFERENT file, a sibling, which `bdrive forget` then prunes
			// from the hub. A trailing space is a legal filename everywhere
			// beardrive runs, and filenames in a synced project are chosen by
			// whoever syncs into it. Escaping it everywhere keeps the same
			// no-positional-reasoning rule as `!` and `#`.
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// trimRuleSpace strips the padding around a rule line, EXCEPT whitespace a
// backslash protects — the dialect's own escape, and the only way EscapeIgnore
// can write a filename that legally ends in a space. Trimming it anyway made
// the rule name the sibling one character shorter.
func trimRuleSpace(line string) string {
	line = strings.TrimLeft(line, " \t\r\n\v\f")
	for len(line) > 0 && strings.ContainsRune(" \t\r\n\v\f", rune(line[len(line)-1])) {
		// An odd run of backslashes before the byte escapes it.
		n := 0
		for n < len(line)-1 && line[len(line)-2-n] == '\\' {
			n++
		}
		if n%2 == 1 {
			break
		}
		line = line[:len(line)-1]
	}
	return line
}

// compile turns one pattern line into a regexp over slash-separated paths.
// The regexp also matches everything under a matched directory. Returns
// ok=false for blanks, comments, and invalid patterns.
func compile(line string) (pattern, bool) {
	line = trimRuleSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return pattern{}, false
	}
	var p pattern
	if strings.HasPrefix(line, "!") {
		p.negate = true
		line = trimRuleSpace(line[1:])
	}
	anchored := strings.HasPrefix(line, "/")
	dirOnly := strings.HasSuffix(line, "/")
	line = strings.Trim(line, "/")
	if line == "" {
		return pattern{}, false
	}
	anchored = anchored || strings.Contains(line, "/")

	var b strings.Builder
	if anchored {
		b.WriteString("^")
	} else {
		b.WriteString("(^|.*/)")
	}
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\\':
			// gitignore's escape, and the only way a path can be written as a
			// rule that means itself. Without it every metacharacter a
			// filename may legally contain (`*`, `?`, a leading `!` or `#`) is
			// a wildcard, a negation or a comment — and `bdrive forget` turns
			// a filename into a rule and then prunes the hub in the same
			// command. Filenames in a synced project are chosen by any
			// teammate. A trailing backslash is a literal one.
			if i+1 < len(line) {
				i++
			}
			b.WriteString(regexp.QuoteMeta(line[i : i+1]))
		case '*':
			if i+1 < len(line) && line[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(line[i : i+1]))
		}
	}
	if dirOnly {
		b.WriteString("/.*$") // must match something *inside* the directory
	} else {
		b.WriteString("(/.*)?$")
	}
	re, err := regexp.Compile(b.String())
	if err != nil {
		return pattern{}, false
	}
	p.re = re
	return p, true
}

// Skip reports whether a file path should not sync.
func (f *Filter) Skip(rel string) bool {
	if rel == IgnoreFile {
		return false // the ignore file itself always syncs so devices share rules
	}
	if f.underNestedMount(rel) || f.ignoredFile(rel) {
		return true
	}
	if len(f.include) == 0 {
		return false
	}
	for _, p := range f.include {
		if p.re.MatchString(rel) {
			return false
		}
	}
	return true
}

// Negated reports whether any `!` rule is in play. Scope narrowing is
// written as negation rules, so this is how callers tell "these rules
// exclude a few things" from "these rules exclude everything but a few
// things" — the difference between a safe prune and a destructive one.
func (f *Filter) Negated() bool { return f.negated }

// PruneDir reports whether a whole directory can be skipped during the
// scan walk. Pruning is conservative: never with `!` rules (a child could
// be re-included) or an include list (a deep child could match).
func (f *Filter) PruneDir(rel string) bool {
	if f.negated || len(f.include) > 0 {
		return false
	}
	return f.ignoredFile(rel + "/")
}

// ignoredFile applies the ignore rules in order; the last match wins, so
// `!` patterns can re-include what an earlier pattern excluded.
func (f *Filter) ignoredFile(rel string) bool {
	ignored := false
	for _, p := range f.ignore {
		if p.re.MatchString(rel) {
			ignored = !p.negate
		}
	}
	return ignored
}
