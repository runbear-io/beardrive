package webapp

import (
	"context"
	"io"
	"net/http"
	"strings"

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
		srv := ygows.NewServerWithPersistence(fileSeed{s})
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
			return ygows.ConnectionConfig{
				ReadOnly: !atLeast(s.pathPerm(r, p, path), PermWrite),
			}, true
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

/*
fileSeed is what makes the seed CLAIM unnecessary.

	The relay could not seed: it never parsed a frame, so it had no way to turn
	a file into a document. Exactly one joiner was therefore told "you are
	first, build it from the bytes you loaded" — with a grace timer, because a
	claim that never produced anything would leave every later joiner holding a
	blank document that the source editor would then cheerfully save over the
	file.

	The hub can just do it. LoadDoc runs once, when the room is created and
	before any client is attached, so there is nothing to claim and nothing to
	race: the document starts as the file, deterministically, every time.

	StoreUpdate is Stage 3's seam — snapshotting the document back to the file
	is what finally retires "whoever stops typing last writes it", and until
	then the client still saves through upload/content exactly as it does now.
	Returning nil is not a stub that forgot to be written; it is this stage
	declining to own the write path yet.
*/
type fileSeed struct{ s *Server }

func (f fileSeed) LoadDoc(room string) ([]byte, error) {
	project, path, ok := strings.Cut(room, "/")
	if !ok {
		return nil, nil
	}
	_, v, err := f.s.projectVolume(project)
	if err != nil {
		return nil, nil // no such project: an empty document, not an error
	}
	ctx := context.Background()
	snap, err := v.snapshot(ctx)
	if err != nil {
		return nil, nil
	}
	fi, ok := snap.files[path]
	if !ok {
		return nil, nil // a file that does not exist yet starts empty
	}
	rc, err := v.source.Open(ctx, path, fi)
	if err != nil {
		return nil, nil
	}
	defer rc.Close()
	// Bounded: a document is held in memory for as long as somebody has it
	// open, and a 500 MB file is not something to seed a CRDT with.
	body, err := io.ReadAll(io.LimitReader(rc, maxSeedBytes))
	if err != nil {
		return nil, nil
	}
	doc := crdt.New()
	txt := doc.GetText("body")
	doc.Transact(func(txn *crdt.Transaction) {
		txt.Insert(txn, 0, string(body), nil)
	})
	return crdt.EncodeStateAsUpdateV1(doc, nil), nil
}

// StoreUpdate is Stage 3. See fileSeed.
func (f fileSeed) StoreUpdate(string, []byte) error { return nil }

// maxSeedBytes bounds what will be turned into a held document. Editing costs
// roughly ten times the content in CRDT items, so this is a memory ceiling
// rather than a file-size opinion; past it the editor falls back to the
// ordinary read/write path, which is what it does for any file it cannot
// render anyway.
const maxSeedBytes = 2 << 20
