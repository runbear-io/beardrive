package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/daemon"
	"github.com/runbear-io/beardrive/internal/store"
)

// gcMinAge is the floor under --older-than: a blob younger than this may be
// content a cycle spooled and has not journaled yet. It matches the daemon's
// own prune.
const gcMinAge = 10 * time.Minute

func gcCmd() *cobra.Command {
	var apply bool
	var olderThan time.Duration
	c := &cobra.Command{
		Use:   "gc [folder]",
		Short: "Free disk space held by old file versions the hub already keeps",
		Long: `Remove this device's local copies of old file versions.

The hub keeps every version of every file, so the copies this device holds of
versions no file is at anymore only cost disk space. "bdrive restore" fetches a
removed version back from the hub when you ask for it.

Kept no matter what: the content of every file as it is now, anything this
device has not pushed to the hub yet (and the history of those files), and any
version that landed on this device more recently than --older-than.

By default this only reports what it would free; pass --apply to remove.
Safe to run while syncing: it waits for the daemon's current cycle.`,
		Example: `  bdrive gc                      # how much would be freed here
  bdrive gc --apply              # free it
  bdrive gc --apply --older-than 720h   # keep the last 30 days' versions`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if olderThan < gcMinAge {
				return fmt.Errorf("--older-than must be at least %s", gcMinAge)
			}
			folder, err := absFolder(args)
			if err != nil {
				return err
			}
			root, _, err := findProject(folder)
			if err != nil {
				return err
			}
			proj, err := mustProject(root)
			if err != nil {
				return err
			}
			vdir, err := config.VolumeDir(proj.ID)
			if err != nil {
				return err
			}
			if _, err := os.Stat(vdir); err != nil {
				return fmt.Errorf("%s is not synced on this device yet (run `bdrive init` there to connect it)", root)
			}
			st, err := store.Open(vdir)
			if err != nil {
				return err
			}
			unlock, ok := st.LockWithin(time.Minute)
			if !ok {
				return fmt.Errorf("a sync cycle kept %s busy; try again in a moment", root)
			}
			defer unlock()
			n, freed, err := daemon.PruneVolumeBlobs(st, olderThan, !apply)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !apply {
				fmt.Fprintf(out, "would free %s (%d old versions) for %s\nrun `bdrive gc --apply` to free it\n",
					humanBytes(freed), n, root)
				return nil
			}
			fmt.Fprintf(out, "freed %s (%d old versions) for %s\n", humanBytes(freed), n, root)
			return nil
		},
	}
	c.Flags().BoolVar(&apply, "apply", false, "remove the old versions (default: only report)")
	c.Flags().DurationVar(&olderThan, "older-than", gcMinAge, "keep versions that landed on this device within this long")
	return c
}
