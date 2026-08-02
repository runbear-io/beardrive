package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
)

// bdrive scope shows and edits which of the mount's subfolders sync. The
// narrowing is ordinary .bdriveignore rules in a bdrive-managed block (see
// scopefile.go) — there is no separate scope setting to keep in step, and no
// reason to hand-write the negation syntax. The daemon re-reads the rules
// every tick, so changes apply within seconds.
func scopeCmd() *cobra.Command {
	var explain bool
	c := &cobra.Command{
		Use:   "scope",
		Short: "Show or change which subfolders sync",
		Long: `Show or change what syncs inside this mount.

The whole mount syncs by default. Narrowing it writes a managed block of
.bdriveignore rules ("only these folders"), which syncs to the team like any
other rule — so everyone sees the same scope. Run from the mount root.

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
			if err := printScope(folder, proj); err != nil {
				return err
			}
			if !explain {
				return nil
			}
			// The rules this device has ACCEPTED, so the explanation matches
			// what the cycle actually uploads (syncer.Filter.SkipUp). A store
			// that will not open is not worth failing `bdrive scope` over: the
			// answer degrades to the live rules, which is what it always was.
			var accepted string
			if vdir, verr := config.VolumeDir(proj.ID); verr == nil {
				if st, serr := store.Open(vdir); serr == nil {
					if sync, serr := st.LoadSync(); serr == nil {
						accepted = sync.IgnoreAccepted
					}
				}
			}
			synced, notSynced, err := syncer.Explain(folder, proj.Include, accepted)
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
		Short: "Add subfolders to what syncs",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, proj, err := scopeTarget()
			if err != nil {
				return err
			}
			dirs, scoped, err := readScopeDirs(folder)
			if err != nil {
				return err
			}
			if !scoped {
				return fmt.Errorf("this project syncs the whole folder already, so %s is included;\n"+
					"to narrow it instead, re-run `bdrive init . --only %s`",
					strings.Join(args, ", "), strings.Join(args, ","))
			}
			add, err := cleanScopeDirs(args)
			if err != nil {
				return err
			}
			seen := map[string]bool{}
			for _, d := range dirs {
				seen[d] = true
			}
			added := 0
			for _, d := range add {
				if seen[d] {
					continue
				}
				if err := mkdirScopeDirs(folder, []string{d}); err != nil {
					return err
				}
				dirs = append(dirs, d)
				seen[d] = true
				added++
			}
			if added == 0 {
				fmt.Println("already syncing")
			} else if err := writeScopeDirs(folder, dirs); err != nil {
				return err
			}
			return printScope(folder, proj)
		},
	}
}

func scopeRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <dir>...",
		Short: "Stop syncing subfolders (deletes nothing)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, proj, err := scopeTarget()
			if err != nil {
				return err
			}
			dirs, scoped, err := readScopeDirs(folder)
			if err != nil {
				return err
			}
			if !scoped {
				return fmt.Errorf("this project syncs the whole folder; add a plain .bdriveignore rule to exclude %s",
					strings.Join(args, ", "))
			}
			kept, err := scopeRemove(dirs, args)
			if err != nil {
				return err
			}
			if len(kept) == 0 {
				return fmt.Errorf("removing the last folder would switch to syncing the whole mount; run `bdrive stop` to stop syncing instead")
			}
			if err := writeScopeDirs(folder, kept); err != nil {
				return err
			}
			fmt.Println("removed from the sync scope — nothing was deleted, locally or on the hub")
			return printScope(folder, proj)
		},
	}
}

// scopeTarget resolves the mount the scope commands act on.
func scopeTarget() (string, config.Project, error) {
	folder, err := absFolder(nil)
	if err != nil {
		return "", config.Project{}, err
	}
	proj, err := mustProject(folder)
	return folder, proj, err
}

// scopeRemove drops the named dirs from the scoped list, tolerating the
// slash-decorated forms people type. Unknown entries are an error.
func scopeRemove(dirs, args []string) ([]string, error) {
	remove := map[string]bool{}
	for _, a := range args {
		want, err := cleanScopeDirs([]string{a})
		if err != nil {
			return nil, err
		}
		found := false
		for _, d := range dirs {
			if d == want[0] {
				remove[d] = true
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("%q is not in the sync scope (see `bdrive scope`)", a)
		}
	}
	var kept []string
	for _, d := range dirs {
		if !remove[d] {
			kept = append(kept, d)
		}
	}
	return kept, nil
}

func printScope(folder string, proj config.Project) error {
	dirs, scoped, err := readScopeDirs(folder)
	if err != nil {
		return err
	}
	// Mounts created before the scope moved into .bdriveignore still carry an
	// include list in .bdrive/config.json; it is still honored, so report it.
	// Both mechanisms can be in force at once and LoadFilter applies both, so
	// this reports every one of them — an audit surface that names one set of
	// rules and stays silent about the other is worse than none.
	if len(proj.Include) > 0 {
		fmt.Println("syncing only (legacy include list in .bdrive/config.json):")
		for _, i := range proj.Include {
			fmt.Println("  ./" + strings.Trim(i, "/"))
		}
		fmt.Println("re-run `bdrive init . --only <dirs>` to move these into .bdriveignore")
		if !scoped {
			return nil
		}
		fmt.Println("\nand a managed .bdriveignore block, also in force:")
		for _, d := range dirs {
			fmt.Println("  ./" + d)
		}
		return nil
	}
	if !scoped {
		fmt.Println("the whole folder syncs")
		return nil
	}
	fmt.Println("syncing only:")
	for _, d := range dirs {
		fmt.Println("  ./" + d)
	}
	return nil
}
