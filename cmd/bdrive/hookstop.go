package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
)

// `bdrive sync --hook-stop <label>` is the turn-end receipt, run by the Claude
// Code Stop hook. The push hook is async and discards its output, so without
// it an agent says "done, wrote x.html 🔗" about a file that never left the
// laptop. One blocking cycle (under the volume flock, so it waits out any
// in-flight async push), then: if any of THIS session's own ops are still
// past PushedOps, block the stop once with a reason naming them.
//
// Session ops match on Op.Session OR the session note: Op.Session is stamped
// only by the hook's own cycles, while the mid-turn writes are committed by
// the async push hook and the daemon, which carry only the note. The note is
// forgeable, but only by this device's user, for a local self-report.
//
// Same posture as --hook: fail open. Any error is a silent, successful exit,
// and stop_hook_active means we already blocked this stop once — never loop.

// stopItem is one path the receipt names, with why it is not on the hub.
type stopItem struct {
	path, reason string
}

const stopReasonPaused = "sync paused — run bdrive init to resume"

// hookStopEvent parses the Stop event from stdin: the session id and whether
// Claude is already continuing because of a stop hook.
func hookStopEvent(cmd *cobra.Command) (sessionID string, active bool) {
	data, _ := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
	var event struct {
		SessionID      string `json:"session_id"`
		StopHookActive bool   `json:"stop_hook_active"`
	}
	if json.Unmarshal(data, &event) != nil {
		return "", false
	}
	return event.SessionID, event.StopHookActive
}

// runHookStopAll is the whole --hook-stop run over every mount under folder.
func runHookStopAll(cmd *cobra.Command, folder, label string) {
	sessionID, active := hookStopEvent(cmd)
	if active || sessionID == "" {
		return // already blocked once / nothing to scope to
	}
	var items []stopItem
	for _, target := range syncTargets(folder) {
		proj, ok, err := config.LoadProject(target)
		if err != nil || !ok {
			continue
		}
		link := hookLinkFor(folder, target, "")
		var got []stopItem
		switch syncBlocked(proj) {
		case "init":
			continue // not this device's mount
		case "paused":
			got = pausedStop(proj, target)
		default:
			got = runHookStop(cmd, target, sessionID, label)
		}
		for _, it := range got {
			if p, ok := hookAgentPath(link, it.path); ok {
				items = append(items, stopItem{p, it.reason})
			}
		}
	}
	emitStopReceipt(cmd, items)
}

// runHookStop cycles one mount and lists this session's ops the hub still
// does not have, mount-relative.
func runHookStop(cmd *cobra.Command, target, sessionID, label string) []stopItem {
	sess, _, err := openSession(cmd.Context(), target, true)
	if err != nil {
		return nil
	}
	defer closeSession(sess)
	stampHookSession(sess, sessionID, label)
	res, err := sess.Cycle(cmd.Context())
	if err != nil {
		return nil
	}

	var items []stopItem
	st, err1 := sess.Store.LoadSync()
	ops, err2 := sess.Store.DeviceOps(sess.Device.ID)
	if err1 == nil && err2 == nil {
		reason := stopReason(res)
		note := label + " session " + sessionID
		idx := map[string]int{} // path -> position in items, so the last op wins
		for _, op := range ops[min(max(st.PushedOps, 0), int64(len(ops))):] {
			if op.Session != sessionID && op.Note != note {
				continue
			}
			p := op.Path
			if op.Kind == journal.KindDelete {
				p += " (deleted)"
			}
			if i, ok := idx[op.Path]; ok {
				items[i].path = p
				continue
			}
			idx[op.Path] = len(items)
			items = append(items, stopItem{p, reason})
		}
	}
	// Reverted paths never get an op, so there is no session to match: every
	// one this cycle reverted is reported.
	for _, p := range res.Reverted {
		items = append(items, stopItem{p, "reverted: read-only folder"})
	}
	return items
}

// stopReason says why unpushed ops are still unpushed, from the cycle that
// just ran.
func stopReason(res *syncer.Result) string {
	switch {
	case res.NoAccess || res.ReadOnly:
		if r := res.Reason(); r != "" {
			return r
		}
		return "the hub refused this device's changes"
	case res.Offline:
		msg := ""
		if res.OfflineErr != nil {
			msg = res.OfflineErr.Error()
		}
		if msg == "" || len([]rune(msg)) > 200 || !journal.SafeText(msg) {
			return "offline"
		}
		return "offline: " + msg
	default:
		return "not pushed yet"
	}
}

// pausedStop reports a paused mount's unscanned files. Opening a session or
// cycling would resume it, so this is a pure read. Not session-scoped: a
// paused mount journals nothing, so there are no session ops to match, and
// any unscanned file there really is not reaching the hub.
func pausedStop(proj config.Project, target string) []stopItem {
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		return nil
	}
	st, err := store.Open(vdir)
	if err != nil {
		return nil
	}
	cache, err := st.LoadCache(proj.ID)
	if err != nil {
		return nil
	}
	sync, err := st.LoadSync()
	if err != nil {
		return nil
	}
	paths, err := syncer.DriftPaths(target, proj.Include, sync.IgnoreAccepted, cache)
	if err != nil {
		return nil
	}
	items := make([]stopItem, len(paths))
	for i, p := range paths {
		items[i] = stopItem{p, stopReasonPaused}
	}
	return items
}

// emitStopReceipt prints nothing when everything reached the hub, else one
// Claude Stop-hook JSON object blocking the stop, paths grouped by reason in
// first-seen order and capped at hookChangedMax.
func emitStopReceipt(cmd *cobra.Command, items []stopItem) {
	if len(items) == 0 {
		return
	}
	var reasons []string
	byReason := map[string][]string{}
	over := 0
	for i, it := range items {
		if i >= hookChangedMax {
			over++
			continue
		}
		if _, ok := byReason[it.reason]; !ok {
			reasons = append(reasons, it.reason)
		}
		byReason[it.reason] = append(byReason[it.reason], "`"+it.path+"`")
	}
	groups := make([]string, len(reasons))
	for i, r := range reasons {
		groups[i] = strings.Join(byReason[r], ", ") + " (" + r + ")"
	}
	msg := "BearDrive: not on the hub yet — " + strings.Join(groups, "; ")
	if over > 0 {
		msg += fmt.Sprintf(", +%d more", over)
	}
	msg += ". Retry `bdrive sync`, or tell the user these files are local only."
	enc, err := json.Marshal(map[string]string{"decision": "block", "reason": msg})
	if err != nil {
		return
	}
	fmt.Fprintln(cmd.OutOrStdout(), string(enc))
}
