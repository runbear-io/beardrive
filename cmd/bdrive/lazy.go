package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/daemon"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
)

// Lazy mode (on-demand files): `bdrive lazy`, `pin`, `unpin`, `cat`, `ls`, and
// the project view they share with `bdrive grep` and the local `bdrive mcp`.

// projectView is one project as this device sees it: the files its journals
// hold (syncer.Index) plus the working folder. Opening it never enrolls the
// folder, takes no lock and writes no state.
type projectView struct {
	root string
	proj config.Project
	sess *syncer.Session
	ix   *syncer.Index
}

// openView finds the project containing dir. withRemote attaches the hub so
// content that is not on this device can be fetched; offline still works for
// everything on disk or in the cache.
func openView(ctx context.Context, dir string, withRemote bool) (*projectView, error) {
	root, proj, err := findProject(dir)
	if err != nil {
		return nil, err
	}
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		return nil, err
	}
	if !dirExists(vdir) {
		return nil, fmt.Errorf("%s is not synced on this device yet (run `bdrive init` there to connect it)", root)
	}
	st, err := store.Open(vdir)
	if err != nil {
		return nil, err
	}
	dev, _ := config.LoadDevice()
	settings, _ := config.LoadSettings()
	sess := &syncer.Session{Folder: root, MountID: proj.ID, Store: st, Device: dev, Account: settings}
	if withRemote && proj.Remote != "" {
		if be, err := remote.Open(ctx, proj.Remote); err == nil {
			sess.Backend = be
		}
	}
	v := &projectView{root: root, proj: proj, sess: sess}
	return v, v.refresh()
}

func (v *projectView) refresh() error {
	ix, err := v.sess.LoadIndex()
	if err != nil {
		return err
	}
	v.ix = ix
	return nil
}

func (v *projectView) close() { closeSession(v.sess) }

// rel turns a user-supplied path (relative to the working directory, or
// absolute) into a project path. "" is the project root.
func (v *projectView) rel(arg string) (string, error) {
	abs, err := filepath.Abs(arg)
	if err != nil {
		return "", err
	}
	r, err := filepath.Rel(v.root, abs)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the project at %s", arg, v.root)
	}
	if r == "." {
		return "", nil
	}
	return filepath.ToSlash(r), nil
}

// entry is one file of the view.
type entry struct {
	size   int64
	onDisk bool
	blob   string // the journaled version; "" for a new local file
}

// files is every file of the project: the journal's, plus new local files
// not journaled yet. The disk wins for size, since it may hold an edit.
func (v *projectView) files() map[string]entry {
	out := make(map[string]entry, len(v.ix.Files))
	for rel, fs := range v.ix.Files {
		out[rel] = entry{size: fs.Size, blob: fs.Blob}
	}
	var accepted string
	if sync, err := v.sess.Store.LoadSync(); err == nil {
		accepted = sync.IgnoreAccepted
	}
	if paths, err := syncer.SyncedFiles(v.root, v.proj.Include, accepted); err == nil {
		for _, rel := range paths {
			e := out[rel]
			e.onDisk = true
			if fi, err := os.Lstat(filepath.Join(v.root, filepath.FromSlash(rel))); err == nil {
				e.size = fi.Size()
			}
			out[rel] = e
		}
	}
	return out
}

// open returns rel's current bytes: disk, then cache, then hub.
func (v *projectView) open(ctx context.Context, rel string) (io.ReadCloser, syncer.Source, error) {
	return v.sess.Open(ctx, v.ix, rel)
}

// readAll reads rel whole (bounded) and returns its sha256.
func (v *projectView) readAll(ctx context.Context, rel string, max int64) ([]byte, string, syncer.Source, error) {
	rc, src, err := v.open(ctx, rel)
	if err != nil {
		return nil, "", "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil {
		return nil, "", "", err
	}
	if int64(len(b)) > max {
		return nil, "", "", fmt.Errorf("%s is larger than %s", rel, humanBytes(max))
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:]), src, nil
}

func lazyCmd() *cobra.Command {
	var threshold, cacheMax string
	c := &cobra.Command{
		Use:   "lazy [on|off] [folder]",
		Short: "Keep large files on the hub until they are read (on-demand files)",
		Long: `Lazy mode keeps large files on the hub instead of downloading them.

With lazy mode on, this device downloads only files at or under the size
threshold (64 KiB by default) and files inside pinned folders (bdrive pin).
Every other file stays on the hub: it is still part of the project, and its
name, size and history are on this device, but its content is fetched only
when something reads it — bdrive cat, bdrive grep, or the bdrive mcp tools —
and then kept in a local cache capped at --cache-max (2 GiB by default; the
least recently read files are evicted first, by bdrive gc and the daemon).

A lazy file that is not on disk is never treated as deleted. Files already on
disk stay on disk and keep syncing. The setting is per device and never syncs.
With no argument, prints the current setting.`,
		Example: `  bdrive lazy on                 # turn it on for this project
  bdrive lazy on --threshold 256KiB --cache-max 5GiB
  bdrive lazy off                # download everything again
  bdrive lazy                    # show the setting and what stays on the hub`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := ""
			if len(args) > 0 {
				mode = args[0]
				args = args[1:]
			}
			if mode != "" && mode != "on" && mode != "off" {
				args, mode = append([]string{mode}, args...), ""
			}
			folder, err := absFolder(args)
			if err != nil {
				return err
			}
			root, proj, err := findProject(folder)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if mode != "" || threshold != "" || cacheMax != "" {
				if mode != "" {
					proj.Lazy.Enabled = mode == "on"
				}
				if threshold != "" {
					n, err := parseSize(threshold)
					if err != nil {
						return fmt.Errorf("--threshold: %w", err)
					}
					proj.Lazy.Threshold = n
				}
				if cacheMax != "" {
					n, err := parseSize(cacheMax)
					if err != nil {
						return fmt.Errorf("--cache-max: %w", err)
					}
					proj.Lazy.CacheMax = n
				}
				if _, err := config.SaveProject(root, proj); err != nil {
					return err
				}
				daemon.WakeSupervisor()
			}
			l := proj.Lazy
			if !l.Enabled {
				fmt.Fprintf(out, "lazy mode is off for %s: every file downloads\n", root)
				return nil
			}
			fmt.Fprintf(out, "lazy mode is on for %s\n", root)
			fmt.Fprintf(out, "  downloads: files up to %s", humanBytes(l.MaxBytes()))
			if len(l.Pinned) > 0 {
				fmt.Fprintf(out, ", and everything in %s", strings.Join(l.Pinned, ", "))
			}
			fmt.Fprintf(out, "\n  cache cap: %s\n", humanBytes(l.CacheBytes()))
			if v, err := openView(cmd.Context(), root, false); err == nil {
				n, b := v.ix.LazyCount()
				fmt.Fprintf(out, "  on the hub only: %s (%s) — read with bdrive cat, bdrive grep or bdrive mcp\n",
					plural(n, "file"), humanBytes(b))
			}
			return nil
		},
	}
	c.Flags().StringVar(&threshold, "threshold", "", "largest file that still downloads, e.g. 64KiB")
	c.Flags().StringVar(&cacheMax, "cache-max", "", "cap on the cache of files read on demand, e.g. 2GiB")
	return c
}

func pinCmd(pin bool) *cobra.Command {
	use, short := "pin <folder>...", "Keep folders fully downloaded in lazy mode"
	if !pin {
		use, short = "unpin <folder>...", "Let pinned folders go back to on-demand"
	}
	return &cobra.Command{
		Use:   use,
		Short: short,
		Long: `pin keeps every file under a folder (or one file) on this device, whatever
its size, when lazy mode is on — use it for anything a tool must open
directly (a build, an editor, git) or that you need offline. unpin undoes it:
files already on disk stay, and later versions are fetched on demand.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			root, proj, err := findProject(cwd)
			if err != nil {
				return err
			}
			v := &projectView{root: root}
			pins := map[string]bool{}
			for _, p := range proj.Lazy.Pinned {
				pins[p] = true
			}
			for _, a := range args {
				rel, err := v.rel(a)
				if err != nil {
					return err
				}
				if rel == "" {
					rel = "."
				}
				if pin {
					pins[rel] = true
				} else if !pins[rel] {
					return fmt.Errorf("%s is not pinned (pinned: %s)", rel, strings.Join(proj.Lazy.Pinned, ", "))
				} else {
					delete(pins, rel)
				}
			}
			proj.Lazy.Pinned = proj.Lazy.Pinned[:0]
			for p := range pins {
				proj.Lazy.Pinned = append(proj.Lazy.Pinned, p)
			}
			sort.Strings(proj.Lazy.Pinned)
			if _, err := config.SaveProject(root, proj); err != nil {
				return err
			}
			daemon.WakeSupervisor()
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "pinned: %s\n", strings.Join(proj.Lazy.Pinned, ", "))
			if !proj.Lazy.Enabled {
				fmt.Fprintln(out, "lazy mode is off, so every file downloads anyway (bdrive lazy on)")
			}
			return nil
		},
	}
}

func catCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cat <file>...",
		Short: "Print a synced file, fetching it from the hub if it is not on disk",
		Long: `Print files of a synced project. A file on disk is read from disk; a file
lazy mode keeps on the hub is read from the local cache, or fetched from the
hub and cached.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			v, err := openView(cmd.Context(), cwd, true)
			if err != nil {
				return err
			}
			defer v.close()
			for _, a := range args {
				rel, err := v.rel(a)
				if err != nil {
					return err
				}
				rc, _, err := v.open(cmd.Context(), rel)
				if err != nil {
					return err
				}
				_, err = io.Copy(cmd.OutOrStdout(), rc)
				rc.Close()
				if err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func lsCmd() *cobra.Command {
	var recursive bool
	c := &cobra.Command{
		Use:   "ls [folder]",
		Short: "List a synced folder, including files kept on the hub",
		Long: `List a folder of a synced project as the project holds it. Files lazy mode
keeps on the hub are listed too, marked "on hub" — read them with bdrive cat.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := absFolder(args)
			if err != nil {
				return err
			}
			v, err := openView(cmd.Context(), dir, false)
			if err != nil {
				return err
			}
			defer v.close()
			rel, err := v.rel(dir)
			if err != nil {
				return err
			}
			rows := listDir(v.files(), rel, recursive)
			if len(rows) == 0 {
				return fmt.Errorf("%s: no synced files here", dir)
			}
			out := cmd.OutOrStdout()
			for _, r := range rows {
				fmt.Fprintln(out, safeField(r, 400))
			}
			return nil
		},
	}
	c.Flags().BoolVarP(&recursive, "recursive", "R", false, "list every file below the folder")
	return c
}

// listDir renders the entries under dir: one level (folders end in /) or,
// recursive, every file. Sizes are human; files not on disk say "on hub".
func listDir(files map[string]entry, dir string, recursive bool) []string {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	dirs := map[string]bool{}
	var rows []string
	for rel, e := range files {
		if !strings.HasPrefix(rel, prefix) {
			continue
		}
		name := rel[len(prefix):]
		if i := strings.IndexByte(name, '/'); i >= 0 && !recursive {
			dirs[name[:i+1]] = true
			continue
		}
		where := ""
		if !e.onDisk {
			where = "  (on hub)"
		}
		rows = append(rows, fmt.Sprintf("%-40s %10s%s", name, humanBytes(e.size), where))
	}
	for d := range dirs {
		rows = append(rows, d)
	}
	sort.Strings(rows)
	return rows
}

// parseSize reads "65536", "64KiB", "64K", "2GiB", "1.5MB" (binary units).
func parseSize(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30}, {"KB", 1 << 10}, {"MB", 1 << 20}, {"GB", 1 << 30}, {"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"B", 1}} {
		if strings.HasSuffix(t, u.suf) {
			t, mult = strings.TrimSpace(strings.TrimSuffix(t, u.suf)), u.m
			break
		}
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil || f <= 0 {
		return 0, fmt.Errorf("%q is not a size (try 64KiB or 2GiB)", s)
	}
	return int64(f * float64(mult)), nil
}
