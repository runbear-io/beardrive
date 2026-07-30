package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/autostart"
	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/daemon"
	"github.com/runbear-io/beardrive/internal/store"
)

// resumeCmd restarts the sync daemon for every project this device syncs. It
// is what the login agent runs, and the one command to reach for when the
// daemons are gone but the projects are fine — after a reboot, a crash, or a
// `killall bdrive`.
//
// It deliberately does NOT touch the pause marker: `bdrive stop` means stay
// stopped, including across a reboot, and only `bdrive init` re-consents.
func resumeCmd() *cobra.Command {
	var quiet bool
	c := &cobra.Command{
		Use:   "resume",
		Short: "Restart the sync daemon for every project on this device",
		Long: `Start the background sync daemon for each project this device syncs and
has not paused. Already-running daemons are left alone, so running it twice
is harmless.

This is what the login agent registered by ` + "`bdrive autostart install`" + ` runs, so
sync comes back by itself after a reboot. Run it by hand if the daemons died
some other way; ` + "`bdrive status`" + ` shows which are running.

Projects paused with ` + "`bdrive stop`" + ` stay paused — resume never overrides that.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			mounts, err := config.LoadMounts()
			if err != nil {
				return err
			}
			var started, running, skipped, failed int
			for id, mi := range mounts {
				// The folder config is the source of truth for whether this is
				// still a project: a moved or deleted folder must not be
				// resurrected here (that is `bdrive init`'s job at the new
				// path), and a daemon started on a vanished folder would exit
				// immediately anyway.
				if _, ok, err := config.LoadProject(mi.Path); err != nil || !ok {
					skipped++
					if !quiet {
						fmt.Printf("  skipped  %s (not a project any more — moved or deleted)\n", mi.Path)
					}
					continue
				}
				vdir, err := config.VolumeDir(id)
				if err != nil {
					failed++
					continue
				}
				if store.Paused(vdir) {
					skipped++
					if !quiet {
						fmt.Printf("  paused   %s (bdrive init resumes it)\n", mi.Path)
					}
					continue
				}
				if pid, ok := daemon.Running(vdir); ok {
					running++
					if !quiet {
						fmt.Printf("  running  %s (pid %d)\n", mi.Path, pid)
					}
					continue
				}
				pid, err := daemon.Start(mi.Path, vdir, 3*time.Second, 10*time.Second)
				if err != nil {
					failed++
					fmt.Fprintf(os.Stderr, "  failed   %s: %v\n", mi.Path, err)
					continue
				}
				started++
				if !quiet {
					fmt.Printf("  started  %s (pid %d)\n", mi.Path, pid)
				}
			}
			if len(mounts) == 0 && !quiet {
				fmt.Println("no beardrive projects on this device (run `bdrive init` in a folder)")
				return nil
			}
			if !quiet {
				fmt.Printf("resumed %d, already running %d, skipped %d, failed %d\n",
					started, running, skipped, failed)
			}
			// A partial failure must not fail the login agent: the projects
			// that did start are syncing, and launchd retrying the whole run
			// would not fix the one that didn't.
			return nil
		},
	}
	c.Flags().BoolVar(&quiet, "quiet", false, "print nothing but errors (used by the login agent)")
	return c
}

// autostartCmd manages the login registration. Bare `bdrive autostart` shows
// the status, mirroring `bdrive hooks`.
func autostartCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "autostart",
		Short: "Show whether sync restarts at login",
		Long: `Show, add, or remove the login registration that restarts syncing after a
reboot. It runs ` + "`bdrive resume`" + `, so it covers every project this device syncs
— one registration per machine, not one per project.

macOS uses a launchd user agent, Linux a systemd user unit (systemd must be the
init system), Windows a per-user Run entry. All are user-level: no sudo,
nothing machine-wide.

` + "`bdrive init`" + ` installs it for you; these subcommands are for checking,
retrying, or opting out.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := autostart.Path()
			if errors.Is(err, autostart.ErrUnsupported) {
				fmt.Println("autostart: not available here (needs macOS, Windows, or Linux with systemd)")
				fmt.Println("  after a reboot, run `bdrive resume` (or `bdrive init` in a project) to start syncing again")
				return nil
			}
			if err != nil {
				return err
			}
			if autostart.Installed() {
				fmt.Printf("autostart: registered  →  %s\n", path)
			} else {
				fmt.Printf("autostart: not registered (run `bdrive autostart install`)\n  would write: %s\n", path)
			}
			return nil
		},
	}
	c.AddCommand(&cobra.Command{
		Use:   "install",
		Short: "Restart syncing at login (idempotent)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := autostart.Install()
			if errors.Is(err, autostart.ErrUnsupported) {
				fmt.Println("autostart: not available here (needs macOS, Windows, or Linux with systemd) — run `bdrive resume` after a reboot")
				return nil
			}
			if err != nil {
				return err
			}
			state := "registered"
			if !res.Changed {
				state = "already registered"
			}
			fmt.Printf("autostart: %s  →  %s\n", state, res.Path)
			return nil
		},
	})
	c.AddCommand(&cobra.Command{
		Use:   "uninstall",
		Short: "Stop restarting syncing at login",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := autostart.Uninstall()
			if errors.Is(err, autostart.ErrUnsupported) {
				fmt.Println("autostart: nothing registered on this platform")
				return nil
			}
			if err != nil {
				return err
			}
			if !res.Changed {
				fmt.Println("autostart: was not registered")
				return nil
			}
			fmt.Printf("autostart: removed  →  %s\n", res.Path)
			fmt.Println("  running daemons keep going; after the next reboot, sync starts on the next `bdrive resume`, `bdrive init`, or agent turn")
			return nil
		},
	})
	return c
}
