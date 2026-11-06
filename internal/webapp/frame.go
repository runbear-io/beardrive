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

	"github.com/runbear-io/beardrive/internal/remote"
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

type frameClaims struct {
	Project string `json:"p"`
	Dir     string `json:"d"` // "" is the project root
	UserID  string `json:"i"`
	Email   string `json:"e"`
	Expires int64  `json:"x"`
}

// frameRouteRe matches a raw-route URL path, in volume mode (/api/raw/...) and
// in hub mode (/api/p/<project>/raw/...).
var frameRouteRe = regexp.MustCompile(`^/api/(?:p/[^/]+/)?raw/([^/]+)/`)

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

// frameUser is authGate's door for raw-route requests, before the provider:
// the cookie is exactly what these requests do not carry. handled is false for
// every other path.
func (s *Server) frameUser(w http.ResponseWriter, r *http.Request) (u User, handled, ok bool) {
	m := frameRouteRe.FindStringSubmatch(r.URL.Path)
	if m == nil {
		return User{}, false, false
	}
	c, valid := s.verifyFrame(r.Context(), m[1])
	if !valid {
		http.Error(w, "this page link has expired; reload the page", http.StatusUnauthorized)
		return User{}, true, false
	}
	return User{ID: c.UserID, Email: c.Email}, true, true
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

func underDir(p, dir string) bool {
	if p == "" || strings.Contains("/"+p+"/", "/../") {
		return false
	}
	return dir == "" || strings.HasPrefix(p, dir+"/")
}
