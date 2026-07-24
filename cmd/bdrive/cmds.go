package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/daemon"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/syncer"
)

func syncCmd() *cobra.Command {
	var note string
	var noteTTL time.Duration
	var hookLabel string
	c := &cobra.Command{
		Use:   "sync [folder]",
		Short: "Sync a mounted folder with its remote now",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(args)
			if err != nil {
				return err
			}
			// Gate before openSession: hooks fire in every folder on every
			// turn, and must never enroll this device or resume a paused
			// project — that is `bdrive init`'s job alone.
			proj, ok, err := config.LoadProject(folder)
			if hookLabel != "" {
				// Agent-hook mode: event JSON on stdin, silent best-effort
				// sync, link-formula context on stdout. Never fails.
				if err != nil || !ok || syncBlocked(proj) != "" {
					return nil
				}
				return runHookSync(cmd, folder, hookLabel)
			}
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%s is not a beardrive project (run `bdrive init` there first)", folder)
			}
			switch syncBlocked(proj) {
			case "init":
				return fmt.Errorf("%s is not synced on this device yet (run `bdrive init` there to connect it)", folder)
			case "paused":
				return fmt.Errorf("syncing is paused for %s (run `bdrive init` there to resume)", folder)
			}
			sess, proj, err := openSession(cmd.Context(), folder, true)
			if err != nil {
				return err
			}
			defer closeSession(sess)
			if cmd.Flags().Changed("note") {
				// Persist the note so the daemon's own scans stamp it too —
				// history then links every change from this working session
				// to its context, not just the ones this invocation catches.
				// An explicit empty --note clears it. Expires after --note-ttl.
				if err := sess.Store.SaveNote(note, noteTTL); err != nil {
					return err
				}
				sess.Note = note
			}
			sess.OnProgress = progressReporter()
			res, err := sess.Cycle(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Printf("synced %s (project %q)\n", folder, proj.Volume)
			printCycle(res)
			return nil
		},
	}
	c.Flags().StringVar(&note, "note", "", "session context stamped onto changes (e.g. an agent session id); shown in history; empty clears")
	c.Flags().DurationVar(&noteTTL, "note-ttl", 30*time.Minute, "how long the note keeps applying to daemon-committed changes")
	c.Flags().StringVar(&hookLabel, "hook", "", "agent-hook mode: read the platform's hook event JSON from stdin, sync with a session note labeled by this value, and emit the project's link-formula context (Claude Code hook JSON) on stdout")
	return c
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
				proj, err := mustProject(folder) // also self-heals the registry
				if err != nil {
					return err
				}
				mounts = map[string]config.MountInfo{proj.ID: {Path: folder, Volume: proj.Volume, Remote: proj.Remote}}
			}
			if len(mounts) == 0 {
				fmt.Println("no beardrive projects on this device (run `bdrive init` in a folder)")
				return nil
			}
			dev, err := config.LoadDevice()
			if err != nil {
				return err
			}
			if settings, _ := config.LoadSettings(); settings.Email != "" {
				who := settings.Email
				if settings.Name != "" {
					who = settings.Name + " <" + settings.Email + ">"
				}
				fmt.Printf("device: %s (%s) signed in as %s\n\n", dev.Name, dev.ID, who)
			} else {
				fmt.Printf("device: %s (%s) as %s\n\n", dev.Name, dev.ID, dev.Author)
			}
			first := true
			for id, mi := range mounts {
				if !first {
					fmt.Println()
				}
				first = false
				folder := mi.Path
				if proj, ok, err := config.LoadProject(folder); err == nil && ok {
					mi.Volume, mi.Remote = proj.Volume, proj.Remote // folder config wins
				} else {
					fmt.Printf("%s\n  (folder missing — moved or deleted; run `bdrive init` at its new location)\n", folder)
					continue
				}
				fmt.Printf("%s\n", folder)
				fmt.Printf("  project:  %s (%s)\n", mi.Volume, id)
				if mi.Remote != "" {
					fmt.Printf("  remote:   %s\n", mi.Remote)
				} else {
					fmt.Printf("  remote:   (none — local only)\n")
				}
				vdir, err := config.VolumeDir(id)
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
				cache, err := sess.Store.LoadCache(id)
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
				// Prefer the signed-in account over the git/OS author fallback,
				// so team history shows hub identities.
				who := op.UserName
				if who == "" {
					who = op.User
				}
				if who == "" {
					who = op.Author
				}
				line := fmt.Sprintf("%s  %s  %-40s  %s on %s", when, kind, op.Path, who, op.DeviceName)
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
	run.Flags().DurationVar(&remoteInterval, "remote-interval", 10*time.Second, "remote sync interval")
	c.AddCommand(run)
	return c
}
