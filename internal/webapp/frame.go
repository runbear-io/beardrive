package webapp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/textmerge"
)

/*
Framed HTML and the files beside it.

A synced HTML page renders in an iframe under `sandbox allow-scripts` with no
allow-same-origin (sandboxInline), so its origin is opaque. Every request such
a document makes for its own images and stylesheets is cross-site as far as
cookies go: the browser withholds the SameSite session cookie, and the hub
answers 401. And /file names its target in ?path=, so a relative src cannot
reach a sibling to begin with — "a.png" resolves against /api/p/<id>/, not
against the page's folder.

So the frame is served from a path-shaped URL that carries its own
credential:

	/api/p/<project>/raw/<token>/<folder>/<page>.html

A relative src then resolves to /api/p/<project>/raw/<token>/<folder>/a.png by
ordinary URL rules, and the token stands in for the cookie the browser will
not send. It is minted for a signed-in reader (/frame) and names that reader,
the project and the page's FOLDER; it reads that folder's subtree and nothing
else, as that reader, through the same lookup and the same per-request
permission check as /file. Inline reads only: no download, no listing.

The page can read its own URL, and the page is whatever the project's writers
put there, so the token must be assumed to leak to them. What leaks is a
reader's read access to one folder for frameTokenTTL: files a writer of that
folder could mostly already see, except under a narrower folder rule beneath
it. The TTL is what bounds that.
*/

const frameTokenTTL = 4 * time.Hour

// frameKeyObject is the HMAC key every hub process signs frame tokens with.
// It lives at the storage root, outside every project prefix, so no project
// route can name it.
const frameKeyObject = "hub/frame-key"

// frameClaims is what a frame token carries. A read token (Write false) names
// a folder the raw route reads; an edit token (Write true) names one file the
// edit beacon may write. Neither ever serves as the other.
type frameClaims struct {
	Project string `json:"p"`
	Dir     string `json:"d"`           // "" is the project root (raw route)
	Path    string `json:"f,omitempty"` // the one file an edit token writes
	Write   bool   `json:"w,omitempty"` // an edit token
	UserID  string `json:"i"`
	Email   string `json:"e"`
	Name    string `json:"n,omitempty"`
	Expires int64  `json:"x"`
}

// frameRouteRe matches a raw-route URL path, in volume mode (/api/raw/...) and
// in hub mode (/api/p/<project>/raw/...).
var frameRouteRe = regexp.MustCompile(`^/api/(?:p/[^/]+/)?raw/([^/]+)/`)

// editFrameRouteRe matches the edit beacon's flush URL in either mode.
var editFrameRouteRe = regexp.MustCompile(`^/api/(?:p/[^/]+/)?ycollab/frame/([^/]+)$`)

// loadFrameKey returns the signing key, creating it on first use. A hub is
// several processes, and a token minted by one must verify on another, so the
// key is shared through storage rather than drawn per process. Volume mode and
// the desktop viewer are one process, and the desktop's Root is read-only.
//
// The create must be atomic: two processes that each write their own key and
// cache it would reject each other's tokens until restart. A root that offers
// no atomic create gets no key, and the viewer falls back to /file.
func (s *Server) loadFrameKey(ctx context.Context) ([]byte, error) {
	s.frameMu.Lock()
	defer s.frameMu.Unlock()
	if s.frameKey != nil {
		return s.frameKey, nil
	}
	if s.Root == nil || s.Desktop {
		k := make([]byte, 32)
		rand.Read(k)
		s.frameKey = k
		return k, nil
	}
	// Exists rather than IsNotExist on Get: S3 reports a missing key in its
	// own error type, which IsNotExist does not know.
	ok, err := s.Root.Exists(ctx, frameKeyObject)
	if err != nil {
		return nil, err
	}
	if !ok {
		k := make([]byte, 32)
		rand.Read(k)
		switch be := s.Root.(type) {
		case remote.ConditionalPutter:
			_, err = be.PutIf(ctx, frameKeyObject, bytes.NewReader(k), int64(len(k)), remote.VersionAbsent)
		case remote.CreateOnlyPutter:
			err = be.PutIfAbsent(ctx, frameKeyObject, bytes.NewReader(k), int64(len(k)))
		default:
			return nil, errors.New("storage root has no atomic create; frame tokens are off")
		}
		if errors.Is(err, remote.ErrVersionMismatch) {
			err = nil // another process created it first; read theirs
		}
		if err != nil {
			return nil, err
		}
	}
	// Read back even after writing: whichever process won is the key.
	rc, err := s.Root.Get(ctx, frameKeyObject)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	k, err := io.ReadAll(io.LimitReader(rc, 1024))
	if err != nil {
		return nil, err
	}
	if len(k) < 32 {
		return nil, fmt.Errorf("frame key at %s is %d bytes", frameKeyObject, len(k))
	}
	s.frameKey = k
	return k, nil
}

func frameMAC(key, payload []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(payload)
	return m.Sum(nil)
}

func (s *Server) signFrame(ctx context.Context, c frameClaims) (string, error) {
	key, err := s.loadFrameKey(ctx)
	if err != nil {
		return "", err
	}
	payload, _ := json.Marshal(c)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(frameMAC(key, payload)), nil
}

func (s *Server) verifyFrame(ctx context.Context, tok string) (frameClaims, bool) {
	var c frameClaims
	body, sig, ok := strings.Cut(tok, ".")
	if !ok {
		return c, false
	}
	enc := base64.RawURLEncoding
	payload, err1 := enc.DecodeString(body)
	mac, err2 := enc.DecodeString(sig)
	if err1 != nil || err2 != nil {
		return c, false
	}
	key, err := s.loadFrameKey(ctx)
	if err != nil || !hmac.Equal(mac, frameMAC(key, payload)) {
		return c, false
	}
	if json.Unmarshal(payload, &c) != nil || time.Now().Unix() > c.Expires {
		return c, false
	}
	return c, true
}

// frameUser is authGate's door for raw-route requests and the edit beacon,
// before the provider: the cookie is exactly what these requests do not
// carry. A read token opens only the raw route and an edit token only the
// beacon's POST. handled is false for every other path.
func (s *Server) frameUser(w http.ResponseWriter, r *http.Request) (u User, handled, ok bool) {
	write := false
	m := frameRouteRe.FindStringSubmatch(r.URL.Path)
	if m == nil && r.Method == http.MethodPost {
		m, write = editFrameRouteRe.FindStringSubmatch(r.URL.Path), true
	}
	if m == nil {
		return User{}, false, false
	}
	c, valid := s.verifyFrame(r.Context(), m[1])
	if !valid || c.Write != write || (write && c.Path == "") {
		http.Error(w, "this page link has expired; reload the page", http.StatusUnauthorized)
		return User{}, true, false
	}
	return User{ID: c.UserID, Email: c.Email, Name: c.Name}, true, true
}

// handleFrame mints the path-shaped URL a page renders from. Asking for it is
// reading the page, so the page must resolve for the caller like /file would.
func (s *Server) handleFrame(v *volume, w http.ResponseWriter, r *http.Request) {
	p, _, code, err := s.lookup(v, r)
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	u := s.requestUser(r)
	project := r.PathValue("project")
	dir := path.Dir(p)
	if dir == "." {
		dir = ""
	}
	exp := time.Now().Add(frameTokenTTL)
	tok, err := s.signFrame(r.Context(), frameClaims{
		Project: project, Dir: dir, UserID: u.ID, Email: u.Email, Expires: exp.Unix(),
	})
	if err != nil {
		storageErr(w, http.StatusBadGateway, "could not prepare the page", err)
		return
	}
	base := "/api/"
	if project != "" {
		base += "p/" + url.PathEscape(project) + "/"
	}
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	writeJSON(w, map[string]any{
		"url":     base + "raw/" + tok + "/" + strings.Join(segs, "/"),
		"expires": exp.UnixMilli(),
	})
}

// handleRaw serves one file under a frame token's folder, inline, exactly as
// /file?path= would serve it to the token's reader.
func (s *Server) handleRaw(v *volume, w http.ResponseWriter, r *http.Request) {
	c, ok := s.verifyFrame(r.Context(), r.PathValue("token"))
	if !ok {
		http.Error(w, "this page link has expired; reload the page", http.StatusUnauthorized)
		return
	}
	if c.Write {
		// An edit token writes one file; it never reads a folder.
		http.NotFound(w, r)
		return
	}
	p := r.PathValue("path")
	if c.Project != r.PathValue("project") || !underDir(p, c.Dir) {
		http.Error(w, "no such file: "+p, http.StatusNotFound)
		return
	}
	// serveBlob reads its target from ?path=, so the one in the URL path is
	// written over whatever the query says. The view flags (comment, edit,
	// print) pass through: they change what the page is wrapped in, never
	// what it may read.
	q := r.URL.Query()
	q.Set("path", p)
	r2 := r.Clone(r.Context())
	r2.URL.RawQuery = q.Encode()
	s.serveBlob(v, w, r2, false)
}

/*
The edit beacon: what a framed page that is being edited sends as it unloads.

A page edited in place lives in the same opaque-origin frame, and its edits
reach the editor by postMessage — one hop the tab can close in the middle of.
The frame's own pagehide still runs, and a keepalive fetch it starts outlives
the page; but it carries no cookie and can read no answer (no-cors). So the
frame is handed, while the page is open, a URL that carries its own
credential: an edit token naming the editor, the project and the ONE file,
good for frameTokenTTL, opening nothing but this POST. It is minted only for
someone who may write that file, and every flush checks that again, so a
revoked editor's open page stops writing at once.

A flush is a set of patches against the exact bytes the page was rendered
from — the hash and the UTF-16 length the editable view stamps on it
(htmledit.go) — each replacing one stamped element's inner range. The hub
rebuilds the edited text from those bytes and hands it to the door the
editor's own save takes (/ycollab/sync on a hub, the upload door on a plain
folder): merged with whatever the document holds now, idempotent when it
already holds the edit, and parked as a conflict copy when somebody changed
the same words, since a page that is gone cannot be asked.
*/

// maxEditPatches bounds one flush: a page edits a handful of elements.
const maxEditPatches = 256

// handleEditFrame mints the edit beacon's URL for one file the caller may write.
func (s *Server) handleEditFrame(v *volume, w http.ResponseWriter, r *http.Request) {
	if !s.Upload.Enabled {
		http.Error(w, "uploads are disabled on this server", http.StatusForbidden)
		return
	}
	p, err := cleanUploadPath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.writablePath(w, r, p) {
		return
	}
	if _, _, code, err := s.lookup(v, r); err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	u := s.requestUser(r)
	project := r.PathValue("project")
	exp := time.Now().Add(frameTokenTTL)
	tok, err := s.signFrame(r.Context(), frameClaims{
		Project: project, Path: p, Write: true,
		UserID: u.ID, Email: u.Email, Name: u.Name, Expires: exp.Unix(),
	})
	if err != nil {
		storageErr(w, http.StatusBadGateway, "could not prepare the editor", err)
		return
	}
	base := "/api/"
	if project != "" {
		base += "p/" + url.PathEscape(project) + "/"
	}
	writeJSON(w, map[string]any{"url": base + "ycollab/frame/" + tok, "expires": exp.UnixMilli()})
}

type editPatch struct {
	Start int    `json:"start"`
	End   int    `json:"end"`
	HTML  string `json:"html"`
}

// handleEditFrameFlush applies an unloading page's unsent edits. Its caller
// is gone before the answer arrives; the answer is for tests and logs.
func (s *Server) handleEditFrameFlush(v *volume, w http.ResponseWriter, r *http.Request) {
	if !s.Upload.Enabled {
		http.Error(w, "uploads are disabled on this server", http.StatusForbidden)
		return
	}
	c, ok := s.verifyFrame(r.Context(), r.PathValue("token"))
	if !ok || !c.Write || c.Path == "" || c.Project != r.PathValue("project") {
		http.Error(w, "this page link has expired; reload the page", http.StatusUnauthorized)
		return
	}
	// Who may write, decided now: the token names the editor, never grants.
	if !s.writablePath(w, r, c.Path) {
		return
	}
	var req struct {
		SrcHash string      `json:"src_hash"`
		SrcLen  int         `json:"src_len"`
		Patches []editPatch `json:"patches"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxSeedBytes+64<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if len(req.Patches) == 0 || len(req.Patches) > maxEditPatches || !blobRe.MatchString(req.SrcHash) {
		http.Error(w, "invalid edit", http.StatusBadRequest)
		return
	}
	// The page is closing and may drop the socket: the write still finishes.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	defer cancel()

	base, found := s.editBase(ctx, v, c.Path, req.SrcHash)
	if !found {
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "the page's version of this file is gone", "path": c.Path})
		return
	}
	want, err := applyEditPatches(base, req.SrcLen, req.Patches)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if fi, ok := s.headFile(ctx, v, c.Path); ok && sharpShrink(fi.Size, int64(len(want))) {
		// Nobody is left to confirm it.
		writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "this save would empty or sharply shrink the file", "shrink": true, "path": c.Path,
		})
		return
	}
	who := s.requestUser(r)
	var out *memWriter
	if c.Project != "" {
		// The editor's own save door, whole: the room's merge, the conflict
		// copy, durability, quota and folder rules.
		body, _ := json.Marshal(map[string]any{
			"base": req.SrcHash, "base_text": base, "text": want, "park_on_conflict": true,
		})
		out = s.reenter(ctx, who, http.MethodPost,
			"/api/p/"+url.PathEscape(c.Project)+"/ycollab/sync?path="+url.QueryEscape(c.Path), body, nil)
	} else {
		out = s.flushToFile(ctx, v, who, c.Path, base, want)
	}
	if ct := out.hdr.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(out.code)
	_, _ = w.Write(out.body.Bytes())
}

// flushToFile is the edit beacon on a plain folder, which has no room: the
// edit is merged against the file as it is now and written conditionally on
// it, and a clash is parked beside it.
func (s *Server) flushToFile(ctx context.Context, v *volume, who User, p, base, want string) *memWriter {
	put := func(path, text string, hdr http.Header) *memWriter {
		hdr.Set(parkOnConflictHeader, "1")
		return s.reenter(ctx, who, http.MethodPut, "/api/upload/content?path="+url.QueryEscape(path), []byte(text), hdr)
	}
	fi, ok := s.headFile(ctx, v, p)
	if !ok {
		w := newMemWriter()
		w.code = http.StatusNotFound
		w.body.WriteString("no such file: " + p)
		return w
	}
	cur, ok := s.readHead(ctx, v, p, fi)
	if !ok {
		w := newMemWriter()
		w.code = http.StatusBadGateway
		w.body.WriteString("could not read " + p)
		return w
	}
	text := want
	switch {
	case string(cur) == want:
		w := newMemWriter()
		writeJSON(w, map[string]any{"ok": true, "path": p, "sha": fi.Blob, "unchanged": true})
		return w
	case string(cur) != base:
		merged, ok := textmerge.Merge([]byte(base), cur, []byte(want))
		if !ok || !validMergedText(p, merged) {
			return put(conflictPath(p, who, time.Now()), want, http.Header{"If-None-Match": {"*"}})
		}
		text = string(merged)
	}
	return put(p, text, http.Header{"If-Match": {fi.Blob}})
}

// reenter runs one request through the API as who, the way putAs does.
func (s *Server) reenter(ctx context.Context, who User, method, target string, body []byte, hdr http.Header) *memWriter {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return &memWriter{code: http.StatusInternalServerError, hdr: http.Header{}}
	}
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	w := newMemWriter()
	s.apiMux.ServeHTTP(w, withUser(req, who))
	return w
}

// editBase is the text of p whose content hash is sha: the file as it is,
// or — on a journal-backed source — a version p itself once held. Never any
// other path's: the hash is checked against p's own history before a blob
// is read, so a caller cannot read arbitrary content by naming its hash.
func (s *Server) editBase(ctx context.Context, v *volume, p, sha string) (string, bool) {
	ok := func(b []byte) bool {
		return len(b) < maxSeedBytes && textmerge.IsText(b) && sha256Hex(b) == sha
	}
	if fi, found := s.headFile(ctx, v, p); found {
		if b, read := s.readHead(ctx, v, p, fi); read && ok(b) {
			return string(b), true
		}
	}
	rs, isRemote := v.source.(*RemoteSource)
	if !isRemote {
		return "", false
	}
	ops, err := rs.loadSourcedOps(ctx)
	if err != nil {
		return "", false
	}
	held := false
	for _, so := range ops {
		if so.Op.Path == p && so.Op.Blob == sha {
			held = true
			break
		}
	}
	if !held {
		return "", false
	}
	rc, err := rs.OpenBlob(ctx, sha)
	if err != nil {
		return "", false
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxSeedBytes+1))
	if err != nil || !ok(b) {
		return "", false
	}
	return string(b), true
}

// readHead reads p's current bytes, bounded like a seed.
func (s *Server) readHead(ctx context.Context, v *volume, p string, fi FileInfo) ([]byte, bool) {
	rc, err := v.source.Open(ctx, p, fi)
	if err != nil {
		return nil, false
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxSeedBytes+1))
	return b, err == nil
}

// applyEditPatches rebuilds an edited page from the bytes it was rendered
// from. Offsets are UTF-16 code units, as the editable view stamps them
// (utf16Len) and as the browser's strings count. Each patch replaces one
// element's inner range, which always sits strictly inside the document —
// past its start tag and before its end tag — so none can replace the whole
// file; they are in order and do not overlap, and none splits a surrogate
// pair.
func applyEditPatches(base string, srcLen int, patches []editPatch) (string, error) {
	units := utf16.Encode([]rune(base))
	if len(units) != srcLen {
		return "", errors.New("the page's length does not match its version of the file")
	}
	inPair := func(i int) bool {
		return i > 0 && i < len(units) && utf16.IsSurrogate(rune(units[i])) && units[i] >= 0xDC00
	}
	out := make([]uint16, 0, len(units))
	prev := 0
	for i, p := range patches {
		if p.Start <= 0 || p.Start > p.End || p.End >= srcLen || p.Start < prev || (i > 0 && p.Start < patches[i-1].End) {
			return "", errors.New("edit ranges are out of order or outside the page")
		}
		if inPair(p.Start) || inPair(p.End) || !textmerge.IsText([]byte(p.HTML)) {
			return "", errors.New("an edit is not text")
		}
		out = append(out, units[prev:p.Start]...)
		out = append(out, utf16.Encode([]rune(p.HTML))...)
		prev = p.End
	}
	out = append(out, units[prev:]...)
	want := string(utf16.Decode(out))
	if len(want) >= maxSeedBytes {
		return "", errors.New("the edited page is too large to save")
	}
	return want, nil
}

func underDir(p, dir string) bool {
	if p == "" || strings.Contains("/"+p+"/", "/../") {
		return false
	}
	return dir == "" || strings.HasPrefix(p, dir+"/")
}
