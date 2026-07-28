package syncer

import (
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Entry is one line of the not-synced list. Path ends in "/" when a whole
// directory collapsed to a single line; Files is how many files it holds.
type Entry struct {
	Path   string
	Files  int
	Nested bool // syncs through its own project — not excluded
}

// IsDir reports whether the entry stands for a whole directory rather than a
// single file.
func (e Entry) IsDir() bool { return strings.HasSuffix(e.Path, "/") }

// Explain reports what the sync cycle would and would not send for a folder.
// It is a pure read: no Session, no volume lock, no network, no writes — the
// answer comes from the same walk the cycle itself uses, so it cannot drift.
func Explain(folder string, include []string) (synced []string, notSynced []Entry, err error) {
	// A fresh filter: addNestedMount mutates it during the walk, so this must
	// never be shared with a live cycle.
	filter, err := loadFilter(folder, include)
	if err != nil {
		return nil, nil, err
	}

	var skipped []string
	dirs := map[string]*Entry{}
	var dirOrder []string     // walk order: parents before children
	keep := map[string]bool{} // dir holds something that must stay visible
	count := map[string]int{} // files under a dir that do not sync

	err = walkFolder(folder, filter, func(abs, rel string, d fs.DirEntry, v verdict) error {
		switch v {
		case vSync:
			synced = append(synced, rel)
			for _, a := range ancestors(rel) {
				keep[a] = true
			}
		case vSkipFile:
			skipped = append(skipped, rel)
			for _, a := range ancestors(rel) {
				count[a]++
			}
		case vDescend:
			dirs[rel] = &Entry{Path: rel + "/"}
			dirOrder = append(dirOrder, rel)
		case vPruneDir:
			n := countFiles(abs)
			dirs[rel] = &Entry{Path: rel + "/", Files: n}
			dirOrder = append(dirOrder, rel)
			for _, a := range ancestors(rel) {
				count[a] += n
			}
		case vNested:
			// Not excluded — it syncs through its own project, so its files
			// are not counted as "do not sync" and its parents stay visible
			// rather than collapsing the annotation away.
			dirs[rel] = &Entry{Path: rel + "/", Nested: true}
			dirOrder = append(dirOrder, rel)
			for _, a := range ancestors(rel) {
				keep[a] = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	// Collapse: a directory with nothing to show individually prints as one
	// counted line, and everything under it is dropped. Parents come first in
	// walk order, so the topmost such directory wins.
	collapsed := map[string]bool{}
	for _, rel := range dirOrder {
		if keep[rel] || underCollapsed(rel, collapsed) {
			continue
		}
		collapsed[rel] = true
		e := dirs[rel]
		if !e.Nested && e.Files == 0 {
			e.Files = count[rel]
		}
		notSynced = append(notSynced, *e)
	}
	for _, rel := range skipped {
		if !underCollapsed(rel, collapsed) {
			notSynced = append(notSynced, Entry{Path: rel})
		}
	}

	sort.Strings(synced)
	sort.Slice(notSynced, func(i, j int) bool { return notSynced[i].Path < notSynced[j].Path })
	return synced, notSynced, nil
}

// NotSyncedFiles is how many files the not-synced list stands for: collapsed
// directories count their whole subtree, nested mounts count zero because
// they do sync — through their own project.
func NotSyncedFiles(notSynced []Entry) int {
	n := 0
	for _, e := range notSynced {
		if e.IsDir() {
			n += e.Files
		} else {
			n++
		}
	}
	return n
}

func ancestors(rel string) []string {
	var out []string
	for d := path.Dir(rel); d != "."; d = path.Dir(d) {
		out = append(out, d)
	}
	return out
}

func underCollapsed(rel string, collapsed map[string]bool) bool {
	for _, a := range ancestors(rel) {
		if collapsed[a] {
			return true
		}
	}
	return false
}

// countFiles counts the files under an already-excluded directory. Readdir
// only — never Stat — because this runs over trees like .git and
// node_modules; a partial count on an unreadable subtree is fine.
func countFiles(abs string) int {
	n := 0
	filepath.WalkDir(abs, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}
