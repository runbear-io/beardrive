package webapp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// History is read straight from the journals: every put/delete ever made,
// newest first, with the account that made it and what the server knows
// about the device it came from. Content is content-addressed and retained
// forever, so each entry links to its exact version — the groundwork for
// the revert/rollback phase, where restoring is just writing an old blob
// back as a new op.

// historyDevice is the slice of the device registry history is allowed to
// report: who/what made the change, not where they connected from. The
// registry keeps observing and persisting the IP (devices.go) — it just
// doesn't ride along here, where every project member reads it. Mirrors
// heatByDevice (reads.go).
type historyDevice struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	OS   string `json:"os,omitempty"`
}

// HistoryEntry is one change as the history API reports it.
type HistoryEntry struct {
	Time string `json:"time"`
	Kind string `json:"kind"` // add | edit | delete
	Path string `json:"path"`
	Size int64  `json:"size,omitempty"`
	Blob string `json:"blob,omitempty"` // sha256; fetch via the blob endpoint
	// Prev is the content this path last had before the op — across a
	// delete too, so a delete names what it removed and a re-add what it
	// replaced. Absent when the path never had content before, and on a
	// delete of a path that was already gone (e.g. two devices deleting it
	// concurrently): that delete removed nothing.
	Prev     string        `json:"prev,omitempty"`
	User     string        `json:"user,omitempty"`
	UserName string        `json:"user_name,omitempty"`
	Author   string        `json:"author,omitempty"` // offline/git fallback identity
	Device   historyDevice `json:"device"`
	Note     string        `json:"note,omitempty"`
	// Session is the agent session the op was committed during (hook-set,
	// see journal.Op.Session). It is the run card's group key and the only
	// place a session id is ever served: it is never enumerated, never a
	// column in /heat's output, and never in ?by=device — it appears here,
	// on the op that carries it, and is accepted as a ?session= filter INPUT.
	Session string `json:"session,omitempty"`
}

// histLess is the display order of the history feed: newest wall-clock time
// first, ties in reverse journal order. Journal order is causal, not
// chronological (see journal.Less) — this is the one place that reconciles
// the two, and the cursor skips with the same function the feed sorts with,
// so paging can never disagree with what a reader sees. journal.Less is a
// total order and (device, seq) is unique per op, so histLess is total too:
// no stable sort needed, and equal timestamps come back in the same order
// on every request.
func histLess(a, b journal.Op) bool {
	if !a.Time.Equal(b.Time) {
		return a.Time.After(b.Time)
	}
	return journal.Less(b, a)
}

// histCursor is the ordering tuple of the last entry of a page — everything
// histLess reads, and nothing else. It rides the wire base64'd so a client
// treats it as opaque: HistoryEntry.time is formatted to whole seconds and
// carries no lamport/seq, so a client-computed cursor would be lossy across
// same-second ops.
type histCursor struct {
	// RFC3339Nano, not UnixNano: Op.Time is unvalidated peer JSON and
	// UnixNano is undefined outside [1678, 2262], so a date of 2300 read back
	// as 1715 — the skip loop then walked past every entry and returned a
	// clean end of feed. One journal push hid the whole audit trail past page
	// one from every other member.
	T string `json:"t"` // op time, RFC3339Nano
	L int64  `json:"l"` // lamport
	S int64  `json:"s"` // per-device seq
	D string `json:"d"` // device
}

func encodeCursor(op journal.Op) string {
	b, err := json.Marshal(histCursor{T: op.Time.UTC().Format(time.RFC3339Nano), L: op.Lamport, S: op.Seq, D: op.Device})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor rebuilds the four ordering fields into a bare Op — JSON
// rather than a delimited string, so a device id can never collide with a
// separator.
func decodeCursor(s string) (journal.Op, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return journal.Op{}, err
	}
	var c histCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return journal.Op{}, err
	}
	ts, err := time.Parse(time.RFC3339Nano, c.T)
	if err != nil {
		return journal.Op{}, err
	}
	return journal.Op{Time: ts.UTC(), Lamport: c.L, Seq: c.S, Device: c.D}, nil
}

// parseHistTime accepts RFC3339 or a bare YYYY-MM-DD (UTC). A bare date is
// the start of that day; `end` bumps it by 24h so ?until=<day> includes the
// whole day — the bound is then exclusive on both parses, which is what an
// inclusive "until this date" means once you stop thinking in instants.
func parseHistTime(s string, end bool) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		if end {
			t = t.Add(time.Nanosecond) // RFC3339 bound is inclusive to the second
		}
		return t.UTC(), true
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, false
	}
	if end {
		t = t.AddDate(0, 0, 1)
	}
	return t.UTC(), true
}

// handleHistory serves ?path=<file> (one file's versions) or
// ?prefix=<folder/> (everything underneath, "" = the whole project),
// newest first by wall-clock time, at most ?n= entries (default 100).
//
// Reader filters (see histFilter) compose with each other and with path/prefix. They are applied in the same
// walk as path/prefix, i.e. BEFORE the sort and the cursor skip, so
// next_cursor keeps meaning "the next matching entry" and paging under a
// filter needs no new machinery.
//
// Paging: the response carries next_cursor when more entries exist, and
// ?cursor= resumes just past the entry it was minted from — so history older
// than one page is reachable, and the UI can tell whether it is hiding
// anything. ?n= alone returns exactly the entries it always did.
//
// A cursor is a position in an ordering, not a snapshot: an offline device
// that pushes mid-scroll lands ops in the middle of the feed by timestamp,
// and the reader sees them on refresh rather than mid-page. Deliberate —
// pinning the feed to a read time means server-side state for the life of a
// scroll.
func (s *Server) handleHistory(v *volume, w http.ResponseWriter, r *http.Request) {
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	q := r.URL.Query()
	path, prefix := q.Get("path"), q.Get("prefix")
	if path != "" && q.Has("prefix") {
		http.Error(w, "use ?path= or ?prefix=, not both", http.StatusBadRequest)
		return
	}
	n := 100
	if raw := q.Get("n"); raw != "" {
		var err error
		if n, err = strconv.Atoi(raw); err != nil || n < 1 {
			http.Error(w, "invalid n", http.StatusBadRequest)
			return
		}
	}
	f, err := parseHistFilter(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	all, err := rs.loadSourcedOps(r.Context())
	if err != nil {
		storageErr(w, http.StatusBadGateway, "history is temporarily unavailable", err)
		return
	}
	sort.SliceStable(all, func(i, j int) bool { return journal.Less(all[i].Op, all[j].Op) })
	// A put is an "add" when the path didn't exist just before it (first
	// version, or first after a delete), an "edit" otherwise. Existence is
	// replayed over ALL ops in journal order, before any path/prefix filter,
	// so a filtered view classifies the same as the full feed.
	kinds := make([]string, len(all))
	prevs := make([]string, len(all))
	exists := make(map[string]bool, len(all))
	last := make(map[string]string, len(all))
	for i, so := range all {
		op := so.Op
		// A delete of a path that is already gone removed nothing, so it
		// names nothing — even though the path once had content.
		if op.Kind != journal.KindDelete || exists[op.Path] {
			prevs[i] = last[op.Path]
		}
		if op.Kind != journal.KindDelete {
			last[op.Path] = op.Blob
		}
		switch {
		case op.Kind == journal.KindDelete:
			kinds[i] = "delete"
			exists[op.Path] = false
		case exists[op.Path]:
			kinds[i] = "edit"
		default:
			kinds[i] = "add"
			exists[op.Path] = true
		}
	}
	type timed struct {
		i  int // index into all
		op journal.Op
	}
	// A file that moved keeps its past — under its old path. ?path= resolves
	// through the move chain so the feed for docs/a.md includes the versions
	// written while it was a.md. Each hop is time-bounded (see segment), so
	// an unrelated NEW a.md created after the move does not leak in. `all`
	// is already sorted by journal.Less, so this costs no extra I/O.
	var chain []segment
	if path != "" {
		ops := make([]journal.Op, len(all))
		for i, sop := range all {
			ops[i] = sop.Op
		}
		chain = chainSegments(buildMoveIndex(ops), path)
	}
	// Resolved once, not per op: history walks every op in the project.
	vis := s.visibility(r)
	matched := make([]timed, 0, len(all))
	for i, sop := range all {
		op := sop.Op
		switch {
		// Before every other filter. History is a per-path audit feed, so an
		// unfiltered row leaks the path, who changed it and when — the same
		// three facts the tree would have leaked.
		case !vis.canRead(op.Path):
			continue
		case path != "" && !inSegments(chain, op.Path, op.Time):
			continue
		case path == "" && prefix != "" && !strings.HasPrefix(op.Path, strings.TrimSuffix(prefix, "/")+"/"):
			continue
		case !f.keep(sop):
			continue
		}
		matched = append(matched, timed{i, op})
	}
	// Truncation happens AFTER the sort: cutting during the walk above would
	// pick the n highest-Lamport entries and merely display them in time order.
	sort.Slice(matched, func(a, b int) bool { return histLess(matched[a].op, matched[b].op) })
	if raw := q.Get("cursor"); raw != "" {
		cur, err := decodeCursor(raw)
		if err != nil {
			http.Error(w, "invalid cursor", http.StatusBadRequest)
			return
		}
		i := 0
		for i < len(matched) && !histLess(cur, matched[i].op) { // skip to just past it
			i++
		}
		matched = matched[i:]
	}
	// Exact, with no over-fetch probe: every op is already in memory, so
	// "there is more" is a length check and the last page simply omits the key.
	var next string
	if len(matched) > n {
		next = encodeCursor(matched[n-1].op)
		matched = matched[:n]
	}
	// Only the page is attributed. The registry join re-checks its store and
	// the org directory on every lookup, so resolving every matched op made a
	// long feed cost seconds per hundred rows on a database-backed hub; each
	// journal is resolved once per request instead.
	visible := s.deviceVisibleIn(projectID(r))
	type resolved struct {
		dev        historyDevice
		registered bool
	}
	devs := map[string]resolved{}
	entries := make([]HistoryEntry, len(matched))
	for k, m := range matched {
		sop, op := all[m.i], m.op
		res, ok := devs[sop.From]
		if !ok {
			// Attribution comes from the journal the op was READ from, never
			// from its own Device field: that field is arbitrary JSON any
			// member with write access can put in their own journal, so
			// trusting it printed another org's machine name into this feed
			// and answered "does this device id exist on the hub?" for anyone
			// who asked. The registry join is scoped the same way heat's is.
			res = resolved{dev: historyDevice{ID: sop.From}}
			if info, ok := s.Devices.LookupIn(sop.From, visible); ok && info.ID != "" {
				res = resolved{historyDevice{ID: info.ID, Name: info.Name, OS: info.OS}, true}
			}
			devs[sop.From] = res
		}
		dev := res.dev
		if !res.registered && op.Device == sop.From {
			dev.Name = op.DeviceName // the journal's owner describing itself
		}
		entries[k] = HistoryEntry{
			Time: op.Time.UTC().Format("2006-01-02T15:04:05Z"), Kind: kinds[m.i],
			Path: op.Path, Size: op.Size, Blob: op.Blob, Prev: prevs[m.i],
			User: op.User, UserName: op.UserName, Author: op.Author,
			Device: dev, Note: op.Note, Session: op.Session,
		}
	}
	out := map[string]any{"entries": entries}
	if next != "" {
		out["next_cursor"] = next
	}
	writeJSON(w, out)
}

// histFilter is the reader-side narrowing shared by the feed and its
// summary: ?q= (case-insensitive substring of the path), ?user= (exact
// account), ?actor=agent|human, ?device= (the journal an op was read from),
// ?since=/?until= (bounds, inclusive at both ends; a bare date is a UTC day,
// an RFC3339 instant carries the reader's own offset).
//
// An op is an agent's when it carries a Session: only the agent sync hook
// sets one, while a note is anyone's to write, so the note never decides it.
type histFilter struct {
	needle, user, actor, device string
	since, until                time.Time
}

func parseHistFilter(q url.Values) (histFilter, error) {
	f := histFilter{
		needle: strings.ToLower(q.Get("q")), // lowered once, not per op
		user:   q.Get("user"),
		actor:  q.Get("actor"),
		device: q.Get("device"),
	}
	if f.actor != "" && f.actor != "agent" && f.actor != "human" {
		return f, fmt.Errorf("invalid actor")
	}
	// since > until is not an error: it means "nothing", which is what it returns.
	for _, b := range []struct {
		name string
		end  bool
		into *time.Time
	}{{"since", false, &f.since}, {"until", true, &f.until}} {
		raw := q.Get(b.name)
		if raw == "" {
			continue
		}
		t, ok := parseHistTime(raw, b.end)
		if !ok {
			return f, fmt.Errorf("invalid %s", b.name)
		}
		*b.into = t
	}
	return f, nil
}

func (f histFilter) keep(sop sourcedOp) bool {
	op := sop.Op
	switch {
	case f.needle != "" && !strings.Contains(strings.ToLower(op.Path), f.needle):
		return false
	case f.user != "" && op.User != f.user:
		return false
	case f.actor == "agent" && op.Session == "", f.actor == "human" && op.Session != "":
		return false
	case f.device != "" && sop.From != f.device:
		return false
	case !f.since.IsZero() && op.Time.Before(f.since):
		return false
	case !f.until.IsZero() && !op.Time.Before(f.until):
		return false
	}
	return true
}

// histDay is one bar of the summary chart: a calendar day in the reader's
// zone, split by who made the changes.
type histDay struct {
	Date  string `json:"date"` // YYYY-MM-DD in the requested zone
	Agent int    `json:"agent"`
	Human int    `json:"human"`
}

// handleHistorySummary serves GET /history/summary: the counts behind the
// Activity header, one bar per calendar day in the reader's zone (?tz= IANA
// name, else ?offset= minutes east of UTC, else UTC). The chart covers the
// last ?days= days (1–31, default 7) — or, when ?since=/?until= are given,
// the days those bounds touch: until's day (today when absent) back to
// since's day (?days= back when absent), keeping the latest 31 when the span
// is longer. Scoping (?prefix=) and every histFilter apply, the bounds
// included, so the header describes the same feed it sits above. It answers in one pass
// over the journals the feed already loads, so the browser never pages
// through a week of history to draw seven bars.
func (s *Server) handleHistorySummary(v *volume, w http.ResponseWriter, r *http.Request) {
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	q := r.URL.Query()
	f, err := parseHistFilter(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	days := 7
	if raw := q.Get("days"); raw != "" {
		if days, err = strconv.Atoi(raw); err != nil || days < 1 || days > 31 {
			http.Error(w, "invalid days", http.StatusBadRequest)
			return
		}
	}
	loc := time.UTC
	if name := q.Get("tz"); name != "" {
		if l, err := time.LoadLocation(name); err == nil {
			loc = l
		} else if off, err := strconv.Atoi(q.Get("offset")); err == nil && off > -24*60 && off < 24*60 {
			loc = time.FixedZone("", off*60)
		}
	} else if off, err := strconv.Atoi(q.Get("offset")); err == nil && off > -24*60 && off < 24*60 {
		loc = time.FixedZone("", off*60)
	}
	prefix := q.Get("prefix")
	last := time.Now().In(loc)
	if !f.until.IsZero() {
		last = f.until.Add(-time.Nanosecond).In(loc) // until is exclusive
	}
	y, m, d := last.Date()
	if !f.since.IsZero() {
		first := f.since.In(loc)
		fy, fm, fd := first.Date()
		span := int(time.Date(y, m, d, 12, 0, 0, 0, time.UTC).Sub(time.Date(fy, fm, fd, 12, 0, 0, 0, time.UTC)).Hours()/24) + 1
		days = min(max(span, 0), 31)
	}
	start := time.Date(y, m, d-days+1, 0, 0, 0, 0, loc)
	out := struct {
		Total   int       `json:"total"`
		Agent   int       `json:"agent"`
		Human   int       `json:"human"`
		Devices int       `json:"devices"`
		Days    []histDay `json:"days"`
	}{Days: make([]histDay, days)}
	index := make(map[string]int, days)
	for i := range out.Days {
		day := time.Date(y, m, d-days+1+i, 0, 0, 0, 0, loc).Format("2006-01-02")
		out.Days[i].Date = day
		index[day] = i
	}
	all, err := rs.loadSourcedOps(r.Context())
	if err != nil {
		storageErr(w, http.StatusBadGateway, "history is temporarily unavailable", err)
		return
	}
	vis := s.visibility(r)
	devices := map[string]bool{}
	// ?all=1 totals every op the filters keep, while the bars still cover
	// the chart window: "All time" in the feed must not be summarised as the
	// last week.
	allTime := q.Get("all") == "1"
	for _, sop := range all {
		op := sop.Op
		if !vis.canRead(op.Path) || !f.keep(sop) || (!allTime && op.Time.Before(start)) {
			continue
		}
		if prefix != "" && !strings.HasPrefix(op.Path, strings.TrimSuffix(prefix, "/")+"/") {
			continue
		}
		i, ok := index[op.Time.In(loc).Format("2006-01-02")]
		if !ok && (!allTime || !op.Time.Before(start)) {
			continue // in the future by this reader's clock
		}
		out.Total++
		devices[sop.From] = true
		agent := op.Session != ""
		if agent {
			out.Agent++
		} else {
			out.Human++
		}
		if ok {
			if agent {
				out.Days[i].Agent++
			} else {
				out.Days[i].Human++
			}
		}
	}
	out.Devices = len(devices)
	writeJSON(w, out)
}

// handleBlob streams one exact version by content hash — view or download
// any point in a file's history.
func (s *Server) handleBlob(v *volume, w http.ResponseWriter, r *http.Request) {
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	sha := r.URL.Query().Get("sha")
	if !blobRe.MatchString(sha) {
		http.Error(w, "invalid sha", http.StatusBadRequest)
		return
	}
	// Content addressing means this door names bytes, not a path — so the
	// question "may you read this?" has to be asked of every path that
	// references them. Answered before the fetch, and answered as a 404: a
	// past version of a hidden file is exactly as absent as one that never
	// existed.
	if !s.visibility(r).visibleSHA(r.Context(), rs, sha) {
		http.Error(w, "no such version", http.StatusNotFound)
		return
	}
	rc, err := rs.OpenBlob(r.Context(), sha)
	if err != nil {
		http.Error(w, "no such version", http.StatusNotFound)
		return
	}
	defer rc.Close()
	name := r.URL.Query().Get("name")
	pv := false
	if name != "" {
		ct := contentType(name)
		// Same wall and the same inert declaration as the live-file door: a
		// past version is the same bytes, so the two must never differ.
		w.Header().Set("Content-Type", inlineType(ct))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		sandboxInline(w, ct)
		download := r.URL.Query().Get("download") == "1"
		if download {
			w.Header().Set("Content-Type", ct)
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", sanitizeFilename(name)))
		}
		// Printing a pinned ?v= prints THAT version. The live-file door and
		// this one serve identical bytes, so a print button that silently
		// fell back to the current ones would be the worst kind of wrong.
		pv = printView(w, r, ct, !download)
	} else {
		// Same door, same stored bytes, two lines down — and it did not get the
		// header the arm above did. The rule is "nosniff on every door that
		// streams stored bytes", so it goes on unconditionally: a declared
		// octet-stream a browser is free to sniff is not a wall.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-Type-Options", "nosniff")
	}
	io.Copy(w, rc)
	if pv {
		io.WriteString(w, printSuffix)
	}
}

func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "/", "-")
	return strings.Map(func(r rune) rune {
		if r < 32 || r == '"' {
			return '-'
		}
		return r
	}, name)
}

// handleHistoryEmptied lists the files that are EMPTIED: their current
// version is 0 bytes and an earlier version — under this path or any path it
// moved from — had content. The tree only knows today's size, which cannot
// tell an emptied note from a file that was always empty, and asking for each
// file's history would scan the journal once per file. The candidates are the
// tree's own 0-byte files (RemoteSource.FilesWithMoves, so the replay rules
// are the tree's), each checked along its move chain the way the per-file
// history feed follows it — in a single pass over the journal.
func (s *Server) handleHistoryEmptied(v *volume, w http.ResponseWriter, r *http.Request) {
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	files, moves, err := rs.FilesWithMoves(r.Context())
	if err != nil {
		storageErr(w, http.StatusBadGateway, "history is temporarily unavailable", err)
		return
	}
	vis := s.visibility(r)
	var candidates []string
	for p, fi := range files {
		if fi.Size == 0 && fi.Link == "" && vis.canRead(p) {
			candidates = append(candidates, p)
		}
	}
	// One pass over the journal: each candidate's chain is indexed by the
	// paths it covers, so an op is checked only against the candidates whose
	// history runs through its path — not every candidate against every op.
	paths := []string{}
	if len(candidates) > 0 {
		all, err := rs.loadSourcedOps(r.Context())
		if err != nil {
			storageErr(w, http.StatusBadGateway, "history is temporarily unavailable", err)
			return
		}
		type hit struct {
			file string
			seg  segment
		}
		into := inboundIndex(moves)
		byPath := map[string][]hit{}
		for _, p := range candidates {
			for _, seg := range chainFrom(into, p) {
				byPath[seg.Path] = append(byPath[seg.Path], hit{p, seg})
			}
		}
		had := map[string]bool{}
		for _, sop := range all {
			op := sop.Op
			if op.Kind == journal.KindDelete || op.Link != "" || op.Size <= 0 || !blobRe.MatchString(op.Blob) {
				continue
			}
			for _, h := range byPath[op.Path] {
				if !had[h.file] && inSegments([]segment{h.seg}, op.Path, op.Time) {
					had[h.file] = true
				}
			}
		}
		for p := range had {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	writeJSON(w, map[string]any{"paths": paths})
}
