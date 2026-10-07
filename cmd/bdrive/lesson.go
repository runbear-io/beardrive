package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
)

// `bdrive lesson "<text>"` files a correction into the project, where the
// sync hook hands it to every teammate's agent on its next turn (see
// hookLessons). One file per DEVICE ID — lessons/<id>.md — is the whole
// safety story: a device is the only writer of its own file, so two people
// recording a lesson at the same moment never produce a conflict copy, the
// way two appends to one shared LESSONS.md would.

// lessonsDir is where lessons live, relative to the mount root.
const lessonsDir = "lessons"

// lessonHeader opens every lesson file. The hook only trusts files that start
// with it, so an unrelated lessons/ folder (course notes, say) is never
// injected into agents as corrections to follow.
const lessonHeader = "# Lessons from "

// lessonMaxRunes bounds one lesson: it lands in every teammate's turn.
const lessonMaxRunes = 500

func lessonCmd() *cobra.Command {
	var folder string
	c := &cobra.Command{
		Use:   "lesson <text>",
		Short: "Record a correction every teammate's agent is told on its next turn",
		Long: `Append one line to lessons/<device-id>.md in this project and sync it.
On their next turn, every teammate's agent is told the new line through the
sync hook (bdrive sync --hook), once.

Each device writes only its own file, so two people recording a lesson at
the same time never conflict. Edit or remove a lesson by editing the file.

Refuses, writing nothing, when lessons/ would never reach the hub: ignored
by .bdriveignore, outside the folder's sync scope, or not writable for you.`,
		Example: `  bdrive lesson "use pnpm, never npm"
  bdrive lesson --folder wiki "notes/readme.md is generated — edit notes/src instead"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			text := lessonText(strings.Join(args, " "))
			if text == "" {
				return errors.New("a lesson needs some text")
			}
			if utf8.RuneCountInString(text) > lessonMaxRunes {
				return fmt.Errorf("a lesson is one line of at most %d characters — it lands in every teammate's turn", lessonMaxRunes)
			}
			abs, err := absFolder([]string{folder})
			if err != nil {
				return err
			}
			targets := syncTargets(abs)
			switch {
			case len(targets) == 0:
				return notAProject(abs)
			case len(targets) > 1:
				return fmt.Errorf("%s holds several BearDrive projects — pick one with --folder <project folder>", abs)
			}
			root := targets[0]
			proj, ok, err := config.LoadProject(root)
			if err != nil {
				return err
			}
			if !ok {
				return notAProject(root)
			}
			switch syncBlocked(proj) {
			case "init":
				return fmt.Errorf("%s is not synced on this device yet (run `bdrive init` there to connect it)", root)
			case "paused":
				return fmt.Errorf("syncing is paused for %s (run `bdrive init` there to resume)", root)
			}
			dev, err := config.LoadDevice()
			if err != nil {
				return err
			}
			rel := lessonsDir + "/" + dev.ID + ".md"
			if err := lessonWritable(root, proj, rel); err != nil {
				return err
			}

			who := dev.Author
			if s, err := config.LoadSettings(); err == nil && s.Email != "" {
				who = s.Email
			}
			if err := appendLesson(filepath.Join(root, filepath.FromSlash(rel)), dev.Name, text, who, time.Now()); err != nil {
				return err
			}

			sess, _, err := openSession(cmd.Context(), root, true)
			if err != nil {
				return err
			}
			defer closeSession(sess)
			res, err := sess.Cycle(cmd.Context())
			out := cmd.OutOrStdout()
			switch {
			case err != nil || res.Offline:
				fmt.Fprintf(out, "Recorded in %s; will sync when online.\n", rel)
			case res.ReadOnly || res.NoAccess:
				msg := "the hub refused this device's changes, so teammates will not see the lesson"
				if reason := res.Reason(); reason != "" {
					msg += ": " + safeField(reason, 300)
				}
				return fmt.Errorf("recorded in %s, but %s", rel, msg)
			default:
				fmt.Fprintf(out, "Recorded in %s — teammates' agents see it on their next turn.\n", rel)
			}
			return nil
		},
	}
	c.Flags().StringVar(&folder, "folder", ".", "the project folder to record the lesson in")
	return c
}

// lessonText collapses a lesson to one line and drops a leading list marker,
// since the file adds its own.
func lessonText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(strings.TrimPrefix(s, "- "))
}

// lessonWritable refuses a lesson that would never reach the hub, before
// anything is written: a file that only ever lives on this disk tells the
// user it was shared when it was not. Reads the scope the last cycle
// persisted, the same unlocked, Stat-guarded way `bdrive stale` does.
func lessonWritable(root string, proj config.Project, rel string) error {
	filter, err := syncer.LoadFilter(root, proj.Include)
	if err != nil {
		return err
	}
	var st store.SyncState
	if vdir, verr := config.VolumeDir(proj.ID); verr == nil && dirExists(vdir) {
		if s, serr := store.Open(vdir); serr == nil {
			st, _ = s.LoadSync()
		}
	}
	filter.AcceptRules(st.IgnoreAccepted)
	if filter.SkipUp(rel) {
		return fmt.Errorf("%s/ is outside this folder's sync scope — run: bdrive scope add %s", lessonsDir, lessonsDir)
	}
	for _, pre := range append(st.ReadOnly, st.Denied...) {
		if strings.HasPrefix(rel, pre) {
			return fmt.Errorf("you can't write to %s/ in this project (folder permission)", lessonsDir)
		}
	}
	if st.Access != store.AccessOK {
		msg := "this device can't push to the project (" + st.Access + ")"
		if st.AccessReason != "" {
			msg += ": " + safeField(st.AccessReason, 300)
		}
		return errors.New(msg)
	}
	return nil
}

// appendLesson adds one lesson line, writing the header first when the file
// is new. A plain append is safe: this device is the file's only writer, and
// the scanner picks it up like any other edit.
func appendLesson(path, deviceName, text, who string, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	var b strings.Builder
	if fi, err := f.Stat(); err == nil && fi.Size() == 0 {
		b.WriteString(lessonHeader + deviceName + "\n\n")
	} else if err == nil {
		// A hand edit may have left the last line unterminated; the new
		// lesson must not glue itself onto it.
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, fi.Size()-1); err == nil && last[0] != '\n' {
			b.WriteString("\n")
		}
	}
	fmt.Fprintf(&b, "- %s — %s, %s\n", text, who, now.Format("2006-01-02"))
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
