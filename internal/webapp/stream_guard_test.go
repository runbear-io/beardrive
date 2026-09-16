package webapp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// flushlessWriter is the shape that took live updates and co-editing down in
// production: a middleware recorder that embeds the ResponseWriter INTERFACE
// (whose method set has no Flush) and adds neither Flush nor Unwrap. Every
// stream behind it used to answer 200 with an empty body, which a browser
// reads as a stream that opened and closed — so it retried, forever, and
// nothing anywhere said why.
type flushlessWriter struct {
	http.ResponseWriter
	status int
}

func (f *flushlessWriter) WriteHeader(c int) { f.status = c; f.ResponseWriter.WriteHeader(c) }

func TestStreamsRefuseAnUnflushableWriter(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	inner := srv.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(&flushlessWriter{ResponseWriter: w, status: 200}, r)
	}))
	defer ts.Close()

	for _, path := range []string{"/events", "/collab?path=a.md"} {
		resp, err := http.Get(ts.URL + "/api/p/" + p.ID + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("%s answered %d with %d bytes; an unflushable stream must "+
				"fail loudly, not hand back an empty 200 the client retries forever",
				path, resp.StatusCode, len(body))
		}
	}
}

// fixedWriter is flushlessWriter plus the one method that repairs it, which
// is the fix applied to the cloud repo's analytics middleware.
type fixedWriter struct {
	http.ResponseWriter
	status int
}

func (f *fixedWriter) WriteHeader(c int)           { f.status = c; f.ResponseWriter.WriteHeader(c) }
func (f *fixedWriter) Unwrap() http.ResponseWriter { return f.ResponseWriter }

// Unwrap is all it takes: http.ResponseController walks it to the real
// writer's Flusher, and the stream behaves as if no middleware were there.
func TestUnwrapRestoresStreamingThroughMiddleware(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	inner := srv.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(&fixedWriter{ResponseWriter: w, status: 200}, r)
	}))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/p/" + p.ID + "/collab?path=a.md")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("collab stream behind the fixed middleware: %d", resp.StatusCode)
	}
	buf := make([]byte, 64)
	if n, err := resp.Body.Read(buf); err != nil || n == 0 {
		t.Fatalf("no hello frame through the fixed middleware: n=%d err=%v", n, err)
	}
}

// The same two routes still stream normally through an ordinary writer — the
// guard must not cost the working case.
func TestStreamsStillOpenThroughAPlainWriter(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/p/" + p.ID + "/collab?path=a.md")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("collab stream: %d", resp.StatusCode)
	}
	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf) // the hello frame, flushed before anything else
	if err != nil || n == 0 {
		t.Fatalf("stream produced no hello frame: n=%d err=%v", n, err)
	}
}
