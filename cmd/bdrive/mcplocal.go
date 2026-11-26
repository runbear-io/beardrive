package main

// The local `bdrive mcp`: a stdio MCP server over ONE synced project on this
// machine, with the hub MCP's tool names (list, read, glob, grep, write,
// edit, delete, history, restore) so an agent learns one interface.
//
// It exists for lazy mode. A file lazy mode keeps on the hub is not on disk,
// so an agent's own Read and Grep tools cannot see it; these tools can. Reads
// come from the working file, else the local blob cache (keyed by sha256, so
// a hit needs no request), else the hub — and are cached. Writes go to the
// working folder, exactly like an editor's, and the sync engine journals
// them; nothing here writes a journal or the state cache.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/daemon"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
	"github.com/runbear-io/beardrive/internal/webapp"
)

const (
	localReadMax      = 64 << 20 // largest file read or edited whole
	localReadLines    = 2000
	localLineShown    = 4 << 10
	localGrepMatches  = 200
	localGlobEntries  = 1000
	localHistoryLimit = 20
)

const localMCPInstructions = `These tools read and write ONE BearDrive project synced on this machine: %s

Paths are relative to that folder (a leading / is accepted). The tool names match
the hub's BearDrive MCP, but nothing here goes over the network unless a file
is not on this machine.

Lazy mode may keep large files on the hub instead of on disk. Those files are
listed (marked "on hub") and readable through these tools, but your own file
tools cannot see them — use read and grep here. Writes land in the local folder
and sync to the hub within seconds; pass the sha you read to write/edit so a
teammate's newer version is never silently overwritten.

A project's AGENTS.md (or CLAUDE.md) says where files go; read it first.`

type localMCP struct {
	mu sync.Mutex
	v  *projectView
}

func runLocalMCP(ctx context.Context, folder string) error {
	v, err := openView(ctx, folder, true)
	if err != nil {
		return err
	}
	defer v.close()
	m := &localMCP{v: v}
	srv := mcp.NewServer(&mcp.Implementation{Name: "beardrive-local", Title: "BearDrive (local)", Version: "1"},
		&mcp.ServerOptions{Instructions: fmt.Sprintf(localMCPInstructions, v.root)})
	m.register(srv)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

type lList struct {
	Path  string `json:"path,omitempty" jsonschema:"Folder relative to the project root; empty for the root"`
	Depth int    `json:"depth,omitempty" jsonschema:"1 (default) lists one level; more lists every file below"`
}
type lRead struct {
	Path   string `json:"path" jsonschema:"File path relative to the project root"`
	Offset int    `json:"offset,omitempty" jsonschema:"Line number to start from (1-based)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum number of lines to return"`
	Raw    bool   `json:"raw,omitempty" jsonschema:"Return the exact bytes with no line numbers or header"`
}
type lGlob struct {
	Pattern string `json:"pattern" jsonschema:"Glob pattern, e.g. **/*.md"`
	Path    string `json:"path,omitempty" jsonschema:"Folder to search under"`
}
type lGrep struct {
	Pattern    string `json:"pattern" jsonschema:"Regular expression (RE2) to search for"`
	Path       string `json:"path,omitempty" jsonschema:"Folder to search under"`
	Glob       string `json:"glob,omitempty" jsonschema:"Only search files matching this glob"`
	IgnoreCase bool   `json:"ignore_case,omitempty" jsonschema:"Case-insensitive match"`
	FilesOnly  bool   `json:"files_only,omitempty" jsonschema:"Return only matching file paths"`
	Limit      int    `json:"limit,omitempty" jsonschema:"Maximum matching lines (default 200)"`
}
type lWrite struct {
	Path    string `json:"path" jsonschema:"File path relative to the project root"`
	Content string `json:"content" jsonschema:"Full new contents of the file"`
	SHA     string `json:"sha,omitempty" jsonschema:"The sha you last read. If the file changed since, the write is refused. Omit when creating a file."`
}
type lEdit struct {
	Path       string `json:"path" jsonschema:"File path relative to the project root"`
	OldString  string `json:"old_string" jsonschema:"Exact text to replace"`
	NewString  string `json:"new_string" jsonschema:"Replacement text"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"Replace every occurrence"`
	SHA        string `json:"sha,omitempty" jsonschema:"The sha you last read. If the file changed since, the edit is refused."`
}
type lPath struct {
	Path string `json:"path" jsonschema:"File path relative to the project root"`
}
type lHistory struct {
	Path  string `json:"path" jsonschema:"File path relative to the project root"`
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum versions (default 20)"`
}
type lRestore struct {
	Path string `json:"path" jsonschema:"File path relative to the project root"`
	SHA  string `json:"sha" jsonschema:"The sha of the version to restore, from history"`
}

func localTool[In any](srv *mcp.Server, m *localMCP, name, title, desc string, readOnly bool, fn func(context.Context, In) (string, error)) {
	closed := false
	ann := &mcp.ToolAnnotations{Title: title, OpenWorldHint: &closed}
	if readOnly {
		ann.ReadOnlyHint, ann.IdempotentHint = true, true
	} else {
		d := true
		ann.DestructiveHint = &d
	}
	mcp.AddTool(srv, &mcp.Tool{Name: name, Title: title, Description: desc, Annotations: ann},
		func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			var out string
			err := m.v.refresh()
			if err == nil {
				out, err = fn(ctx, in)
			}
			if err != nil {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out}}}, nil, nil
		})
}

func (m *localMCP) register(srv *mcp.Server) {
	localTool(srv, m, "list", "List files", "List files and folders. Files kept on the hub by lazy mode are marked (on hub).", true, m.list)
	localTool(srv, m, "read", "Read file", "Read a file with line numbers, from disk, the local cache, or the hub. Prints its sha for write/edit. raw:true returns the exact bytes.", true, m.read)
	localTool(srv, m, "glob", "Find files by name", "Find files by name pattern; ** spans folders, and a pattern with no slash matches at any depth.", true, m.glob)
	localTool(srv, m, "grep", "Search file contents", "Search file contents with a regular expression: files on disk, the local cache, and (for files lazy mode keeps on the hub) the hub. Says when results are cut.", true, m.grep)
	localTool(srv, m, "write", "Write file", "Create or overwrite a file in the local folder; it syncs within seconds. Pass the sha you read when changing an existing file.", false, m.write)
	localTool(srv, m, "edit", "Edit file", "Replace exact text in a file. Pass the sha you read to be refused if it changed meanwhile.", false, m.edit)
	localTool(srv, m, "delete", "Delete file", "Delete a file from the local folder; the delete syncs to everyone.", false, m.delete)
	localTool(srv, m, "history", "File history", "Who changed a file, when, and the sha of each version, from this device's journals.", true, m.history)
	localTool(srv, m, "restore", "Restore file version", "Bring back a past version of a file, by a sha from history.", false, m.restore)
}

// clean turns a tool path into a project path; "" is the root.
func cleanToolPath(p string) (string, error) {
	p = strings.Trim(path.Clean("/"+strings.TrimSpace(p)), "/")
	if p == "" {
		return "", nil
	}
	if !journal.SafePath(p) || config.ReservedPath(p) {
		return "", fmt.Errorf("%s: not a path in this project", p)
	}
	return p, nil
}

func (m *localMCP) list(_ context.Context, in lList) (string, error) {
	dir, err := cleanToolPath(in.Path)
	if err != nil {
		return "", err
	}
	rows := listDir(m.v.files(), dir, in.Depth > 1)
	if len(rows) == 0 {
		return "", fmt.Errorf("no such folder: /%s", dir)
	}
	if len(rows) > localGlobEntries {
		rows = append(rows[:localGlobEntries], fmt.Sprintf("[%d more — list a subfolder]", len(rows)-localGlobEntries))
	}
	return strings.Join(rows, "\n"), nil
}

func (m *localMCP) read(ctx context.Context, in lRead) (string, error) {
	rel, err := cleanToolPath(in.Path)
	if err != nil || rel == "" {
		return "", fmt.Errorf("name a file to read")
	}
	b, sha, src, err := m.v.readAll(ctx, rel, localReadMax)
	if err != nil {
		return "", err
	}
	if in.Raw {
		return string(b), nil
	}
	lines := strings.SplitAfter(string(b), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	start := max(in.Offset, 1)
	limit := in.Limit
	if limit <= 0 {
		limit = localReadLines
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s  sha:%s  (%d lines, from %s)\n", rel, sha, len(lines), src)
	end := min(len(lines), start-1+limit)
	for i := start - 1; i < end; i++ {
		l := strings.TrimRight(lines[i], "\r\n")
		if len(l) > localLineShown {
			l = l[:localLineShown] + " [line cut]"
		}
		fmt.Fprintf(&sb, "%6d\t%s\n", i+1, l)
	}
	if end < len(lines) {
		fmt.Fprintf(&sb, "[%d more lines — read with offset:%d]\n", len(lines)-end, end+1)
	}
	return sb.String(), nil
}

func (m *localMCP) glob(_ context.Context, in lGlob) (string, error) {
	dir, err := cleanToolPath(in.Path)
	if err != nil {
		return "", err
	}
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	files := m.v.files()
	var hits []string
	for rel, e := range files {
		if strings.HasPrefix(rel, prefix) && webapp.MatchGlob(in.Pattern, rel[len(prefix):]) {
			mark := ""
			if !e.onDisk {
				mark = "  (on hub)"
			}
			hits = append(hits, rel+mark)
		}
	}
	sort.Strings(hits)
	if len(hits) == 0 {
		return "No files match " + in.Pattern + ".", nil
	}
	more := ""
	if len(hits) > localGlobEntries {
		more = fmt.Sprintf("\n[%d more — narrow the pattern or path]", len(hits)-localGlobEntries)
		hits = hits[:localGlobEntries]
	}
	return strings.Join(hits, "\n") + more, nil
}

func (m *localMCP) grep(ctx context.Context, in lGrep) (string, error) {
	if in.Pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}
	expr := in.Pattern
	if in.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", fmt.Errorf("bad pattern: %v", err)
	}
	dir, err := cleanToolPath(in.Path)
	if err != nil {
		return "", err
	}
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	keep := func(rel string) bool {
		return strings.HasPrefix(rel, prefix) && (in.Glob == "" || webapp.MatchGlob(in.Glob, rel[len(prefix):]))
	}
	limit := in.Limit
	if limit <= 0 {
		limit = localGrepMatches
	}
	var (
		sb        strings.Builder
		lines     int
		files     = map[string]bool{}
		truncated bool
	)
	emit := func(rel string, h hit) bool {
		if lines >= limit {
			truncated = true
			return false
		}
		files[rel] = true
		if in.FilesOnly {
			fmt.Fprintln(&sb, rel)
		} else {
			t := h.text
			if len(t) > 400 {
				t = t[:400] + "…"
			}
			fmt.Fprintf(&sb, "%s:%d:%s\n", rel, h.line, t)
		}
		lines++
		return true
	}
	var local []string
	for rel, e := range m.v.files() {
		if e.onDisk && keep(rel) {
			local = append(local, rel)
		}
	}
	sort.Strings(local)
	grepLocal(m.v.root, local, re, in.FilesOnly, func(rel string, hits []hit) bool {
		for _, h := range hits {
			if !emit(rel, h) {
				return false
			}
		}
		return true
	})
	var notes []string
	if !truncated {
		var cut bool
		notes, cut = lazyGrep(ctx, m.v, re, expr, in.FilesOnly, keep, emit)
		truncated = truncated || cut
	}
	var out strings.Builder
	if lines == 0 {
		fmt.Fprintf(&out, "No matches for %s.\n", in.Pattern)
	} else {
		fmt.Fprintf(&out, "%d match(es) in %d file(s):\n%s", lines, len(files), sb.String())
	}
	for _, n := range notes {
		fmt.Fprintf(&out, "[%s]\n", n)
	}
	if truncated {
		fmt.Fprintf(&out, "[results cut at %d — raise limit or narrow path/glob]\n", limit)
	}
	return out.String(), nil
}

// current is rel's content and sha as of now, fetching a lazy file's hub
// version first — which also puts it in the store, so a concurrent edit on
// another device merges against it instead of forking a conflict copy.
func (m *localMCP) current(ctx context.Context, rel string) (b []byte, sha string, exists bool, err error) {
	abs := filepath.Join(m.v.root, filepath.FromSlash(rel))
	_, onDisk := os.Lstat(abs)
	if _, known := m.v.ix.Files[rel]; onDisk != nil && !known {
		return nil, "", false, nil
	}
	b, sha, _, err = m.v.readAll(ctx, rel, localReadMax)
	if err != nil {
		return nil, "", false, err
	}
	return b, sha, true, nil
}

func staleSHA(rel, want, have string) error {
	if want == "" || strings.HasPrefix(have, strings.ToLower(want)) && len(want) >= 8 {
		return nil
	}
	return fmt.Errorf("%s changed since you read it (now sha:%s); read it again and redo the change", rel, have)
}

func (m *localMCP) put(rel string, content []byte) (string, error) {
	if err := m.v.sess.Writable(rel); err != nil {
		return "", err
	}
	abs := filepath.Join(m.v.root, filepath.FromSlash(rel))
	// Containment BEFORE MkdirAll: MkdirAll follows symlinks, so checking
	// after it would already have created folders outside the project.
	if !store.UnderRoot(m.v.root, filepath.Dir(abs)) {
		return "", fmt.Errorf("%s: not a path in this project", rel)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	if !store.UnderRoot(m.v.root, filepath.Dir(abs)) {
		return "", fmt.Errorf("%s: not a path in this project", rel)
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Lstat(abs); err == nil {
		if !fi.Mode().IsRegular() {
			return "", fmt.Errorf("%s is not a regular file", rel)
		}
		mode = fi.Mode().Perm()
	}
	if err := store.WriteFileAtomic(abs, content, mode); err != nil {
		return "", err
	}
	daemon.WakeSupervisor()
	b, sha, _, err := m.v.readAll(context.Background(), rel, localReadMax)
	if err != nil || len(b) != len(content) {
		return "", fmt.Errorf("wrote %s but could not read it back", rel)
	}
	return fmt.Sprintf("wrote %s (%d bytes, sha:%s); it syncs to the hub within seconds", rel, len(content), sha), nil
}

func (m *localMCP) write(ctx context.Context, in lWrite) (string, error) {
	rel, err := cleanToolPath(in.Path)
	if err != nil || rel == "" || strings.HasSuffix(in.Path, "/") {
		return "", fmt.Errorf("name a file to write")
	}
	_, sha, exists, err := m.current(ctx, rel)
	if err != nil {
		return "", err
	}
	if in.SHA != "" && !exists {
		return "", fmt.Errorf("%s does not exist; omit sha to create it", rel)
	}
	if exists {
		if err := staleSHA(rel, in.SHA, sha); err != nil {
			return "", err
		}
	}
	return m.put(rel, []byte(in.Content))
}

func (m *localMCP) edit(ctx context.Context, in lEdit) (string, error) {
	rel, err := cleanToolPath(in.Path)
	if err != nil || rel == "" {
		return "", fmt.Errorf("name a file to edit")
	}
	if in.OldString == "" {
		return "", fmt.Errorf("old_string is required")
	}
	b, sha, exists, err := m.current(ctx, rel)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("%s: no such file", rel)
	}
	if err := staleSHA(rel, in.SHA, sha); err != nil {
		return "", err
	}
	text := string(b)
	n := strings.Count(text, in.OldString)
	switch {
	case n == 0:
		return "", fmt.Errorf("old_string not found in %s", rel)
	case n > 1 && !in.ReplaceAll:
		return "", fmt.Errorf("old_string occurs %d times in %s; add context or pass replace_all", n, rel)
	}
	if in.ReplaceAll {
		text = strings.ReplaceAll(text, in.OldString, in.NewString)
	} else {
		text = strings.Replace(text, in.OldString, in.NewString, 1)
	}
	return m.put(rel, []byte(text))
}

func (m *localMCP) delete(_ context.Context, in lPath) (string, error) {
	rel, err := cleanToolPath(in.Path)
	if err != nil || rel == "" {
		return "", fmt.Errorf("name a file to delete")
	}
	abs := filepath.Join(m.v.root, filepath.FromSlash(rel))
	fi, err := os.Lstat(abs)
	if errors.Is(err, os.ErrNotExist) {
		if _, known := m.v.ix.Files[rel]; known {
			// Lazy mode never treats an absent file as deleted, so there is
			// no local action that deletes one. Say how, rather than pretend.
			return "", fmt.Errorf("%s is kept on the hub (lazy mode), not on this disk: delete it from the hub (its web page or the hub's MCP), or `bdrive pin` its folder first", rel)
		}
		return "", fmt.Errorf("%s: no such file", rel)
	}
	if err != nil {
		return "", err
	}
	if fi.IsDir() || !store.UnderRoot(m.v.root, filepath.Dir(abs)) {
		return "", fmt.Errorf("%s is not a file", rel)
	}
	if err := m.v.sess.Writable(rel); err != nil {
		return "", err
	}
	if err := os.Remove(abs); err != nil {
		return "", err
	}
	daemon.WakeSupervisor()
	return "deleted " + rel + "; the delete syncs within seconds", nil
}

func (m *localMCP) history(_ context.Context, in lHistory) (string, error) {
	rel, err := cleanToolPath(in.Path)
	if err != nil || rel == "" {
		return "", fmt.Errorf("name a file")
	}
	limit := in.Limit
	if limit <= 0 {
		limit = localHistoryLimit
	}
	ops, err := syncer.LogEntries(m.v.sess.Store, rel, 0)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	n := 0
	for _, op := range ops {
		if op.Path != rel {
			continue
		}
		if n == limit {
			sb.WriteString("[older versions not shown — raise limit]\n")
			break
		}
		who := op.UserName
		if who == "" {
			who = op.DeviceName
		}
		what := "deleted"
		if op.Kind == journal.KindPut {
			what = fmt.Sprintf("sha:%s  %s", op.Blob, humanBytes(op.Size))
		}
		fmt.Fprintf(&sb, "%s  %s  %s\n", op.Time.Local().Format("2006-01-02 15:04"), safeField(who, 60), what)
		n++
	}
	if n == 0 {
		return "", fmt.Errorf("%s has no history on this device", rel)
	}
	return sb.String(), nil
}

func (m *localMCP) restore(ctx context.Context, in lRestore) (string, error) {
	rel, err := cleanToolPath(in.Path)
	if err != nil || rel == "" || len(in.SHA) < 8 {
		return "", fmt.Errorf("name a file and a sha from history")
	}
	ops, err := syncer.LogEntries(m.v.sess.Store, rel, 0)
	if err != nil {
		return "", err
	}
	for _, op := range ops {
		if op.Path == rel && op.Kind == journal.KindPut && strings.HasPrefix(op.Blob, strings.ToLower(in.SHA)) {
			if err := m.v.sess.Restore(ctx, rel, op.Blob); err != nil {
				return "", err
			}
			daemon.WakeSupervisor()
			return fmt.Sprintf("restored %s to sha:%s; it syncs within seconds", rel, op.Blob), nil
		}
	}
	return "", fmt.Errorf("no version of %s with sha %s (see history)", rel, in.SHA)
}

// mcpServeRun is `bdrive mcp` with no subcommand.
func mcpServeRun(cmd *cobra.Command, folder string) error {
	if folder == "" {
		var err error
		if folder, err = os.Getwd(); err != nil {
			return err
		}
	}
	return runLocalMCP(cmd.Context(), folder)
}
