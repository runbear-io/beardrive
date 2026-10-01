package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/runbear-io/beardrive/internal/docrefs"
	"github.com/runbear-io/beardrive/internal/store"
)

// The turn-start half of BEA-279: when a teammate's change lands, name the
// synced docs that link the changed file and are now outgrown by it —
// `bdrive stale`'s meaning, scoped to this turn's inbound paths. Advisory and
// best-effort like the rest of the hook: any error drops the sentence, never
// the turn.

// docIndexEntry is one doc's unresolved references, valid while the doc's
// content is Blob. Unresolved on purpose: resolving at query time against the
// current synced set catches a doc that names a file which appears later,
// without re-parsing the doc.
type docIndexEntry struct {
	Blob  string   `json:"blob"`
	Cands []string `json:"cands"`
}

// docIndexParses counts docs read from the blob store — the test's proof
// that a quiet turn re-parses nothing.
var docIndexParses int

// refreshDocIndex brings the mount's reverse-link index up to date with its
// synced set, re-parsing only docs whose blob changed. Docs are read from
// the content-addressed store, never the working file, which may be mid-edit.
// A garbage index is rebuilt rather than trusted or fatal: every entry is
// re-checked against its blob anyway.
func refreshDocIndex(st *store.Store, mountID string, cache map[string]store.CachedFile) (map[string]docIndexEntry, error) {
	path, err := st.DocIndexPath(mountID)
	if err != nil {
		return nil, err
	}
	idx := map[string]docIndexEntry{}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if json.Unmarshal(data, &idx) != nil || idx == nil {
			idx = map[string]docIndexEntry{}
		}
	}
	dirty := false
	for doc := range idx {
		if _, ok := cache[doc]; !ok || !docrefs.IsMarkdown(doc) {
			delete(idx, doc)
			dirty = true
		}
	}
	for rel, cf := range cache {
		if !docrefs.IsMarkdown(rel) || idx[rel].Blob == cf.Blob {
			continue
		}
		f, err := st.OpenBlob(cf.Blob)
		if err != nil {
			continue // not here yet: next turn
		}
		docIndexParses++
		idx[rel] = docIndexEntry{Blob: cf.Blob, Cands: docrefs.Candidates(f)}
		f.Close()
		dirty = true
	}
	if dirty {
		if err := store.WriteJSONAtomic(path, idx); err != nil {
			return nil, err
		}
	}
	return idx, nil
}

// outgrownDoc is one doc a teammate's change has outrun, and the inbound
// files that did it.
type outgrownDoc struct {
	doc  string
	refs []string
}

// inboundOutgrown returns the docs that link a path pulled in this turn and
// are now older than it. Only runs when something non-deleted came in, so a
// quiet turn costs nothing.
func inboundOutgrown(st *store.Store, mountID string, inbound []store.InboundEvent) ([]outgrownDoc, error) {
	changed := map[string]bool{}
	for _, e := range inbound {
		if !e.Deleted {
			changed[e.Path] = true
		}
	}
	if len(changed) == 0 {
		return nil, nil
	}
	cache, err := st.LoadCache(mountID)
	if err != nil {
		return nil, err
	}
	idx, err := refreshDocIndex(st, mountID, cache)
	if err != nil {
		return nil, err
	}
	ops, err := st.AllOps()
	if err != nil {
		return nil, err
	}
	written := docrefs.WriteTimes(ops)
	exists := func(p string) bool { _, ok := cache[p]; return ok }
	var out []outgrownDoc
	for _, doc := range slices.Sorted(maps.Keys(idx)) {
		docTime, ok := written[doc]
		if !ok {
			continue
		}
		var refs []string
		for _, ref := range docrefs.Resolve(doc, idx[doc].Cands, exists) {
			if changed[ref] && written[ref].After(docTime) {
				refs = append(refs, ref)
			}
		}
		if len(refs) > 0 {
			out = append(out, outgrownDoc{doc: doc, refs: refs})
		}
	}
	return out, nil
}

// hookOutgrown renders the sentence, paths mapped the way hookChanged maps
// them and capped the same way.
func hookOutgrown(links []hookLink) string {
	var parts []string
	over := 0
	for _, l := range links {
		for _, o := range l.outgrown {
			doc, ok := hookAgentPath(l, o.doc)
			if !ok {
				continue
			}
			var refs []string
			for _, r := range o.refs {
				if p, ok := hookAgentPath(l, r); ok {
					refs = append(refs, "`"+p+"`")
				}
			}
			if len(refs) == 0 {
				continue
			}
			if len(parts) >= hookChangedMax {
				over++
				continue
			}
			parts = append(parts, fmt.Sprintf("`%s` (links %s)", doc, strings.Join(refs, ", ")))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	s := "Docs that link a file changed since your last turn may be out of date. Check before relying on them: " + strings.Join(parts, ", ")
	if over > 0 {
		s += fmt.Sprintf(", +%d more", over)
	}
	return s + "."
}
