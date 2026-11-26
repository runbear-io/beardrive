package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"sync"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
)

// errNoMatch is grep's "found nothing" exit, not a failure: the convention is
// status 1 with no message, so `bdrive grep x || echo none` composes. Real
// errors (bad pattern, not a project) still print — see grepCmd.
var errNoMatch = errors.New("no match")

// binarySniff is how much of a file is read to decide it is not text. A NUL
// byte in the first 8 KB is the rule: cheap, and the same one grep uses.
const binarySniff = 8 << 10

// maxLineScan bounds one line, so a minified bundle that syncs cannot make
// grep buffer it whole. Past this the rest of that file is skipped.
const maxLineScan = 1 << 20

func grepCmd() *cobra.Command {
	var (
		ignoreCase bool
		fixed      bool
		filesOnly  bool
		limit      int
	)
	c := &cobra.Command{
		Use:   "grep <pattern> [folder]",
		Short: "Search the text inside the files this project syncs",
		Long: `Search file contents in a synced folder.

pattern is a Go regular expression (RE2), or a literal string with -F. Only
the files this project actually syncs are searched: a .bdriveignore rule or a
narrowed ` + "`bdrive scope`" + ` excludes a file from search exactly as it excludes it
from sync, and binary files are skipped.

It searches the real files on disk — a pure read with no daemon and no lock,
so it never blocks on a sync in progress. With lazy mode on (bdrive lazy), the
files kept on the hub are searched too: from the local cache when this device
holds them, otherwise by the hub, with a note on stderr saying so. Exit
status is 0 when something matched and 1 when nothing did.`,
		Example: `  bdrive grep 'retention.*fold'   # regexp over the whole project
  bdrive grep -i -l TODO          # case-insensitive, paths only
  bdrive grep -F 'a[b]c' docs     # literal string, inside ./docs`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			err := runGrep(cmd, args[0], args[1:], ignoreCase, fixed, filesOnly, limit)
			// SilenceErrors below is for errNoMatch alone, so anything else
			// has to print itself — cobra no longer will.
			if err != nil && !errors.Is(err, errNoMatch) {
				fmt.Fprintln(cmd.ErrOrStderr(), "Error:", err)
			}
			return err
		},
	}
	// Silenced so a no-match exits 1 without printing anything, the way grep
	// does — errNoMatch is a status, not a failure, and cobra would otherwise
	// render it as an error and a usage block.
	c.SilenceErrors = true
	c.SilenceUsage = true
	c.Flags().BoolVarP(&ignoreCase, "ignore-case", "i", false, "match case-insensitively")
	c.Flags().BoolVarP(&fixed, "fixed-strings", "F", false, "treat the pattern as a literal string, not a regexp")
	c.Flags().BoolVarP(&filesOnly, "files-with-matches", "l", false, "print matching paths only, one per line")
	// -n means the same here as in `bdrive log`: max rows out.
	c.Flags().IntVarP(&limit, "limit", "n", 200, "max matching lines printed (0 = all)")
	return c
}

func runGrep(cmd *cobra.Command, pattern string, folderArg []string, ignoreCase, fixed, filesOnly bool, limit int) error {
	if fixed {
		pattern = regexp.QuoteMeta(pattern)
	}
	if ignoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("bad pattern: %w", err)
	}
	folder, err := absFolder(folderArg)
	if err != nil {
		return err
	}
	// LoadProject, not ResolveMount: ResolveMount self-heals the registry
	// path, i.e. it enrolls this device. A read-only query must not have that
	// side effect — the rule logReads follows for the same reason.
	proj, found, err := config.LoadProject(folder)
	if err != nil {
		return err
	}
	if !found {
		return notAProject(folder)
	}

	// The rules this device has ACCEPTED, so results match what the cycle
	// actually uploads (syncer.Filter.SkipUp). Best-effort, exactly as
	// `bdrive scope --explain` does it: store.Open takes no volume flock, so
	// this cannot block behind a running daemon, and a store that will not
	// open degrades the answer to the live rules rather than failing a read.
	//
	// The Stat guard is this command's own: store.Open MkdirAlls the volume
	// directory, and a search must not create one for a project that has never
	// synced. No store means no accepted rules, which is what "" already says.
	var accepted string
	if vdir, verr := config.VolumeDir(proj.ID); verr == nil && dirExists(vdir) {
		if st, serr := store.Open(vdir); serr == nil {
			if sync, serr := st.LoadSync(); serr == nil {
				accepted = sync.IgnoreAccepted
			}
		}
	}

	paths, err := syncer.SyncedFiles(folder, proj.Include, accepted)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	var lines, files int
	truncated := false
	lastFile := ""
	// emit prints one hit and reports whether there is room for another.
	emit := func(rel string, h hit) bool {
		if limit > 0 && lines >= limit {
			truncated = true
			return false
		}
		if rel != lastFile {
			files++
			lastFile = rel
		}
		if filesOnly {
			fmt.Fprintln(out, safeField(rel, 160))
		} else {
			// Both the path and the matched text are content a teammate
			// wrote and synced — the same trust level `bdrive log` treats
			// journal strings with, over a wider surface. safeField, or a
			// lone CR repaints the row and U+202E reverses it.
			fmt.Fprintf(out, "%s:%d: %s\n", safeField(rel, 160), h.line, safeField(h.text, 400))
		}
		lines++
		return true
	}
	grepLocal(folder, paths, re, filesOnly, func(rel string, hits []hit) bool {
		for _, h := range hits {
			if !emit(rel, h) {
				return false
			}
		}
		return true
	})
	// Lazy mode: the files this device keeps on the hub are part of the
	// project too. Searched from the local cache when held, and by the hub
	// otherwise — never downloaded just to be searched.
	if !truncated && proj.Lazy.Enabled {
		if v, err := openView(cmd.Context(), folder, true); err == nil {
			defer v.close()
			searched := make(map[string]bool, len(paths))
			for _, p := range paths {
				searched[p] = true
			}
			notes, cut := lazyGrep(cmd.Context(), v, re, pattern, filesOnly, func(string) bool { return true }, searched, emit)
			truncated = truncated || cut
			for _, n := range notes {
				fmt.Fprintln(cmd.ErrOrStderr(), "note:", n)
			}
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(), "note: files lazy mode keeps on the hub were not searched: %v\n", err)
		}
	}
	if lines == 0 {
		return errNoMatch
	}
	if !filesOnly {
		fmt.Fprintf(out, "%s, %s\n", plural(files, "file"), plural(lines, "matching line"))
	}
	if truncated {
		fmt.Fprintf(out, "output limited to %d lines — use -n 0 for all\n", limit)
	}
	return nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// hit is one matching line: its 1-based number and its text.
type hit struct {
	line int
	text string
}

// grepFile returns the matching lines in one file, or nothing when the file is
// binary or unreadable — an unreadable file is skipped, never fatal, the same
// posture the scan takes. With filesOnly it stops at the first match.
func grepFile(folder, abs string, re *regexp.Regexp, filesOnly bool) []hit {
	f, _, err := store.OpenRegularUnderRoot(folder, abs)
	if err != nil {
		return nil
	}
	defer f.Close()
	return grepReader(f, re, filesOnly)
}

// grepReader is grepFile over any reader: a working file or a cached blob.
func grepReader(f io.Reader, re *regexp.Regexp, filesOnly bool) []hit {
	head := make([]byte, binarySniff)
	n, err := f.Read(head)
	if err != nil && err != io.EOF {
		return nil
	}
	head = head[:n]
	if bytes.IndexByte(head, 0) >= 0 {
		return nil // binary
	}

	var hits []hit
	sc := bufio.NewScanner(io.MultiReader(bytes.NewReader(head), f))
	sc.Buffer(make([]byte, 0, 64<<10), maxLineScan)
	for lineno := 1; sc.Scan(); lineno++ {
		if !re.Match(sc.Bytes()) {
			continue
		}
		hits = append(hits, hit{line: lineno, text: sc.Text()})
		if filesOnly {
			break
		}
	}
	return hits // sc.Err() ignored: an over-long line ends this file, not the run
}

// hubGrepPage bounds one hub grep request's path list (the hub's own cap).
const hubGrepPage = 5000

// lazyGrep searches the project files that are not on disk: from the blob
// cache when this device holds the version, and through the hub's paged grep
// otherwise. keep filters candidate paths (scope, glob); searched names the
// working files the local pass already searched — only those are skipped, so
// a local-only file at a path the project also holds (excluded here, synced by
// a teammate) still has its journaled version searched. emit returns false
// once the caller's limit is reached, which ends the search (cut reports it).
// notes are sentences about what could not be searched — a search is never
// narrowed silently.
func lazyGrep(ctx context.Context, v *projectView, re *regexp.Regexp, expr string, filesOnly bool,
	keep func(rel string) bool, searched map[string]bool, emit func(rel string, h hit) bool) (notes []string, cut bool) {
	var remoteOnly []string
	rels := make([]string, 0, len(v.ix.Files))
	for rel := range v.ix.Files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		if !keep(rel) {
			continue
		}
		if searched[rel] {
			continue // the local pass searched the working file
		}
		fs := v.ix.Files[rel]
		if !v.sess.Store.HasBlob(fs.Blob) {
			remoteOnly = append(remoteOnly, rel)
			continue
		}
		f, err := v.sess.Store.OpenBlob(fs.Blob)
		if err != nil {
			remoteOnly = append(remoteOnly, rel)
			continue
		}
		hits := grepReader(f, re, filesOnly)
		f.Close()
		for _, h := range hits {
			if !emit(rel, h) {
				return notes, true
			}
		}
	}
	if len(remoteOnly) == 0 {
		return notes, false
	}
	g, ok := v.sess.Backend.(remote.Grepper)
	if !ok || v.sess.Backend == nil {
		return append(notes, fmt.Sprintf("%s kept on the hub were not searched (hub unreachable)", plural(len(remoteOnly), "file"))), false
	}
	var binary, unreadable, missing int
	hubSearched := 0
	for len(remoteOnly) > 0 {
		page := remoteOnly[:min(len(remoteOnly), hubGrepPage)]
		resp, err := g.Grep(ctx, remote.GrepRequest{Pattern: expr, FilesOnly: filesOnly, Paths: page, MaxMatches: 1000})
		if err != nil || resp.Scanned == 0 {
			if err == nil {
				err = fmt.Errorf("no progress")
			}
			notes = append(notes, fmt.Sprintf("%s kept on the hub were not searched: %v", plural(len(remoteOnly), "file"), err))
			break
		}
		hubSearched += resp.Scanned
		missing += len(resp.Missing)
		binary += len(resp.Binary)
		unreadable += len(resp.Unreadable)
		if filesOnly {
			for _, f := range resp.Files {
				if !emit(f, hit{}) {
					return notes, true
				}
			}
		} else {
			for _, m := range resp.Matches {
				if !emit(m.Path, hit{line: m.Line, text: m.Text}) {
					return notes, true
				}
			}
			if resp.Total > len(resp.Matches) {
				// The page stopped at its match cap inside its last file:
				// resume there would repeat matches, so say so instead.
				notes = append(notes, fmt.Sprintf("the hub found %d more match(es) than it returned; narrow the pattern", resp.Total-len(resp.Matches)))
				cut = true
			}
		}
		remoteOnly = remoteOnly[resp.Scanned:]
	}
	notes = append(notes, fmt.Sprintf("searched %s on the hub (kept there by lazy mode)", plural(hubSearched, "file")))
	if binary > 0 || unreadable > 0 || missing > 0 {
		notes = append(notes, fmt.Sprintf("skipped on the hub: %d binary, %d unreadable, %d no longer there or not visible to you", binary, unreadable, missing))
	}
	return notes, cut
}

// grepLocal searches working files in parallel and hands each file's hits to
// fn in path order, stopping when fn returns false. Files are processed in
// bounded batches, so memory is one batch of hits and an early stop does not
// read the rest of the tree.
func grepLocal(folder string, paths []string, re *regexp.Regexp, filesOnly bool, fn func(rel string, hits []hit) bool) {
	workers := runtime.NumCPU()
	batch := 64 * workers
	for start := 0; start < len(paths); start += batch {
		chunk := paths[start:min(len(paths), start+batch)]
		out := make([][]hit, len(chunk))
		var wg sync.WaitGroup
		next := make(chan int)
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range next {
					out[i] = grepFile(folder, filepath.Join(folder, filepath.FromSlash(chunk[i])), re, filesOnly)
				}
			}()
		}
		for i := range chunk {
			next <- i
		}
		close(next)
		wg.Wait()
		for i, hits := range out {
			if len(hits) > 0 && !fn(chunk[i], hits) {
				return
			}
		}
	}
}
