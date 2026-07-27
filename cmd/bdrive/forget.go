package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/syncer"
)

// forgetCmd is the one-step "stop syncing this and clean it up": it writes the
// rule into .bdriveignore (which syncs, so every device agrees) and then runs
// a prune cycle to remove what already reached the hub. Nothing is deleted
// from disk, here or anywhere else.
func forgetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "forget <path>...",
		Short: "Stop syncing a path and remove it from the hub (keeps local files)",
		Long: `Add a path to .bdriveignore and remove what already synced from the hub.

Local files are never touched — not here, not on teammates' devices. Only the
hub's copy goes away, and because .bdriveignore syncs, every device stops
tracking the path as it picks up the rule.

Use this to clean up something that synced before you meant to exclude it.
Nothing is destroyed: the removal is an ordinary journaled delete, so it shows
in bdrive log and the hub keeps every past version in history.`,
		Example: `  bdrive forget .omc              # stop syncing ./.omc and drop it from the hub
  bdrive forget notes/private.md
  bdrive sync --prune             # re-run the cleanup for rules you added by hand`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			root, proj, err := findProject(cwd)
			if err != nil {
				return err
			}
			switch syncBlocked(proj) {
			case "init":
				return fmt.Errorf("%s is not synced on this device yet (run `bdrive init` there to connect it)", root)
			case "paused":
				return fmt.Errorf("syncing is paused for %s (run `bdrive init` there to resume)", root)
			}

			// Resolve every path first: an argument outside the project is an
			// error that writes nothing at all.
			rules := make([]string, 0, len(args))
			for _, arg := range args {
				rule, err := ignoreRule(root, arg)
				if err != nil {
					return err
				}
				rules = append(rules, rule)
			}
			added, err := appendIgnoreRules(root, rules)
			if err != nil {
				return err
			}
			for _, rule := range rules {
				if added[rule] {
					fmt.Printf("added `%s` to %s\n", rule, syncer.IgnoreFile)
				} else {
					fmt.Printf("`%s` was already in %s\n", rule, syncer.IgnoreFile)
				}
			}

			sess, proj, err := openSession(cmd.Context(), root, true)
			if err != nil {
				return err
			}
			defer closeSession(sess)
			sess.Prune = true
			sess.OnProgress = progressReporter()
			res, err := sess.Cycle(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Printf("synced %s (project %q)\n", root, proj.Volume)
			printCycle(res)
			if res.Pruned == 0 {
				fmt.Println("  (nothing left to remove from the hub)")
			}
			return nil
		},
	}
}

// ignoreRule turns a command-line path into a .bdriveignore line, relative to
// the mount root. Directories get a trailing slash so the rule covers their
// contents, matching gitignore's reading of the same syntax.
func ignoreRule(root, arg string) (string, error) {
	abs, err := filepath.Abs(arg)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the project at %s", abs, root)
	}
	rel = filepath.ToSlash(rel)
	if rel == syncer.IgnoreFile {
		return "", fmt.Errorf("%s carries the rules themselves and always syncs", syncer.IgnoreFile)
	}
	if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
		rel += "/"
	}
	return rel, nil
}

// appendIgnoreRules adds any rules the file does not already carry, and
// reports which ones it wrote. Idempotent: re-running only prunes.
func appendIgnoreRules(root string, rules []string) (map[string]bool, error) {
	path := filepath.Join(root, syncer.IgnoreFile)
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	present := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		present[strings.TrimSpace(line)] = true
	}
	added := map[string]bool{}
	body := string(data)
	for _, rule := range rules {
		if present[rule] || added[rule] {
			continue
		}
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		body += rule + "\n"
		added[rule] = true
	}
	if len(added) == 0 {
		return added, nil
	}
	return added, os.WriteFile(path, []byte(body), 0o644)
}
