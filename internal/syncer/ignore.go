package syncer

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
}

type pattern struct {
	re     *regexp.Regexp
	negate bool
}

// loadFilter builds the filter for a folder from its .bdriveignore (if any)
// plus the include list from the .bdrive settings file.
func loadFilter(folder string, include []string) (*Filter, error) {
	f := &Filter{}
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

// compile turns one pattern line into a regexp over slash-separated paths.
// The regexp also matches everything under a matched directory. Returns
// ok=false for blanks, comments, and invalid patterns.
func compile(line string) (pattern, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return pattern{}, false
	}
	var p pattern
	if strings.HasPrefix(line, "!") {
		p.negate = true
		line = strings.TrimSpace(line[1:])
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
	if f.ignoredFile(rel) {
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
