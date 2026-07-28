package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/syncer"
)

// bdrive scope shows and edits the project's sync scope — the include list
// in .bdrive/config.json that `init --shared` seeds — so growing or
// shrinking what syncs never means hand-editing JSON. The daemon re-reads
// the config every tick, so changes apply within seconds.
func scopeCmd() *cobra.Command {
	var explain bool
	c := &cobra.Command{
		Use:   "scope",
		Short: "Show or change which subfolders sync (the include list)",
		Long: `Show or change the project's sync scope: the include list in
.bdrive/config.json, as set by init --shared. An empty list means the whole
folder syncs. Run from the mount root; the daemon picks changes up within
seconds.

--explain walks the folder and prints every path it found, split into what
syncs and what does not, so you can verify what leaves this machine instead
of taking it on trust. It is a pure read: no daemon, no lock, no network.

Removing a folder stops syncing it but deletes nothing — local files stay,
and the hub keeps everything already synced. To take something off the hub
too, use ` + "`bdrive forget <path>`" + `.`,
		Example: `  bdrive scope             # show what syncs
  bdrive scope --explain   # list every path: synced vs not synced
  bdrive scope add docs    # also sync ./docs
  bdrive scope rm docs     # stop syncing ./docs (files stay everywhere)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(nil)
			if err != nil {
				return err
			}
			proj, err := mustProject(folder)
			if err != nil {
				return err
			}
			printScope(proj)
			if !explain {
				return nil
			}
			synced, notSynced, err := syncer.Explain(folder, proj.Include)
			if err != nil {
				return err
			}
			printExplain(synced, notSynced)
			return nil
		},
	}
	// Flags, not PersistentFlags: add/rm must not inherit --explain.
	c.Flags().BoolVar(&explain, "explain", false, "list every path, split into what syncs and what does not")
	c.AddCommand(scopeAddCmd(), scopeRmCmd())
	return c
}

// printExplain renders the two lists. All the filtering rules live in
// internal/syncer — this only formats what the walk decided.
func printExplain(synced []string, notSynced []syncer.Entry) {
	fmt.Printf("\nsynced (%s)\n", comma(len(synced)))
	for _, p := range synced {
		fmt.Println("  " + p)
	}
	fmt.Printf("\nnot synced (%s)\n", comma(syncer.NotSyncedFiles(notSynced)))
	for _, e := range notSynced {
		note := ""
		switch {
		case e.Nested:
			note = "(own project — syncs separately)"
		case e.IsDir():
			note = fmt.Sprintf("(%s files)", comma(e.Files))
			if e.Files == 1 {
				note = "(1 file)"
			}
		}
		fmt.Println(strings.TrimRight(fmt.Sprintf("  %-30s %s", e.Path, note), " "))
	}
	fmt.Printf("\n%s files sync, %s do not.\n\n", comma(len(synced)), comma(syncer.NotSyncedFiles(notSynced)))
	fmt.Println("Excluded paths never leave this machine. Anything that synced before its rule")
	fmt.Println("existed may still be on the hub — `bdrive forget <path>` takes it off.")
}

// comma groups thousands: 2486 -> "2,486".
func comma(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func scopeAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <dir>...",
		Short: "Add shared subfolders to the sync scope",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(nil)
			if err != nil {
				return err
			}
			proj, err := mustProject(folder)
			if err != nil {
				return err
			}
			if len(proj.Include) == 0 {
				return fmt.Errorf("this project syncs the whole folder; adding %s would narrow it to only that — re-run `bdrive init` with --shared if you want a scoped sync", strings.Join(args, ", "))
			}
			incs, err := cleanShared(args)
			if err != nil {
				return err
			}
			seen := map[string]bool{}
			for _, i := range proj.Include {
				seen[i] = true
			}
			added := 0
			for _, inc := range incs {
				if seen[inc] {
					continue
				}
				if err := os.MkdirAll(filepath.Join(folder, filepath.FromSlash(strings.TrimSuffix(inc, "/"))), 0o755); err != nil {
					return err
				}
				proj.Include = append(proj.Include, inc)
				seen[inc] = true
				added++
			}
			if added == 0 {
				fmt.Println("already in the sync scope")
			} else if _, err := config.SaveProject(folder, proj); err != nil {
				return err
			}
			printScope(proj)
			return nil
		},
	}
}

func scopeRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <dir>...",
		Short: "Remove shared subfolders from the sync scope (deletes nothing)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(nil)
			if err != nil {
				return err
			}
			proj, err := mustProject(folder)
			if err != nil {
				return err
			}
			kept, err := scopeRemove(proj.Include, args)
			if err != nil {
				return err
			}
			if len(kept) == 0 {
				return fmt.Errorf("removing the last shared folder would switch to syncing the whole folder; run `bdrive stop` to stop syncing instead")
			}
			proj.Include = kept
			if _, err := config.SaveProject(folder, proj); err != nil {
				return err
			}
			fmt.Println("removed from the sync scope — nothing was deleted, locally or on the hub")
			printScope(proj)
			return nil
		},
	}
}

// scopeRemove drops the named dirs from the include list, matching each
// argument literally, in normalized "/dir/" form, and in the pre-anchoring
// "dir/" form that configs written before the anchoring fix still hold
// (hand-edited configs may hold arbitrary patterns). Unknown entries are an
// error.
func scopeRemove(include, args []string) ([]string, error) {
	remove := map[string]bool{}
	for _, a := range args {
		keys := map[string]bool{strings.TrimSpace(a): true}
		if norm, err := cleanShared([]string{a}); err == nil {
			keys[norm[0]] = true
			keys[strings.TrimPrefix(norm[0], "/")] = true
		}
		found := false
		for _, i := range include {
			if keys[i] {
				remove[i] = true
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("%q is not in the sync scope (see `bdrive scope`)", a)
		}
	}
	var kept []string
	for _, i := range include {
		if !remove[i] {
			kept = append(kept, i)
		}
	}
	return kept, nil
}

func printScope(proj config.Project) {
	if len(proj.Include) == 0 {
		fmt.Println("the whole folder syncs (no include list)")
		return
	}
	fmt.Println("syncing only:")
	for _, i := range proj.Include {
		fmt.Println("  ./" + strings.Trim(i, "/"))
	}
}
