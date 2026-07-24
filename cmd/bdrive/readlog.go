package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
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
piped on stdin (any platform's PostToolUse-style payload). Coverage is
tool-aware: native read tools report their file paths directly, grep-style
search tools count the files their matches came from, and shell commands
count the existing files named as arguments (a "cat notes.md" or
"grep -n foo wiki/a.md" is a read). Listing tools (glob, ls) are ignored —
seeing a file's name is not reading it.

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
			// LoadProject, not ResolveMount: a hook must never enroll this
			// device (registry self-heal) — and syncBlocked keeps a paused
			// or never-inited project's spool from even being created.
			proj, found, err := config.LoadProject(folder)
			if err != nil || !found || syncBlocked(proj) != "" {
				return nil // not an actively synced project: fast no-op
			}
			data, _ := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
			paths := extractEventPaths(data, folder)
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

// Tool families, matched on the lowercased tool name from the event. Shell
// and search tools carry no read-path fields — their reads are mined from
// the command line / the match results instead — and listing tools are
// dropped entirely: seeing a file's name is not reading it. (A grep-style
// event must NOT fall through to the generic key walk: its `path` field is
// the search SCOPE, usually a directory.)
var (
	shellTools = map[string]bool{"bash": true, "shell": true, "run_shell_command": true, "execute_command": true}
	matchTools = map[string]bool{"grep": true, "search_file_content": true, "search": true, "ripgrep": true}
	listTools  = map[string]bool{"glob": true, "ls": true, "list_directory": true, "find_files": true}
)

const maxMinedPaths = 200 // bound stat() work; the hook runs on every tool call

// extractEventPaths pulls the file paths an agent just read out of a hook
// event JSON, dispatching on the tool that fired: read tools report their
// paths in well-known fields, shell commands and search results are mined
// heuristically (existing regular files only). Non-project paths are
// filtered by the caller.
func extractEventPaths(data []byte, folder string) []string {
	var root any
	if json.Unmarshal(data, &root) != nil {
		return nil
	}
	switch tool := eventToolName(root); {
	case listTools[tool]:
		return nil
	case shellTools[tool]:
		return statFiles(commandTokens(collectKeyStrings(root, "command")), folder)
	case matchTools[tool]:
		return statFiles(matchCandidates(root), folder)
	}
	return keyWalkPaths(root)
}

// eventToolName finds the tool that fired, wherever the platform puts it
// (Claude/Hermes tool_name, Gemini tool.name).
func eventToolName(root any) string {
	m, ok := root.(map[string]any)
	if !ok {
		return ""
	}
	if s, ok := m["tool_name"].(string); ok {
		return strings.ToLower(s)
	}
	if t, ok := m["tool"].(map[string]any); ok {
		if s, ok := t["name"].(string); ok {
			return strings.ToLower(s)
		}
	}
	return ""
}

// keyWalkPaths is the read-tool extraction: any path-shaped field anywhere
// in the event is a read (no existence check — a just-read file can already
// be gone by hook time).
func keyWalkPaths(root any) []string {
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

// collectKeyStrings gathers every string value stored under the given key
// anywhere in the event (e.g. all "command" fields of a shell tool call).
func collectKeyStrings(root any, key string) []string {
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, val := range t {
				if k == key {
					if s, ok := val.(string); ok {
						out = append(out, s)
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

var cmdSplitRe = regexp.MustCompile(`\|\||&&|[|;\n]`)

// commandTokens pulls read-candidate tokens out of shell command lines:
// per pipeline segment, redirection targets are cut (an "echo x > f" is a
// write, not a read), flags are dropped, quotes stripped. Which tokens are
// real files is decided by statFiles.
func commandTokens(commands []string) []string {
	var out []string
	for _, command := range commands {
		for _, seg := range cmdSplitRe.Split(command, -1) {
			if i := strings.IndexByte(seg, '>'); i >= 0 {
				seg = seg[:i]
			}
			for _, tok := range strings.Fields(seg) {
				tok = strings.Trim(tok, `"'`+"`")
				if tok == "" || strings.HasPrefix(tok, "-") {
					continue
				}
				out = append(out, tok)
				if len(out) >= maxMinedPaths {
					return out
				}
			}
		}
	}
	return out
}

// matchCandidates mines a search tool's result for the files the matches
// came from: every string in the response, line by line, both whole ("a
// filenames list") and up to the first colon ("path:12:matched text").
func matchCandidates(root any) []string {
	m, ok := root.(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	for _, key := range []string{"tool_response", "tool_output", "response", "result", "output"} {
		var strs []string
		var walk func(v any)
		walk = func(v any) {
			switch t := v.(type) {
			case string:
				strs = append(strs, t)
			case map[string]any:
				for _, val := range t {
					walk(val)
				}
			case []any:
				for _, it := range t {
					walk(it)
				}
			}
		}
		walk(m[key])
		for _, s := range strs {
			for _, line := range strings.Split(s, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				out = append(out, line)
				if i := strings.IndexByte(line, ':'); i > 0 {
					out = append(out, line[:i])
				}
				if len(out) >= maxMinedPaths {
					return out
				}
			}
		}
	}
	return out
}

// statFiles keeps the candidates that are existing regular files (absolute,
// or relative to the mount folder) — the guard that turns heuristic tokens
// into trustworthy reads.
func statFiles(candidates []string, folder string) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range candidates {
		abs := c
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(folder, c)
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		if fi, err := os.Stat(abs); err == nil && fi.Mode().IsRegular() {
			out = append(out, c)
		}
	}
	return out
}
