package webapp

import (
	"compress/gzip"
	"net/http"
	"strings"
)

// Response compression for everything the viewer serves.
//
// The hub shipped every JSON response and every static asset raw. On a real
// project that is a 1.65 MB file tree that gzips to 148 KB, a 1.34 MB script
// bundle that gzips to 431 KB, and a browser that asked for
// `Accept-Encoding: gzip, br` on every one of them. See
// docs/network-efficiency-prd.md for the measurements.
//
// This is deliberately NOT the /store/* sync wire, which negotiates its own
// encoding end to end (store.go, remote/compress.go) and is passed through
// untouched here — a response that already carries Content-Encoding is never
// compressed twice.
//
// The dangerous case is streaming. A ResponseWriter wrapper that implements
// neither Flush nor Unwrap leaves SSE unflushable, which is how live updates
// and co-editing once died behind an analytics middleware (see
// refuseUnstreamable in events.go, which exists because of it). gzipWriter
// implements both, and text/event-stream is excluded from compression
// outright: the frames are small and the stream is long-lived, so buffering
// them to save bytes trades the whole point of the stream for nothing.

// gzipResponses compresses responses whose content type benefits, for clients
// that asked. The decision is made at WriteHeader, because Content-Type is
// what decides it and the handler sets that.
func gzipResponses(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The sync wire is left alone WHOLESALE, not just when it has already
		// encoded a body. /store/* answers are read by syncer.pull, which
		// skips a journal that did not grow and then resumes at a BYTE
		// OFFSET — a length that means something other than "bytes on this
		// socket" is how that loop re-downloads forever or resumes
		// mid-stream. store.go negotiates its own encoding end to end and is
		// the only thing that gets to decide there.
		if strings.Contains(r.URL.Path, "/store/") ||
			!acceptsGzip(r.Header.Get("Accept-Encoding")) {
			h.ServeHTTP(w, r)
			return
		}
		gw := &gzipWriter{ResponseWriter: w}
		defer gw.close()
		h.ServeHTTP(gw, r)
	})
}

// acceptsGzip is a token scan, not a substring test: "gzip;q=0" means the
// client has explicitly refused it, and a Contains check reads that as yes.
func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		token, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(token), "gzip") {
			continue
		}
		for _, p := range strings.Split(params, ";") {
			if k, v, ok := strings.Cut(p, "="); ok &&
				strings.EqualFold(strings.TrimSpace(k), "q") &&
				strings.TrimSpace(v) == "0" {
				return false
			}
		}
		return true
	}
	return false
}

type gzipWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
	// decided is set the first time headers are committed — by WriteHeader, or
	// by the implicit 200 the first Write produces. Everything about whether
	// this response is compressed is settled there and never revisited.
	decided bool
}

func (g *gzipWriter) WriteHeader(code int) {
	g.decide(code)
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipWriter) decide(code int) {
	if g.decided {
		return
	}
	g.decided = true
	h := g.Header()
	// Whether this body is compressed depends on the request's
	// Accept-Encoding, so any cache holding it has to key on that — true
	// whichever way the decision below goes.
	if compressible(h.Get("Content-Type")) {
		h.Add("Vary", "Accept-Encoding")
	}
	switch {
	case h.Get("Content-Encoding") != "":
		return // already encoded by the handler: /store/* blobs
	case !compressible(h.Get("Content-Type")):
		return
	case code == http.StatusNoContent, code == http.StatusNotModified:
		return // no body to compress, and a body here would be a bug
	case code == http.StatusPartialContent, h.Get("Content-Range") != "":
		// A range is a byte offset into the PLAINTEXT. Compressing the slice
		// leaves Content-Range describing bytes that are no longer there.
		// http.FileServerFS serves the embedded assets and answers ranges, so
		// this is reachable, not theoretical.
		return
	}
	h.Set("Content-Encoding", "gzip")
	// The handler measured the plaintext. Whatever it said is now wrong, and
	// a wrong Content-Length is a truncated response, not a slow one.
	//
	// It is kept under another name rather than dropped: the file viewer
	// decides whether a body is too large to render BEFORE reading it
	// (useBlob.ts), and that check is the difference between a "too large"
	// card and pulling a 50 MB file into a tab. Same-origin, so fetch() can
	// read it; every caller treats it as the hint Content-Length already was.
	if cl := h.Get("Content-Length"); cl != "" {
		h.Set("X-Uncompressed-Length", cl)
	}
	h.Del("Content-Length")
	g.gz = gzip.NewWriter(g.ResponseWriter)
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	g.decide(http.StatusOK)
	if g.gz == nil {
		return g.ResponseWriter.Write(b)
	}
	return g.gz.Write(b)
}

// Flush reaches the socket through the gzip writer rather than around it:
// flushing the socket alone would leave the frame sitting in the compressor,
// which is a stream that silently stops arriving.
func (g *gzipWriter) Flush() {
	if g.gz != nil {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap is what http.ResponseController and refuseUnstreamable walk, so a
// handler can still find the real writer's capabilities through this one.
func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func (g *gzipWriter) close() {
	if g.gz != nil {
		_ = g.gz.Close() // writes the trailer; without it the body is invalid
	}
}

/* compressible is an allowlist, and the direction matters: an unknown type is
   left alone rather than compressed hopefully.

   Everything this hub serves that is already compressed — images, fonts, PDFs,
   the export tarball, blobs the sync wire encoded — is binary with a type of
   its own, so a denylist would have to be complete to be safe and an allowlist
   only has to be right. Re-compressing a PNG spends CPU to add bytes.

   ponytail: no minimum size, so a 12-byte {"ok":true} gains ~20 bytes of gzip
   framing. Add a buffer-until-threshold if tiny JSON responses ever dominate
   a profile; they do not today, and the buffering is where the bugs live. */
func compressible(contentType string) bool {
	ct, _, _ := strings.Cut(contentType, ";")
	ct = strings.ToLower(strings.TrimSpace(ct))
	if strings.HasPrefix(ct, "text/") {
		// The one text type that must never be buffered: see the note above.
		return ct != "text/event-stream"
	}
	switch ct {
	case "application/json", "application/javascript", "application/xml",
		"application/wasm", "image/svg+xml":
		return true
	}
	return false
}
