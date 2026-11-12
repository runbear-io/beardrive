package webapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
	ygows "github.com/reearth/ygo/provider/websocket"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/textmerge"
)

/* The hub holding the document, instead of relaying bytes between browsers.

   collab.go is a relay: it never parses a frame, so nobody owns the document
   and every property that needs an owner had to be faked somewhere else — a
   seed CLAIM with a grace timer (two clients seeding one file build two
   documents that duplicate every character on merge), a byte cap on a log
   that only grows, a rebuild-from-scratch when that cap is hit, and a
   client-side snapshot rule ("whoever stops typing last writes the file")
   that makes N co-editors write N versions of identical text.

   Worse, it has a failure mode with teeth: a client that loses the relay
   falls back to editing its own buffer, and two browsers then hold two
   documents and overwrite each other. That cost a user six characters and is
   why upload/content now takes If-Match and parks the loser as a conflict
   copy (#234). The copy is the right safety net; needing one every time a
   laptop changes network is not.

   So the hub holds the document. See docs/collab-provider-prd.md.

   THE DECISION THIS RESTS ON: until now the hub never parsed a
   client-supplied CRDT update — collab.go stores opaque bytes and hands them
   on. This ends that, deliberately and on the record (PRD §The decision,
   2026-09-19), with the conditions it carried: mounted behind the same
   proj() wrapper every other per-project route uses, so folder permissions
   and org walls apply unchanged, and the old relay stays reachable for a
   release.

   Pure Go, no cgo: a cgo y-crdt would break the cross-compiled release the
   way a cgo sqlite would, which is the constraint that made this a relay in
   the first place and the one that changed. */

// collabContext is captured when background work is created, so Close can
// cancel work already waiting in storage without rebinding its context.
func (s *Server) collabContext() context.Context {
	s.collabMu.Lock()
	defer s.collabMu.Unlock()
	if s.collabBG == nil {
		s.collabBG, s.collabCancel = context.WithCancel(context.Background())
	}
	return s.collabBG
}

func (s *Server) collabHookContext() context.Context {
	s.collabMu.Lock()
	defer s.collabMu.Unlock()
	if s.collabClosing != nil {
		return s.collabClosing
	}
	if s.collabBG == nil {
		s.collabBG, s.collabCancel = context.WithCancel(context.Background())
	}
	return s.collabBG
}

// ydocs is the document server, built once. Rooms are created on demand and
// swept when idle (ygo's own idle sweep), which is the shape the memory
// measurement argued for: a held document is ~100 KB while actively edited —
// two orders of magnitude under the 8 MiB log cap it replaces — so the limit
// worth having is eviction, not bytes.
func (s *Server) ydocs() *ygows.Server {
	s.yOnce.Do(func() {
		srv := ygows.NewServer()
		// A room outlives its last peer by collabRoomIdleTimeout: a reload, a
		// laptop lid or a proxy dropping an idle socket comes back to the
		// same warm document instead of a reseed from storage, and the
		// unload (the final snapshot) runs only once the room is truly idle.
		srv.RoomIdleTimeout = collabRoomIdleTimeout
		// Seeding, remembering and snapshotting, in the three places ygo
		// offers: the document is built from the file before anyone attaches,
		// kept addressable while the room lives, and written back when the
		// last editor leaves.
		srv.OnLoadDocument = func(ctx context.Context, room string, doc *crdt.Doc) error {
			bg := s.collabContext()
			ctx, cancel := context.WithCancel(ctx)
			stop := context.AfterFunc(bg, cancel)
			defer func() {
				stop()
				cancel()
			}()
			if bg.Err() != nil {
				cancel()
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			s.rooms().load(room, doc)
			// Without a relay, only the lease holder can open the room.
			if s.collabRelay == nil && !s.holdLease(ctx, room) {
				s.rooms().drop(room)
				if err := ctx.Err(); err != nil {
					return err
				}
				return errRoomElsewhere
			}
			if err := s.seedDoc(ctx, room, doc); err != nil {
				s.rooms().drop(room)
				s.releaseLease(s.collabHookContext(), room)
				return err
			}
			if err := ctx.Err(); err != nil {
				s.rooms().drop(room)
				s.releaseLease(s.collabHookContext(), room)
				return err
			}
			s.watchRoom(room, doc)
			s.keepLease(room)
			return nil
		}
		srv.OnLastPeer = func(_ context.Context, room string) {
			// The last peer leaving writes the document promptly, but the
			// room stays resident (RoomIdleTimeout) so this is not final:
			// OnUnloadDocument owns the final snapshot.
			s.snapshot(s.collabHookContext(), room, false)
		}
		srv.OnUnloadDocument = func(_ context.Context, room string) {
			ctx := s.collabHookContext()
			r := s.rooms()
			doc, _, ok := r.get(room)
			if !ok {
				return
			}
			s.finalSnapshot(ctx, room)
			// Close owns the final caller-context snapshot after it cancels
			// an unload already in flight. Keep its document addressable.
			if ctx.Err() != nil {
				return
			}
			r.dropIfDoc(room, doc, func() { s.releaseLease(ctx, room) })
		}
		/* Authorization runs per CONNECTION, after proj() has already decided
		   this caller may see this project.

		   A read-only member is not refused: they receive the document and
		   everyone's cursors and their own writes are dropped server-side,
		   which is what "read-only" has always meant everywhere else in this
		   hub. Refusing them outright would make a file they are allowed to
		   READ fail to open. */
		srv.Authorize = func(r *http.Request) (ygows.ConnectionConfig, bool) {
			p, ok := projectFromCtx(r)
			if !ok {
				return ygows.ConnectionConfig{}, false
			}
			path := r.URL.Query().Get("path")
			writable := atLeast(s.pathPerm(r, p, path), PermWrite)
			if writable {
				// Who a snapshot is attributed to. History showing "the
				// server" for a version a person typed would be a regression:
				// the hub holds the pen, it is never the author. Approximate
				// in the same way the client's own save already is — it names
				// whoever most recently sat down to write, not whoever typed
				// each character.
				s.rooms().writer(projectID(r)+"/"+path, s.requestUser(r))
			}
			return ygows.ConnectionConfig{ReadOnly: !writable}, true
		}
		s.y = srv
	})
	return s.y
}

// pathPerm is what this caller may do to one path: the project's level,
// narrowed or widened by a folder rule.
//
// A predicate, not a gate. writablePath answers the same question by writing
// a 403 onto the response, which is right for a door and wrong for a
// decision — this one has to say "read-only" without ending the request.
func (s *Server) pathPerm(r *http.Request, p Project, path string) string {
	base := s.projectPermOf(r, p)
	if len(p.Folders) == 0 || base == PermAdmin {
		return base
	}
	return folderLevel(p, normEmail(s.requestUser(r).Email), path, base)
}

// handleYCollab joins the document for one file.
//
// The room name is DERIVED HERE and never taken from the caller. ygo reads it
// from PathValue("room") or the URL's last segment, so a route that let the
// client name the room would let any member of any project join any other
// document by asking for its name — the project id in the path would be
// decoration. Mounting the name as (project, path) after proj() has resolved
// the project is what keeps the org wall in front of it.
func (s *Server) handleYCollab(v *volume, w http.ResponseWriter, r *http.Request) {
	_ = v
	path, err := cleanUploadPath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// A hidden path is 404, never 403: a 403 confirms the file is there.
	// Same rule the viewer's pathFilter applies (folders.go).
	if vis := s.visibility(r); !vis.canRead(path) {
		http.NotFound(w, r)
		return
	}
	room := projectID(r) + "/" + path
	r.SetPathValue("room", room)
	// Held for the whole connection: an HTTP sync closing a room it opened
	// must not delete it from under this socket's join (enter).
	defer s.rooms().enter(room)()
	s.ydocs().ServeHTTP(w, r)
}

// collabRoomIdleTimeout is how long an empty room stays resident before ygo
// unloads it. A var so a test need not wait it out.
var collabRoomIdleTimeout = 2 * time.Minute

// ycollabSyncOrigin marks the updates /ycollab/sync applies to a room. The
// relay forwards every origin but its own, so a pushed update still reaches
// other processes; local peers get it by broadcast.
var ycollabSyncOrigin = &struct{ name string }{"ycollab/sync"}

// maxYCollabSyncBody bounds a /ycollab/sync request: a base64 update up to
// maxHistoryBytes plus a text and a base text up to maxSeedBytes each.
const maxYCollabSyncBody = maxHistoryBytes*4/3 + 3*maxSeedBytes + 4096

type ycollabSyncReq struct {
	Update        string  `json:"update,omitempty"`
	Base          string  `json:"base,omitempty"`
	BaseText      *string `json:"base_text,omitempty"`
	Text          *string `json:"text,omitempty"`
	ConfirmShrink bool    `json:"confirm_shrink,omitempty"`
	// Peek reads the room without writing anything: its sha, text and
	// state as a Yjs update. A client whose earlier sync failed after the
	// room may already have applied it learns from this what landed.
	Peek      bool     `json:"peek,omitempty"`
	PriorSent []string `json:"prior_sent,omitempty"`
	// ParkOnConflict asks a text checkpoint that clashes with the live room
	// to be kept beside the file as a conflict copy, in this request: a tab
	// closing sends one keepalive request and runs no code after it, so it
	// cannot answer a 409 with a second request parking its text itself.
	ParkOnConflict bool `json:"park_on_conflict,omitempty"`
}

/*
handleYCollabSync puts one client's state into a file's live room over plain
HTTP — the path a browser takes when its socket is down, or to confirm a
shrink the room would otherwise refuse.

	update is a Yjs V1 update from a synced document: applied as is, so a
	returning browser's typing merges by item identity like any peer's.
	Without one, text is three-way merged against the room's current text
	(base_text, or the blob named by base, as the ancestor) and the merge is
	spliced in hunk by hunk. A same-line clash is a 409 carrying the sha the
	room stands on.

The room then snapshots through the ordinary upload door, so every write rule
still applies; the answer is the sha the room now stands on and its text. A
room this request had to open is closed again once written.
*/
func (s *Server) handleYCollabSync(v *volume, w http.ResponseWriter, r *http.Request) {
	path, err := cleanUploadPath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.writablePath(w, r, path) {
		return
	}
	var req ycollabSyncReq
	if err := json.NewDecoder(io.LimitReader(r.Body, maxYCollabSyncBody)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	var update []byte
	if req.Update != "" {
		if update, err = base64.StdEncoding.DecodeString(req.Update); err != nil {
			http.Error(w, "update is not base64", http.StatusBadRequest)
			return
		}
		if len(update) > maxHistoryBytes {
			http.Error(w, "update too large to co-edit", http.StatusRequestEntityTooLarge)
			return
		}
	}
	if req.Text != nil && len(*req.Text) >= maxSeedBytes {
		http.Error(w, "file too large to co-edit", http.StatusRequestEntityTooLarge)
		return
	}
	ctx := r.Context()
	project := projectID(r)
	room := project + "/" + path
	baseText := ""
	if len(update) == 0 && req.Text != nil {
		switch {
		case req.BaseText != nil:
			// Present, even empty: "" is a real ancestor (a file that
			// was blank), never "absent, read the blob instead".
			baseText = *req.BaseText
		case req.Base != "":
			got, ok := s.blobText(ctx, project, req.Base)
			if !ok || len(got) > maxSeedBytes {
				writeJSONStatus(w, http.StatusConflict, map[string]any{
					"error": "the base version cannot be read", "sha": s.rooms().base(room), "path": path,
				})
				return
			}
			baseText = got
		}
	}

	/* A room this request had to open — or an idle one it touched, which
	   ygo will no longer reap (Apply clears the idle stamp) — is closed again
	   on the way out, unless a socket is in it or joining it by then. */
	leave := s.rooms().enter(room)
	closeRoom := func() { _ = s.ydocs().CloseRoom(room, false) } // unload: final snapshot, lease released
	release := func() {
		leave()
		s.rooms().closeIfUnused(room, closeRoom)
	}

	if req.Peek && len(update) == 0 && req.Text == nil && !req.ConfirmShrink {
		/* Read-only: no splice, no snapshot, no syncDurable. A peek answers
		   even while storage refuses writes, because it promises nothing
		   about storage — only what the live room holds right now. */
		var peekUpdate []byte
		var peekText string
		/* A room already held may carry a write that just failed — an
		   unconfirmed shrink above all, which a close's final snapshot
		   would park as a conflict copy. Only a room this peek opened is
		   closed now; a held one gets the failed write's idle window. */
		_, _, wasHeld := s.rooms().get(room)
		done := release
		if wasHeld {
			done = func() {
				leave()
				time.AfterFunc(collabRoomIdleTimeout, func() { s.rooms().closeIfUnused(room, closeRoom) })
			}
		}
		err := s.ydocs().Apply(ctx, room, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
			peekUpdate, peekText = doc.EncodeStateAsUpdate(), doc.GetText("body").ToString()
		})
		if err != nil && !errors.Is(err, ygows.ErrNoChanges) {
			if errors.Is(err, errRoomElsewhere) {
				leave()
				writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error(), "retry": true})
				return
			}
			done()
			http.Error(w, fmt.Sprintf("co-editing: %v", err), http.StatusBadGateway)
			return
		}
		sha := s.rooms().base(room)
		done()
		resp := map[string]any{"ok": true, "path": path, "sha": sha, "text": peekText}
		if len(peekUpdate) > 0 {
			resp["update"] = base64.StdEncoding.EncodeToString(peekUpdate)
		}
		writeJSON(w, resp)
		return
	}

	var (
		conflict, merged, tooLarge, pendingBusy, shrinkRefused bool
		cur                                                    string
		applyErr                                               error
		// wantUpdate and wantText are the room's document with this request
		// in it, captured inside Apply: the version that must be on storage
		// before the answer may say ok. Read after Apply returns, a keystroke
		// landing in between would be folded into what this request promises.
		wantUpdate []byte
		wantText   string
		captured   bool
	)
	err = s.ydocs().Apply(ctx, room, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		txt := doc.GetText("body")
		cur = txt.ToString()
		defer func() {
			if applyErr == nil && !conflict && !tooLarge && !pendingBusy {
				wantUpdate, wantText, captured = doc.EncodeStateAsUpdate(), txt.ToString(), true
			}
		}()
		switch {
		case len(update) > 0:
			/* Measured on a scratch copy first: a live room that took an
			   update past maxSeedBytes could neither be snapshotted whole nor
			   seeded again, and an applied update cannot be taken back. */
			if doc.PendingStats().Items > 0 {
				pendingBusy = true
				return
			}
			probe := crdt.New()
			if applyErr = probe.ApplyUpdate(doc.EncodeStateAsUpdate()); applyErr != nil {
				return
			}
			if applyErr = crdt.ApplyUpdateV1(probe, update, nil); applyErr != nil {
				return
			}
			if probe.PendingStats().Items > 0 {
				applyErr = errors.New("update has unintegrated pending items")
				return
			}
			if len(probe.GetText("body").ToString()) >= maxSeedBytes {
				tooLarge = true
				return
			}
			applyErr = crdt.ApplyUpdateV1(doc, update, ycollabSyncOrigin)
		case req.Text != nil:
			want := *req.Text
			anc := baseText
			for _, sent := range req.PriorSent {
				if len(sent) < maxSeedBytes && sent != anc && textmerge.RetainsChanges([]byte(anc), []byte(sent), []byte(cur)) {
					anc = sent
					break
				}
			}
			if cur != anc && cur != want {
				const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
				if anc == "" && req.Base != "" && req.Base != emptySHA256 && sharpShrink(int64(len(cur)), int64(len(want))) {
					if !req.ConfirmShrink {
						shrinkRefused = true
						return
					}
					if req.Base != s.rooms().base(room) {
						conflict = true
						return
					}
				} else {
					out, ok := textmerge.Merge([]byte(anc), []byte(cur), []byte(want))
					if !ok || !validMergedText(path, out) {
						conflict = true
						return
					}
					want, merged = string(out), true
				}
			}
			if len(want) >= maxSeedBytes {
				tooLarge = true
				return
			}
			spliceVia(transact, txt, cur, want)
		}
	})
	closeIfOpened := release
	if err != nil && !errors.Is(err, ygows.ErrNoChanges) {
		if errors.Is(err, errRoomElsewhere) {
			leave()
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error(), "retry": true})
			return
		}
		closeIfOpened()
		http.Error(w, fmt.Sprintf("co-editing: %v", err), http.StatusBadGateway)
		return
	}
	if applyErr != nil {
		closeIfOpened()
		http.Error(w, "update does not apply: "+applyErr.Error(), http.StatusBadRequest)
		return
	}
	if pendingBusy {
		closeIfOpened()
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{"error": "room has unintegrated updates; retry", "retry": true})
		return
	}
	if len(update) > 0 && !tooLarge {
		// Applied outside Apply's capture, so the room's own peers on this
		// process are told here; the relay already has it.
		_ = s.ydocs().BroadcastUpdate(ctx, room, update)
	}
	if shrinkRefused {
		closeIfOpened()
		writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "this save would empty or sharply shrink the file",
			"shrink": true,
			"sha":    s.rooms().base(room),
			"size":   len(cur),
		})
		return
	}
	if conflict && !tooLarge && req.ParkOnConflict && req.Text != nil &&
		strings.TrimSpace(*req.Text) != "" && s.apiMux != nil {
		// Through the upload door, as every park is: quota, folder rules,
		// the journal, the data index and change frames all apply. Created
		// only under a free name: another tab's copy is never replaced.
		pw, parked := s.parkNewCopy(ctx, s.requestUser(r), project, path, *req.Text)
		closeIfOpened()
		if parked == "" {
			code := pw.code
			if code < 400 {
				code = http.StatusBadGateway
			}
			if ct := pw.hdr.Get("Content-Type"); ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			w.WriteHeader(code)
			_, _ = w.Write(pw.body.Bytes())
			return
		}
		resp := map[string]any{"ok": true, "path": path, "sha": s.rooms().base(room), "text": cur, "parked": parked}
		stampReceipt(w, resp, pw.hdr.Get(receiptHeader)) // the copy's own write
		writeJSON(w, resp)
		return
	}
	if conflict || tooLarge {
		closeIfOpened()
		code, msg := http.StatusConflict, "the same lines changed in the live document"
		if tooLarge {
			code, msg = http.StatusRequestEntityTooLarge, "the merge is too large to co-edit"
			if len(update) > 0 {
				msg = "the update makes the file too large to co-edit"
			}
		}
		writeJSONStatus(w, code, map[string]any{"error": msg, "sha": s.rooms().base(room), "text": cur, "path": path})
		return
	}
	s.rooms().writer(room, s.requestUser(r))
	text, synced := wantText, wantUpdate // the room's state with this request in it
	if !captured {
		// Apply had nothing to run (no changes): what the room holds now.
		if doc, _, held := s.rooms().get(room); held {
			synced = doc.EncodeStateAsUpdate()
			text, _ = replayed(synced)
		}
	}
	if req.ConfirmShrink {
		s.rooms().confirmShrink(room, text)
	}
	sha := ""
	if s.apiMux != nil {
		// ok means durable, and durable is proven on storage, never inferred
		// from the room: a snapshot that met an outside write merges it into
		// the live document (reconcile) and leaves writing the merge to the
		// next pause — or, with a keystroke landing mid-reconcile, moves
		// nothing at all (moveDoc), and the request's edit lives only in
		// memory. Until storage holds it the browser keeps its edits.
		var fail *snapFailure
		sha, fail = s.syncDurable(ctx, room, text, synced)
		if fail != nil {
			/* The room holds the text and the file does not: quota (403), an
			   unconfirmed sharp shrink (422) or storage (502). Saying ok here
			   would have the browser drop its dirty state for a write that
			   never happened, so it gets the door's own refusal and keeps
			   it — retrying, or asking about the shrink. The room is not
			   closed now: its final snapshot would park the unconfirmed
			   shrink as a conflict copy before the editor could confirm it.
			   It stays for the idle window (the failed write's own retries
			   run meanwhile) and is closed then if nobody came back. */
			leave()
			time.AfterFunc(collabRoomIdleTimeout, func() { s.rooms().closeIfUnused(room, closeRoom) })
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fail.code)
			_, _ = w.Write(fail.body)
			return
		}
	}
	// The receipt of the op that made the file say sha, when syncDurable
	// proved it on storage and this process's fold already names that op: a
	// reader on another hub process echoing it then sees the synced text.
	receipt := ""
	if sha != "" && v != nil {
		if fi, ok := s.headFile(ctx, v, path); ok && fi.Blob == sha {
			receipt = fileReceipt(fi)
		}
	}
	if sha == "" {
		sha = s.rooms().base(room)
	}
	if doc, _, held := s.rooms().get(room); held {
		text, _ = replayed(doc.EncodeStateAsUpdate())
	}
	release()
	resp := map[string]any{"ok": true, "path": path, "sha": sha, "text": text, "merged": merged}
	if len(synced) > 0 {
		/* The room's state with this request in it, as a Yjs update: a
		   client staging a checkpoint over HTTP while it joins the socket
		   applies it to learn the exact item IDs the room created for its
		   text, so the socket's own copy of them merges as a no-op instead
		   of the client re-inserting the same lines a second time. */
		resp["update"] = base64.StdEncoding.EncodeToString(synced)
	}
	stampReceipt(w, resp, receipt)
	writeJSON(w, resp)
}

// ycollabSyncDurableWait bounds how long /ycollab/sync on a process that does
// not hold a room's lease waits for the holder to write the synced text: the
// relay delivers it at once and the holder writes snapshotIdle later — but
// only once the room pauses. While several editors type continuously on the
// holder, every keystroke re-arms the idle timer, so the holder's write can
// slip all the way to snapshotMax. This wait must therefore exceed
// snapshotMax, or a non-holder answers a spurious 503 for an edit the holder
// was always going to write. A var so a test need not wait it out.
var ycollabSyncDurableWait = 8 * time.Second

/*
awaitDurable waits for the file to hold a sync this process applied but
another process writes: the stored file says text, or the document state
stored with it knows synced. It answers the sha that holds it, or a failure
for the browser to keep its edits by.

	No relay: the holder never hears this process's update, so nothing will
	write it — refused at once.

	elsewhere is false for this process's own room after syncDurable's
	immediate snapshots left the edit off storage: the same wait, without
	the relay requirement.

	A holder that cannot write (quota, an unconfirmed shrink, storage) looks
	the same from here as a slow one, so running out of time is a 503, never
	an ok for text nothing durable holds. A lease that expires meanwhile is
	this process's to take: each second it snapshots again, and a write it
	makes itself, or a refusal it gets, ends the wait.
*/
func (s *Server) awaitDurable(ctx context.Context, room, text string, synced []byte, elsewhere bool) (string, *snapFailure) {
	if elsewhere && s.collabRelay == nil {
		return "", unwrittenSync("another process writes this file and cannot receive this edit")
	}
	wait := ycollabSyncDurableWait
	if dl, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(dl))
	}
	deadline := time.Now().Add(wait)
	nextSnap := time.Now().Add(time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if sha, ok := s.syncIsDurableOnStorage(ctx, room, text, synced); ok {
			return sha, nil
		}
		now := time.Now()
		if now.After(deadline) {
			return "", unwrittenSync("the edit is not written yet")
		}
		if now.After(nextSnap) {
			nextSnap = now.Add(time.Second)
			if fail := s.snapshot(ctx, room, false); fail != nil && !fail.elsewhere {
				return "", fail
			}
		}
		select {
		case <-ctx.Done():
			return "", unwrittenSync("the edit is not written yet")
		case <-tick.C:
		}
	}
}

// unwrittenSync is the 503 a /ycollab/sync answers for an edit storage does
// not hold yet: the browser keeps its edits and retries.
func unwrittenSync(msg string) *snapFailure {
	body, _ := json.Marshal(map[string]any{"error": msg, "retry": true})
	return &snapFailure{code: http.StatusServiceUnavailable, body: body}
}

// syncDurable writes a /ycollab/sync's edit and answers the sha storage holds
// it under, or the failure the browser keeps its edits by. A few snapshots
// back to back settle what one leaves for the next pause (a reconciled merge,
// a move a keystroke refused); past those it waits, snapshotting each second,
// up to ycollabSyncDurableWait. Another process holding the lease waits for
// that process's write the same way.
func (s *Server) syncDurable(ctx context.Context, room, text string, synced []byte) (string, *snapFailure) {
	for range 3 {
		fail := s.snapshot(ctx, room, false)
		if fail != nil {
			if fail.elsewhere {
				return s.awaitDurable(ctx, room, text, synced, true)
			}
			return "", fail
		}
		if sha, ok := s.syncIsDurableOnStorage(ctx, room, text, synced); ok {
			return sha, nil
		}
	}
	return s.awaitDurable(ctx, room, text, synced, false)
}

// syncIsDurableOnStorage is the positive proof a sync's ok rests on: storage,
// not the room, holds the edit — the current file says text, the document
// state stored with the current file knows every item and deletion of
// synced, or text sits in a conflict copy this room wrote beside the file.
// Anything else, including an edit only the live document holds, is not
// durable.
func (s *Server) syncIsDurableOnStorage(ctx context.Context, room, text string, synced []byte) (string, bool) {
	project, path, _ := strings.Cut(room, "/")
	body, sha, _, found, err := s.freshRoomFile(ctx, room, s.roomHead(ctx, project, path))
	if err != nil {
		return "", false
	}
	if found {
		if string(body) == text {
			return sha, true
		}
		if synced != nil {
			if h := s.roomHistory(ctx, project, sha); h != nil && knows(h, synced) {
				return sha, true
			}
		}
	}
	if s.rooms().parkedDurably(room, text) {
		return s.rooms().base(room), true
	}
	return "", false
}

// seedDoc is what makes the seed CLAIM unnecessary.
//
// The relay could not seed: it never parsed a frame, so it had no way to turn
// a file into a document. Exactly one joiner was therefore told "you are
// first, build it from the bytes you loaded" — with a grace timer, because a
// claim that never produced anything left every later joiner holding a blank
// document that the source editor would then cheerfully save over the file.
//
// The hub can just do it. This runs once, when the room is created and before
// any client is attached, so there is nothing to claim and nothing to race.
func (s *Server) seedDoc(ctx context.Context, room string, doc *crdt.Doc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	project, path, _ := strings.Cut(room, "/")
	head := s.roomHeadRecord(ctx, project, path)
	body, sha, win, found, err := s.freshRoomFile(ctx, room, head.SHA)
	if err != nil {
		// Refused, so the editor retries. Opened empty, its first snapshot
		// would write that emptiness over a file nobody could read.
		return fmt.Errorf("%s: reading the file: %w", room, err)
	}
	if !found {
		return nil // a file that does not exist yet starts empty
	}
	if len(body) >= maxSeedBytes {
		return fmt.Errorf("%s is too large to co-edit", room) // a truncated seed would be written back over the rest
	}
	// The base is set for an empty file too: it is what keeps every write
	// this room makes conditional.
	s.rooms().rebase(room, sha)
	if len(body) == 0 {
		return nil
	}
	text := string(body)

	/* The document is rebuilt from the same HISTORY, not from the bytes.

	   A room is evicted the moment its last peer disconnects, and the browser
	   that disconnected still holds the document — a laptop lid, a proxy
	   dropping an idle socket — and y-websocket reconnects it into whatever
	   room exists then. If that room was seeded from the file as a fresh
	   insert, the browser's copy and the hub's copy are two documents that
	   happen to spell the same text, and the CRDT keeps both: the whole file,
	   twice. Every reconnect appended one more copy. Shipped that way.

	   So a snapshot stores the document's state beside the blob it wrote
	   (ydoc/<sha>), and a room for a file that IS that blob replays it: the
	   same items, the same ids, and a returning browser's state is a no-op to
	   merge.

	   The NEWEST state any room stored for this path (ydoc/head/<path>) wins
	   over the one filed under the file's own blob, spliced to the file
	   where the file moved on. A file somebody wrote from outside has no
	   state of its own, so this is how the outside change lands as one edit
	   — which is also how a returning browser's unsynced typing lands beside
	   that write instead of on top of a second copy of the file. And a file
	   taken BACK to an older version from outside does have a state of its
	   own, from back then — one that knows nothing of the edits since. The
	   relay replays a room's retained log when a process activates the room
	   (cluster/redis), and against that older state those edits would go
	   right back into a file somebody deliberately reverted. Built on the
	   newest state they are known and deleted, and replaying them changes
	   nothing. Only a hub that never saw this file being edited starts from
	   the bytes. */
	var newest []byte
	if head.SHA != "" && head.SHA != sha {
		newest = s.roomHistory(ctx, project, head.SHA)
		if newest != nil && head.Win != "" && win != "" && win < head.Win {
			/* The journal fence. The head names a version that comes AFTER
			   the one this process's fold resolves to, in replay order — the
			   fold is stale (another process wrote, and even a forced
			   refresh has not caught up). Splicing the head back to these
			   older bytes would tombstone the newer edits, and the relay
			   would carry those deletions to every process holding the room.
			   So the room is built from the head as it stands; its next
			   write is held to that version. */
			if headText, ok := replayed(newest); ok && len(headText) < maxSeedBytes && doc.ApplyUpdate(newest) == nil {
				s.rooms().rebase(room, head.SHA)
				return nil
			}
			return fmt.Errorf("%s: this process's view of the file is older than the stored document", room)
		}
	}
	if newest == nil {
		if state := s.roomHistory(ctx, project, sha); state != nil && replayInto(doc, state, text) {
			return nil
		}
	}
	/* Built on a scratch document and claimed before it is applied: two
	   processes opening a file no room has held both reach this point, and a
	   seed is a set of items under a fresh random client id, so two of them
	   spell the same text twice when they meet. Whichever process stores
	   ydoc/<sha> first has seeded the file; the other applies that state
	   instead of its own (claimRoomHistory). */
	tmp := crdt.New()
	ttxt := tmp.GetText("body")
	seeded := false
	if newest != nil {
		if was, ok := replayed(newest); ok && tmp.ApplyUpdate(newest) == nil {
			splice(tmp, ttxt, was, text)
			seeded = true
		}
	}
	if !seeded {
		tmp.Transact(func(txn *crdt.Transaction) {
			ttxt.Insert(txn, 0, text, nil)
		})
	}
	state := s.claimRoomHistory(ctx, project, path, sha, tmp.EncodeStateAsUpdate(), text, newest)
	if !replayInto(doc, state, text) {
		return fmt.Errorf("%s: the stored document does not spell the file", room)
	}
	return nil
}

// claimRoomHistory stores a seed as ydoc/<sha> and returns the state every
// process must build the room from: the stored one when it spells the file
// and knows everything the newest state does, ours otherwise — written with
// compare-and-swap, so two processes seeding at once end up with one of
// their seeds rather than one each.
func (s *Server) claimRoomHistory(ctx context.Context, project, path, sha string, state []byte, body string, newest []byte) []byte {
	be, ok := s.projectBackend(project)
	if !ok || sha == "" || len(state) > maxHistoryBytes {
		return state
	}
	cond, ok := be.(remote.ConditionalPutter)
	if !ok {
		s.storeRoomHistory(ctx, project, path, sha, state)
		return state
	}
	key := historyKey(sha)
	var err error
	for range 3 {
		var theirs []byte
		var v remote.Version
		theirs, v, err = readVersioned(ctx, cond, key)
		if err != nil {
			break
		}
		if theirs != nil {
			if got, ok := replayed(theirs); ok && got == body && knows(theirs, newest) {
				return theirs
			}
		}
		if _, err = cond.PutIf(ctx, key, bytes.NewReader(state), int64(len(state)), v); err == nil {
			s.storeRoomHead(ctx, project, path, sha, state)
			return state
		}
		if !errors.Is(err, remote.ErrVersionMismatch) {
			break
		}
	}
	log.Printf("bdrive: claiming room history %s/%s: %v", project, sha, err)
	return state
}

// knows reports whether a stored state holds every item AND every deletion
// of another — the only comparison a CRDT offers: two states that spell the
// same text are not the same document.
//
// It merges other into a scratch copy of state and asks whether anything
// changed. DiffUpdateV1(other, sv) == empty cannot answer that: a V1 update
// always carries its whole delete set, so any history holding one deletion
// read as "missing something" even against itself.
func knows(state, other []byte) bool {
	if other == nil {
		return true
	}
	probe, ok := docFrom(state)
	if !ok {
		return false
	}
	before := probe.EncodeStateAsUpdate()
	svBefore := probe.StateVector()
	txtBefore := probe.GetText("body").ToString()
	if err := probe.ApplyUpdate(other); err != nil {
		return false
	}
	if !sameVector(svBefore, probe.StateVector()) || probe.GetText("body").ToString() != txtBefore {
		return false
	}
	// No item was added, so the delete set is all that can still differ —
	// and it is all a diff against the full state vector encodes.
	dsBefore, err1 := crdt.DiffUpdateV1(before, svBefore)
	dsAfter, err2 := crdt.DiffUpdateV1(probe.EncodeStateAsUpdate(), svBefore)
	return err1 == nil && err2 == nil && bytes.Equal(dsBefore, dsAfter)
}

func sameVector(a, b crdt.StateVector) bool {
	if len(a) != len(b) {
		return false
	}
	for c, n := range a {
		if m, ok := b[c]; !ok || m != n {
			return false
		}
	}
	return true
}

func docFrom(state []byte) (*crdt.Doc, bool) {
	tmp := crdt.New()
	if err := tmp.ApplyUpdate(state); err != nil {
		return nil, false
	}
	return tmp, true
}

// replayed is the text a stored state spells, on a scratch document, so a
// corrupt or foreign state is found out before it touches a live one.
func replayed(state []byte) (string, bool) {
	tmp := crdt.New()
	if err := tmp.ApplyUpdate(state); err != nil {
		return "", false
	}
	return tmp.GetText("body").ToString(), true
}

// replayInto applies a stored state to the room's document only when it
// spells exactly `want` — the file the room is for.
func replayInto(doc *crdt.Doc, state []byte, want string) bool {
	if got, ok := replayed(state); !ok || got != want {
		return false
	}
	return doc.ApplyUpdate(state) == nil
}

// splice turns txt from one text into another with the minimal set of
// edits: a line diff, each changed span trimmed to the characters that
// differ (textmerge.DiffHunks). Everything unchanged between two edits stays
// the same CRDT items, so a browser's typing inside it is never deleted with
// it. Offsets are UTF-16 units, which is what Yjs counts in.
func splice(doc *crdt.Doc, txt *crdt.YText, from, to string) {
	spliceVia(func(fn func(*crdt.Transaction)) { doc.Transact(fn) }, txt, from, to)
}

// spliceVia is splice through a caller's transaction runner — ygo's Apply
// hands one out, and only edits made through it reach the room's peers.
func spliceVia(transact func(func(*crdt.Transaction)), txt *crdt.YText, from, to string) {
	hunks := textmerge.DiffHunks(from, to)
	if len(hunks) == 0 {
		return
	}
	transact(func(txn *crdt.Transaction) {
		// Last to first, so each hunk's offsets into `from` still hold.
		for i := len(hunks) - 1; i >= 0; i-- {
			h := hunks[i]
			if h.End > h.Start {
				txt.Delete(txn, h.Start, h.End-h.Start)
			}
			if h.Insert != "" {
				txt.Insert(txn, h.Start, h.Insert, nil)
			}
		}
	})
}

// notifyRoomExternalWrite brings open rooms up to date after a device push.
// Clean rooms follow the file; dirty rooms use the snapshot's three-way merge.
func (s *Server) notifyRoomExternalWrite(project string, paths []string) {
	if project == "" || len(paths) == 0 {
		return
	}
	// Do not initialize either once just because a device wrote a file. The
	// registry is published under collabMu by rooms().
	s.collabMu.Lock()
	r := s.roomReg
	s.collabMu.Unlock()
	if r == nil {
		return
	}
	for _, path := range paths {
		room := project + "/" + path
		doc, who, ok := r.get(room)
		if !ok {
			continue
		}
		ctx := s.collabHookContext()
		base := r.base(room)
		text, valid := replayed(doc.EncodeStateAsUpdate())
		if !valid {
			continue
		}
		clean := who.Email == ""
		if !clean {
			if base == "" {
				clean = text == ""
			} else if baseText, found := s.blobText(ctx, project, base); found {
				clean = text == baseText
			}
		}
		if clean {
			_, sha, found, err := s.roomBytes(ctx, room)
			if err != nil || !found {
				continue
			}
			// A keystroke or a reopened room during the storage reads must
			// not be overwritten by a decision made about the old document.
			if s.foldRoomWrite(ctx, project, path, sha, func(current *crdt.Doc) bool {
				return current == doc && r.base(room) == base && current.GetText("body").ToString() == text
			}) {
				continue
			}
		}
		s.snapshotRoom(s.collabContext(), room)
	}
}

// foldIntoRoom puts a write the hub itself made to a file — a restore — into
// that file's live room, as one edit every peer receives, and moves the
// room's base to it. Without this the room keeps the text from before the
// write, and its next snapshot either writes that back over it or, refused
// for a stale base, parks a conflict copy. Reports whether a room took it;
// no room means no document to keep in step. A version too large for a room
// to hold ends the room instead.
func (s *Server) foldIntoRoom(ctx context.Context, project, path, sha string) bool {
	return s.foldRoomWrite(ctx, project, path, sha, nil)
}

// foldRoomWrite optionally checks the captured room inside Apply, where a
// concurrent editor cannot slip a keystroke between the check and the splice.
func (s *Server) foldRoomWrite(ctx context.Context, project, path, sha string, unchanged func(*crdt.Doc) bool) bool {
	room := project + "/" + path
	if _, _, ok := s.rooms().get(room); !ok {
		return false
	}
	body, cur, found, err := s.roomBytes(ctx, room)
	if err != nil || !found || cur != sha {
		return false
	}
	if len(body) >= maxSeedBytes {
		// Too large to hold, so not folded as a prefix: the room ends with no
		// snapshot (dropped from the registry first, so the unload finds
		// nothing to write), and a reconnect is refused by seedDoc.
		s.dropRoom(room)
		_ = s.ydocs().CloseRoom(room, true)
		return false
	}
	folded := false
	err = s.ydocs().Apply(ctx, room, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		if unchanged != nil && !unchanged(doc) {
			return
		}
		txt := doc.GetText("body")
		spliceVia(transact, txt, txt.ToString(), string(body))
		folded = true
	})
	if err != nil || !folded {
		return false
	}
	s.rooms().rebase(room, sha)
	if doc, _, ok := s.rooms().get(room); ok {
		s.storeRoomHistory(ctx, project, path, sha, doc.EncodeStateAsUpdate())
	}
	return true
}

// adoptRoomWrite moves a live room's base to a write that spells exactly the
// room's document — an editor in the room confirming a shrink the hub would
// not make on its own. The room already holds that text, so its next
// snapshot must build on this version rather than be refused for a stale
// base and parked as a conflict copy. A write that differs from the
// document is an outsider's and leaves the room alone.
func (s *Server) adoptRoomWrite(ctx context.Context, project, path, sha string, body io.ReadSeeker) {
	room := project + "/" + path
	doc, _, ok := s.rooms().get(room)
	if !ok || sha == "" {
		return
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return
	}
	written, err := io.ReadAll(io.LimitReader(body, maxSeedBytes+1))
	if err != nil {
		return
	}
	state := doc.EncodeStateAsUpdate()
	if text, ok := replayed(state); !ok || text != string(written) {
		return
	}
	s.rooms().rebase(room, sha)
	s.storeRoomHistory(ctx, project, path, sha, state)
}

// A room's history lives in the project's own storage as ydoc/<sha>: the
// document state that spells the blob. Never exported, never quota'd, never a
// read — hub-internal, like a journal. Absent is ordinary (a file no room has
// held, an older hub, an import); failing to store one is logged and the
// room still works, it just cannot be rebuilt losslessly.
func historyKey(sha string) string { return "ydoc/" + sha }

func (s *Server) roomHistory(ctx context.Context, project, sha string) []byte {
	be, ok := s.projectBackend(project)
	if !ok || sha == "" {
		return nil
	}
	rc, err := be.Get(ctx, historyKey(sha))
	if err != nil {
		if !remote.IsNotExist(err) {
			log.Printf("bdrive: reading room history %s/%s: %v", project, sha, err)
		}
		return nil
	}
	defer rc.Close()
	state, err := io.ReadAll(io.LimitReader(rc, maxHistoryBytes))
	if err != nil {
		return nil
	}
	return state
}

func (s *Server) storeRoomHistory(ctx context.Context, project, path, sha string, state []byte) {
	if ctx.Err() != nil {
		return
	}
	be, ok := s.projectBackend(project)
	if !ok || sha == "" || len(state) > maxHistoryBytes {
		return
	}
	if err := be.Put(ctx, historyKey(sha), bytes.NewReader(state), int64(len(state))); err != nil {
		log.Printf("bdrive: storing room history %s/%s: %v", project, sha, err)
		return
	}
	s.storeRoomHead(ctx, project, path, sha, state)
}

// ydoc/head/<path> names the newest state stored for a path — what the next
// room for it builds from, on any process (seedDoc).
func headKey(path string) string { return "ydoc/head/" + path }

// roomHeadRec is what ydoc/head/<path> holds: the blob of the newest stored
// state and, when it was known, the replay-order key (winKey, hex) of the
// journal op that produced that blob. win is the fence a stale fold is held
// to: hex preserves winKey's byte order, so comparing two of them compares
// the ops the way journal.Less does. A head written by an older hub is a
// bare sha, read with win "".
type roomHeadRec struct {
	SHA string `json:"sha"`
	Win string `json:"win,omitempty"`
}

func parseRoomHead(b []byte) roomHeadRec {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return roomHeadRec{}
	}
	if b[0] != '{' {
		return roomHeadRec{SHA: string(b)}
	}
	var rec roomHeadRec
	if json.Unmarshal(b, &rec) != nil {
		return roomHeadRec{}
	}
	rec.SHA = strings.TrimSpace(rec.SHA)
	return rec
}

// maxHeadBytes bounds a head record: a sha and a winKey, whose length grows
// with the device id and the path.
const maxHeadBytes = 16 << 10

func (s *Server) roomHead(ctx context.Context, project, path string) string {
	return s.roomHeadRecord(ctx, project, path).SHA
}

func (s *Server) roomHeadRecord(ctx context.Context, project, path string) roomHeadRec {
	be, ok := s.projectBackend(project)
	if !ok {
		return roomHeadRec{}
	}
	rc, err := be.Get(ctx, headKey(path))
	if err != nil {
		return roomHeadRec{}
	}
	defer rc.Close()
	b, _ := io.ReadAll(io.LimitReader(rc, maxHeadBytes))
	return parseRoomHead(b)
}

// fileWin is the hex winKey of the op that produced fi, or "" when the fold
// did not record one.
func fileWin(fi FileInfo, path string) string {
	if fi.DeviceID == "" && fi.Lamport == 0 && fi.Seq == 0 {
		return ""
	}
	return hex.EncodeToString(winKey(fi.winOp(path)))
}

func (s *Server) storeRoomHead(ctx context.Context, project, path, sha string, state []byte) {
	if ctx.Err() != nil {
		return
	}
	be, ok := s.projectBackend(project)
	if !ok {
		return
	}
	// The op that wrote sha, when this process's fold resolves the path to
	// it — every caller stores a state right after writing or reading it.
	win := ""
	if _, fi, found, err := s.roomFileInfo(ctx, project+"/"+path); err == nil && found && fi.Blob == sha {
		win = fileWin(fi, path)
	}
	rec, _ := json.Marshal(roomHeadRec{SHA: sha, Win: win})
	// newer reports whether rec may replace prev, or that nothing is to do.
	newer := func(prev roomHeadRec) (write bool) {
		if prev.SHA == sha {
			return win != "" && prev.Win != win // learning the fence for the same head
		}
		if prev.Win != "" && win != "" {
			// Both ends of the comparison are journal positions: the head
			// never moves back in replay order, whatever the states say.
			return win > prev.Win
		}
		if prev.SHA != "" && len(state) > 0 {
			if prevState := s.roomHistory(ctx, project, prev.SHA); prevState != nil {
				stateHasPrev := knows(state, prevState)
				prevHasState := knows(prevState, state)
				if prevHasState && !stateHasPrev {
					return false
				}
				if !stateHasPrev || prevHasState {
					if _, curSHA, found, ferr := s.roomBytes(ctx, project+"/"+path); ferr == nil && found && curSHA == prev.SHA {
						return false
					}
				}
			}
		}
		return true
	}
	if cp, ok := be.(remote.ConditionalPutter); ok {
		const maxHeadAttempts = 8
		for range maxHeadAttempts {
			existing, version, err := readVersioned(ctx, cp, headKey(path))
			if err != nil {
				log.Printf("bdrive: reading room head %s/%s: %v", project, path, err)
				return
			}
			if !newer(parseRoomHead(existing)) {
				return
			}
			_, err = cp.PutIf(ctx, headKey(path), bytes.NewReader(rec), int64(len(rec)), version)
			if err == nil {
				return
			}
			if !errors.Is(err, remote.ErrVersionMismatch) {
				log.Printf("bdrive: storing room head %s/%s: %v", project, path, err)
				return
			}
		}
		log.Printf("bdrive: storing room head %s/%s: concurrent head contention", project, path)
		return
	}
	if prev := s.roomHeadRecord(ctx, project, path); !newer(prev) {
		return
	}
	if err := be.Put(ctx, headKey(path), bytes.NewReader(rec), int64(len(rec))); err != nil {
		log.Printf("bdrive: storing room head %s/%s: %v", project, path, err)
	}
}

func (s *Server) projectBackend(project string) (remote.Backend, bool) {
	_, v, err := s.projectVolume(project)
	if err != nil {
		return nil, false
	}
	src, ok := v.source.(*RemoteSource)
	if !ok {
		return nil, false
	}
	return src.Backend, true
}

// roomBytes is the file behind a room, bounded, with the blob it currently is
// — the sha every write to it must name in If-Match. found is false only when
// the file is not there; a file that is there but cannot be read right now is
// an error, never an empty file.
func (s *Server) roomBytes(ctx context.Context, room string) (body []byte, sha string, found bool, err error) {
	body, fi, found, err := s.roomFile(ctx, room)
	return body, fi.Blob, found, err
}

// roomFileInfo resolves a room's path in the fold that includes every write
// already accepted (volume.current).
func (s *Server) roomFileInfo(ctx context.Context, room string) (*volume, FileInfo, bool, error) {
	project, path, ok := strings.Cut(room, "/")
	if !ok || !projectIDRe.MatchString(project) {
		return nil, FileInfo{}, false, nil
	}
	if s.Projects != nil {
		if _, known := s.Projects.Get(project); !known {
			return nil, FileInfo{}, false, nil
		}
	}
	_, v, err := s.projectVolume(project)
	if err != nil {
		return nil, FileInfo{}, false, err
	}
	// A room is built from what the file IS, not from a snapshot a background
	// rebuild has not caught up on yet.
	snap, err := v.current(ctx)
	if err != nil {
		return nil, FileInfo{}, false, err
	}
	fi, ok := snap.files[path]
	if !ok {
		return v, FileInfo{}, false, nil
	}
	return v, fi, true, nil
}

// roomFile is roomBytes with the whole FileInfo, provenance included.
func (s *Server) roomFile(ctx context.Context, room string) ([]byte, FileInfo, bool, error) {
	v, fi, found, err := s.roomFileInfo(ctx, room)
	if err != nil || !found {
		return nil, FileInfo{}, false, err
	}
	_, path, _ := strings.Cut(room, "/")
	rc, err := v.source.Open(ctx, path, fi)
	if err != nil {
		return nil, FileInfo{}, false, err
	}
	defer rc.Close()
	// One byte past the bound, so a caller can tell a whole file from a
	// prefix: anything longer than maxSeedBytes is only the start of one.
	body, err := io.ReadAll(io.LimitReader(rc, maxSeedBytes+1))
	if err != nil {
		return nil, FileInfo{}, false, err
	}
	return body, fi, true, nil
}

// freshRoomFile is roomFile for a seed, with the op that produced the file
// (win, see roomHeadRec). volume.current covers the writes THIS process
// accepted; another process's are seen only once the cached fold expires.
// A head naming a different blob says somebody wrote since, so the fold is
// rebuilt from the journals before the seed trusts it.
func (s *Server) freshRoomFile(ctx context.Context, room, headSHA string) (body []byte, sha, win string, found bool, err error) {
	body, fi, found, err := s.roomFile(ctx, room)
	if err == nil && headSHA != "" && (!found || fi.Blob != headSHA) {
		project, _, _ := strings.Cut(room, "/")
		if _, v, verr := s.projectVolume(project); verr == nil {
			v.invalidate()
			body, fi, found, err = s.roomFile(ctx, room)
		}
	}
	if err != nil || !found {
		return nil, "", "", found, err
	}
	_, path, _ := strings.Cut(room, "/")
	return body, fi.Blob, fileWin(fi, path), true, nil
}

// snapshotRoom writes the document back to the file.
//
// A SAFETY NET, not the primary writer — the browser still saves on idle
// exactly as it did. Two writes of one text is not two versions: identical
// content journals nothing (upload.go), so whichever lands second is free.
// What this adds is the case no client can cover, which is every client going
// away at once: a closed laptop used to lose whatever had not yet reached the
// 700ms idle save.
//
// Re-entering through the API rather than calling the uploader directly is
// what makes it honest. Quota, folder permissions, the no-op check, journaling
// and the change frame are then the same code every other write goes through;
// a second path into the file would be a second set of rules to keep in
// agreement.
func (s *Server) snapshotRoom(ctx context.Context, room string) { _ = s.snapshot(ctx, room, false) }

// finalSnapshot is the write made as the room empties: nobody is left to see
// the document, so a version the file refuses is parked beside it instead.
func (s *Server) finalSnapshot(ctx context.Context, room string) { _ = s.snapshot(ctx, room, true) }

// snapshotSeam, when set by a test, runs inside a snapshot once it has read
// the document and before it reads the file.
var snapshotSeam func(room string)

// snapshot runs one room's snapshots one at a time. Each pause fires on its own
// timer goroutine and the room's last peer leaving fires another, so without
// this two could write and file history in either order: the slower one would
// name an older version as the room's head and base after a newer one, and the
// next room would be built from it.
func (s *Server) snapshot(ctx context.Context, room string, final bool) *snapFailure {
	unlock := s.rooms().lockSnapshots(room)
	defer unlock()
	return s.snapshotLocked(ctx, room, final)
}

// snapFailure is a snapshot that left the room's text unwritten because the
// file could not be read or the upload door refused it — quota (403), an
// unconfirmed sharp shrink (422), storage (502). code and body are what the
// door answered, so a caller relaying it (/ycollab/sync) hands the browser
// the same refusal a direct save would have got. Nil for everything else,
// including a refusal the room settled itself (reconcile) and a room another
// process writes.
type snapFailure struct {
	code int
	body []byte
	// elsewhere: another process holds the room's lease, so this one wrote
	// nothing and cannot say whether the file holds the text. Not a refusal
	// — the caller has to look at storage for the holder's write
	// (awaitDurable).
	elsewhere bool
}

// snapshotLocked is snapshot for a caller already holding the room's
// snapshot lock.
func (s *Server) snapshotLocked(ctx context.Context, room string, final bool) (failed *snapFailure) {
	if ctx.Err() != nil {
		return
	}
	project, path, ok := strings.Cut(room, "/")
	if !ok {
		return
	}
	doc, who, ok := s.rooms().get(room)
	if !ok || who.Email == "" {
		return // nobody with write access ever joined: nothing to attribute
	}
	// The base is read BEFORE the state. A restore folded in between moves
	// the base and the document together; read the other way round, this
	// snapshot would name the restored version with text from before it and
	// undo the restore. Read first, the write is refused and settled below.
	base := s.rooms().base(room)
	// The text is derived FROM the captured state, not read beside it: a
	// keystroke landing between the two would store a history that spells a
	// different file than the blob it is filed under, and a room rebuilt
	// from it would refuse the replay and duplicate exactly as before.
	state := doc.EncodeStateAsUpdate()
	text, ok := replayed(state)
	if !ok {
		return
	}
	if snapshotSeam != nil {
		snapshotSeam(room)
	}
	cur, curFile, have, err := s.roomFile(ctx, room)
	curSha, curWin := curFile.Blob, fileWin(curFile, path)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		// Never write over a file this process cannot see. A pause looks
		// again on the next one; a final snapshot has no next one, and the
		// room is dropped right after it, so the text is held until the
		// file can be read.
		log.Printf("bdrive: not snapshotting %s: reading the file: %v", path, err)
		if final && strings.TrimSpace(text) != "" {
			s.finishLater(ctx, room, who, text, base, state)
		}
		return storageFailure(err)
	}
	if have && curSha == s.rooms().yieldedTo(room) {
		// Already parked beside this version, which the room cannot hold.
		// Edits since are kept the same way when the room empties.
		if final && strings.TrimSpace(text) != "" && !s.rooms().isParked(room, text) {
			s.rooms().park(room, text)
			s.parkCopy(ctx, who, project, path, conflictPath(path, who, time.Now()), text)
		}
		s.rooms().settled(room)
		return
	}
	if have && len(cur) > maxSeedBytes {
		// Larger than any room holds, so somebody outside wrote it, and cur
		// is only its start: it is never compared to the text or merged.
		s.reconcile(ctx, room, who, text, "", false, string(cur), curSha, final)
		return
	}
	if !have && base != "" {
		// This room wrote the file and somebody deleted it since. The
		// deletion is theirs: the room does not bring the file back while
		// it is edited, and when it empties its text goes beside it, once.
		if final && strings.TrimSpace(text) != "" && !s.rooms().isParked(room, text) {
			s.rooms().park(room, text)
			s.parkCopy(ctx, who, project, path, conflictPath(path, who, time.Now()), text)
		}
		s.rooms().settled(room)
		return
	}
	if have && string(cur) == text {
		if s.rooms().base(room) != curSha {
			s.storeRoomHistory(ctx, project, path, curSha, state)
			s.rooms().rebase(room, curSha)
			// Written by another process. Its change frame reached only
			// its own browsers; ours still show the edit as unsaved until
			// a frame tells them the file now says what they see.
			s.events().publish(project, changeEvent{Type: "change", Puts: 1, Source: "collab", Paths: []string{path}})
		}
		s.rooms().settled(room)
		return // the file already says this
	}
	if !have && strings.TrimSpace(text) == "" {
		s.rooms().settled(room)
		return // nothing typed into a file that does not exist: nothing to create
	}
	if !s.holdLease(ctx, room) {
		// Another process writes this file. Look again once it has had
		// time to, so this process's browsers see the save land.
		bg := s.collabContext()
		if ctx.Err() == nil && bg.Err() == nil {
			// Looked before and the file still does not say this: the
			// writer is late or wedged, and until somebody writes it the
			// relay's retained window is all a later process can replay.
			if s.rooms().waited(room) {
				s.republishRoom(ctx, room, state)
			}
			s.rooms().awaitWriter(room, func() { s.snapshotRoom(bg, room) })
		}
		return &snapFailure{elsewhere: true}
	}
	if s.apiMux == nil {
		return
	}
	head := s.roomHeadRecord(ctx, project, path)
	// A restore can point at a blob this room authored before. Its stored
	// Yjs state is then a subset of doc too, but that is not proof that this
	// journal winner is our snapshot: a newer op for a different blob than
	// the recorded head proves an outside write AFTER that snapshot. Keep
	// the old base, so the conditional put refuses and reconcile splices the
	// outside version into the live room instead of overwriting the restore.
	outside := head.Win != "" && curWin != "" && curWin > head.Win && curSha != head.SHA
	if have && curSha != base && !outside && s.contains(ctx, doc, project, curSha) {
		// The head was written by this room on another process — the
		// previous lease holder — and the relay has already delivered every
		// item it wrote, so build on it instead of parking beside it.
		base = curSha
		s.rooms().rebase(room, curSha)
	}
	if have && base == "" {
		// The file appeared after the room opened. It is somebody else's
		// write, and an unconditional one from here would erase it.
		s.reconcile(ctx, room, who, text, "", true, string(cur), curSha, final)
		return
	}
	w := s.putAs(ctx, who, project, path, text, base)
	switch w.code {
	case http.StatusOK:
		if sha := reportedSha(w); sha != "" {
			s.storeRoomHistory(ctx, project, path, sha, state)
			s.rooms().rebase(room, sha)
		}
		s.rooms().settled(room)
	case http.StatusConflict:
		// Somebody OUTSIDE the room wrote the file — an agent, the CLI, a
		// device — since the hub last did. Everyone inside the room shares
		// this document, so a 409 here can never be a co-editor.
		theirs, theirSha, found, err := s.roomBytes(ctx, room)
		if err != nil || !found {
			// Looked at again on the next pause, against the same base. A
			// final snapshot has none, so its text is carried past the room.
			if final && strings.TrimSpace(text) != "" {
				s.finishLater(ctx, room, who, text, base, state)
			}
			if err != nil {
				return storageFailure(err)
			}
			return
		}
		// A base this source cannot read back still parks rather than merges.
		baseText, haveBase := s.blobText(ctx, project, base)
		s.reconcile(ctx, room, who, text, baseText, haveBase, string(theirs), theirSha, final)
	case http.StatusUnprocessableEntity:
		/* sharpShrink: the room would empty or gut the file, and no editor in
		   a live room writes for itself, so nobody can confirm it. The file
		   keeps its bytes. While editors remain the document is still theirs;
		   once the room empties, the room's text is parked as a conflict copy
		   so a deliberate clear is never silently dropped — unless there is
		   nothing in it the file does not already hold. */
		if !final || s.rooms().isParked(room, text) {
			log.Printf("bdrive: not snapshotting %s: the room's text would empty or sharply shrink it", path)
			if !final {
				failed = &snapFailure{code: w.code, body: w.body.Bytes()}
			}
			return
		}
		if fileHolds(string(cur), text) || s.copyHolds(ctx, project, path, text) {
			log.Printf("bdrive: %s: the room's text would empty or sharply shrink it and adds nothing; dropped", path)
			return
		}
		// Marked before the write: parkCopy owns this text from here,
		// retrying past the room's lifetime, so the second final snapshot
		// must not start another.
		s.rooms().park(room, text)
		copy := conflictPath(path, who, time.Now())
		s.parkCopy(ctx, who, project, path, copy, text)
		return
	default:
		// The door or the storage behind it failed. While editors remain,
		// look again with backoff: otherwise the room's text waits for the
		// next keystroke, or for nobody if the typing has stopped. A final
		// snapshot's text is owned by the unload path.
		log.Printf("bdrive: snapshotting %s: upload answered %d", path, w.code)
		if final {
			return
		}
		code := w.code
		if code < 400 {
			code = http.StatusBadGateway // a write that never answered is a failed write
		}
		failed = &snapFailure{code: code, body: w.body.Bytes()}
		if bg := s.collabContext(); ctx.Err() == nil && bg.Err() == nil {
			// Nothing durable holds this text but this process's document
			// and the relay's retained window — which trims by age. Each
			// failed attempt re-publishes the whole state, so a process
			// opening the room later still replays it however long the
			// outage lasts.
			s.republishRoom(ctx, room, state)
			s.rooms().retryLater(room, func() { s.snapshotRoom(bg, room) })
		}
	}
	return
}

// republishRoom hands a room's whole state to the relay again. Applying it is
// a no-op for every process that already has it, so it is safe to repeat; its
// point is the relay's RETAINED window, which is the only place a process
// opening the room later can learn edits no snapshot has written yet. Without
// a relay there is no other process to tell.
func (s *Server) republishRoom(ctx context.Context, room string, state []byte) {
	if s.collabRelay == nil || len(state) == 0 {
		return
	}
	if err := s.collabRelay.Publish(ctx, cluster.Outbound{Room: room, Kind: cluster.KindSync, Data: state}); err != nil {
		log.Printf("bdrive: re-publishing %s to the relay: %v", room, err)
	}
}

// storageFailure is the snapFailure for a file this process could not read
// back before writing: a 502, as the upload door answers a storage error.
func storageFailure(err error) *snapFailure {
	body, _ := json.Marshal(map[string]any{"error": "reading the file: " + err.Error()})
	return &snapFailure{code: http.StatusBadGateway, body: body}
}

/*
reconcile settles a room's text against a file somebody outside the room
wrote: an agent appending to the notes, a device syncing, the CLI.

	Edits to different lines merge, three ways against the version the room
	last wrote: the merged text goes into the live document as one edit
	every editor receives, and the room's next pause writes it on top of
	their version. Text the file already holds needs no copy either — the
	file wins and the document follows it. Only lines both sides changed,
	with something in the room's version the file does not have, are parked
	beside the file as a conflict copy.

	A final snapshot has no editors left to receive an edit, so it writes
	the merge straight to the file instead.
*/
func (s *Server) reconcile(ctx context.Context, room string, who User, text, baseText string, haveBase bool, theirs, theirSha string, final bool) {
	project, path, _ := strings.Cut(room, "/")
	target, ok, whole := reconcileTarget(path, text, baseText, haveBase, theirs, func(t string) bool {
		return fileHolds(theirs, t) || s.copyHolds(ctx, project, path, t)
	})
	park := func() {
		// parkCopy owns the text from here and retries a failing store, so
		// the room can move onto their version without it being the only
		// place the text lives.
		if strings.TrimSpace(text) != "" && !s.rooms().isParked(room, text) {
			s.rooms().park(room, text)
			s.parkCopy(ctx, who, project, path, conflictPath(path, who, time.Now()), text)
		}
		if !whole {
			// Writing on top of a version the room never held in full
			// would erase what it could not read.
			s.rooms().yield(room, theirSha)
			return
		}
		// The document takes their version, not just the base: built on
		// their version while still holding the room's text, the next pause
		// would write the room's text over theirs, conditional on it and so
		// accepted.
		s.moveDoc(ctx, room, text, theirs, theirSha, true, final)
	}
	if !ok {
		park()
		return
	}
	if final {
		if target != theirs {
			// The last chance to keep this text: refused again, it is parked.
			w := s.putAs(ctx, who, project, path, target, theirSha)
			if w.code != http.StatusOK {
				park()
				return
			}
			sha := reportedSha(w)
			if sha == "" {
				s.moveDoc(ctx, room, text, target, theirSha, false, true)
				return
			}
			theirSha = sha
		}
		s.moveDoc(ctx, room, text, target, theirSha, true, true)
		return
	}
	/* Not against text alone. This read text before the storage round trips
	   that found their write, and with several editors typing, somebody
	   always typed in between: a move refused on any keystroke never lands
	   while the meeting goes on, and their write stays out of the room. So
	   the document as it stands, inside the same Apply that splices it, is
	   merged against their version again — microseconds, no I/O, nobody can
	   type in between. A copy already parked is not consulted here: that is
	   storage, and a live text it alone would hold waits for the next pause. */
	recompute := func(cur string) (string, bool, bool) {
		to, ok, _ := reconcileTarget(path, cur, baseText, haveBase, theirs, func(t string) bool { return fileHolds(theirs, t) })
		return to, to == theirs, ok
	}
	from, landed, moved := s.moveDocLive(ctx, room, text, target, theirSha, target == theirs, recompute)
	if moved && landed == from {
		// The document did not change, so nothing re-arms its timer: what it
		// holds is still unwritten.
		s.snapshotLocked(ctx, room, false)
	}
}

// reconcileTarget is reconcile's decision, with no storage in it but holds:
// the text the room should move to when the file says theirs, the room says
// text and both came from baseText — or !ok, when only parking keeps text.
// whole is false for a side too large or not text, which is never merged.
func reconcileTarget(path, text, baseText string, haveBase bool, theirs string, holds func(text string) bool) (target string, ok, whole bool) {
	// roomBytes and blobText stop just past maxSeedBytes. A prefix taken for the
	// whole file would be merged and written back over its tail, so a file
	// that large is only ever parked beside.
	whole = len(theirs) < maxSeedBytes && len(baseText) < maxSeedBytes
	// Only text is merged or appended: a binary outside write mixed with the
	// room's lines would be a file that is neither.
	whole = whole && textmerge.IsText([]byte(theirs)) && textmerge.IsText([]byte(baseText)) && textmerge.IsText([]byte(text))
	appended := whole && appendedBoth(baseText, haveBase, theirs, text)
	var merged []byte
	canMerge := false
	if whole && haveBase && !appended {
		merged, canMerge = textmerge.Merge([]byte(baseText), []byte(text), []byte(theirs))
		canMerge = canMerge && validMergedText(path, merged)
	}
	switch {
	case !whole:
		// parked by the caller
	case appended:
		// Both sides only added to the end: an agent appending to a daily
		// note while somebody types at its foot. The line merge orders two
		// insertions at one point by content; this room is the file's only
		// writer, so it keeps them as they happened: theirs, then what the
		// room added, each exactly as written, repeated lines and blank lines
		// included. Too big to write, it is parked — never handed to the
		// line merge or the checks below, which match lines and would take a
		// repeated line or a blank one as already in the file.
		if both := joinAppends(theirs, strings.TrimPrefix(text, baseText)); len(both) < maxSeedBytes {
			return both, true, whole
		}
	case canMerge:
		// Two sides under the bound can insert past it together, and a
		// merge no room could open again is parked rather than written.
		if len(merged) < maxSeedBytes {
			return string(merged), true, whole
		}
	case holds(text):
		return theirs, true, whole
	case haveBase && strings.TrimSpace(baseText) == "":
		// Both sides wrote into an empty file — a template arriving while
		// somebody already typed. There is no ancestor to order them by, and
		// neither is a revision of the other: keep both, theirs first.
		if both := appendNew(theirs, text); len(both) < maxSeedBytes {
			return both, true, whole
		}
	}
	return "", false, whole
}

// moveDoc brings a room's document from text to to, as one edit, and rebases
// the room onto sha, reporting whether it did. A document typed into since it
// read text is left alone with its base: that typing is in neither version,
// and the next pause settles it against sha again.
//
// The document has to move with the base, or a room reopened before it
// unloads would write its old text over this version. Nobody is connected to
// a room in its final snapshot, and ydocs().Apply would open the room again,
// so a final move splices the held document directly.
//
// filed says to is exactly what sha holds. The moved state is then stored
// under sha, as a snapshot stores what it writes: the next room for the file
// replays the same items, and a returning browser that typed some of them
// merges once instead of beside a second copy spliced in from the bytes.
func (s *Server) moveDoc(ctx context.Context, room, text, to, sha string, filed, final bool) bool {
	if !final {
		_, _, moved := s.moveDocLive(ctx, room, text, to, sha, filed, nil)
		return moved
	}
	var state []byte
	moved := false
	if doc, _, held := s.rooms().get(room); held {
		txt := doc.GetText("body")
		if txt.ToString() == text {
			spliceVia(func(fn func(*crdt.Transaction)) { doc.Transact(fn) }, txt, text, to)
			state, moved = doc.EncodeStateAsUpdate(), true
		}
	}
	if moved {
		s.settleMove(ctx, room, to, sha, filed, state)
	}
	return moved
}

// moveDocOrigin marks the delta a move applies to a live room. Like
// ycollabSyncOrigin, the relay forwards it and local peers get it by
// broadcast.
var moveDocOrigin = &struct{ name string }{"ycollab/move"}

// moveDocLive is moveDoc on a live room, which a keystroke can reach at any
// moment: ygo's Apply holds the document's lock only inside transact, and
// reading the text takes that lock too, so no text read can be pinned to the
// splice that follows it. The move is therefore never made in character
// offsets on the live document. It is made on a private copy of one atomic
// snapshot of it, and only the resulting update — items anchored to the
// snapshot's items by ID, a delete set by ID — is applied to the room, where
// it integrates around anything typed meanwhile instead of landing a few
// characters off.
//
// The snapshot spelling something other than text means it was typed into.
// Without recompute it is left alone (park: typing nobody parked must never
// be overwritten). With it, recompute answers from the snapshot the text to
// move to, whether that is exactly what sha holds, and whether there is a
// move at all. from is the snapshot's text and landed the text moved to.
func (s *Server) moveDocLive(ctx context.Context, room, text, to, sha string, filed bool, recompute func(cur string) (to string, filed, ok bool)) (from, landed string, moved bool) {
	yd := s.ydocs()
	doc := yd.GetDoc(room)
	if doc == nil {
		// Not resident: open it as Apply always has, then work on it.
		if err := yd.Apply(ctx, room, func(*crdt.Doc, func(func(*crdt.Transaction))) {}); err != nil && !errors.Is(err, ygows.ErrNoChanges) {
			return "", "", false
		}
		if doc = yd.GetDoc(room); doc == nil {
			return "", "", false
		}
	}
	snap := doc.EncodeStateAsUpdate()
	scratch := crdt.New()
	if err := crdt.ApplyUpdateV1(scratch, snap, nil); err != nil || scratch.PendingStats().Items > 0 {
		return "", "", false // an update still missing its causes: the next pause
	}
	txt := scratch.GetText("body")
	cur := txt.ToString()
	if cur != text {
		if recompute == nil {
			return "", "", false
		}
		liveTo, liveFiled, ok := recompute(cur)
		if !ok {
			return "", "", false // a true conflict: the next pause settles it
		}
		to, filed = liveTo, liveFiled
	}
	state := snap
	if cur != to {
		var deltas [][]byte
		unsub := scratch.OnUpdate(func(u []byte, _ any) { deltas = append(deltas, append([]byte(nil), u...)) })
		spliceVia(func(fn func(*crdt.Transaction)) { scratch.Transact(fn) }, txt, cur, to)
		unsub()
		if len(deltas) == 0 {
			return "", "", false
		}
		delta := deltas[0]
		if len(deltas) > 1 {
			merged, err := crdt.MergeUpdatesV1(deltas...)
			if err != nil {
				return "", "", false
			}
			delta = merged
		}
		// Applied to the hub's document — which feeds the relay, persistence
		// and this room's snapshot timer — then broadcast to this process's
		// sockets, which an update applied outside Apply does not reach.
		if err := crdt.ApplyUpdateV1(doc, delta, moveDocOrigin); err != nil {
			return "", "", false
		}
		_ = yd.BroadcastUpdate(ctx, room, delta)
		state = doc.EncodeStateAsUpdate()
	}
	/* state may hold a keystroke that landed between snap and the apply.
	   It is not re-checked, rolled back or held off the rebase: an update
	   anchored to items merges with it exactly as two peers' keystrokes
	   merge — its inserts are never in the move's delete set, its deletes
	   can only name items both already had — so neither edit is lost, even
	   on one line. The document then holds everything sha holds, so rebasing
	   onto sha is what makes the next pause write move and keystroke
	   together on top of it. Left on the old base, that pause would meet sha
	   again, reconcile text that already carries their write, and park
	   copies of what the file holds. settleMove files state under sha only
	   when it spells exactly to. */
	s.settleMove(ctx, room, to, sha, filed, state)
	return cur, to, true
}

// settleMove rebases a room whose document moved to to, storing the moved
// state under sha when to is exactly what sha holds.
func (s *Server) settleMove(ctx context.Context, room, to, sha string, filed bool, state []byte) {
	if filed {
		if spelled, ok := replayed(state); ok && spelled == to {
			project, path, _ := strings.Cut(room, "/")
			s.storeRoomHistory(ctx, project, path, sha, state)
		}
	}
	s.rooms().rebase(room, sha)
}

// appendedBoth reports whether theirs and text each extend base, which ends a
// line, with additions of their own, and not the same ones.
func appendedBoth(base string, haveBase bool, theirs, text string) bool {
	if !haveBase || base == "" || !strings.HasSuffix(base, "\n") ||
		!strings.HasPrefix(theirs, base) || !strings.HasPrefix(text, base) {
		return false
	}
	x, y := theirs[len(base):], text[len(base):]
	return x != "" && y != "" && x != y
}

// joinAppends is file followed by added, starting it on a line of its own.
func joinAppends(file, added string) string {
	if !strings.HasSuffix(file, "\n") {
		file += "\n"
	}
	return file + added
}

// appendNew is file followed by the lines of text the file does not already
// hold, in their order. Lines are matched in order and one for one, so a line
// typed twice is kept twice.
func appendNew(file, text string) string {
	rest := matchLines(file, text)
	out := file
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if len(rest) == 0 {
		return out
	}
	return out + strings.Join(rest, "\n") + "\n"
}

// fileHolds reports whether a text adds nothing to a file: empty, or its
// non-blank lines appear in the file in the same order, each as often.
// Parking such a text beside the file makes a conflict copy a reader has to
// open only to find nothing in it.
func fileHolds(file, text string) bool {
	rest := matchLines(file, text)
	return len(rest) == 0
}

// matchLines walks the non-blank lines of text against those of file in
// order, each file line matching at most once. rest is what found no match.
func matchLines(file, text string) (rest []string) {
	var have []string
	for _, l := range strings.Split(file, "\n") {
		if l = strings.TrimRight(l, "\r"); strings.TrimSpace(l) != "" {
			have = append(have, l)
		}
	}
	at := 0
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimRight(l, "\r"); strings.TrimSpace(l) == "" {
			continue
		}
		k := at
		for k < len(have) && have[k] != l {
			k++
		}
		if k < len(have) {
			at = k + 1
			continue
		}
		rest = append(rest, l)
	}
	return rest
}

// copyHolds reports whether a conflict copy of this path already holds
// exactly this text, so parking it again would only add a duplicate.
func (s *Server) copyHolds(ctx context.Context, project, path, text string) bool {
	_, v, err := s.projectVolume(project)
	if err != nil {
		return false
	}
	snap, err := v.current(ctx)
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(text))
	want := hex.EncodeToString(sum[:])
	prefix := path + ".bdrive-conflict-"
	for p, fi := range snap.files {
		if strings.HasPrefix(p, prefix) && fi.Blob == want {
			return true
		}
	}
	return false
}

// blobText reads one past version of a file by its blob, bounded like a seed.
// The empty sha is the empty text.
func (s *Server) blobText(ctx context.Context, project, sha string) (string, bool) {
	if sha == "" {
		return "", true
	}
	_, v, err := s.projectVolume(project)
	if err != nil {
		return "", false
	}
	rs, ok := v.source.(*RemoteSource)
	if !ok {
		return "", false
	}
	rc, err := rs.OpenBlob(ctx, sha)
	if err != nil {
		return "", false
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxSeedBytes+1))
	if err != nil {
		return "", false
	}
	return string(b), true
}

// finishOnClose bounds how long shutdown waits on a final snapshot that
// cannot read its file. Cloud Run gives a stopping instance ten seconds.
var finishOnClose = 5 * time.Second

// Backoff for a refused room's conflict copy, doubling up to a ceiling. Vars
// so a test need not wait out real backoff.
var (
	parkBackoff    = 30 * time.Second
	parkBackoffMax = 10 * time.Minute
)

// parkCopy writes a room's refused text beside its file. A failing store is
// retried in the background for as long as this process lives, with no retry
// budget: once the room is evicted this text exists nowhere else, so giving
// up would be the loss the copy exists to prevent. Held in memory only, so a
// hub restart before the store recovers still loses it.
//
// The copy is only ever created, never written over: a name another copy
// already holds (another room, or a closing tab, in the same second) moves it
// a second on. A retry finding its own text under a name it reached before
// is answered unchanged and counts as written.
func (s *Server) parkCopy(ctx context.Context, who User, project, path, copy, text string) {
	try := func(ctx context.Context) bool {
		var cw *memWriter
		for i := range maxConflictNameTries {
			name, ok := shiftConflictName(copy, i)
			if !ok {
				if i > 0 {
					break
				}
				name = copy
			}
			cw = s.putVia(ctx, who, project, name, text, "", true)
			if cw.code == http.StatusConflict {
				continue
			}
			if cw.code == http.StatusOK {
				log.Printf("bdrive: %s: the room's text could not be written to it; parked beside it as %s", path, name)
				s.rooms().parkedOnStorage(project+"/"+path, text)
				return true
			}
			break
		}
		log.Printf("bdrive: %s: could not park the room's refused text (%d bytes) as %s (%d); retrying", path, len(text), copy, cw.code)
		return false
	}
	backoff, backoffMax := parkBackoff, parkBackoffMax
	if try(ctx) {
		return
	}
	bg := s.collabContext()
	if bg.Err() != nil {
		return
	}
	go func() {
		wait := backoff
		timer := time.NewTimer(wait)
		defer timer.Stop()
		for {
			select {
			case <-bg.Done():
				return
			case <-timer.C:
				if bg.Err() != nil || try(bg) {
					return
				}
				wait = min(wait*2, backoffMax)
				timer.Reset(wait)
			}
		}
	}()
}

// finishLater is a final snapshot that could not read its file, carried
// past the room: retried with backoff for as long as this process lives,
// like parkCopy, and settled the way the snapshot would have settled it once
// the file can be read. Every write it makes is conditional on base, or on
// the version it then reads.
func (s *Server) finishLater(ctx context.Context, room string, who User, text, base string, state []byte) {
	project, path, _ := strings.Cut(room, "/")
	// The file now spells exactly the room's text, so the room's state is
	// its history: a browser reconnecting with that state then replays onto
	// it instead of beside it. That stays true if a room opened meanwhile,
	// since the state spells the file; the base does not, and is left to it.
	settled := func(ctx context.Context, sha string) {
		if sha != "" {
			s.storeRoomHistory(ctx, project, path, sha, state)
			s.rooms().rebaseUnheld(room, sha)
		}
	}
	// attempt runs under the room's snapshot lock. The caller, a snapshot,
	// holds it while this function runs; a retry after it returns takes it
	// again (try).
	attempt := func(ctx context.Context) bool {
		cur, curSha, have, err := s.roomBytes(ctx, room)
		if err != nil {
			return false
		}
		// The file opened again in the meantime: that room holds the pen and
		// its own base, and this text is kept beside the file unless the file
		// already says it. Asked again right before each write below; a room
		// that opens after that still finds every write conditional on the
		// version it seeded from, or refused.
		yieldToRoom := func() bool {
			if _, _, live := s.rooms().get(room); !live {
				return false
			}
			if !have || len(cur) > maxSeedBytes || !fileHolds(string(cur), text) {
				s.parkCopy(ctx, who, project, path, conflictPath(path, who, time.Now()), text)
			}
			return true
		}
		if yieldToRoom() {
			return true
		}
		switch {
		case have && len(cur) <= maxSeedBytes && string(cur) == text:
			settled(ctx, curSha)
			return true
		case !have && base != "":
			// The room wrote this file and somebody deleted it since. The
			// deletion is theirs; the room's text goes beside it.
			s.parkCopy(ctx, who, project, path, conflictPath(path, who, time.Now()), text)
			return true
		case !have:
			if yieldToRoom() {
				return true
			}
			w := s.putAs(ctx, who, project, path, text, "")
			if w.code != http.StatusOK {
				return false
			}
			settled(ctx, reportedSha(w))
			return true
		case curSha == base:
			if yieldToRoom() {
				return true
			}
			w := s.putAs(ctx, who, project, path, text, base)
			switch w.code {
			case http.StatusOK:
				settled(ctx, reportedSha(w))
				return true
			case http.StatusUnprocessableEntity:
				if !fileHolds(string(cur), text) {
					s.parkCopy(ctx, who, project, path, conflictPath(path, who, time.Now()), text)
				}
				return true
			}
			return false // a 409 is settled against their version next time
		}
		if yieldToRoom() {
			return true
		}
		baseText, haveBase := s.blobText(ctx, project, base)
		s.reconcile(ctx, room, who, text, baseText, haveBase, string(cur), curSha, true)
		return true
	}
	try := func(ctx context.Context) bool {
		unlock := s.rooms().lockSnapshots(room)
		defer unlock()
		return attempt(ctx)
	}
	bg := s.collabContext()
	if bg.Err() != nil {
		// Shutting down: Close cancels the collab context before its final
		// snapshots, so no retry can outlive this call. What time the
		// caller's context has left, up to finishOnClose, is the last chance.
		ctx, cancel := context.WithTimeout(ctx, finishOnClose)
		defer cancel()
		for wait := parkBackoff; ctx.Err() == nil && !attempt(ctx); wait = min(wait*2, parkBackoffMax) {
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
		}
		return
	}
	go func() {
		wait := parkBackoff
		timer := time.NewTimer(wait)
		defer timer.Stop()
		for {
			select {
			case <-bg.Done():
				return
			case <-timer.C:
				if bg.Err() != nil || try(bg) {
					return
				}
				wait = min(wait*2, parkBackoffMax)
				timer.Reset(wait)
			}
		}
	}()
}

// putAs writes one path through the ordinary upload door as a human, with
// the base the write is conditional on ("" for unconditional).
func (s *Server) putAs(ctx context.Context, who User, project, path, text, base string) *memWriter {
	return s.putVia(ctx, who, project, path, text, base, false)
}

// parkNewCopy writes text as a new conflict copy of path through the upload
// door, as who. A copy is named to the second, and two tabs of one account
// can close in the same one: each name is created only if it is free
// (If-None-Match: *, which a second hub process is held to as well), and a
// taken one moves the copy a second on, so neither replaces the other.
// parked is the name it was written under, "" when it was not.
func (s *Server) parkNewCopy(ctx context.Context, who User, project, path, text string) (pw *memWriter, parked string) {
	t0 := time.Now()
	for i := range maxConflictNameTries {
		name := conflictPath(path, who, t0.Add(time.Duration(i)*time.Second))
		pw = s.putVia(ctx, who, project, name, text, "", true)
		if pw.code == http.StatusConflict {
			continue
		}
		if pw.code == http.StatusOK {
			parked = name
		}
		return pw, parked
	}
	return pw, ""
}

// putVia is putAs, or with create a write that only creates path.
func (s *Server) putVia(ctx context.Context, who User, project, path, text, base string, create bool) *memWriter {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		"/api/p/"+url.PathEscape(project)+"/upload/content?path="+url.QueryEscape(path),
		strings.NewReader(text))
	if err != nil {
		return &memWriter{code: 0, hdr: http.Header{}}
	}
	if create {
		req.Header.Set("If-None-Match", "*")
	}
	if base != "" {
		req.Header.Set("If-Match", base)
		// An editor in the room confirmed this exact text shrinking the
		// file (/ycollab/sync confirm_shrink).
		if s.rooms().shrinkConfirmed(project+"/"+path, text) {
			req.Header.Set(confirmShrinkHeader, "1")
		}
	}
	// The room settles a refusal itself, against its live document
	// (reconcile); the door merging on its behalf would file the room's
	// state under a blob that spells something else.
	req.Header.Set(noMergeHeader, "1")
	// The human, never the hub: an op authored by "the server" is a
	// regression in History even when the server is holding the pen.
	req = withUser(req, who)
	w := newMemWriter()
	s.apiMux.ServeHTTP(w, req)
	return w
}

// reportedSha reads the blob the upload door reports — on a 200 the one it wrote,
// on a 409 the one that is there instead.
func reportedSha(w *memWriter) string {
	var out struct {
		Sha string `json:"sha"`
	}
	_ = json.Unmarshal(w.body.Bytes(), &out)
	return out.Sha
}

// conflictPath is the sync path's conflict name, for a room's version parked
// beside a file somebody else wrote: <name>.bdrive-conflict-<who>-<utc>.
func conflictPath(path string, who User, at time.Time) string {
	name := who.Name
	if name == "" {
		name = who.Email
	}
	if name == "" {
		name = "hub"
	}
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, name)
	return path + ".bdrive-conflict-" + safe + "-" + at.UTC().Format(conflictStampLayout)
}

// conflictStampLayout is the second a conflict copy's name ends in. The sync
// engine and the browser parse exactly this form.
const conflictStampLayout = "20060102T150405Z"

// shiftConflictName is a conflict copy's name moved step seconds on, and
// whether name is one at all.
func shiftConflictName(name string, step int) (string, bool) {
	i := strings.LastIndex(name, "-")
	if i < 0 || !strings.Contains(name[:i], ".bdrive-conflict-") {
		return "", false
	}
	at, err := time.Parse(conflictStampLayout, name[i+1:])
	if err != nil {
		return "", false
	}
	return name[:i+1] + at.Add(time.Duration(step)*time.Second).UTC().Format(conflictStampLayout), true
}

// roomRegistry keeps a live room's document addressable, and remembers who may
// be attributed for it. ygo hands the document to OnLoadDocument and then only
// ever names the room again, so without this a snapshot would have nothing to
// write.
type roomRegistry struct {
	mu sync.Mutex
	// flushing counts snapshots running on a timer's goroutine, and renewing
	// the lease renewers still running, so a caller tearing rooms down can
	// wait for the ones already in flight (settle).
	flushing int
	renewing int
	docs     map[string]*crdt.Doc
	gens     map[string]uint64
	users    map[string]User
	// bases is the blob each room last wrote or was seeded from: the If-Match
	// of its next snapshot. It is what turns "the hub is the only writer" from
	// a convention into a guarantee that an outside writer is never silently
	// overwritten.
	bases map[string]string
	// pending is a room's armed snapshot, and the wall-clock deadline it may
	// not be pushed past by more typing.
	pending map[string]*roomFlush
	unsubs  map[string]func()
	// parked is the text a room last set aside as a conflict copy, so the
	// two final snapshots (last peer, unload) park it once.
	parked map[string]string
	// stored is the text of the conflict copy a room last got onto storage:
	// what lets /ycollab/sync call an edit set aside there durable.
	stored map[string]string
	// yielded is a file version this room parked beside and will not write
	// over: one too large to hold or merge, so the room's text cannot be
	// settled against it and every later write would erase it.
	yielded map[string]string
	// joins counts who is using each room right now — a socket for its whole
	// connection, a /ycollab/sync for its whole request — and closing holds
	// the rooms closeIfUnused is closing, each closed once its close returns;
	// both under joinMu, which is never held across a close (enter).
	joinMu  sync.Mutex
	joins   map[string]int
	closing map[string]chan struct{}
	// leases is what this process holds for each room, under its own lock
	// because taking one is a storage round-trip (lease.go).
	leaseMu  sync.Mutex
	leases   map[string]*roomLease
	renewals map[string]chan struct{}
	// waits counts how many times a room on a process that does not write
	// it has looked for the writer's write since the last edit.
	waits map[string]int
	// retries counts a room's snapshot attempts the upload door failed, for
	// backoff (retryLater).
	retries map[string]int
	// shrinkOK is the sha256 of a text an editor confirmed may sharply shrink
	// the file (confirm_shrink on /ycollab/sync).
	shrinkOK map[string]string
	// snapLocks serializes each room's snapshots (lockSnapshots).
	snapLocks map[string]*roomSnapLock
}

// roomSnapLock is one room's snapshot lock, kept only while someone holds or
// waits for it.
type roomSnapLock struct {
	sync.Mutex
	users int
}

// lockSnapshots takes room's snapshot lock and returns its release.
func (r *roomRegistry) lockSnapshots(room string) (unlock func()) {
	r.mu.Lock()
	if r.snapLocks == nil {
		r.snapLocks = map[string]*roomSnapLock{}
	}
	l := r.snapLocks[room]
	if l == nil {
		l = &roomSnapLock{}
		r.snapLocks[room] = l
	}
	l.users++
	r.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		r.mu.Lock()
		if l.users--; l.users == 0 {
			delete(r.snapLocks, room)
		}
		r.mu.Unlock()
	}
}

type roomFlush struct {
	timer    *time.Timer
	deadline time.Time
}

func (s *Server) rooms() *roomRegistry {
	s.roomOnce.Do(func() {
		s.collabMu.Lock()
		defer s.collabMu.Unlock()
		s.roomReg = &roomRegistry{
			docs: map[string]*crdt.Doc{}, gens: map[string]uint64{}, users: map[string]User{},
			bases: map[string]string{}, pending: map[string]*roomFlush{}, unsubs: map[string]func(){},
			parked: map[string]string{}, stored: map[string]string{}, yielded: map[string]string{}, leases: map[string]*roomLease{}, renewals: map[string]chan struct{}{},
			waits: map[string]int{}, retries: map[string]int{}, shrinkOK: map[string]string{},
		}
	})
	return s.roomReg
}

func (r *roomRegistry) load(room string, doc *crdt.Doc) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gens[room]++
	r.docs[room] = doc
	return r.gens[room]
}

func (r *roomRegistry) rebase(room, sha string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bases[room] = sha
}

// rebaseUnheld moves the base only while no room holds the file, checked and
// set under one lock: a room opened in between keeps the base it seeded.
func (r *roomRegistry) rebaseUnheld(room, sha string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, held := r.docs[room]; held {
		return false
	}
	r.bases[room] = sha
	return true
}

func (r *roomRegistry) isParked(room, text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev, ok := r.parked[room]
	return ok && prev == text
}

// park records text as set aside for room, once its copy is written.
func (r *roomRegistry) park(room, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.parked[room] = text
}

// parkedOnStorage records that room's text is written as a conflict copy,
// for a room still open: a copy finished after the room is gone has no
// sync left to answer.
func (r *roomRegistry) parkedOnStorage(room, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, open := r.docs[room]; open {
		r.stored[room] = text
	}
}

// parkedDurably reports whether text is what the room last wrote as a
// conflict copy.
func (r *roomRegistry) parkedDurably(room, text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev, ok := r.stored[room]
	return ok && prev == text
}

func (r *roomRegistry) yield(room, sha string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.yielded[room] = sha
}

func (r *roomRegistry) yieldedTo(room string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.yielded[room]
}

func (r *roomRegistry) base(room string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bases[room]
}

/*
The hub is the only writer while a room is live.

	Every co-editor used to write the file themselves, 700ms after their own
	last keystroke, each against whatever If-Match they last saw. With one
	document shared by N people that is N writers racing for one file, and
	every lost race parked somebody's text beside the file as a "conflict" that
	was the file against itself. Two rounds of client-side retry logic later,
	it was still happening, because the race is between reading the base and
	the write arriving, and no amount of care about timing closes that.

	So nobody in the room writes. The hub holds the document, sees every
	update, and writes ONCE per pause: two seconds after the last change, or
	six seconds after the first if the typing never pauses — Hocuspocus's
	onStoreDocument shape, with a deadline short enough that a non-holder's
	/ycollab/sync (ycollabSyncDurableWait) always outlasts it. One
	writer means an If-Match refusal can only ever come from someone outside
	the room, which is exactly the case it should be reserved for.

	OnUpdate must return at once — nothing on it may block or re-enter the
	document — so all it does is (re)arm a timer. The write runs on the
	timer's goroutine.
*/
// Vars, not consts: a test that waits ten real seconds to see a deadline fire
// is a test nobody runs. Production never sets them.
// snapshotMax keeps a continuously edited room at no more than one write per
// six seconds (≈0.167/s, six times under GCS's one-write-per-second limit on
// a single object) and must stay below ycollabSyncDurableWait.
var (
	snapshotIdle = 2 * time.Second
	snapshotMax  = 6 * time.Second
)

func (s *Server) watchRoom(room string, doc *crdt.Doc) {
	bg := s.collabContext()
	unsub := doc.OnUpdate(func(_ []byte, _ any) {
		if bg.Err() == nil {
			s.rooms().touched(room, func() { s.snapshotRoom(bg, room) })
		}
	})
	r := s.rooms()
	r.mu.Lock()
	if old := r.unsubs[room]; old != nil {
		old()
	}
	r.unsubs[room] = unsub
	r.mu.Unlock()
}

// touched (re)arms a room's snapshot: idle from now, but never later than
// max from when the burst began.
func (r *roomRegistry) touched(room string, flush func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.waits, room)
	now := time.Now()
	p := r.pending[room]
	if p == nil {
		p = &roomFlush{deadline: now.Add(snapshotMax)}
		r.pending[room] = p
	} else {
		p.timer.Stop()
	}
	wait := snapshotIdle
	if left := p.deadline.Sub(now); left < wait {
		wait = max(left, 0)
	}
	r.arm(room, p, wait, flush)
}

// arm schedules a room's flush. Caller holds r.mu.
func (r *roomRegistry) arm(room string, p *roomFlush, wait time.Duration, flush func()) {
	p.timer = time.AfterFunc(wait, func() {
		r.mu.Lock()
		delete(r.pending, room)
		r.flushing++
		r.mu.Unlock()
		defer func() {
			r.mu.Lock()
			r.flushing--
			r.mu.Unlock()
		}()
		flush()
	})
}

// maxWriterWaits bounds how long a process that does not write a room keeps
// looking for the writer's write: the writer has snapshotMax to make it and
// this process up to Refresh to see it, and a writer that cannot write (a
// refused quota) must not keep a timer alive forever.
const maxWriterWaits = 10

// writerWaitLimit is maxWriterWaits, or enough looks to outlast a lease whose
// holder died: once it expires the next look takes it (holdLease) and this
// process writes the room itself.
func writerWaitLimit() int {
	n := maxWriterWaits
	if snapshotIdle > 0 {
		n = max(n, int(leaseTTL/snapshotIdle)+2)
	}
	return n
}

// waited reports whether a room has already looked once for another
// process's write since its last edit (awaitWriter).
func (r *roomRegistry) waited(room string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.waits[room] > 0
}

// enter marks room as in use until leave is called.
//
// A room /ycollab/sync opened is closed again once written, and that close
// must never take a room out from under a socket that is joining it.
//
// ygo cannot do this for us: RoomIdleTimeout reaps only a room a peer LEFT
// (handleDisconnect stamps idleSince), and Apply clears that stamp — so a
// room only ever touched over HTTP, or an idle one an HTTP sync touched,
// would stay resident, leased and never finally written for the life of the
// process. And CloseRoom(force=false) checks for registered peers only: a
// socket between ServeHTTP's room lookup and its registration is not one
// yet, and the close deletes the room it is about to join.
//
// So every user of a room — a socket for the whole of its ServeHTTP, a sync
// request for the whole of its handler — holds a count under joinMu, and
// the close runs only when that count is zero. The close itself is a final
// snapshot, a storage round-trip, so it runs with joinMu released: the room
// is marked closing instead, and only an enter of THAT room waits for it —
// every other room keeps entering and leaving. A socket that arrives during
// the close waits for it, then opens a fresh room seeded from the file the
// close just wrote.
func (r *roomRegistry) enter(room string) (leave func()) {
	r.joinMu.Lock()
	for {
		ch, busy := r.closing[room]
		if !busy {
			break
		}
		r.joinMu.Unlock()
		<-ch
		r.joinMu.Lock()
	}
	if r.joins == nil {
		r.joins = map[string]int{}
	}
	r.joins[room]++
	r.joinMu.Unlock()
	return func() {
		r.joinMu.Lock()
		if r.joins[room]--; r.joins[room] <= 0 {
			delete(r.joins, room)
		}
		r.joinMu.Unlock()
	}
}

// closeIfUnused runs shut while nobody is in the room, holding off every
// enter of that room — and only that room — until it returns. A close
// already running for the room is left to finish alone.
func (r *roomRegistry) closeIfUnused(room string, shut func()) {
	r.joinMu.Lock()
	if r.joins[room] > 0 || r.closing[room] != nil {
		r.joinMu.Unlock()
		return
	}
	if r.closing == nil {
		r.closing = map[string]chan struct{}{}
	}
	ch := make(chan struct{})
	r.closing[room] = ch
	delete(r.joins, room)
	r.joinMu.Unlock()
	defer func() {
		r.joinMu.Lock()
		delete(r.closing, room)
		close(ch)
		r.joinMu.Unlock()
	}()
	shut()
}

// awaitWriter looks at the file again after another process has had time to
// write it, so that this process's browsers see the save land.
func (r *roomRegistry) awaitWriter(room string, flush func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending[room] != nil || r.waits[room] >= writerWaitLimit() {
		return
	}
	r.waits[room]++
	p := &roomFlush{deadline: time.Now().Add(snapshotMax)}
	r.pending[room] = p
	r.arm(room, p, snapshotIdle, flush)
}

// snapshotRetryMax caps the backoff between snapshots the upload door
// failed. A var so a test need not wait out real backoff.
var snapshotRetryMax = 30 * time.Second

// retryLater arms another snapshot after a failed one, doubling the wait up
// to snapshotRetryMax. There is no attempt limit: while the room is live
// and unwritten its text exists nowhere durable, so giving up would leave it
// to the relay's age-trimmed window. The chain ends by itself — a write that
// lands (or a file that already says this) settles the room, and a closed
// room's flush finds no room and arms nothing. An edit arming its own
// snapshot meanwhile supersedes it.
func (r *roomRegistry) retryLater(room string, flush func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.retries == nil {
		r.retries = map[string]int{}
	}
	if r.pending[room] != nil {
		return
	}
	n := r.retries[room]
	r.retries[room] = n + 1
	wait := snapshotIdle << min(n, 6) // shift bounded: never overflows
	if wait <= 0 || wait > snapshotRetryMax {
		wait = snapshotRetryMax
	}
	p := &roomFlush{deadline: time.Now().Add(wait)}
	r.pending[room] = p
	r.arm(room, p, wait, flush)
}

// confirmShrink records that an editor confirmed text may sharply shrink
// the file; the room's write of exactly that text then says so.
func (r *roomRegistry) confirmShrink(room, text string) {
	sum := sha256.Sum256([]byte(text))
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shrinkOK == nil {
		r.shrinkOK = map[string]string{}
	}
	r.shrinkOK[room] = hex.EncodeToString(sum[:])
}

func (r *roomRegistry) shrinkConfirmed(room, text string) bool {
	r.mu.Lock()
	want, ok := r.shrinkOK[room]
	r.mu.Unlock()
	if !ok {
		return false
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:]) == want
}

// settled is the file saying what the room says: nothing left to wait for.
func (r *roomRegistry) settled(room string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.waits, room)
	delete(r.retries, room)
}

// settle waits until no armed or running snapshot is left. Drop the rooms
// first: an armed timer for a live room would otherwise keep re-arming.
func (r *roomRegistry) settle() {
	for {
		r.mu.Lock()
		busy := r.flushing > 0 || r.renewing > 0 || len(r.pending) > 0
		r.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *roomRegistry) writer(room string, u User) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users[room] = u
}

func (r *roomRegistry) get(room string) (*crdt.Doc, User, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	doc, ok := r.docs[room]
	return doc, r.users[room], ok
}

func (r *roomRegistry) drop(room string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropLocked(room)
}

// dropIfDoc serializes replacement with both forgetting the old document and
// releasing its lease. A new load cannot inherit a lease the old unload frees.
func (r *roomRegistry) dropIfDoc(room string, doc *crdt.Doc, release func()) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.docs[room] != doc {
		return false
	}
	r.dropLocked(room)
	release()
	return true
}

// dropLocked removes room-local state. Caller holds r.mu.
func (r *roomRegistry) dropLocked(room string) {
	if p := r.pending[room]; p != nil {
		p.timer.Stop() // OnUnloadDocument has already snapshotted; nothing left to flush
		delete(r.pending, room)
	}
	if u := r.unsubs[room]; u != nil {
		u()
		delete(r.unsubs, room)
	}
	if stop := r.renewals[room]; stop != nil {
		close(stop)
		delete(r.renewals, room)
	}
	delete(r.waits, room)
	delete(r.retries, room)
	delete(r.shrinkOK, room)
	delete(r.docs, room)
	delete(r.users, room)
	delete(r.parked, room)
	delete(r.stored, room)
	delete(r.yielded, room)
	// bases outlives the room on purpose: it is how the NEXT room for this
	// file finds the history to rebuild from when somebody wrote the file
	// from outside in between (seedDoc). One sha per file ever edited.
}

// maxSeedBytes bounds what becomes a held document. Editing costs roughly ten
// times the content in CRDT items, so this is a memory ceiling rather than an
// opinion about file size.
const maxSeedBytes = 2 << 20

// maxHistoryBytes bounds a stored room history, which carries every edit ever
// made in a room and not just the text. Past it the room still works from the
// bytes; what it loses is a lossless rebuild for a returning browser.
const maxHistoryBytes = 8 << 20

// errRoomElsewhere is a room another process holds, on a hub with no relay.
var errRoomElsewhere = errors.New("another hub process holds this document")

// dropRoom forgets a room and gives its lease back.
func (s *Server) dropRoom(room string) {
	s.rooms().drop(room)
	s.releaseLease(s.collabHookContext(), room)
}

// contains reports whether the room's document already holds everything the
// stored state for sha does — the test for "the head is a version of this
// document, not an outsider's write". Compared by item identity, which is the
// only comparison a CRDT offers: two texts that read the same are not the
// same document (seedDoc).
func (s *Server) contains(ctx context.Context, doc *crdt.Doc, project, sha string) bool {
	state := s.roomHistory(ctx, project, sha)
	if state == nil {
		return false
	}
	return knows(doc.EncodeStateAsUpdate(), state)
}

// emptyUpdate is what DiffUpdateV1 answers when nothing is missing.
var emptyUpdate = crdt.EncodeStateAsUpdateV1(crdt.New(), nil)

// Close writes every live room back and stops the document server. Cloud Run
// gives a process ten seconds between SIGTERM and SIGKILL, and a peer's
// reconnect lands on a fresh process that builds the room from what was
// stored — so a room not written here is up to one snapshot interval of
// typing that exists only in browsers. Bounded by ctx; a Close past its
// deadline costs the tail, never a hang.
func (s *Server) Close(ctx context.Context) error {
	_ = s.collabContext()
	s.collabMu.Lock()
	s.collabClosing = ctx
	s.collabCancel()
	s.collabMu.Unlock()
	defer func() {
		s.collabMu.Lock()
		s.collabClosing = nil
		s.collabMu.Unlock()
	}()
	r := s.rooms()
	r.mu.Lock()
	rooms := make([]string, 0, len(r.docs))
	for room := range r.docs {
		rooms = append(rooms, room)
	}
	r.mu.Unlock()
	for _, room := range rooms {
		s.snapshotRoom(ctx, room)
	}
	var err error
	if s.y != nil {
		err = s.y.Shutdown(ctx)
	}
	// Shutdown closes peers, and a room with none — one the relay or Apply
	// created — is never unloaded by it. Include rooms that finished loading
	// after the first snapshot, so their leases and workers cannot outlive us.
	r.mu.Lock()
	rooms = rooms[:0]
	for room := range r.docs {
		rooms = append(rooms, room)
	}
	r.mu.Unlock()
	for _, room := range rooms {
		r.drop(room)
		s.releaseLease(ctx, room)
	}
	if s.collabRelay != nil {
		s.collabRelay.Close()
	}
	if err == nil {
		err = ctx.Err()
	}
	return err
}
