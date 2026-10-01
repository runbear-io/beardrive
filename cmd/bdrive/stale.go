package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/docrefs"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
)

func staleCmd() *cobra.Command {
	var (
		filesOnly bool
		limit     int
	)
	c := &cobra.Command{
		Use:   "stale [folder]",
		Short: "Find docs whose code has moved on since they were written",
		Long: `Report synced markdown that references files written after the doc itself.

Staleness here is not age: a doc is outgrown when a file it links to has a
newer last-write time than the doc. Only the files this project actually syncs
are scanned — a .bdriveignore rule or a narrowed ` + "`bdrive scope`" + ` excludes a file
from this command exactly as it excludes it from sync.

Write times come from the local journal, not from the filesystem: materialize
stamps a peer's file with THIS device's mtime, so on a freshly synced machine
every mtime is the same and only the journal still knows when each file was
really written.

It is a pure read with no daemon, no lock, and no network, so it works offline
and never blocks on a sync in progress. Exit status is 0 whether or not
anything is stale — this is advisory output, not a gate.`,
		Example: `  bdrive stale          # every outgrown doc, with the references that aged it
  bdrive stale -l       # paths only, one per line
  bdrive stale -n 5     # the five worst`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStale(cmd, args, filesOnly, limit)
		},
	}
	c.Flags().BoolVarP(&filesOnly, "files-with-matches", "l", false, "print outgrown paths only, one per line")
	// -n means the same here as in `bdrive log` and `bdrive grep`: max rows out.
	c.Flags().IntVarP(&limit, "limit", "n", 50, "max docs printed (0 = all)")
	return c
}

// staleRef is one reference that has outrun its doc.
type staleRef struct {
	path string
	gap  time.Duration
}

// staleDoc is one outgrown doc and the references that aged it, worst first.
type staleDoc struct {
	path string
	refs []staleRef
}

func runStale(cmd *cobra.Command, folderArg []string, filesOnly bool, limit int) error {
	folder, err := absFolder(folderArg)
	if err != nil {
		return err
	}
	// LoadProject, not ResolveMount: ResolveMount self-heals the registry
	// path, i.e. it enrolls this device. A read-only query must not have that
	// side effect — the same rule grep follows.
	proj, found, err := config.LoadProject(folder)
	if err != nil {
		return err
	}
	if !found {
		return notAProject(folder)
	}

	// The accepted rules and the journal both come from the volume store,
	// opened the way grep opens it: Stat-guarded, because store.Open MkdirAlls
	// and a read must not create a volume for a project that has never synced,
	// and unlocked, because store.Open takes no volume flock — a running
	// daemon never blocks this.
	var (
		accepted string
		ops      []journal.Op
	)
	if vdir, verr := config.VolumeDir(proj.ID); verr == nil && dirExists(vdir) {
		if st, serr := store.Open(vdir); serr == nil {
			if sync, serr := st.LoadSync(); serr == nil {
				accepted = sync.IgnoreAccepted
			}
			if all, serr := st.AllOps(); serr == nil {
				ops = all
			}
		}
	}

	paths, err := syncer.SyncedFiles(folder, proj.Include, accepted)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	written := docrefs.WriteTimes(ops)
	if len(written) == 0 {
		fmt.Fprintln(out, "no history yet")
		return nil
	}

	synced := make(map[string]bool, len(paths))
	for _, rel := range paths {
		synced[rel] = true
	}

	var docs []staleDoc
	for _, rel := range paths {
		if !docrefs.IsMarkdown(rel) {
			continue
		}
		docTime, ok := written[rel]
		if !ok {
			continue // never synced: nothing to date it by
		}
		var refs []staleRef
		for _, r := range docrefs.Outgrown(docTime, staleRefs(filepath.Join(folder, rel), rel, synced), lookupTime(written)) {
			refs = append(refs, staleRef{path: r.Path, gap: r.Gap})
		}
		if len(refs) == 0 {
			continue
		}
		docs = append(docs, staleDoc{path: rel, refs: refs})
	}
	// Worst first: the doc with the reference that has outrun it furthest.
	sort.SliceStable(docs, func(i, j int) bool { return docs[i].refs[0].gap > docs[j].refs[0].gap })

	total := 0
	for _, d := range docs {
		total += len(d.refs)
	}
	shown := docs
	truncated := false
	if limit > 0 && len(shown) > limit {
		shown, truncated = shown[:limit], true
	}

	for _, d := range shown {
		// Every path here is a string a teammate chose — a file name, or text
		// inside a synced doc. safeField, or a lone CR repaints the row and
		// U+202E reverses it.
		name := safeField(d.path, 160)
		if filesOnly {
			fmt.Fprintln(out, name)
			continue
		}
		fmt.Fprintf(out, "%-40s %-16s (oldest gap %s)\n",
			name, plural(len(d.refs), "file")+" newer", staleGap(d.refs[0].gap))
		for _, r := range d.refs {
			fmt.Fprintf(out, "  %-38s %s newer\n", safeField(r.path, 160), staleGap(r.gap))
		}
	}
	if !filesOnly {
		if len(docs) == 0 {
			fmt.Fprintln(out, "no outgrown docs")
		} else {
			fmt.Fprintf(out, "\n%s, %s\n", plural(len(docs), "outgrown doc"), plural(total, "stale reference"))
		}
	}
	if truncated {
		fmt.Fprintf(out, "output limited to %s — use -n 0 for all\n", plural(limit, "doc"))
	}
	// Exit 0 either way. grep's "1 means nothing found" convention inverts
	// here — it would fail on a clean project — and this is advisory in the
	// same sense the agent hook's context is: nothing is blocked by it.
	return nil
}

// staleGap renders how far a reference has outrun its doc. Sub-day gaps read
// as <1d rather than 0d, which would look like no gap at all.
func staleGap(d time.Duration) string {
	days := int(d.Hours() / 24)
	if days < 1 {
		return "<1d"
	}
	return fmt.Sprintf("%dd", days)
}

// staleRefs returns the synced paths one doc references, deduped. Unreadable
// files are skipped, never fatal, the same posture the scan takes.
func staleRefs(abs, rel string, synced map[string]bool) []string {
	f, err := os.Open(abs)
	if err != nil {
		return nil
	}
	defer f.Close()
	return docrefs.Resolve(rel, docrefs.Candidates(f), func(p string) bool { return synced[p] })
}

func lookupTime(m map[string]time.Time) func(string) (time.Time, bool) {
	return func(p string) (time.Time, bool) { t, ok := m[p]; return t, ok }
}
