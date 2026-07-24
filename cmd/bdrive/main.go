// bdrive is the BearDrive CLI: mount a folder, and its
// contents stay synchronized across devices and teammates through a
// BearDrive hub, with full per-file change history and offline support.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
)

// version is set at release time via -ldflags "-X main.version=...".
var version = "0.1.0-dev"

func main() {
	root := &cobra.Command{
		Use:   "bdrive",
		Short: "BearDrive: a synced file system for AI agents",
		Long: `bdrive — the BearDrive CLI. A mountable, offline-first, synced file
system for AI agents.

Mount any folder and BearDrive keeps it synchronized across your devices and
teammates through a BearDrive hub (bdrive web — self-hosted or BearDrive
Cloud). Every change is journaled — you can always see which device and
author changed which file, and when. Files are real files on disk, so
everything keeps working offline; changes sync when the remote is reachable.`,
		SilenceUsage: true,
		Version:      version,
	}
	root.SetVersionTemplate("beardrive {{.Version}}\n")
	root.AddCommand(
		loginCmd(),
		logoutCmd(),
		initCmd(),
		shareCmd(),
		urlCmd(),
		stopCmd(),
		syncCmd(),
		readLogCmd(),
		hooksCmd(),
		skillCmd(),
		statusCmd(),
		logCmd(),
		exportCmd(),
		importCmd(),
		webCmd(),
		whoamiCmd(),
		daemonCmd(),
		versionCmd(),
	)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the bdrive version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("beardrive", version)
		},
	}
}

func whoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show this device's identity used in change tracking",
		RunE: func(cmd *cobra.Command, args []string) error {
			dev, err := config.LoadDevice()
			if err != nil {
				return err
			}
			home, err := config.Home()
			if err != nil {
				return err
			}
			fmt.Printf("device id:   %s\n", dev.ID)
			fmt.Printf("device name: %s\n", dev.Name)
			settings, serr := config.LoadSettings()
			if serr != nil {
				fmt.Printf("account:     unknown — cannot read settings: %v\n", serr)
			} else if settings.Email != "" {
				who := settings.Email
				if settings.Name != "" {
					who = settings.Name + " <" + settings.Email + ">"
				}
				fmt.Printf("account:     %s (from `bdrive login`; changes are attributed to this)\n", who)
				fmt.Printf("author:      %s (git/OS fallback, used only when signed out)\n", dev.Author)
			} else {
				fmt.Printf("account:     not signed in — changes are attributed to the author below (run `bdrive login`)\n")
				fmt.Printf("author:      %s (detected from git config / OS user)\n", dev.Author)
			}
			fmt.Printf("beardrive home:    %s\n", home)
			return nil
		},
	}
}
