package webapp

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"unicode/utf16"

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
			s.finalSnapshot(s.collabHookContext(), room)
		}
		srv.OnUnloadDocument = func(_ context.Context, room string) {
			ctx := s.collabHookContext()
			s.finalSnapshot(ctx, room)
			// Close owns the final caller-context snapshot after it cancels
			// an unload already in flight. Keep its document addressable.
			if ctx.Err() != nil {
				return
			}
			s.rooms().drop(room)
			s.releaseLease(ctx, room)
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
	r.SetPathValue("room", projectID(r)+"/"+path)
	s.ydocs().ServeHTTP(w, r)
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
	body, sha, found, err := s.roomBytes(ctx, room)
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
	project, path, _ := strings.Cut(room, "/")
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
	if head := s.roomHead(ctx, project, path); head != "" && head != sha {
		newest = s.roomHistory(ctx, project, head)
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
			s.storeRoomHead(ctx, project, path, sha)
			return state
		}
		if !errors.Is(err, remote.ErrVersionMismatch) {
			break
		}
	}
	log.Printf("bdrive: claiming room history %s/%s: %v", project, sha, err)
	return state
}

// knows reports whether a stored state holds every item of another — the
// only comparison a CRDT offers: two states that spell the same text are not
// the same document.
func knows(state, other []byte) bool {
	if other == nil {
		return true
	}
	have, ok := docFrom(state)
	if !ok {
		return false
	}
	missing, err := crdt.DiffUpdateV1(other, have.StateVector())
	return err == nil && bytes.Equal(missing, emptyUpdate)
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

// splice turns txt from one text into another with a single edit: the common
// prefix and suffix are kept, the middle replaced. Offsets are UTF-16 units,
// which is what Yjs counts in.
func splice(doc *crdt.Doc, txt *crdt.YText, from, to string) {
	spliceVia(func(fn func(*crdt.Transaction)) { doc.Transact(fn) }, txt, from, to)
}

// spliceVia is splice through a caller's transaction runner — ygo's Apply
// hands one out, and only edits made through it reach the room's peers.
func spliceVia(transact func(func(*crdt.Transaction)), txt *crdt.YText, from, to string) {
	a, b := []rune(from), []rune(to)
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	n := 0
	for n < len(a)-p && n < len(b)-p && a[len(a)-1-n] == b[len(b)-1-n] {
		n++
	}
	at := len(utf16.Encode(a[:p]))
	del := len(utf16.Encode(a[p : len(a)-n]))
	ins := string(b[p : len(b)-n])
	if del == 0 && ins == "" {
		return
	}
	transact(func(txn *crdt.Transaction) {
		if del > 0 {
			txt.Delete(txn, at, del)
		}
		if ins != "" {
			txt.Insert(txn, at, ins, nil)
		}
	})
}

// foldIntoRoom puts a write the hub itself made to a file — a restore — into
// that file's live room, as one edit every peer receives, and moves the
// room's base to it. Without this the room keeps the text from before the
// write, and its next snapshot either writes that back over it or, refused
// for a stale base, parks a conflict copy. Reports whether a room took it;
// no room means no document to keep in step. A version too large for a room
// to hold ends the room instead.
func (s *Server) foldIntoRoom(ctx context.Context, project, path, sha string) bool {
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
	s.storeRoomHead(ctx, project, path, sha)
}

// ydoc/head/<path> names the newest state stored for a path — what the next
// room for it builds from, on any process (seedDoc).
func headKey(path string) string { return "ydoc/head/" + path }

func (s *Server) roomHead(ctx context.Context, project, path string) string {
	be, ok := s.projectBackend(project)
	if !ok {
		return ""
	}
	rc, err := be.Get(ctx, headKey(path))
	if err != nil {
		return ""
	}
	defer rc.Close()
	sha, _ := io.ReadAll(io.LimitReader(rc, 128))
	return strings.TrimSpace(string(sha))
}

func (s *Server) storeRoomHead(ctx context.Context, project, path, sha string) {
	if ctx.Err() != nil {
		return
	}
	be, ok := s.projectBackend(project)
	if !ok {
		return
	}
	if err := be.Put(ctx, headKey(path), strings.NewReader(sha), int64(len(sha))); err != nil {
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
	project, path, ok := strings.Cut(room, "/")
	if !ok || !projectIDRe.MatchString(project) {
		return nil, "", false, nil
	}
	if s.Projects != nil {
		if _, known := s.Projects.Get(project); !known {
			return nil, "", false, nil
		}
	}
	_, v, err := s.projectVolume(project)
	if err != nil {
		return nil, "", false, err
	}
	// A room is built from what the file IS, not from a snapshot a background
	// rebuild has not caught up on yet.
	snap, err := v.current(ctx)
	if err != nil {
		return nil, "", false, err
	}
	fi, ok := snap.files[path]
	if !ok {
		return nil, "", false, nil
	}
	rc, err := v.source.Open(ctx, path, fi)
	if err != nil {
		return nil, "", false, err
	}
	defer rc.Close()
	// One byte past the bound, so a caller can tell a whole file from a
	// prefix: anything longer than maxSeedBytes is only the start of one.
	body, err = io.ReadAll(io.LimitReader(rc, maxSeedBytes+1))
	if err != nil {
		return nil, "", false, err
	}
	return body, fi.Blob, true, nil
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
func (s *Server) snapshotRoom(ctx context.Context, room string) { s.snapshot(ctx, room, false) }

// finalSnapshot is the write made as the room empties: nobody is left to see
// the document, so a version the file refuses is parked beside it instead.
func (s *Server) finalSnapshot(ctx context.Context, room string) { s.snapshot(ctx, room, true) }

func (s *Server) snapshot(ctx context.Context, room string, final bool) {
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
	cur, curSha, have, err := s.roomBytes(ctx, room)
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
		return
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
			s.rooms().awaitWriter(room, func() { s.snapshotRoom(bg, room) })
		}
		return
	}
	if s.apiMux == nil {
		return
	}
	if have && curSha != base && s.contains(ctx, doc, project, curSha) {
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
	}
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
	// roomBytes and blobText stop just past maxSeedBytes. A prefix taken for the
	// whole file would be merged and written back over its tail, so a file
	// that large is only ever parked beside.
	whole := len(theirs) < maxSeedBytes && len(baseText) < maxSeedBytes
	// Only text is merged or appended: a binary outside write mixed with the
	// room's lines would be a file that is neither.
	whole = whole && textmerge.IsText([]byte(theirs)) && textmerge.IsText([]byte(baseText)) && textmerge.IsText([]byte(text))
	var merged []byte
	canMerge := false
	if whole && haveBase {
		merged, canMerge = textmerge.Merge([]byte(baseText), []byte(text), []byte(theirs))
	}
	target, ok := "", false
	switch {
	case !whole:
		// parked below
	case canMerge && len(merged) < maxSeedBytes:
		// Two sides under the bound can insert past it together, and a
		// merge no room could open again is parked rather than written.
		target, ok = string(merged), true
	case fileHolds(theirs, text) || s.copyHolds(ctx, project, path, text):
		target, ok = theirs, true
	case haveBase && strings.TrimSpace(baseText) == "":
		// Both sides wrote into an empty file — a template arriving while
		// somebody already typed. There is no ancestor to order them by, and
		// neither is a revision of the other: keep both, theirs first.
		if both := appendNew(theirs, text); len(both) < maxSeedBytes {
			target, ok = both, true
		}
	}
	park := func() {
		if strings.TrimSpace(text) == "" {
			// Nothing to keep; the room still steps off their version.
			if whole {
				s.rooms().rebase(room, theirSha)
			} else {
				s.rooms().yield(room, theirSha)
			}
			return
		}
		// parkCopy owns the text from here and retries a failing store, so
		// the room can move onto their version without it being the only
		// place the text lives.
		if !s.rooms().isParked(room, text) {
			s.rooms().park(room, text)
			s.parkCopy(ctx, who, project, path, conflictPath(path, who, time.Now()), text)
		}
		if !whole {
			// Writing on top of a version the room never held in full
			// would erase what it could not read.
			s.rooms().yield(room, theirSha)
			return
		}
		s.rooms().rebase(room, theirSha)
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
			if sha := reportedSha(w); sha != "" {
				theirSha = sha
			}
		}
		// The document moves with the base, or a room reopened before it
		// unloads would write its old text over this version. Nobody is
		// connected to a room in its final snapshot, and ydocs().Apply
		// would open the room again, so the held document is spliced
		// directly. Typed into since, it keeps its base and is settled
		// against this version on its next snapshot.
		if doc, _, held := s.rooms().get(room); held {
			txt := doc.GetText("body")
			if txt.ToString() == text {
				spliceVia(func(fn func(*crdt.Transaction)) { doc.Transact(fn) }, txt, text, target)
				s.rooms().rebase(room, theirSha)
			}
		}
		return
	}
	applied := false
	_ = s.ydocs().Apply(ctx, room, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		txt := doc.GetText("body")
		// Typing since the snapshot read the document is not in the merge;
		// leave the document alone and merge again on the next pause.
		if txt.ToString() != text {
			return
		}
		spliceVia(transact, txt, text, target)
		applied = true
	})
	if applied || target == text {
		// The document is now built on their version; the next pause
		// writes it there, conditional on it.
		s.rooms().rebase(room, theirSha)
		if target == text {
			s.snapshotRoom(ctx, room)
		}
	}
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
func (s *Server) parkCopy(ctx context.Context, who User, project, path, copy, text string) {
	try := func(ctx context.Context) bool {
		cw := s.putAs(ctx, who, project, copy, text, "")
		if cw.code == http.StatusOK {
			log.Printf("bdrive: %s: the room's text could not be written to it; parked beside it as %s", path, copy)
			return true
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
	try := func(ctx context.Context) bool {
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
	bg := s.collabContext()
	if bg.Err() != nil {
		// Shutting down: Close cancels the collab context before its final
		// snapshots, so no retry can outlive this call. What time the
		// caller's context has left, up to finishOnClose, is the last chance.
		ctx, cancel := context.WithTimeout(ctx, finishOnClose)
		defer cancel()
		for wait := parkBackoff; ctx.Err() == nil && !try(ctx); wait = min(wait*2, parkBackoffMax) {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		"/api/p/"+url.PathEscape(project)+"/upload/content?path="+url.QueryEscape(path),
		strings.NewReader(text))
	if err != nil {
		return &memWriter{code: 0, hdr: http.Header{}}
	}
	if base != "" {
		req.Header.Set("If-Match", base)
	}
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
	return path + ".bdrive-conflict-" + safe + "-" + at.UTC().Format("20060102T150405Z")
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
	// yielded is a file version this room parked beside and will not write
	// over: one too large to hold or merge, so the room's text cannot be
	// settled against it and every later write would erase it.
	yielded map[string]string
	// leases is what this process holds for each room, under its own lock
	// because taking one is a storage round-trip (lease.go).
	leaseMu  sync.Mutex
	leases   map[string]*roomLease
	renewals map[string]chan struct{}
	// waits counts how many times a room on a process that does not write
	// it has looked for the writer's write since the last edit.
	waits map[string]int
}

type roomFlush struct {
	timer    *time.Timer
	deadline time.Time
}

func (s *Server) rooms() *roomRegistry {
	s.roomOnce.Do(func() {
		s.roomReg = &roomRegistry{
			docs: map[string]*crdt.Doc{}, users: map[string]User{},
			bases: map[string]string{}, pending: map[string]*roomFlush{}, unsubs: map[string]func(){},
			parked: map[string]string{}, yielded: map[string]string{}, leases: map[string]*roomLease{}, renewals: map[string]chan struct{}{},
			waits: map[string]int{},
		}
	})
	return s.roomReg
}

func (r *roomRegistry) load(room string, doc *crdt.Doc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.docs[room] = doc
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
	ten seconds after the first if the typing never pauses — the numbers
	Hocuspocus settled on for onStoreDocument, for the same reasons. One
	writer means an If-Match refusal can only ever come from someone outside
	the room, which is exactly the case it should be reserved for.

	OnUpdate must return at once — nothing on it may block or re-enter the
	document — so all it does is (re)arm a timer. The write runs on the
	timer's goroutine.
*/
// Vars, not consts: a test that waits ten real seconds to see a deadline fire
// is a test nobody runs. Production never sets them.
var (
	snapshotIdle = 2 * time.Second
	snapshotMax  = 10 * time.Second
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

// awaitWriter looks at the file again after another process has had time to
// write it, so that this process's browsers see the save land.
func (r *roomRegistry) awaitWriter(room string, flush func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending[room] != nil || r.waits[room] >= maxWriterWaits {
		return
	}
	r.waits[room]++
	p := &roomFlush{deadline: time.Now().Add(snapshotMax)}
	r.pending[room] = p
	r.arm(room, p, snapshotIdle, flush)
}

// settled is the file saying what the room says: nothing left to wait for.
func (r *roomRegistry) settled(room string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.waits, room)
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
	delete(r.docs, room)
	delete(r.users, room)
	delete(r.parked, room)
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
	missing, err := crdt.DiffUpdateV1(state, doc.StateVector())
	return err == nil && bytes.Equal(missing, emptyUpdate)
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
