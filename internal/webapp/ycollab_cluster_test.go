package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"

	"github.com/runbear-io/beardrive/internal/remote"
)

/* Two hub processes holding one document.

   max_instance_count = 1 never meant one process: a rollout keeps the old
   revision serving its open websockets beside the new one, and Cloud Run
   overshoots the cap under load. These pin what two processes sharing a
   storage root must do — seed a file once between them, let exactly one
   write it, pass that job on when the writer's room ends, and converge over
   a relay — so that raising the cap is a configuration change. */

// casBackend adds compare-and-swap to a file:// root, so two Servers in one
// test process share it the way two hub instances share a bucket.
type casBackend struct {
	remote.Backend
	mu  sync.Mutex
	ver map[string]int
	// beforePut runs once, inside the next PutIf, standing in for another
	// process writing in the window between a read and its write.
	beforePut func(key string)
}

func (b *casBackend) GetVersioned(ctx context.Context, key string) (io.ReadCloser, remote.Version, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rc, err := b.Backend.Get(ctx, key)
	if err != nil {
		return nil, remote.VersionAbsent, err
	}
	return rc, remote.Version(strconv.Itoa(b.ver[key])), nil
}

func (b *casBackend) PutIf(ctx context.Context, key string, r io.Reader, size int64, expect remote.Version) (remote.Version, error) {
	if hook := b.beforePut; hook != nil {
		b.beforePut = nil
		hook(key)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	cur := remote.VersionAbsent
	if ok, _ := b.Backend.Exists(ctx, key); ok {
		cur = remote.Version(strconv.Itoa(b.ver[key]))
	}
	if cur != expect {
		return remote.VersionAbsent, remote.ErrVersionMismatch
	}
	if err := b.Backend.Put(ctx, key, r, size); err != nil {
		return remote.VersionAbsent, err
	}
	b.ver[key]++
	return remote.Version(strconv.Itoa(b.ver[key])), nil
}

var coEditor = User{Email: "editor@example.com", Name: "An Editor"}

// newHubPair is two hub processes on one storage root and one project.
func newHubPair(t *testing.T) (a, b *Server, p Project) {
	t.Helper()
	fs, err := remote.Open(context.Background(), "file://"+t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	be := &casBackend{Backend: fs, ver: map[string]int{}}
	db, err := OpenProjectDB(filepath.Join(t.TempDir(), "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, _, err = db.GetOrCreate("proj", "")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(id string) *Server {
		s := &Server{
			Root: be, Projects: db, Device: Identity{ID: id, Name: id, Author: id + "@test"},
			Refresh: 0, Upload: UploadConfig{Enabled: true},
		}
		s.Handler()
		t.Cleanup(func() {
			s.Close(context.Background())
			s.rooms().settle()
		})
		return s
	}
	return mk("hub-a"), mk("hub-b"), p
}

func openVia(s *Server, room string) error {
	return s.ydocs().OnLoadDocument(context.Background(), room, crdt.New())
}

func TestCollabCloseWritesLiveRooms(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/file?path=notes.md"
	if rec := putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "before\n", ""); rec.Code != http.StatusOK {
		t.Fatalf("seed write: %d", rec.Code)
	}
	doc := openRoom(t, srv, p.ID+"/notes.md")
	typeInto(doc, "typed just before the deploy\n")
	if err := srv.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := get(t, h, url).Body.String(); got != "before\ntyped just before the deploy\n" {
		t.Fatalf("after Close the file says %q", got)
	}
}

func TestCollabSecondProcessRefusesARoomWithoutARelay(t *testing.T) {
	a, b, p := newHubPair(t)
	if rec := putContent(t, a.Handler(), "/api/p/"+p.ID+"/upload/content?path=notes.md", "base\n", ""); rec.Code != http.StatusOK {
		t.Fatalf("seed write: %d", rec.Code)
	}
	room := p.ID + "/notes.md"
	if err := openVia(a, room); err != nil {
		t.Fatal(err)
	}
	if err := openVia(b, room); !errors.Is(err, errRoomElsewhere) {
		t.Fatalf("a second process opened a held room: %v", err)
	}
	a.dropRoom(room)
	if err := openVia(b, room); err != nil {
		t.Fatalf("after the holder's room ended: %v", err)
	}
	b.dropRoom(room)
}

func TestCollabLeaseOutlivesADeadHolderOnlyByItsTTL(t *testing.T) {
	old := leaseTTL
	leaseTTL = 120 * time.Millisecond
	t.Cleanup(func() { leaseTTL = old })
	a, b, p := newHubPair(t)
	if rec := putContent(t, a.Handler(), "/api/p/"+p.ID+"/upload/content?path=notes.md", "base\n", ""); rec.Code != http.StatusOK {
		t.Fatalf("seed write: %d", rec.Code)
	}
	room := p.ID + "/notes.md"
	if err := openVia(a, room); err != nil {
		t.Fatal(err)
	}
	a.rooms().drop(room) // SIGKILLed: the room is gone, the lease is not released
	if err := openVia(b, room); !errors.Is(err, errRoomElsewhere) {
		t.Fatalf("a lease a dead process still holds was taken at once: %v", err)
	}
	time.Sleep(2 * leaseTTL)
	if err := openVia(b, room); err != nil {
		t.Fatalf("an expired lease was not taken: %v", err)
	}
	b.dropRoom(room)
}

// A seed is a set of items under a fresh client id, so two processes seeding
// one file from its bytes hold two documents that spell the same text — and
// the merge keeps both. The first to store ydoc/<sha> has seeded the file;
// the other must build from that.
func TestCollabSeedsAFileOnceAcrossProcesses(t *testing.T) {
	a, b, p := newHubPair(t)
	if rec := putContent(t, a.Handler(), "/api/p/"+p.ID+"/upload/content?path=notes.md", "base\n", ""); rec.Code != http.StatusOK {
		t.Fatalf("seed write: %d", rec.Code)
	}
	room := p.ID + "/notes.md"
	docA, docB := crdt.New(), crdt.New()
	be := a.Root.(*casBackend)
	// b seeds while a is between "no history yet" and claiming it.
	be.beforePut = func(key string) {
		if !strings.Contains(key, "/ydoc/") {
			t.Fatalf("first conditional write was %s, not the seed", key)
		}
		if err := b.seedDoc(room, docB); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.seedDoc(room, docA); err != nil {
		t.Fatal(err)
	}
	if err := docA.ApplyUpdate(docB.EncodeStateAsUpdate()); err != nil {
		t.Fatal(err)
	}
	if got := docA.GetText("body").ToString(); got != "base\n" {
		t.Fatalf("the two processes' seeds merged to %q", got)
	}
}

// A file taken back to an older version from outside, then reopened: the room
// must spell the file AND already know the edits the revert undid, so a relay
// replaying them (cluster/redis replays a room's retained log on activation)
// puts nothing back.
func TestCollabReopenedRoomKnowsTheEditsARevertUndid(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	room := p.ID + "/notes.md"
	write := func(text string) {
		t.Helper()
		if rec := putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", text, ""); rec.Code != http.StatusOK {
			t.Fatalf("write %q: %d %s", text, rec.Code, rec.Body.String())
		}
	}
	write("base\n")
	doc := openRoom(t, srv, room)
	typeInto(doc, "more\n")
	srv.snapshotRoom(context.Background(), room)
	if got := get(t, h, "/api/p/"+p.ID+"/file?path=notes.md").Body.String(); got != "base\nmore\n" {
		t.Fatalf("snapshot wrote %q", got)
	}
	later := doc.EncodeStateAsUpdate()
	srv.rooms().drop(room)

	write("base\n") // reverted from outside: this blob has a stored state from before "more"
	re := crdt.New()
	if err := srv.seedDoc(room, re); err != nil {
		t.Fatal(err)
	}
	if got := re.GetText("body").ToString(); got != "base\n" {
		t.Fatalf("reopened room spells %q, file says %q", got, "base\n")
	}
	if err := re.ApplyUpdate(later); err != nil {
		t.Fatal(err)
	}
	if got := re.GetText("body").ToString(); got != "base\n" {
		t.Fatalf("a replayed edit put the reverted text back: %q", got)
	}
}

// The comparison contains rests on: a document's own state diffed against its
// own state vector is the empty update, byte for byte.
func TestCollabEmptyDiffIsTheEmptyUpdate(t *testing.T) {
	doc := crdt.New()
	typeInto(doc, "some text")
	missing, err := crdt.DiffUpdateV1(doc.EncodeStateAsUpdate(), doc.StateVector())
	if err != nil {
		t.Fatal(err)
	}
	if string(missing) != string(emptyUpdate) {
		t.Fatalf("diff of a document against itself = %x, empty update = %x", missing, emptyUpdate)
	}
}

func TestCollabProcessesConvergeOverAMemRelay(t *testing.T) {
	a, b, p := newHubPair(t)
	relay := cluster.NewMemRelay()
	for _, s := range []*Server{a, b} {
		if err := s.useCollabRelay(relay); err != nil {
			t.Fatal(err)
		}
	}
	convergeAcross(t, a, b, p)
}

func TestCollabProcessesConvergeOverRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	a, b, p := newHubPair(t)
	for _, s := range []*Server{a, b} {
		s.Collab.Redis = mr.Addr()
		if err := s.ConnectCollab(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	convergeAcross(t, a, b, p)
}

// convergeAcross is one file edited from two processes: every edit reaches
// the other process, one process writes the file, and when its room ends the
// other takes the pen and builds on what was written instead of parking a
// conflict copy beside it.
func convergeAcross(t *testing.T, a, b *Server, p Project) {
	t.Helper()
	fastRoom(t, 50*time.Millisecond, 500*time.Millisecond)
	h := a.Handler()
	if rec := putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "base\n", ""); rec.Code != http.StatusOK {
		t.Fatalf("seed write: %d", rec.Code)
	}
	room := p.ID + "/notes.md"
	url := "/api/p/" + p.ID + "/file?path=notes.md"
	for _, s := range []*Server{a, b} {
		s.rooms().writer(room, coEditor)
	}
	typeVia := func(s *Server, text string) {
		t.Helper()
		err := s.ydocs().Apply(context.Background(), room, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := doc.GetText("body")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, txt.Len(), text, nil) })
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	textOf := func(s *Server) string {
		doc := s.ydocs().GetDoc(room)
		if doc == nil {
			return ""
		}
		return doc.GetText("body").ToString()
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s: a=%q b=%q file=%q", what, textOf(a), textOf(b), get(t, h, url).Body.String())
	}

	typeVia(a, "from A\n")
	waitFor("A writing the file", func() bool { return get(t, h, url).Body.String() == "base\nfrom A\n" })
	// B opens the room mid-session: built from what A stored, caught up by
	// the relay. (The Redis relay subscribes a node only to the rooms it
	// hosts, so nothing reaches B before this.)
	typeVia(b, "")
	waitFor("A's edit reaching B", func() bool { return textOf(b) == "base\nfrom A\n" })

	typeVia(b, "from B\n")
	waitFor("B's edit reaching A", func() bool { return textOf(a) == "base\nfrom A\nfrom B\n" })
	waitFor("A writing B's edit", func() bool { return get(t, h, url).Body.String() == "base\nfrom A\nfrom B\n" })

	// A's room ends. B must take the pen and build on A's last write.
	if err := a.ydocs().CloseRoom(room, true); err != nil {
		t.Fatal(err)
	}
	typeVia(b, "after A left\n")
	waitFor("B writing after taking over", func() bool { return get(t, h, url).Body.String() == "base\nfrom A\nfrom B\nafter A left\n" })

	// B's edit also reaches A once A's room is rebuilt — from the stored
	// state, so nothing is doubled.
	typeVia(a, "A is back\n")
	waitFor("A rebuilt and converged", func() bool {
		return textOf(a) == "base\nfrom A\nfrom B\nafter A left\nA is back\n" && textOf(b) == textOf(a)
	})

	var feed struct {
		Entries []struct{ Path string } `json:"entries"`
	}
	if err := json.Unmarshal(get(t, h, "/api/p/"+p.ID+"/history?prefix=&n=100").Body.Bytes(), &feed); err != nil {
		t.Fatal(err)
	}
	for _, e := range feed.Entries {
		if strings.Contains(e.Path, ".bdrive-conflict-") {
			t.Fatalf("converged editing still parked %s", e.Path)
		}
	}
	// Torn down here, before fastRoom's cleanup restores the timings: a
	// snapshot timer or lease renewer still running would read them racing
	// that write.
	for _, s := range []*Server{a, b} {
		s.Close(context.Background())
		s.rooms().settle()
	}
}
