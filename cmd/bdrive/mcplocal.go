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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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
			if late := m.sweepDisplaced(); len(late) > 0 {
				note := fmt.Sprintf("\n[a file was saved by another program while it was being replaced; that save is kept as %s — check it and merge by hand]", strings.Join(late, ", "))
				if err != nil {
					err = fmt.Errorf("%w%s", err, note)
				} else {
					out += note
				}
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

// mcpBeforeRename is a test seam: it runs between put's final check and its
// rename, where an outside writer would race.
var mcpBeforeRename func()

// nowSHA is the sha of rel as it stands right now: the working file when on
// disk, else the version the journals fold to, else "" (no such file).
func (m *localMCP) nowSHA(rel string) (string, error) {
	abs := filepath.Join(m.v.root, filepath.FromSlash(rel))
	if f, _, err := store.OpenRegularUnderRoot(m.v.root, abs); err == nil {
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	} else if _, lerr := os.Lstat(abs); lerr == nil {
		return "", fmt.Errorf("%s is not a regular file", rel)
	}
	if err := m.v.refresh(); err != nil {
		return "", err
	}
	return m.v.ix.Files[rel].Blob, nil
}

// put writes content at rel, but only if rel still holds base (the sha the
// caller checked; "" for "no such file"). An editor's save or the daemon's
// pull landing after the caller's check is refused, never overwritten: see
// swapIn.
var errNoSwap = errors.New("atomic swap not supported here")

// Test seams around swapIn's two exchanges.
var (
	mcpBeforeSwapBack func()
	mcpBeforeCleanup  func()
	swapFn            = swapFiles
)

// swapIn moves the staged file tmp (whose sha is ours) to abs only if abs
// still holds base.
//
// A check followed by rename(2) always leaves a window in which another
// writer's save is overwritten. So the check comes AFTER the move: the file
// is atomically SWAPPED in (renamex_np / renameat2), which leaves whatever
// abs held at that instant at tmp, and that displaced file is what gets
// checked. If it is not base, someone else wrote in between: swap back and
// refuse. Whatever any swap displaces is deleted only when it is content we
// can name — base (already journaled) or our own bytes; anything else is a
// third party's save and is kept beside the file. A file that is not on disk
// is created with link(2), which fails if anything appeared meanwhile. A
// filesystem with no atomic exchange gets no overwrite at all.
func (m *localMCP) swapIn(rel, abs, tmp, base, ours string) error {
	if _, err := os.Lstat(abs); errors.Is(err, os.ErrNotExist) {
		now, err := m.nowSHA(rel)
		if err != nil {
			return err
		}
		if now != base {
			return changedErr(rel, now)
		}
		if err := os.Link(tmp, abs); err != nil {
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("%s appeared while you were changing it; nothing was written — read it again", rel)
			}
			return err
		}
		return nil
	}
	switch err := swapFn(tmp, abs); {
	case errors.Is(err, errNoSwap):
		return fmt.Errorf("%s: this filesystem cannot replace a file atomically, so overwriting it here could lose a concurrent edit; change it with your own file tools", rel)
	case err != nil:
		return err
	}
	prev, _ := fileSHA(tmp)
	if prev == base {
		return nil // tmp now holds base; put's cleanup removes it
	}
	if mcpBeforeSwapBack != nil {
		mcpBeforeSwapBack()
	}
	if err := swapFn(tmp, abs); err != nil {
		return fmt.Errorf("%s changed while you were changing it, and putting the other version back failed (%v); it is at %s", rel, err, keepDisplaced(abs, tmp))
	}
	if back, _ := fileSHA(tmp); back != ours {
		// abs was saved AGAIN between the two swaps: that save is now at tmp.
		return fmt.Errorf("%s is being changed by something else right now; nothing was written, and a version saved during the check is at %s", rel, keepDisplaced(abs, tmp))
	}
	return changedErr(rel, prev)
}

func (m *localMCP) displacedDir() string {
	return filepath.Join(m.v.root, config.ProjectDir, "displaced")
}

// displacedMeta is the sidecar beside each displaced file.
type displacedMeta struct {
	Path string `json:"path"` // project path it was displaced from
	Base string `json:"base"` // its sha at the moment of the swap
}

// preserveDisplaced moves the file a swap displaced from rel into
// .bdrive/displaced (never synced), with a sidecar naming rel and the sha it
// held; sweepDisplaced decides its fate later. If that fails — say rel sits
// on another filesystem than the project root (EXDEV) — the file is kept
// next to rel instead, in the same directory and so on the same filesystem,
// under a visible name, and the returned note tells the caller where.
func (m *localMCP) preserveDisplaced(rel, abs, tmp, base string) (note string) {
	dir := m.displacedDir()
	id := strconv.FormatInt(time.Now().UnixNano(), 10)
	meta, _ := json.Marshal(displacedMeta{Path: rel, Base: base})
	if os.MkdirAll(dir, 0o700) == nil &&
		store.WriteFileAtomic(filepath.Join(dir, id+".json"), meta, 0o600) == nil {
		if os.Rename(tmp, filepath.Join(dir, id)) == nil {
			return ""
		}
		os.Remove(filepath.Join(dir, id+".json"))
	}
	kept := keepDisplaced(abs, tmp)
	if kept == tmp {
		return fmt.Sprintf("the version this replaced could not be moved aside and is still at %s", tmp)
	}
	r, _ := filepath.Rel(m.v.root, kept)
	return fmt.Sprintf("the version this replaced is kept as %s (it syncs; delete it when done)", filepath.ToSlash(r))
}

// sweepDisplaced settles displaced versions. One that still holds exactly
// the sha it was displaced with is the previous version, already in history,
// and stays where it is. One that CHANGED was
// written after the swap — an editor saving through a descriptor it opened
// before — and is never deleted: it is moved next to its file as a visible,
// syncing "<name>.bdrive-mcp-late-<id>" and reported. It returns those paths.
func (m *localMCP) sweepDisplaced() []string {
	dir := m.displacedDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var promoted []string
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		var meta displacedMeta
		if err != nil || json.Unmarshal(raw, &meta) != nil {
			continue
		}
		file := filepath.Join(dir, id)
		sha, err := fileSHA(file)
		if err != nil {
			os.Remove(filepath.Join(dir, e.Name())) // the file itself is gone
			continue
		}
		if sha == meta.Base {
			// Untouched so far. Never deleted automatically: a program may
			// still hold it open and write to it at any later moment, and an
			// unlinked inode would take that write with it. Removing the
			// folder is the user's call (see README, On-demand files).
			continue
		}
		rel, err := cleanToolPath(meta.Path)
		if err != nil || rel == "" {
			continue // keep it where it is; never delete a changed one
		}
		name := path.Base(rel)
		if len(name) > 64 {
			name = name[:64]
		}
		late := path.Join(path.Dir(rel), name+".bdrive-mcp-late-"+id)
		dst := filepath.Join(m.v.root, filepath.FromSlash(late))
		if !store.UnderRoot(m.v.root, filepath.Dir(dst)) || os.MkdirAll(filepath.Dir(dst), 0o755) != nil {
			continue
		}
		if os.Link(file, dst) == nil {
			os.Remove(file)
			os.Remove(filepath.Join(dir, e.Name()))
			promoted = append(promoted, late)
		}
	}
	return promoted
}

// keepDisplaced moves a file a swap displaced to a visible name beside abs.
// The name is short whatever abs is called, so it cannot fail on a name
// length limit; if the rename fails anyway the file stays where it is (a
// .bdrive-tmp- name the scanner ignores) and that path is reported.
func keepDisplaced(abs, tmp string) string {
	name := filepath.Base(abs)
	if len(name) > 64 {
		name = name[:64]
	}
	kept := filepath.Join(filepath.Dir(abs), fmt.Sprintf("%s.bdrive-mcp-displaced-%d", name, time.Now().UnixNano()))
	if os.Rename(tmp, kept) != nil {
		return tmp
	}
	return kept
}

func changedErr(rel, now string) error {
	if now == "" {
		return fmt.Errorf("%s was deleted while you were changing it; nothing was written", rel)
	}
	return fmt.Errorf("%s changed while you were changing it (now sha:%s); nothing was written — read it again and redo the change", rel, now)
}

func fileSHA(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (m *localMCP) put(rel string, content []byte, base string) (out string, err error) {
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
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".bdrive-tmp-mcp-*")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(content)
	ours := hex.EncodeToString(sum[:])
	staged, err := tmp.Stat()
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	// The staging path is removed only while it still names OUR staged file.
	// After a swap it names the file that was displaced from abs, and that
	// inode is never unlinked: an editor holding it open may still write to
	// it, at any later moment. It moves to .bdrive/displaced (never synced,
	// pruned after a day) where such a write stays recoverable.
	defer func() {
		if mcpBeforeCleanup != nil {
			mcpBeforeCleanup()
		}
		fi, lerr := os.Lstat(tmp.Name())
		if lerr != nil {
			return // moved aside already
		}
		if os.SameFile(fi, staged) {
			os.Remove(tmp.Name())
			return
		}
		if note := m.preserveDisplaced(rel, abs, tmp.Name(), base); note != "" {
			if err != nil {
				err = fmt.Errorf("%w\n[%s]", err, note)
			} else {
				out += "\n[" + note + "]"
			}
		}
	}()
	_, werr := tmp.Write(content)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp.Name(), mode)
	}
	if werr != nil {
		return "", werr
	}
	if mcpBeforeRename != nil {
		mcpBeforeRename()
	}
	if err := m.swapIn(rel, abs, tmp.Name(), base, ours); err != nil {
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
	if !exists {
		sha = ""
	}
	return m.put(rel, []byte(in.Content), sha)
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
	return m.put(rel, []byte(text), sha)
}

func (m *localMCP) delete(ctx context.Context, in lPath) (string, error) {
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
	// The scan journals a delete only for a file this mount tracks. A file
	// that is on disk but not tracked yet — say a lazy path just written
	// here, before the daemon scanned it — would vanish locally while the
	// hub keeps its copy. Journal it first with one sync cycle.
	if _, known := m.v.ix.Files[rel]; known && !m.v.ix.Materialized(rel) {
		if _, err := m.v.sess.Cycle(ctx); err != nil {
			return "", fmt.Errorf("%s is not synced from this device yet, and syncing it failed: %v", rel, err)
		}
		if err := m.v.refresh(); err != nil {
			return "", err
		}
		if !m.v.ix.Materialized(rel) {
			return "", fmt.Errorf("%s is not synced from this device yet; try again after the next sync", rel)
		}
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
	if err := m.v.sess.Writable(rel); err != nil {
		return "", err
	}
	ops, err := syncer.LogEntries(m.v.sess.Store, rel, 0)
	if err != nil {
		return "", err
	}
	for _, op := range ops {
		if op.Path == rel && op.Kind == journal.KindPut && strings.HasPrefix(op.Blob, strings.ToLower(in.SHA)) {
			// Through put, like write: the same conditional swap and the same
			// keeping of whatever it displaces.
			if err := m.v.sess.EnsureContent(ctx, rel, journal.FileState{Blob: op.Blob, Size: op.Size}); err != nil {
				return "", err
			}
			content, err := os.ReadFile(m.v.sess.Store.BlobPath(op.Blob))
			if err != nil {
				return "", err
			}
			base, err := m.nowSHA(rel)
			if err != nil {
				return "", err
			}
			res, err := m.put(rel, content, base)
			if err != nil {
				return "", err
			}
			// put's reply may carry a note about where a displaced version
			// was kept; keep it.
			_, note, _ := strings.Cut(res, "\n")
			msg := fmt.Sprintf("restored %s to sha:%s; it syncs within seconds", rel, op.Blob)
			if note != "" {
				msg += "\n" + note
			}
			return msg, nil
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
