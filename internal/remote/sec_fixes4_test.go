package remote

// Round 5 — the target is round 4's own fixes (b616c94) in this package:
// deviceToken's origin binding (sameOrigin), dropCredentialOffOrigin, and
// prefixed.safeKey, the single containment primitive multi-tenancy rests on.
//
// Every test asserts the SECURE behavior, so it goes green the moment the hole
// is closed and stays as a permanent regression test. Helpers are prefixed
// secfx4; no existing file is touched.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// sameOrigin — the comparison that decides who receives the device token.
// ---------------------------------------------------------------------------

// The permissive direction first: nothing that is a different origin may
// compare equal. These all pass today; they stay as the regression wall around
// round 4's critical (a shared folder's .bdrive/config.json choosing where this
// device's hub credential is sent).
func TestSec_SameOrigin_NeverAcceptsADifferentServer(t *testing.T) {
	const settings = "https://hub.example.com"
	for _, remoteURL := range []string{
		"http://hub.example.com",               // scheme downgrade
		"https://evil.example.com",             // different host
		"https://hub.example.com.evil.example", // suffix
		"https://evilhub.example.com",          // prefix
		"https://hub.example.com@evil.example", // userinfo confusion
		"https://hub.example.com:8443",         // different port
		"https://xn--hb-viaa.example.com",      // punycode lookalike
		"",                                     // empty
		"://hub.example.com",                   // malformed
		"https://",                             // no host
	} {
		if sameOrigin(remoteURL, settings) {
			t.Errorf("sameOrigin(%q, %q) = true — the device token would be sent there", remoteURL, settings)
		}
	}
	// and an empty/malformed settings server never matches anything
	for _, s := range []string{"", "://x", "https://", "  "} {
		if sameOrigin("https://hub.example.com", s) {
			t.Errorf("sameOrigin(hub, %q) = true", s)
		}
	}
}

// The false-negative direction, which is a security problem of its own shape:
// a credential silently not sent is a sync that fails with 401 forever and no
// message naming the cause, and the documented recovery ("run `bdrive login`
// again") does not fix it because login writes the same string back.
//
// sameOrigin compares url.URL.Host verbatim, and Host carries the port. The
// default port for the scheme is the same origin by every definition (RFC 6454,
// the browser same-origin rule, and every URL library), but here
// "https://hub.example.com:443" and "https://hub.example.com" are two servers.
// A hub behind a config that spells the port, a `bdrive init` URL copied from a
// browser's address bar, or a settings.json written by an older release is
// enough. The trailing-dot FQDN form is the same class.
func TestSec_SameOrigin_AcceptsTheSameServerSpelledDifferently(t *testing.T) {
	for _, tc := range []struct{ remoteURL, settings string }{
		{"https://hub.example.com:443", "https://hub.example.com"},
		{"https://hub.example.com", "https://hub.example.com:443"},
		{"http://hub.example.com:80", "http://hub.example.com"},
		{"https://HUB.example.com", "https://hub.example.com"},  // case (passes today)
		{"https://hub.example.com/", "https://hub.example.com"}, // trailing slash (passes today)
	} {
		if !sameOrigin(tc.remoteURL, tc.settings) {
			t.Errorf("sameOrigin(%q, %q) = false — the same server, so the device token is dropped "+
				"and every sync 401s with nothing explaining why (Host is compared verbatim, "+
				"so the scheme's default port reads as a different origin)", tc.remoteURL, tc.settings)
		}
	}
}

// ---------------------------------------------------------------------------
// dropCredentialOffOrigin — what a hub's 3xx can still take with it.
// ---------------------------------------------------------------------------

// secfx4Recorder is a server that records the headers of every request.
type secfx4Recorder struct {
	mu   sync.Mutex
	hdrs []http.Header
}

func (r *secfx4Recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.hdrs = append(r.hdrs, req.Header.Clone())
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"objects":[]}`)
}

func (r *secfx4Recorder) seen() []http.Header {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]http.Header(nil), r.hdrs...)
}

// Round 4 stopped the Authorization header following a hub's redirect off its
// origin. It left the redirect itself in place, and the device identity
// headers on it — "every endpoint this backend calls is the hub's own API,
// where a 3xx is not part of the contract anyway" is an argument for refusing
// the redirect, not for following it with a smaller payload.
//
// X-Bdrive-Device-Name is the machine's own name ("Alice's MacBook"), and
// X-Bdrive-Device is the id that identifies this device across every project
// on the hub. A hub that is compromised, or simply misconfigured behind a
// redirecting proxy, points them at any host it likes and gets both, plus the
// OS/arch and the fact that this address is running BearDrive at all.
func TestSec_HTTPBackend_ACrossOriginRedirectCarriesNoDeviceIdentity(t *testing.T) {
	t.Setenv("BDRIVE_TOKEN", "device-token-abc")
	t.Setenv("BDRIVE_HOME", t.TempDir())

	elsewhere := &secfx4Recorder{}
	other := httptest.NewServer(elsewhere)
	defer other.Close()

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer hub.Close()

	be, err := Open(context.Background(), hub.URL+"/p/p-00000001")
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()

	// The device identity the CLI always sends.
	if hb, ok := be.(*httpBackend); ok {
		hb.device.ID = "alice-laptop-7c31"
		hb.device.Name = "Alice's MacBook"
	}
	if _, err := be.List(context.Background(), "journal/"); err != nil {
		t.Logf("list through the redirect: %v", err)
	}

	for _, h := range elsewhere.seen() {
		if got := h.Get("Authorization"); got != "" {
			t.Errorf("the device token followed a cross-origin redirect: %q", got)
		}
		if got := h.Get("X-Bdrive-Device") + h.Get("X-Bdrive-Device-Name") + h.Get("X-Bdrive-Os"); got != "" {
			t.Errorf("a third-party host reached by the hub's 302 received this device's identity: "+
				"device=%q name=%q os=%q\n"+
				"dropCredentialOffOrigin deletes only Authorization; the redirect is still followed "+
				"and the machine name, device id and OS go with it",
				h.Get("X-Bdrive-Device"), h.Get("X-Bdrive-Device-Name"), h.Get("X-Bdrive-Os"))
		}
	}
	if len(elsewhere.seen()) > 0 {
		t.Errorf("the sync client followed a cross-origin redirect off the hub (%d request(s) to %s); "+
			"a 3xx is not part of the store API's contract and should be refused, not followed",
			len(elsewhere.seen()), other.URL)
	}
}

// ---------------------------------------------------------------------------
// prefixed.safeKey — "a key is either already safe or refused".
// ---------------------------------------------------------------------------

// secfx4Recording is a Backend that records the keys it is handed, so the test
// sees what the namespace actually resolved to.
type secfx4Recording struct {
	mu   sync.Mutex
	keys []string
}

func (b *secfx4Recording) note(k string) {
	b.mu.Lock()
	b.keys = append(b.keys, k)
	b.mu.Unlock()
}
func (b *secfx4Recording) Put(_ context.Context, key string, _ io.Reader, _ int64) error {
	b.note(key)
	return nil
}
func (b *secfx4Recording) Get(_ context.Context, key string) (io.ReadCloser, error) {
	b.note(key)
	return io.NopCloser(strings.NewReader("")), nil
}
func (b *secfx4Recording) Exists(_ context.Context, key string) (bool, error) {
	b.note(key)
	return false, nil
}
func (b *secfx4Recording) List(_ context.Context, prefix string) ([]Object, error) {
	b.note(prefix)
	return nil, nil
}
func (b *secfx4Recording) Close() error { return nil }

func (b *secfx4Recording) got() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.keys...)
}

// safeKey's whole test is path.Clean(trimmed) == trimmed, and "." is
// Clean-stable. So it is accepted, and prefixed.key builds "<project>/." —
// which is the PROJECT DIRECTORY on the file backend (filepath.Join collapses
// it) and a literal, distinct object name on S3 and GCS. One key, three
// different targets depending on where the hub stores its data, and on a file://
// hub a Get of it opens the project directory rather than being refused.
//
// The containment primitive should refuse a key that is not a key rather than
// let each backend decide what it means.
func TestSec_Prefixed_ADotIsNotAKey(t *testing.T) {
	rec := &secfx4Recording{}
	p := Prefixed(rec, "project-a")

	for _, key := range []string{".", "./", "."} {
		if _, err := p.Get(context.Background(), key); err == nil {
			t.Errorf("Prefixed.Get(%q) was accepted", key)
		}
		if err := p.Put(context.Background(), key, strings.NewReader("x"), 1); err == nil {
			t.Errorf("Prefixed.Put(%q) was accepted", key)
		}
	}
	if got := rec.got(); len(got) > 0 {
		t.Errorf("a key that is not a key reached the storage backend as %q\n"+
			"safeKey tests only path.Clean(key) == key, and \".\" is Clean-stable — "+
			"it resolves to the project's own directory on file://, and to a literal "+
			"\"<project>/.\" object on S3/GCS", got)
	}
}

// The regression wall around round 4's fix: keys that escape the project
// namespace stay refused, in and out.
func TestSec_Prefixed_StillContainsTheNamespace(t *testing.T) {
	rec := &secfx4Recording{}
	p := Prefixed(rec, "project-a")
	for _, key := range []string{
		"../project-b/blobs/x", "..", "/etc/passwd", "a/../../project-b/x",
		"blobs/../../project-b/x", "./blobs/x",
	} {
		if _, err := p.Get(context.Background(), key); err == nil {
			t.Errorf("Get(%q) escaped the project prefix", key)
		}
	}
	if got := rec.got(); len(got) > 0 {
		t.Errorf("escaping keys reached the backend: %q", got)
	}
}
