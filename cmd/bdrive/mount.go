package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/daemon"
	"github.com/runbear-io/beardrive/internal/store"
)

func mntCmd() *cobra.Command {
	var remoteURL, volume string
	var foreground bool
	var scanInterval, remoteInterval time.Duration
	c := &cobra.Command{
		Use:     "mnt <folder>",
		Aliases: []string{"mount"},
		Short:   "Mount a folder as a synced beardrive volume",
		Long: `Mount a folder as a synced beardrive volume.

Existing files in the folder are imported into the volume. If a remote is
configured (--remote, or previously via "bdrive remote set"), the volume syncs
with it and with every other device mounting the same remote. A background
daemon keeps the folder in sync until "bdrive umnt".`,
		Example: `  bdrive mnt ./notes
  bdrive mnt ./notes --remote s3://my-bucket/notes
  bdrive mnt ./notes --remote gs://my-bucket/notes
  bdrive mnt ./shared --remote file:///Volumes/nas/beardrive/shared`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(args)
			if err != nil {
				return err
			}
			return runMount(cmd.Context(), folder, remoteURL, volume, foreground, scanInterval, remoteInterval)
		},
	}
	c.Flags().StringVarP(&remoteURL, "remote", "r", "", "remote to sync with (s3://bucket/prefix, gs://bucket/prefix, file:///path)")
	c.Flags().StringVarP(&volume, "volume", "v", "", "volume name (default: folder basename)")
	c.Flags().BoolVarP(&foreground, "foreground", "f", false, "run the sync daemon in the foreground")
	c.Flags().DurationVar(&scanInterval, "scan-interval", 3*time.Second, "how often to scan the folder for local changes")
	c.Flags().DurationVar(&remoteInterval, "remote-interval", 10*time.Second, "how often to sync with the remote")
	return c
}

// runMount mounts a folder: resolve settings, register the mount, run the
// initial cycle, start the daemon. Shared by `bdrive mnt` and `bdrive init`.
func runMount(ctx context.Context, folder, remoteURL, volume string, foreground bool, scanInterval, remoteInterval time.Duration) error {
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return err
	}

	// Settings resolution: flags win, then the folder's .bdrive file,
	// then the global registry. The result is written back to both,
	// so the project file travels with the folder and the registry
	// knows which mounts are active on this device.
	mounts, err := config.LoadMounts()
	if err != nil {
		return err
	}
	reg, registered := mounts[folder]
	proj, _, err := config.LoadProject(folder)
	if err != nil {
		return err
	}
	effVolume := proj.Volume
	if effVolume == "" && registered {
		effVolume = reg.Volume
	}
	if volume != "" && effVolume != "" && volume != effVolume {
		return fmt.Errorf("%s is already mounted as volume %q", folder, effVolume)
	}
	if volume != "" {
		effVolume = volume
	}
	if effVolume == "" {
		effVolume = filepath.Base(folder)
	}
	effRemote := remoteURL
	if effRemote == "" {
		effRemote = proj.Remote
	}
	if effRemote == "" && registered {
		effRemote = reg.Remote
	}
	mi := config.MountInfo{Volume: effVolume, Remote: effRemote}
	mounts[folder] = mi
	if err := config.SaveMounts(mounts); err != nil {
		return err
	}
	proj.Volume, proj.Remote = effVolume, effRemote
	if err := config.SaveProject(folder, proj); err != nil {
		return err
	}
	vdir, err := config.VolumeDir(mi.Volume)
	if err != nil {
		return err
	}
	if _, err := store.Open(vdir); err != nil {
		return err
	}

	dev, err := config.LoadDevice()
	if err != nil {
		return err
	}

	// Initial cycle: import existing files, pull remote state.
	sess, _, err := openSession(ctx, folder, true)
	if err != nil {
		return err
	}
	res, err := sess.Cycle(ctx)
	closeSession(sess)
	if err != nil {
		return err
	}

	fmt.Printf("mounted %s\n", folder)
	fmt.Printf("  volume:  %s\n", mi.Volume)
	if mi.Remote != "" {
		fmt.Printf("  remote:  %s\n", mi.Remote)
	} else {
		fmt.Printf("  remote:  (none — local only; set one with `bdrive remote set %s <url>`)\n", folder)
	}
	fmt.Printf("  device:  %s (%s) as %s\n", dev.Name, dev.ID, dev.Author)
	fmt.Printf("  config:  %s (volume/remote/include; add a .bdriveignore next to it to exclude paths)\n",
		filepath.Join(folder, config.ProjectFile))
	printCycle(res)

	if foreground {
		return daemon.Run(folder, scanInterval, remoteInterval)
	}
	pid, err := daemon.Start(folder, vdir, scanInterval, remoteInterval)
	if err != nil {
		return fmt.Errorf("start sync daemon: %w", err)
	}
	fmt.Printf("  daemon:  running (pid %d, scan %s, remote sync %s)\n", pid, scanInterval, remoteInterval)
	return nil
}
func umntCmd() *cobra.Command {
	var forget bool
	c := &cobra.Command{
		Use:     "umnt <folder>",
		Aliases: []string{"umount", "unmount"},
		Short:   "Stop syncing a mounted folder",
		Long: `Stop the sync daemon for a folder. Files stay on disk and the volume's
history is kept; "bdrive mnt" the folder again to resume syncing.

With --forget the folder is also removed from the mount registry (local
volume data under ~/.bdrive/volumes is still kept).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(args)
			if err != nil {
				return err
			}
			mi, err := mustMount(folder)
			if err != nil {
				return err
			}
			vdir, err := config.VolumeDir(mi.Volume)
			if err != nil {
				return err
			}
			stopped, err := daemon.Stop(vdir, config.MountID(folder))
			if err != nil {
				return err
			}
			if stopped {
				fmt.Printf("stopped sync daemon for %s\n", folder)
			} else {
				fmt.Printf("no daemon running for %s\n", folder)
			}
			if forget {
				mounts, err := config.LoadMounts()
				if err != nil {
					return err
				}
				delete(mounts, folder)
				if err := config.SaveMounts(mounts); err != nil {
					return err
				}
				fmt.Printf("forgot mount %s (volume %q kept under ~/.bdrive/volumes)\n", folder, mi.Volume)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&forget, "forget", false, "also remove the folder from the mount registry")
	return c
}
