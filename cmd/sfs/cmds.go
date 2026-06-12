package main

import (
	"fmt"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/runbear-io/sfs/internal/config"
	"github.com/runbear-io/sfs/internal/daemon"
	"github.com/runbear-io/sfs/internal/journal"
	"github.com/runbear-io/sfs/internal/syncer"
)

func syncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync [folder]",
		Short: "Sync a mounted folder with its remote now",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(args)
			if err != nil {
				return err
			}
			sess, mi, err := openSession(cmd.Context(), folder, true)
			if err != nil {
				return err
			}
			defer closeSession(sess)
			res, err := sess.Cycle(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Printf("synced %s (volume %q)\n", folder, mi.Volume)
			printCycle(res)
			return nil
		},
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status [folder]",
		Short: "Show mount, sync, and daemon status",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mounts, err := config.LoadMounts()
			if err != nil {
				return err
			}
			if len(args) > 0 {
				folder, err := absFolder(args)
				if err != nil {
					return err
				}
				mi, ok := mounts[folder]
				if !ok {
					return fmt.Errorf("%s is not an sfs mount", folder)
				}
				mounts = map[string]config.MountInfo{folder: mi}
			}
			if len(mounts) == 0 {
				fmt.Println("no sfs mounts (create one with `sfs mnt <folder>`)")
				return nil
			}
			dev, err := config.LoadDevice()
			if err != nil {
				return err
			}
			fmt.Printf("device: %s (%s) as %s\n\n", dev.Name, dev.ID, dev.Author)
			first := true
			for folder, mi := range mounts {
				if !first {
					fmt.Println()
				}
				first = false
				fmt.Printf("%s\n", folder)
				fmt.Printf("  volume:   %s\n", mi.Volume)
				if mi.Remote != "" {
					fmt.Printf("  remote:   %s\n", mi.Remote)
				} else {
					fmt.Printf("  remote:   (none — local only)\n")
				}
				vdir, err := config.VolumeDir(mi.Volume)
				if err != nil {
					return err
				}
				if pid, ok := daemon.Running(vdir); ok {
					fmt.Printf("  daemon:   running (pid %d)\n", pid)
				} else {
					fmt.Printf("  daemon:   stopped\n")
				}
				sess, _, err := openSession(cmd.Context(), folder, false)
				if err != nil {
					continue
				}
				cache, err := sess.Store.LoadCache()
				if err == nil {
					var total int64
					for _, c := range cache {
						total += c.Size
					}
					fmt.Printf("  files:    %d (%s)\n", len(cache), humanBytes(total))
				}
				st, err := sess.Store.LoadSync()
				myOps, err2 := sess.Store.DeviceOps(dev.ID)
				if err == nil && err2 == nil {
					pending := int64(len(myOps)) - st.PushedOps
					if pending < 0 {
						pending = 0
					}
					fmt.Printf("  pending:  %d local change(s) not yet pushed\n", pending)
				}
			}
			return nil
		},
	}
}

func logCmd() *cobra.Command {
	var limit int
	var pathFilter string
	c := &cobra.Command{
		Use:   "log [folder]",
		Short: "Show change history: who changed which file, when, on which device",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(args)
			if err != nil {
				return err
			}
			sess, _, err := openSession(cmd.Context(), folder, false)
			if err != nil {
				return err
			}
			entries, err := syncer.LogEntries(sess.Store, pathFilter, limit)
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				fmt.Println("no history yet")
				return nil
			}
			for _, op := range entries {
				when := op.Time.Local().Format("2006-01-02 15:04:05")
				kind := op.Kind
				if kind == journal.KindPut {
					kind = "put   "
				} else {
					kind = "delete"
				}
				line := fmt.Sprintf("%s  %s  %-40s  %s on %s", when, kind, op.Path, op.Author, op.DeviceName)
				if op.Kind == journal.KindPut {
					line += fmt.Sprintf("  (%s)", humanBytes(op.Size))
				}
				if op.Note != "" {
					line += "  [" + op.Note + "]"
				}
				fmt.Println(line)
			}
			return nil
		},
	}
	c.Flags().IntVarP(&limit, "limit", "n", 50, "max entries to show (0 = all)")
	c.Flags().StringVarP(&pathFilter, "path", "p", "", "only show history for this file or directory")
	return c
}

func remoteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "remote [folder]",
		Short: "Show or set the cloud remote of a mounted folder",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(args)
			if err != nil {
				return err
			}
			mi, err := mustMount(folder)
			if err != nil {
				return err
			}
			if mi.Remote == "" {
				fmt.Println("(none)")
			} else {
				fmt.Println(mi.Remote)
			}
			return nil
		},
	}
	set := &cobra.Command{
		Use:   "set <folder> <url>",
		Short: "Set the remote (s3://bucket/prefix, gs://bucket/prefix, file:///path)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(args[:1])
			if err != nil {
				return err
			}
			raw := args[1]
			u, err := url.Parse(raw)
			if err != nil || (u.Scheme != "s3" && u.Scheme != "gs" && u.Scheme != "file") {
				return fmt.Errorf("invalid remote %q (want s3://bucket/prefix, gs://bucket/prefix, or file:///path)", raw)
			}
			mounts, err := config.LoadMounts()
			if err != nil {
				return err
			}
			mi, ok := mounts[folder]
			if !ok {
				return fmt.Errorf("%s is not an sfs mount (run `sfs mnt %s` first)", folder, folder)
			}
			mi.Remote = raw
			mounts[folder] = mi
			if err := config.SaveMounts(mounts); err != nil {
				return err
			}
			fmt.Printf("remote of %s set to %s\n", folder, raw)
			fmt.Println("run `sfs sync` to sync now (a running daemon picks it up automatically)")
			return nil
		},
	}
	c.AddCommand(set)
	return c
}

func daemonCmd() *cobra.Command {
	c := &cobra.Command{
		Use:    "daemon",
		Short:  "Manage the background sync daemon",
		Hidden: true,
	}
	var scanInterval, remoteInterval time.Duration
	run := &cobra.Command{
		Use:   "run <folder>",
		Short: "Run the sync daemon in the foreground (internal)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(args)
			if err != nil {
				return err
			}
			return daemon.Run(folder, scanInterval, remoteInterval)
		},
	}
	run.Flags().DurationVar(&scanInterval, "scan-interval", 3*time.Second, "local scan interval")
	run.Flags().DurationVar(&remoteInterval, "remote-interval", 30*time.Second, "remote sync interval")
	c.AddCommand(run)
	return c
}
