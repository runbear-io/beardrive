package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/runbear-io/beardrive/internal/remote"
)

/* Review comments: a person leaves feedback on a file while reading it in the
   viewer, and an agent sweeps the open ones with `bdrive comments`.

   Storage is the project's own object store, under comments/ — hub-internal
   like ydoc/, never synced to devices, but charged to the org's storage like
   any write. Each change to a thread (open, reply, resolve, reopen) is its
   own immutable object:

       comments/<thread>/<unix-nanos>-<rand>.json

   so a thread is the fold of its events and no object ever has two writers.
   That is the property journals rely on, for the same reason: two hub
   processes answering two people at once cannot lose either write, and there
   is no read-modify-write to race. Immutable objects also make the cache
   trivial — an event read once is never read again.

   Deleting a thread writes comments/<thread>/deleted.json. The backend has no
   delete, so the tombstone is what retires a thread: the listing alone says
   which threads are gone, so their events are never fetched, never cached
   and never counted against the limits below — deleting gives room back.

   A comment is anchored by what the reader SAW: the quoted text plus a little
   context either side (the W3C TextQuoteSelector shape), not an offset into
   the source. The viewer renders markdown and HTML, so the source offset is
   not something the browser knows, and an offset would go stale with the
   first edit anyway. The quote is also exactly what an agent needs: it greps
   the file for it.

   Nothing here decides who may READ the project; proj() has. Every handler
   still filters by path, because a folder rule can hide one file's comments
   from a member who can see the rest of the project. */

const (
	commentsPrefix = "comments/"
	// maxCommentBody bounds one message. Review feedback, not a document.
	maxCommentBody = 4000
	maxQuote       = 1000
	maxQuoteCtx    = 100
	maxElement     = 300
	// A read member can comment, and every comment is a stored object, so the
	// number of events is bounded twice: per author, so one account cannot
	// crowd everyone else out of a project, and per project, so the listing
	// a viewer folds on every request stays a size worth folding.
	//
	// commentCacheBudget bounds the bytes of decoded events this process
	// keeps across all projects. Past it the least recently read project's
	// events are dropped and re-read from storage next time.
	commentCacheBudget = 64 << 20
	// maxCommentEvent bounds what a stored event may cost to read back, so a
	// corrupt or foreign object under comments/ cannot balloon a listing.
	maxCommentEvent = 64 << 10
	// commentListTTL is how long one process trusts its own listing of
	// comments/. A write through THIS process drops it at once; the window
	// only delays a write made through another hub process, and keeps a file
	// backend from walking the whole project root on every viewer poll.
	commentListTTL = 3 * time.Second
	// commentFetchers bounds concurrent reads when a listing names events this
	// process has not seen yet — a cold start against an object store.
	commentFetchers = 8
)

// Both caps count the events of LIVE threads only; see the tombstone above.
// Variables so a test can reach them without writing thousands of objects.
//
// ponytail: fixed caps, not a plan knob. Real review traffic is orders of
// magnitude below them; a hub that hits one wants a config field.
var (
	maxCommentEventsPerAuthor  = 2000
	maxCommentEventsPerProject = 20000
)

var (
	commentKeyRe    = regexp.MustCompile(`^comments/([0-9a-f]{12})/([0-9]{19}-[0-9a-f]{8})\.json$`)
	tombstoneKeyRe  = regexp.MustCompile(`^comments/([0-9a-f]{12})/deleted\.json$`)
	commentThreadRe = regexp.MustCompile(`^[0-9a-f]{12}$`)
	// elementPathRe is the only shape an element anchor may take: what
	// cssPath in lib/anchor.ts builds. Anything else — prose, an instruction
	// to whoever reads the comment — is dropped, because an element path is
	// shown to a reviewer as code and read by an agent as a location.
	elementPathRe = regexp.MustCompile(`^[a-z][a-z0-9-]*(#[A-Za-z][\w-]*|:nth-of-type\([0-9]{1,4}\))?( > [a-z][a-z0-9-]*(#[A-Za-z][\w-]*|:nth-of-type\([0-9]{1,4}\))?){0,11}$`)
	shaRe         = regexp.MustCompile(`^[0-9a-f]{64}$`)

	errCommentsOff = errors.New("comments are not available on this server")
)

// CommentAnchor is where on the page a thread was started. Every field is
// optional: a thread with no anchor is about the whole file.
type CommentAnchor struct {
	// Quote is the text the reader selected, as rendered.
	Quote string `json:"quote,omitempty"`
	// Prefix and Suffix are the rendered text just before and after Quote,
	// for telling apart two places that say the same thing.
	Prefix string `json:"prefix,omitempty"`
	Suffix string `json:"suffix,omitempty"`
	// Line is the 1-based source line, when the viewer showed the source
	// itself (plain text, code) and therefore knew it.
	Line int `json:"line,omitempty"`
	// Element describes a picked element on a rendered HTML page, as a CSS
	// selector path. Set for a pin on something with no text to quote.
	Element string `json:"element,omitempty"`
}

type CommentMessage struct {
	ID     string    `json:"id"`
	Author string    `json:"author,omitempty"`
	Name   string    `json:"name,omitempty"`
	Body   string    `json:"body"`
	Time   time.Time `json:"time"`
}

type CommentThread struct {
	ID string `json:"id"`
	// Path is where the file is NOW. A thread follows its file across renames
	// the way a share link does (resolveShare), so feedback left before a
	// move is still found on the moved file.
	Path         string           `json:"path"`
	Anchor       CommentAnchor    `json:"anchor"`
	Sha          string           `json:"sha,omitempty"` // the file version commented on
	Author       string           `json:"author,omitempty"`
	Name         string           `json:"name,omitempty"`
	Created      time.Time        `json:"created"`
	Updated      time.Time        `json:"updated"`
	Messages     []CommentMessage `json:"messages"`
	Resolved     bool             `json:"resolved"`
	ResolvedBy   string           `json:"resolved_by,omitempty"`
	ResolvedName string           `json:"resolved_name,omitempty"`
	ResolvedAt   *time.Time       `json:"resolved_at,omitempty"`
	// Outdated: the file has changed since the comment was left, so the
	// quote may already be gone. Missing: the file itself is gone.
	Outdated bool `json:"outdated,omitempty"`
	Missing  bool `json:"missing,omitempty"`

	origPath string
}

// commentEvent is one stored object. Anchor and Sha are set on "open" only;
// Body on open and reply, and optionally on resolve/reopen (a note saying
// what was done). Path is authoritative on "open"; on the others it is the
// file's path at the time, carried for the change notification alone.
type commentEvent struct {
	Kind   string         `json:"kind"` // open | reply | resolve | reopen
	Thread string         `json:"thread"`
	Path   string         `json:"path,omitempty"`
	Anchor *CommentAnchor `json:"anchor,omitempty"`
	Sha    string         `json:"sha,omitempty"`
	Body   string         `json:"body,omitempty"`
	Author string         `json:"author,omitempty"`
	Name   string         `json:"name,omitempty"`
	Time   time.Time      `json:"time"`

	key  string
	size int // stored bytes, for the cache budget
}

// commentCache holds the events this process has read, per project. Events
// are immutable, so an entry is never stale — only the listing of which
// events exist expires, and a whole project is dropped when the cache is
// over its byte budget.
type commentCache struct {
	mu       sync.Mutex
	projects map[string]*projectComments
	bytes    int
	// One lock per project serializes this process's writers, so a reply
	// and a resolve sent together still land in the order they were sent.
	locks sync.Map
}

type projectComments struct {
	events   map[string]commentEvent // by object key
	bytes    int
	used     time.Time
	keys     []string
	listedAt time.Time
}

func newCommentCache() *commentCache {
	return &commentCache{projects: map[string]*projectComments{}}
}

func (s *Server) commentStore() *commentCache {
	s.commentsOnce.Do(func() { s.comments = newCommentCache() })
	return s.comments
}

// project returns a project's entry, creating it. Callers hold c.mu.
func (c *commentCache) project(id string) *projectComments {
	pc := c.projects[id]
	if pc == nil {
		pc = &projectComments{events: map[string]commentEvent{}}
		c.projects[id] = pc
	}
	pc.used = time.Now()
	return pc
}

// add caches events for a project, then evicts least recently used OTHER
// projects until the cache fits its budget. Callers hold c.mu.
func (c *commentCache) add(id string, evs ...commentEvent) {
	pc := c.project(id)
	for _, ev := range evs {
		if _, ok := pc.events[ev.key]; ok {
			continue
		}
		pc.events[ev.key] = ev
		pc.bytes += ev.size
		c.bytes += ev.size
	}
	for c.bytes > commentCacheBudget {
		oldest := ""
		for k, p := range c.projects {
			if k != id && (oldest == "" || p.used.Before(c.projects[oldest].used)) {
				oldest = k
			}
		}
		if oldest == "" {
			return // one project alone is over budget; it is still the one being read
		}
		c.bytes -= c.projects[oldest].bytes
		delete(c.projects, oldest)
	}
}

func (c *commentCache) lock(project string) *sync.Mutex {
	m, _ := c.locks.LoadOrStore(project, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// commentsAvailable reports whether this server can hold comments at all:
// hub mode, where every project has an object store of its own. The
// single-folder viewer and the desktop sidecar have nowhere to put them.
func (s *Server) commentsAvailable() bool { return s.Root != nil && !s.Desktop }

// loadThreads folds every stored event for a project into threads, deleted
// ones dropped. Path is still the path the thread was opened on; callers
// resolve it against a snapshot (placeThreads).
func (s *Server) loadThreads(ctx context.Context, project string) (map[string]*CommentThread, error) {
	evs, err := s.loadCommentEvents(ctx, project)
	if err != nil {
		return nil, err
	}
	return foldComments(evs), nil
}

func (s *Server) loadCommentEvents(ctx context.Context, project string) ([]commentEvent, error) {
	if !s.commentsAvailable() {
		return nil, errCommentsOff
	}
	be, ok := s.projectBackend(project)
	if !ok {
		return nil, errCommentsOff
	}
	c := s.commentStore()
	keys, err := c.list(ctx, project, be)
	if err != nil {
		return nil, err
	}
	return c.fetch(ctx, project, be, keys)
}

func (c *commentCache) list(ctx context.Context, project string, be remote.Backend) ([]string, error) {
	c.mu.Lock()
	pc := c.project(project)
	keys, at := pc.keys, pc.listedAt
	c.mu.Unlock()
	if !at.IsZero() && time.Since(at) < commentListTTL {
		return keys, nil
	}
	objs, err := be.List(ctx, commentsPrefix)
	if err != nil {
		return nil, err
	}
	dead := map[string]bool{}
	for _, o := range objs {
		if m := tombstoneKeyRe.FindStringSubmatch(o.Key); m != nil {
			dead[m[1]] = true
		}
	}
	keys = make([]string, 0, len(objs))
	for _, o := range objs {
		if m := commentKeyRe.FindStringSubmatch(o.Key); m != nil && !dead[m[1]] {
			keys = append(keys, o.Key)
		}
	}
	c.mu.Lock()
	pc = c.project(project)
	pc.keys, pc.listedAt = keys, time.Now()
	for k, ev := range pc.events {
		if dead[ev.Thread] {
			delete(pc.events, k)
			pc.bytes -= ev.size
			c.bytes -= ev.size
		}
	}
	c.mu.Unlock()
	return keys, nil
}

// fetch returns the events behind keys, reading only the ones this process
// has never seen. An unreadable object is skipped rather than failing the
// listing: one damaged comment must not hide every other one.
func (c *commentCache) fetch(ctx context.Context, project string, be remote.Backend, keys []string) ([]commentEvent, error) {
	out := make([]commentEvent, 0, len(keys))
	var missing []string
	c.mu.Lock()
	pc := c.project(project)
	for _, k := range keys {
		if ev, ok := pc.events[k]; ok {
			out = append(out, ev)
		} else {
			missing = append(missing, k)
		}
	}
	c.mu.Unlock()
	if len(missing) == 0 {
		return out, nil
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		sem  = make(chan struct{}, commentFetchers)
		read []commentEvent
	)
	for _, k := range missing {
		wg.Add(1)
		sem <- struct{}{}
		go func(k string) {
			defer wg.Done()
			defer func() { <-sem }()
			ev, err := readCommentEvent(ctx, be, k)
			if err != nil {
				if !remote.IsNotExist(err) {
					log.Printf("beardrive: reading comment %s/%s: %v", project, k, err)
				}
				return
			}
			mu.Lock()
			read = append(read, ev)
			mu.Unlock()
		}(k)
	}
	wg.Wait()
	c.mu.Lock()
	c.add(project, read...)
	c.mu.Unlock()
	return append(out, read...), ctx.Err()
}

func readCommentEvent(ctx context.Context, be remote.Backend, key string) (commentEvent, error) {
	rc, err := be.Get(ctx, key)
	if err != nil {
		return commentEvent{}, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxCommentEvent+1))
	if err != nil {
		return commentEvent{}, err
	}
	if len(data) > maxCommentEvent {
		return commentEvent{}, fmt.Errorf("comment event too large")
	}
	var ev commentEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return commentEvent{}, err
	}
	// The key is the authority on which thread an event belongs to; a body
	// that disagrees with it was not written by this code.
	m := commentKeyRe.FindStringSubmatch(key)
	if m == nil || ev.Thread != m[1] {
		return commentEvent{}, fmt.Errorf("comment event does not match its key")
	}
	ev.key, ev.size = key, len(data)
	return ev, nil
}

// foldComments replays events into threads. Order within a thread is (time,
// key): the key's nanosecond stamp breaks ties between events written in the
// same instant, and its random tail makes the order total.
func foldComments(evs []commentEvent) map[string]*CommentThread {
	sort.Slice(evs, func(i, j int) bool {
		if !evs[i].Time.Equal(evs[j].Time) {
			return evs[i].Time.Before(evs[j].Time)
		}
		return evs[i].key < evs[j].key
	})
	threads := map[string]*CommentThread{}
	for _, ev := range evs {
		t := threads[ev.Thread]
		if ev.Kind == "open" {
			if t != nil || ev.Path == "" {
				continue // a second open for one id is not something this code writes
			}
			t = &CommentThread{
				ID: ev.Thread, Path: ev.Path, origPath: ev.Path, Sha: ev.Sha,
				Author: ev.Author, Name: ev.Name, Created: ev.Time, Updated: ev.Time,
			}
			if ev.Anchor != nil {
				t.Anchor = *ev.Anchor
			}
			t.Messages = []CommentMessage{eventMessage(ev)}
			threads[ev.Thread] = t
			continue
		}
		if t == nil {
			continue // an event for a thread whose open we never saw
		}
		t.Updated = ev.Time
		switch ev.Kind {
		case "reply":
			t.Messages = append(t.Messages, eventMessage(ev))
		case "resolve", "reopen":
			if ev.Body != "" {
				t.Messages = append(t.Messages, eventMessage(ev))
			}
			t.Resolved = ev.Kind == "resolve"
			if t.Resolved {
				at := ev.Time
				t.ResolvedBy, t.ResolvedName, t.ResolvedAt = ev.Author, ev.Name, &at
			} else {
				t.ResolvedBy, t.ResolvedName, t.ResolvedAt = "", "", nil
			}
		}
	}
	return threads
}

func eventMessage(ev commentEvent) CommentMessage {
	return CommentMessage{
		ID:     strings.TrimSuffix(ev.key[strings.LastIndexByte(ev.key, '/')+1:], ".json"),
		Author: ev.Author, Name: ev.Name, Body: ev.Body, Time: ev.Time,
	}
}

// placeThreads resolves each thread against the current file listing — where
// its file lives now, whether it changed since, whether it is gone — and
// drops the ones this caller may not see: every path the file has held since
// the comment must be readable. A comment quotes its file, and neither a move
// out of a hidden folder nor a delete inside one makes what was said about it
// there public.
func placeThreads(threads map[string]*CommentThread, snap *snapshot, f pathFilter) []*CommentThread {
	out := make([]*CommentThread, 0, len(threads))
	for _, t := range threads {
		// Always followed, never "the old path still has a file": a file that
		// moved or was deleted after the comment is not the one now sitting
		// at its address. And EVERY path the file has held must be readable,
		// gone or not — a reply written while it sat in a restricted folder
		// does not become public because the file was then deleted there.
		last, chain, live := followFile(snap.moves, snap.files, t.origPath, t.Created)
		t.Path, t.Missing = last, !live
		if live {
			t.Outdated = t.Sha != "" && snap.files[last].Blob != t.Sha
		}
		visible := true
		for _, p := range chain {
			visible = visible && f.canRead(p)
		}
		if !visible {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Created.Before(out[j].Created)
	})
	return out
}

// errCommentLimit is a write refused by a cap or by the plan; the message is
// shown to the person commenting.
type errCommentLimit struct{ msg string }

func (e errCommentLimit) Error() string { return e.msg }

// writeCommentEvent stores one event and makes it visible to this process at
// once — the listing cache would otherwise hide a person's own comment from
// them for up to commentListTTL.
//
// A comment is stored bytes in the org's storage like any other write, so it
// is checked against the plan and recorded as usage. ydoc/ is exempt because
// the hub writes it on its own, once per blob; these are written on request
// by anyone who can read the project.
func (s *Server) writeCommentEvent(ctx context.Context, project string, ev commentEvent) error {
	be, ok := s.projectBackend(project)
	if !ok {
		return errCommentsOff
	}
	evs, err := s.loadCommentEvents(ctx, project)
	if err != nil {
		return err
	}
	if len(evs) >= maxCommentEventsPerProject {
		return errCommentLimit{"this project has reached its limit of comments"}
	}
	mine := 0
	for _, e := range evs {
		if e.Author == ev.Author {
			mine++
		}
	}
	if mine >= maxCommentEventsPerAuthor {
		return errCommentLimit{"you have reached the limit of comments in this project"}
	}
	ev.key = fmt.Sprintf("%s%s/%019d-%s.json", commentsPrefix, ev.Thread, ev.Time.UnixNano(), randHex(4))
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	ev.size = len(data)
	// Against what is already promised to presigned uploads too, the same
	// boundary every other write door checks (reserve.go).
	org := s.orgOf(project)
	s.reconcileGrants(ctx, project, be)
	if err := s.quota().CheckWrite(org, int64(len(data))+s.reservedBytes(org)); err != nil {
		return errCommentLimit{err.Error()}
	}
	if err := be.Put(ctx, ev.key, bytes.NewReader(data), int64(len(data))); err != nil {
		return err
	}
	s.quota().RecordUsage(org, int64(len(data)))
	c := s.commentStore()
	c.mu.Lock()
	c.add(project, ev)
	c.project(project).listedAt = time.Time{}
	c.mu.Unlock()
	// No paths: the stream has no per-reader filter, and a path under a
	// folder hidden from some subscriber must not reach them. A viewer only
	// needs to know to refetch its comment list, which is filtered.
	s.events().publish(project, changeEvent{Type: "comments"})
	return nil
}

// cleanCommentText keeps what a person typed, minus what renders as nothing:
// C0/C1 controls other than newline and tab, and every format character
// (category Cf, plus the tag block). Comments are read by agents, and text a
// reviewer cannot see in the UI must not reach an agent as instruction —
// trimText's rule, with line breaks kept because feedback has paragraphs.
func cleanCommentText(s string, max int) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	n := 0
	for _, r := range s {
		switch {
		case r == '\n', r == '\t':
		case r == '\r', r == 0x2028, r == 0x2029:
			r = '\n'
		case unicode.IsControl(r):
			continue
		case unicode.Is(unicode.Cf, r), r >= 0xe0000 && r <= 0xe01ef:
			continue
		case r == utf8.RuneError:
			continue
		}
		if n >= max {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// cleanQuote is cleanCommentText for anchor text, which is matched against
// the file rather than read: whitespace inside it is kept as it was, only the
// ends are not trimmed away from the context fields.
func cleanQuote(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			continue
		}
		if unicode.Is(unicode.Cf, r) || (r >= 0xe0000 && r <= 0xe01ef) || r == utf8.RuneError {
			continue
		}
		if n >= max {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

func cleanAnchor(a CommentAnchor) CommentAnchor {
	out := CommentAnchor{
		Quote:  strings.TrimSpace(cleanQuote(a.Quote, maxQuote)),
		Prefix: cleanQuote(a.Prefix, maxQuoteCtx),
		Suffix: cleanQuote(a.Suffix, maxQuoteCtx),
	}
	if el := strings.TrimSpace(a.Element); len(el) <= maxElement && elementPathRe.MatchString(el) {
		out.Element = el
	}
	if a.Line > 0 && a.Line < 1<<30 {
		out.Line = a.Line
	}
	if out.Quote == "" {
		out.Prefix, out.Suffix = "", ""
	}
	return out
}

// commentActor is who a write is attributed to. A hub always has auth; the
// auth-less fallback exists for fixtures and is attributed to nobody.
func (s *Server) commentActor(r *http.Request) (email, name string) {
	u := s.requestUser(r)
	name = u.Name
	if name == "" {
		name = u.Email
	}
	return normEmail(u.Email), name
}

// ---- HTTP ----

func commentsErr(w http.ResponseWriter, err error) {
	if errors.Is(err, errCommentsOff) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	var limit errCommentLimit
	if errors.As(err, &limit) {
		http.Error(w, limit.msg, http.StatusForbidden)
		return
	}
	storageErr(w, http.StatusBadGateway, "comments are temporarily unavailable", err)
}

// handleCommentList: GET /api/p/<id>/comments?path=|prefix=&status=open|resolved|all
//
// status defaults to open — the sweep an agent runs is "what is still asking
// for something".
func (s *Server) handleCommentList(v *volume, w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := q.Get("status")
	switch status {
	case "":
		status = "open"
	case "open", "resolved", "all":
	default:
		http.Error(w, "status must be open, resolved or all", http.StatusBadRequest)
		return
	}
	path, prefix := q.Get("path"), strings.TrimPrefix(q.Get("prefix"), "/")
	threads, err := s.loadThreads(r.Context(), r.PathValue("project"))
	if err != nil {
		commentsErr(w, err)
		return
	}
	snap, err := v.snapshot(r.Context())
	if err != nil {
		storageErr(w, http.StatusBadGateway, "content temporarily unavailable", err)
		return
	}
	out := []*CommentThread{}
	for _, t := range placeThreads(threads, snap, s.visibility(r)) {
		if path != "" && t.Path != path {
			continue
		}
		if prefix != "" && !strings.HasPrefix(t.Path, strings.TrimSuffix(prefix, "/")+"/") && t.Path != prefix {
			continue
		}
		if (status == "open" && t.Resolved) || (status == "resolved" && !t.Resolved) {
			continue
		}
		out = append(out, t)
	}
	writeJSON(w, map[string]any{"comments": out})
}

// handleCommentCreate: POST /api/p/<id>/comments {path, body, anchor?, sha?}
//
// Read access is enough to comment. Review feedback mostly comes from the
// people who read a document rather than the ones who write it, and a
// comment changes nothing but the comment list: whoever acts on it does so
// with their own write access.
func (s *Server) handleCommentCreate(v *volume, w http.ResponseWriter, r *http.Request) {
	if !s.commentsAvailable() {
		http.Error(w, errCommentsOff.Error(), http.StatusNotFound)
		return
	}
	var req struct {
		Path   string        `json:"path"`
		Body   string        `json:"body"`
		Anchor CommentAnchor `json:"anchor"`
		Sha    string        `json:"sha"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxCommentEvent)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	p, err := cleanUploadPath(req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	body := cleanCommentText(req.Body, maxCommentBody)
	if body == "" {
		http.Error(w, "a comment needs some text", http.StatusBadRequest)
		return
	}
	// 404 for a hidden path, like every read door: a 403 would confirm the
	// file is there.
	if !s.visibility(r).canRead(p) {
		http.Error(w, "no such file", http.StatusNotFound)
		return
	}
	snap, err := v.snapshot(r.Context())
	if err != nil {
		storageErr(w, http.StatusBadGateway, "content temporarily unavailable", err)
		return
	}
	fi, ok := snap.files[p]
	if !ok {
		http.Error(w, "no such file", http.StatusNotFound)
		return
	}
	// The version the reader was looking at, when they say and it is one;
	// otherwise the current one. Only ever used to say "this file changed
	// since", so a wrong claim costs the claimant a wrong hint and nothing else.
	sha := fi.Blob
	if shaRe.MatchString(req.Sha) {
		sha = req.Sha
	}
	anchor := cleanAnchor(req.Anchor)
	email, name := s.commentActor(r)
	ev := commentEvent{
		Kind: "open", Thread: randHex(6), Path: p, Sha: sha, Body: body,
		Author: email, Name: name, Time: time.Now().UTC(),
	}
	if anchor != (CommentAnchor{}) {
		ev.Anchor = &anchor
	}
	project := r.PathValue("project")
	mu := s.commentStore().lock(project)
	mu.Lock()
	err = s.writeCommentEvent(r.Context(), project, ev)
	mu.Unlock()
	if err != nil {
		commentsErr(w, err)
		return
	}
	s.answerThread(v, w, r, ev.Thread)
}

// handleCommentUpdate: POST /api/p/<id>/comments/<thread> {action, body?}
//
// action is reply, resolve or reopen. Resolving and reopening take an
// optional note — what was changed, or why it is not done — which joins the
// thread as a message. Repeating a resolve on a resolved thread with no note
// writes nothing: an agent re-running its sweep must not stack events.
func (s *Server) handleCommentUpdate(v *volume, w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
		Body   string `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxCommentEvent)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	body := cleanCommentText(req.Body, maxCommentBody)
	switch req.Action {
	case "reply":
		if body == "" {
			http.Error(w, "a reply needs some text", http.StatusBadRequest)
			return
		}
	case "resolve", "reopen":
	default:
		http.Error(w, "action must be reply, resolve or reopen", http.StatusBadRequest)
		return
	}
	project := r.PathValue("project")
	mu := s.commentStore().lock(project)
	mu.Lock()
	t, ok := s.visibleThread(v, w, r)
	if !ok {
		mu.Unlock()
		return
	}
	if body == "" && ((req.Action == "resolve" && t.Resolved) || (req.Action == "reopen" && !t.Resolved)) {
		mu.Unlock()
		s.answerThread(v, w, r, t.ID)
		return
	}
	email, name := s.commentActor(r)
	err := s.writeCommentEvent(r.Context(), project, commentEvent{
		Kind: req.Action, Thread: t.ID, Path: t.Path, Body: body,
		Author: email, Name: name, Time: s.nextCommentTime(t),
	})
	mu.Unlock()
	if err != nil {
		commentsErr(w, err)
		return
	}
	s.answerThread(v, w, r, t.ID)
}

// handleCommentDelete: DELETE /api/p/<id>/comments/<thread>. The thread's
// author or a project admin; resolving is how everyone else closes one.
func (s *Server) handleCommentDelete(v *volume, w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	mu := s.commentStore().lock(project)
	mu.Lock()
	defer mu.Unlock()
	t, ok := s.visibleThread(v, w, r)
	if !ok {
		return
	}
	email, name := s.commentActor(r)
	isAdmin := false
	if p, ok := projectFromCtx(r); ok {
		isAdmin = s.projectPermOf(r, p) == PermAdmin
	}
	if !isAdmin && (email == "" || email != t.Author) {
		http.Error(w, "only the comment's author or a project admin can delete it", http.StatusForbidden)
		return
	}
	if err := s.retireThread(r.Context(), project, t.ID, email, name); err != nil {
		commentsErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// retireThread writes a thread's tombstone and drops its events from this
// process. Never refused by the plan or the caps: deleting is how a crowded
// project gets room back. The tombstone is a few dozen bytes, recorded as
// usage like any write.
func (s *Server) retireThread(ctx context.Context, project, thread, email, name string) error {
	be, ok := s.projectBackend(project)
	if !ok {
		return errCommentsOff
	}
	data, err := json.Marshal(map[string]any{"author": email, "name": name, "time": time.Now().UTC()})
	if err != nil {
		return err
	}
	if err := be.Put(ctx, commentsPrefix+thread+"/deleted.json", bytes.NewReader(data), int64(len(data))); err != nil {
		return err
	}
	s.quota().RecordUsage(s.orgOf(project), int64(len(data)))
	c := s.commentStore()
	c.mu.Lock()
	pc := c.project(project)
	for k, ev := range pc.events {
		if ev.Thread == thread {
			delete(pc.events, k)
			pc.bytes -= ev.size
			c.bytes -= ev.size
		}
	}
	pc.listedAt = time.Time{}
	c.mu.Unlock()
	s.events().publish(project, changeEvent{Type: "comments"})
	return nil
}

// nextCommentTime is now, nudged past the thread's last event: fold orders by
// time, and a hub clock that stepped backwards must not land a resolve
// before the reply it follows.
func (s *Server) nextCommentTime(t *CommentThread) time.Time {
	now := time.Now().UTC()
	if !now.After(t.Updated) {
		now = t.Updated.Add(time.Microsecond)
	}
	return now
}

// visibleThread finds the {thread} in the URL among the threads this caller
// may see, answering 404 itself when it is not one of them — absent and
// hidden look the same from outside.
func (s *Server) visibleThread(v *volume, w http.ResponseWriter, r *http.Request) (*CommentThread, bool) {
	id := r.PathValue("thread")
	if !commentThreadRe.MatchString(id) {
		http.Error(w, "no such comment", http.StatusNotFound)
		return nil, false
	}
	threads, err := s.loadThreads(r.Context(), r.PathValue("project"))
	if err != nil {
		commentsErr(w, err)
		return nil, false
	}
	snap, err := v.snapshot(r.Context())
	if err != nil {
		storageErr(w, http.StatusBadGateway, "content temporarily unavailable", err)
		return nil, false
	}
	t, ok := threads[id]
	if !ok {
		http.Error(w, "no such comment", http.StatusNotFound)
		return nil, false
	}
	placed := placeThreads(map[string]*CommentThread{id: t}, snap, s.visibility(r))
	if len(placed) == 0 {
		http.Error(w, "no such comment", http.StatusNotFound)
		return nil, false
	}
	return placed[0], true
}

func (s *Server) answerThread(v *volume, w http.ResponseWriter, r *http.Request, id string) {
	r.SetPathValue("thread", id)
	t, ok := s.visibleThread(v, w, r)
	if !ok {
		return
	}
	writeJSON(w, map[string]any{"comment": t})
}

// commentSuffix is appended to an HTML file served for commenting. Appended
// rather than spliced in, as printSuffix is: HTML's parser moves trailing
// content into the body, so the stored bytes stream through untouched and
// nothing has to be buffered or parsed.
//
// The bootstrap is a classic script at the static root for inline-edit.js's
// reasons (htmledit.go, editScriptURL): the frame is sandboxed without
// allow-same-origin, and a module load from an opaque origin would need CORS.
const commentSuffix = `<script src="/inline-comment.js"></script>`

// commentView reports whether this request asked for the commentable variant
// of an inline HTML render. text/html only, for printView's reason: trailing
// content after the root element is fatal to an XML parse.
func commentView(r *http.Request, ct string) bool {
	if r.URL.Query().Get("comment") != "1" {
		return false
	}
	ct = strings.ToLower(ct)
	return strings.HasPrefix(ct, "text/html")
}
