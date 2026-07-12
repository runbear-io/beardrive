package main

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
)

// read-log is the agent read hook's command: it runs after every file-read
// tool call, so it must be a fast, silent no-op in every case that isn't
// "a synced file was just read" — no network, no locking, one appended line.
func readLogCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "read-log [folder]",
		Short: "Record agent file reads from a hook event (JSON on stdin)",
		Long: `Record which project files an agent just read, from the hook event JSON
piped on stdin (any platform's PostToolUse-style payload: file paths are
found wherever they appear in the event).

Reads are queued locally in the volume store and drained to the hub on the
next sync, where they show up as agent traffic in the project's read heat.
Registered automatically by "bdrive hooks install"; there is rarely a reason
to run it by hand.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(args)
			if err != nil {
				return nil
			}
			proj, found, err := config.ResolveMount(folder)
			if err != nil || !found {
				return nil // not a beardrive project: fast no-op
			}
			data, _ := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
			paths := extractEventPaths(data)
			if len(paths) == 0 {
				return nil
			}
			filter, err := syncer.LoadFilter(folder, proj.Include)
			if err != nil {
				return nil
			}
			vdir, err := config.VolumeDir(proj.ID)
			if err != nil {
				return nil
			}
			st, err := store.Open(vdir)
			if err != nil {
				return nil
			}
			for _, p := range paths {
				abs := p
				if !filepath.IsAbs(abs) {
					abs = filepath.Join(folder, p)
				}
				rel, err := filepath.Rel(folder, abs)
				if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
					continue // outside the mount
				}
				rel = filepath.ToSlash(rel)
				if filter.Skip(rel) {
					continue // not part of the project (ignore/include rules)
				}
				st.LogRead(rel) // best-effort; the hook must never fail the turn
			}
			return nil
		},
	}
}

// Keys that carry file paths in the hook payloads of the supported agent
// platforms (Claude Code tool_input.file_path, Gemini/Hermes read_file
// path/absolute_path, multi-read paths arrays).
var (
	eventPathKeys     = map[string]bool{"file_path": true, "absolute_path": true, "path": true, "notebook_path": true}
	eventPathListKeys = map[string]bool{"paths": true, "file_paths": true}
)

// extractEventPaths pulls candidate file paths out of an arbitrary hook
// event JSON. The hook only fires on read-tool matchers, so any path-shaped
// field is a read; non-project paths are filtered by the caller.
func extractEventPaths(data []byte) []string {
	var root any
	if json.Unmarshal(data, &root) != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, val := range t {
				switch {
				case eventPathKeys[k]:
					if s, ok := val.(string); ok {
						add(s)
					}
				case eventPathListKeys[k]:
					if arr, ok := val.([]any); ok {
						for _, it := range arr {
							if s, ok := it.(string); ok {
								add(s)
							}
						}
					}
				}
				walk(val)
			}
		case []any:
			for _, it := range t {
				walk(it)
			}
		}
	}
	walk(root)
	return out
}
