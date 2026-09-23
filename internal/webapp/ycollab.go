package webapp

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/reearth/ygo/crdt"
	ygows "github.com/reearth/ygo/provider/websocket"
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
		srv.OnLoadDocument = func(_ context.Context, room string, doc *crdt.Doc) error {
			s.rooms().load(room, doc)
			if err := s.seedDoc(room, doc); err != nil {
				return err
			}
			// AFTER the seed, so the bytes the document was built from are
			// never mistaken for an edit that needs writing back.
			s.watchRoom(room, doc)
			return nil
		}
		srv.OnLastPeer = func(ctx context.Context, room string) { s.snapshotRoom(ctx, room) }
		srv.OnUnloadDocument = func(ctx context.Context, room string) {
			s.snapshotRoom(ctx, room)
			s.rooms().drop(room)
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
func (s *Server) seedDoc(room string, doc *crdt.Doc) error {
	body, sha, ok := s.roomBytes(room)
	if !ok || len(body) == 0 {
		return nil // a file that does not exist yet starts empty
	}
	s.rooms().rebase(room, sha)
	txt := doc.GetText("body")
	doc.Transact(func(txn *crdt.Transaction) {
		txt.Insert(txn, 0, string(body), nil)
	})
	return nil
}

// roomBytes is the file behind a room, bounded, with the blob it currently is
// — the sha every write to it must name in If-Match.
func (s *Server) roomBytes(room string) ([]byte, string, bool) {
	project, path, ok := strings.Cut(room, "/")
	if !ok {
		return nil, "", false
	}
	_, v, err := s.projectVolume(project)
	if err != nil {
		return nil, "", false
	}
	ctx := context.Background()
	snap, err := v.snapshot(ctx)
	if err != nil {
		return nil, "", false
	}
	fi, ok := snap.files[path]
	if !ok {
		return nil, "", false
	}
	rc, err := v.source.Open(ctx, path, fi)
	if err != nil {
		return nil, "", false
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, maxSeedBytes))
	if err != nil {
		return nil, "", false
	}
	return body, fi.Blob, true
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
func (s *Server) snapshotRoom(ctx context.Context, room string) {
	project, path, ok := strings.Cut(room, "/")
	if !ok {
		return
	}
	doc, who, ok := s.rooms().get(room)
	if !ok || who.Email == "" {
		return // nobody with write access ever joined: nothing to attribute
	}
	text := doc.GetText("body").ToString()
	cur, curSha, have := s.roomBytes(room)
	if have && string(cur) == text {
		s.rooms().rebase(room, curSha)
		return // the file already says this
	}
	if s.apiMux == nil {
		return
	}
	base := s.rooms().base(room)
	w := s.putAs(ctx, who, project, path, text, base)
	switch w.code {
	case http.StatusOK:
		if sha := reportedSha(w); sha != "" {
			s.rooms().rebase(room, sha)
		}
	case http.StatusConflict:
		/* Somebody OUTSIDE the room wrote the file — an agent, the CLI, a
		   device — since the hub last did. Everyone inside the room shares
		   this document, so a 409 here can never be a co-editor; the room
		   itself is the only thing that writes on their behalf.

		   Same answer the client gave when it held the pen: theirs is the
		   file, ours goes beside it under the conflict name the sync path has
		   always used, attributed to the human who was editing. Then the room
		   rebases onto the new head, so the NEXT snapshot is not refused for
		   the same reason forever. Nothing is lost on either side; a reader
		   is told, via the change frame the copy raises, that two versions
		   exist. Folding the outside write INTO the live document is the
		   better answer and is the hub's to give, since it is the one place
		   the splice could be made exactly once — filed, not done. */
		copy := conflictPath(path, who, time.Now())
		s.putAs(ctx, who, project, copy, text, "")
		if head := reportedSha(w); head != "" {
			s.rooms().rebase(room, head)
		}
		log.Printf("bdrive: %s was written outside its co-editing room; the room's version is beside it as %s", path, copy)
	}
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
	mu    sync.Mutex
	docs  map[string]*crdt.Doc
	users map[string]User
	// bases is the blob each room last wrote or was seeded from: the If-Match
	// of its next snapshot. It is what turns "the hub is the only writer" from
	// a convention into a guarantee that an outside writer is never silently
	// overwritten.
	bases map[string]string
	// pending is a room's armed snapshot, and the wall-clock deadline it may
	// not be pushed past by more typing.
	pending map[string]*roomFlush
	unsubs  map[string]func()
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
	unsub := doc.OnUpdate(func(_ []byte, _ any) { s.rooms().touched(room, func() { s.snapshotRoom(context.Background(), room) }) })
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
	p.timer = time.AfterFunc(wait, func() {
		r.mu.Lock()
		delete(r.pending, room)
		r.mu.Unlock()
		flush()
	})
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
	delete(r.docs, room)
	delete(r.users, room)
	delete(r.bases, room)
}

// maxSeedBytes bounds what becomes a held document. Editing costs roughly ten
// times the content in CRDT items, so this is a memory ceiling rather than an
// opinion about file size.
const maxSeedBytes = 2 << 20
