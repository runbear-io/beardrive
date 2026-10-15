package remote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"
)

// Every hub endpoint must turn a 403 into ErrForbidden: that sentinel is what
// tells the syncer "you were refused" rather than "the network is down", and a
// miss on any one call would put that path back into a silent forever-retry.
func TestHubForbiddenIsSentinel(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "you have read-only access to this project", http.StatusForbidden)
	}))
	defer ts.Close()

	be, err := Open(context.Background(), ts.URL+"/p/p-0123abcd")
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	ctx := context.Background()

	calls := map[string]func() error{
		"List":   func() error { _, err := be.List(ctx, "journal/"); return err },
		"Get":    func() error { _, err := be.Get(ctx, "journal/d.jsonl"); return err },
		"Exists": func() error { _, err := be.Exists(ctx, "journal/d.jsonl"); return err },
		"Put":    func() error { return be.Put(ctx, "blobs/abc", strings.NewReader("hi"), 2) },
	}
	for name, call := range calls {
		err := call()
		if err == nil {
			t.Errorf("%s: no error on 403", name)
			continue
		}
		if !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: %v does not wrap ErrForbidden", name, err)
		}
		if !strings.Contains(err.Error(), "read-only") {
			t.Errorf("%s: the server's message is lost: %v", name, err)
		}
	}
	if rr, ok := be.(ReadReporter); ok {
		if err := rr.ReportReads(ctx, ReadKindAgent, []ReadEvent{{Path: "a.md"}}); !errors.Is(err, ErrForbidden) {
			t.Errorf("ReportReads: %v does not wrap ErrForbidden", err)
		}
	}
}

// A 403 relayed from the object store is an expired presigned URL, not an
// authorization answer. Mapping it would park a healthy device in permanent
// read-only over a transient signing problem.
func TestPresignedForbiddenIsNotAuthz(t *testing.T) {
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "<Error><Code>AccessDenied</Code></Error>", http.StatusForbidden)
	}))
	defer storage.Close()

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/store/sign") {
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"mode":"direct","exists":false,"url":"` + storage.URL + `/blob","method":"PUT"}`))
	}))
	defer hub.Close()

	be, err := Open(context.Background(), hub.URL+"/p/p-0123abcd")
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	err = be.Put(context.Background(), "blobs/abc", bytes.NewReader([]byte("hi")), 2)
	if err == nil {
		t.Fatal("direct upload to a 403 target should fail")
	}
	if errors.Is(err, ErrForbidden) {
		t.Fatalf("a presigned-target 403 must not be ErrForbidden: %v", err)
	}
}

func TestHTTPBackend_SharesTransportAndIsolatesProjectHeadersAndStreams(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	// Project URLs require at least four characters, so use accepted IDs.
	tokens := map[string]string{"p-0001": "token-one", "p-0002": "token-two"}
	sendEvent := make(chan struct{}, 1)
	streamClosed := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		project, endpoint, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/api/p/"), "/")
		token, known := tokens[project]
		if !ok || !known {
			t.Errorf("unexpected project path: %s", r.URL.Path)
			http.Error(w, "unknown project", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("%s authorization = %q, want its own token", project, got)
			http.Error(w, "wrong token", http.StatusUnauthorized)
			return
		}
		if got := r.Header.Get("X-Bdrive-Device"); got != "device-"+project {
			t.Errorf("%s device = %q, want its own device", project, got)
		}
		if got := r.Header.Get("X-Bdrive-Device-Name"); got != "name-"+project {
			t.Errorf("%s device name = %q, want its own name", project, got)
		}
		if got := r.Header.Get(PermsCapability); got != "1" {
			t.Errorf("%s permissions capability = %q, want 1", project, got)
		}
		switch endpoint {
		case "events":
			if project != "p-0001" || r.Header.Get("Accept") != "text/event-stream" {
				t.Errorf("unexpected event request: %s", r.URL.Path)
			}
			defer close(streamClosed)
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte(": ready\n\n"))
			w.(http.Flusher).Flush()
			select {
			case <-sendEvent:
				w.Write([]byte("data: changed\n\n"))
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
			<-r.Context().Done()
		case "store/object":
			if got := r.URL.Query().Get("key"); got != "blobs/"+project {
				t.Errorf("%s object key = %q, want its own key", project, got)
			}
			w.Write([]byte(project))
		default:
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("BDRIVE_TOKEN", tokens["p-0001"])
	first, err := newHTTPBackend(ts.URL + "/p/p-0001")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	t.Setenv("BDRIVE_TOKEN", tokens["p-0002"])
	second, err := newHTTPBackend(ts.URL + "/p/p-0002")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	for _, b := range []*httpBackend{first, second} {
		b.device.ID = "device-" + b.project
		b.device.Name = "name-" + b.project
		if b.hc.Transport != sharedHTTPTransport {
			t.Fatal("backend does not use the shared transport")
		}
		if b.hc.Timeout != 5*time.Minute {
			t.Fatalf("store timeout = %s, want 5m", b.hc.Timeout)
		}
	}
	if sharedHTTPTransport == http.DefaultTransport {
		t.Fatal("shared transport must be a clone, not the default transport")
	}
	if sharedHTTPTransport.MaxIdleConns != 100 || sharedHTTPTransport.MaxIdleConnsPerHost != 20 ||
		sharedHTTPTransport.IdleConnTimeout != 90*time.Second || !sharedHTTPTransport.ForceAttemptHTTP2 {
		t.Fatal("shared transport does not have the global daemon pooling settings")
	}

	readProject := func(b *httpBackend) httptrace.GotConnInfo {
		t.Helper()
		var conn httptrace.GotConnInfo
		requestCtx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { conn = info },
		})
		body, err := b.Get(requestCtx, "blobs/"+b.project)
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(body)
		closeErr := body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read %s: read=%v, close=%v", b.project, readErr, closeErr)
		}
		if string(data) != b.project {
			t.Fatalf("%s received another project's content: %q", b.project, data)
		}
		return conn
	}
	firstConn := readProject(first)
	secondConn := readProject(second)
	if !secondConn.Reused || secondConn.Conn != firstConn.Conn {
		t.Fatal("projects did not reuse the same pooled connection")
	}
	var streamConn httptrace.GotConnInfo
	streamCtx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { streamConn = info },
	})
	events, err := first.Watch(streamCtx)
	if err != nil {
		t.Fatal(err)
	}
	if !streamConn.Reused || streamConn.Conn != secondConn.Conn {
		t.Fatal("Watch did not use the shared connection pool")
	}

	// The HTTP/1 stream holds its connection open; another project must still
	// complete store requests, even without HTTP/2 multiplexing.
	concurrentConn := readProject(second)
	select {
	case <-streamClosed:
		t.Fatal("Watch closed before the concurrent store request completed")
	default:
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	afterClose := readProject(second)
	if !afterClose.Reused || afterClose.Conn != concurrentConn.Conn {
		t.Fatal("closing one project discarded a sibling's idle connection")
	}
	sendEvent <- struct{}{}
	select {
	case _, ok := <-events:
		if !ok {
			t.Fatal("closing the backend interrupted its caller-owned Watch")
		}
	case <-ctx.Done():
		t.Fatal("Watch did not deliver an event after concurrent store requests")
	}
	cancel()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("Watch emitted an unexpected extra event")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not close after cancellation")
	}
}
