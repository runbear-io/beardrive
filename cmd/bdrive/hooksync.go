package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
	"github.com/spf13/cobra"
)

// `bdrive sync --hook <label>` is the agent-hook flavor of sync, run by the
// Claude Code UserPromptSubmit hook at every turn start. It does three
// things: pulls (a normal cycle), stamps the session note so every change
// this turn is attributed to the agent session, and — the part that keeps
// agents current no matter how stale their skill copy is — emits the
// project's gated-link formula as additionalContext, so the agent can
// append a hub link to any synced file path it mentions.
//
// Everything is best-effort: a hook must never fail the turn, so every
// error path is a silent, successful exit.

// hookNoteTTL mirrors `bdrive sync --note-ttl`'s default: the daemon's own
// scans keep stamping this session's changes for a while.
const hookNoteTTL = 30 * time.Minute

// emitContext is false for every mount after the first in one hook run: the
// hook's stdout contract is a single JSON object.
func runHookSync(cmd *cobra.Command, folder, label string, emitContext bool) error {
	// The platform pipes its event JSON on stdin; the session id is all we
	// need from it here.
	data, _ := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
	var event struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(data, &event) // malformed input: just sync

	sess, proj, err := openSession(cmd.Context(), folder, true)
	if err != nil {
		return nil // not a mount / no session: fast no-op
	}
	defer closeSession(sess)

	if event.SessionID != "" {
		note := label + " session " + event.SessionID
		if err := sess.Store.SaveNote(note, hookNoteTTL); err == nil {
			sess.Note = note
		}
	}

	// The pull. Offline is fine — the link formula below is still valid
	// for teammates who are online.
	if _, err := sess.Cycle(cmd.Context()); err != nil {
		return nil // never break the turn
	}

	if !emitContext {
		return nil
	}
	server, projectID, err := splitHubRemote(proj.Remote)
	if err != nil {
		return nil // non-hub remote: nothing to link to
	}
	base := server + "/" + projectID

	ctx := fmt.Sprintf(
		"beardrive: this folder syncs to %s (the project's hub page; files are at %s/<url-encoded path>). "+
			"Link convention: whenever you mention a synced file's path in prose, append its gated hub link on an emoji, formatted exactly as: `<path>` [🔗](%s/<url-encoded path>) — the path stays plain text, the hyperlink goes on the emoji only. "+
			"These links require hub sign-in + project membership, so they are safe to paste anywhere internal. "+
			"Only link files that actually sync (inside the shared scope, not ignored); keep paths inside code blocks or commands plain; give a raw URL only when the user needs to paste it outside this conversation. "+
			"`bdrive share <file>` mints PUBLIC no-account links — use it only when the user explicitly asks for a public link.",
		base, base, base)

	if filter, err := syncer.LoadFilter(folder, proj.Include); err == nil {
		if lines := staleLines(sess.Store, filter, time.Now().UTC()); len(lines) > 0 {
			ctx += "\n\nbeardrive: possibly out-of-date files in this project —\n" +
				strings.Join(lines, "\n") +
				"\nIf you read one of these, treat it as possibly out of date and say so when you cite it."
		}
	}

	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "UserPromptSubmit",
			"additionalContext": ctx,
		},
	}
	enc, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	fmt.Fprintln(cmd.OutOrStdout(), string(enc))
	return nil
}

// The staleness watchlist: files old enough to have rotted, sitting in a
// directory people are still editing. The hub's Dashboard already plots this
// judgment (reads × days-since-change) for a human who does not have the page
// open mid-task; these lines put the same judgment in front of the agent that
// is about to cite the file.
const (
	staleAge    = 90 * 24 * time.Hour // higher bar than the Dashboard's 30d: this injects on every turn
	churnWindow = 30 * 24 * time.Hour // matches the Dashboard's window
	maxStale    = 5                   // bounds the injected text to a few lines
)

// staleLines ranks the project's stale-but-surrounded-by-churn files from the
// local journals — never mtimes: a teammate who just ran `bdrive init` has
// today's mtime on every materialized file, so an mtime version would report a
// brand-new project to exactly the joiner it helps most. Best-effort: any
// error means no watchlist, never a failed turn.
func staleLines(st *store.Store, filter *syncer.Filter, now time.Time) []string {
	ops, err := st.AllOps()
	if err != nil {
		return nil
	}
	live := journal.Replay(ops) // deleted paths drop out here

	latest := make(map[string]journal.Op, len(live)) // newest put per live path
	for _, op := range ops {
		if op.Kind != journal.KindPut {
			continue
		}
		if _, ok := live[op.Path]; !ok {
			continue
		}
		if prev, ok := latest[op.Path]; !ok || journal.Less(prev, op) {
			latest[op.Path] = op
		}
	}

	churn := make(map[string]int) // dir -> files changed inside the churn window
	for p, op := range latest {
		if now.Sub(op.Time) < churnWindow {
			churn[path.Dir(p)]++
		}
	}

	type candidate struct {
		path  string
		days  int
		who   string
		churn int
	}
	var cands []candidate
	for p, op := range latest {
		age := now.Sub(op.Time)
		if age < staleAge {
			continue
		}
		n := churn[path.Dir(p)]
		if n < 1 || filter.Skip(p) { // out-of-scope files aren't on disk here
			continue
		}
		who := op.User
		if who == "" {
			who = op.UserName
		}
		if who == "" {
			who = op.Author
		}
		cands = append(cands, candidate{path: p, days: int(age.Hours() / 24), who: who, churn: n})
	}
	// Ties break on path: the same data must produce the same list every turn.
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.days*a.churn != b.days*b.churn {
			return a.days*a.churn > b.days*b.churn
		}
		return a.path < b.path
	})
	if len(cands) > maxStale {
		cands = cands[:maxStale]
	}

	lines := make([]string, 0, len(cands))
	for _, c := range cands {
		by := ""
		if c.who != "" {
			by = fmt.Sprintf(" (by %s)", c.who)
		}
		lines = append(lines, fmt.Sprintf("  %s — last changed %dd ago%s; %d files near it changed in the last 30d",
			c.path, c.days, by, c.churn))
	}
	return lines
}
