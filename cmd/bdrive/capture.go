package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/syncer"
)

// captureDir is hard-coded on purpose. A per-project setting is the thing to
// add when a team asks for a different name — until then, one convention is
// what makes `| bdrive capture` mean the same thing in every project, and
// changing it later is a folder rename, which sync already handles.
const captureDir = "inbox"

// isInteractive is a var so the test can flip it: `go test` hands the process
// /dev/null on stdin, which makes the TTY branch otherwise unreachable.
var isInteractive = stdinIsTTY

// captureCmd files whatever is piped into it as a dated markdown file in the
// project's inbox/ — the one step between an agent finishing a piece of work
// and teammates (and their agents) actually seeing it.
func captureCmd() *cobra.Command {
	var share bool
	c := &cobra.Command{
		Use:   "capture",
		Short: "File piped input into this project's inbox/",
		Long: `Read stdin and write it verbatim to inbox/<date>-<time>.md in this
project, then print that path. A pipe is the whole interface: there is no
file argument and no inline text.

The file is an ordinary file in the project, so the daemon journals it
within seconds and History attributes it to this device and account like any
other change. --share syncs it immediately and mints a public link for it.

If the project's sync scope excludes inbox/ (bdrive init --only, bdrive
scope add), capture refuses rather than writing a file that would sit on
this machine forever without ever reaching the hub.`,
		Example: `  claude -p "summarise today's incident" | bdrive capture
  claude -p "write the release notes" | bdrive capture --share
  git log --oneline -20 | bdrive capture`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if isInteractive() {
				return fmt.Errorf(`nothing on stdin; pipe something in (e.g. claude -p "..." | bdrive capture)`)
			}
			// Not io.LimitReader: readlog.go and hooksync.go bound an
			// untrusted hook event, this is a transcript the user piped in
			// and silently truncating it is the one unacceptable failure.
			data, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return err
			}
			if len(data) == 0 {
				return fmt.Errorf("nothing on stdin; captured nothing")
			}

			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			root, proj, err := findProject(cwd)
			if err != nil {
				return err
			}

			// A paused or never-enrolled project still gets the file: it
			// syncs the moment someone runs `bdrive init` again. --share is
			// the exception — it cannot run the cycle its promise depends on.
			switch blocked := syncBlocked(proj); {
			case blocked == "":
			case share:
				return fmt.Errorf("--share needs syncing, and %s for %s (run `bdrive init` there to %s)",
					map[string]string{"init": "this device has not connected", "paused": "syncing is paused"}[blocked],
					root, map[string]string{"init": "connect it", "paused": "resume"}[blocked])
			default:
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s is not syncing (%s); the capture stays on this machine until you run `bdrive init` there\n", root, blocked)
			}

			rel := filepath.ToSlash(filepath.Join(captureDir, time.Now().Format("2006-01-02-150405")+".md"))

			// The scope gate, before anything touches disk — not even an
			// empty inbox/. SkipUp, not Skip, and with the accepted rules
			// installed: that is exactly what the scan applies, so this
			// answers "will the cycle upload it?" rather than "do the live
			// rules mention it?". A teammate's pulled `!inbox/` widening rule
			// reads as "syncs" to a bare filter and is silently not uploaded.
			if f, ferr := syncer.LoadFilter(root, proj.Include); ferr == nil {
				f.AcceptRules(acceptedRules(proj.ID))
				if f.SkipUp(rel) {
					return fmt.Errorf("%s/ is excluded from this project's sync rules, so nothing captured there would reach the hub\n"+
						"run `bdrive scope add %s` first (or drop the rule from .bdriveignore)", captureDir, captureDir)
				}
			}

			if err := os.MkdirAll(filepath.Join(root, captureDir), 0o755); err != nil {
				return err
			}
			// O_EXCL on the real name, not store.WriteFileAtomic: the rename
			// an atomic write ends with is exactly what would lose a race
			// between two captures in the same second.
			rel, err = writeCapture(root, rel, data)
			if err != nil {
				return err
			}

			// Path first, always: with --share a later failure (a 409 on a
			// credential, say) must not lose where the bytes went.
			fmt.Fprintln(cmd.OutOrStdout(), rel)
			if !share {
				return nil
			}
			return shareCapture(cmd, root, proj, rel)
		},
	}
	c.Flags().BoolVar(&share, "share", false, "sync it now and print a public link for it")
	return c
}

// writeCapture writes data at rel under root, stepping the name to -2, -3, …
// until one is free, and returns the name it used.
func writeCapture(root, rel string, data []byte) (string, error) {
	base := rel[:len(rel)-len(".md")]
	for n := 1; ; n++ {
		try := rel
		if n > 1 {
			try = fmt.Sprintf("%s-%d.md", base, n)
		}
		f, err := os.OpenFile(filepath.Join(root, filepath.FromSlash(try)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		defer f.Close()
		if _, err := f.Write(data); err != nil {
			return "", err
		}
		return try, f.Close()
	}
}

// shareCapture pushes the capture and mints its link. The hub can only share
// what it already holds, so this runs a cycle first rather than telling the
// user to wait for the daemon.
func shareCapture(cmd *cobra.Command, root string, proj config.Project, rel string) error {
	settings, err := config.LoadSettings()
	if err != nil {
		return err
	}
	server, projectID, err := splitHubRemote(proj.Remote)
	if err != nil {
		return err
	}
	sess, _, err := openSession(cmd.Context(), root, true)
	if err != nil {
		return err
	}
	defer closeSession(sess)
	if _, err := sess.Cycle(cmd.Context()); err != nil {
		return err
	}
	link, _, err := mintShare(settings, server, projectID, rel, 0, false)
	var sec secretsError
	if errors.As(err, &sec) {
		// The bytes are safe and their path is already on stdout — only the
		// link was refused, and capture grows no --force of its own.
		return fmt.Errorf("%s\nThe capture itself is written and syncing; `bdrive share %s --force` shares it anyway", sec.msg, rel)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), link)
	printIfPrivate(link)
	return nil
}
