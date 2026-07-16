package main

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

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

func runHookSync(cmd *cobra.Command, folder, label string) error {
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

	server, projectID, err := splitHubRemote(proj.Remote)
	if err != nil {
		return nil // non-hub remote: nothing to link to
	}
	base := server + "/" + projectID

	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName": "UserPromptSubmit",
			"additionalContext": fmt.Sprintf(
				"beardrive: this folder syncs to %s (the project's hub page; files are at %s/<url-encoded path>). "+
					"Link convention: whenever you mention a synced file's path in prose, append its gated hub link on an emoji, formatted exactly as: `<path>` [🔗](%s/<url-encoded path>) — the path stays plain text, the hyperlink goes on the emoji only. "+
					"These links require hub sign-in + project membership, so they are safe to paste anywhere internal. "+
					"Only link files that actually sync (inside the shared scope, not ignored); keep paths inside code blocks or commands plain; give a raw URL only when the user needs to paste it outside this conversation. "+
					"`bdrive share <file>` mints PUBLIC no-account links — use it only when the user explicitly asks for a public link.",
				base, base, base),
		},
	}
	enc, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	fmt.Fprintln(cmd.OutOrStdout(), string(enc))
	return nil
}
