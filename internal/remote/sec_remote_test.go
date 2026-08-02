package remote

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

// Round 4, rows 5 and 11, attacked from inside internal/remote.
//
// remote.Prefixed is the single containment primitive for multi-tenancy on the
// hub: every project lives at <root>/<project-id>/ because Prefixed glues that
// string on the front of every key. localBackend is the storage under every
// file:// hub and every test fixture. httpBackend is the client every
// storage-blind device syncs through — the mirror image of the hub trusting a
// device.
//
// All helpers here are prefixed secrem.

// ---- a recording backend, so we can see the exact key the layer below is
// asked for (that is the containment question) ----

type secremCall struct {
	Op   string
	Key  string
	Size int64
	TTL  time.Duration
}

type secremRecorder struct {
	mu    sync.Mutex
	calls []secremCall
	list  []Object // what List hands back, whatever was asked
}

func (b *secremRecorder) record(op, key string, size int64, ttl time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, secremCall{Op: op, Key: key, Size: size, TTL: ttl})
}

func (b *secremRecorder) keys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.calls))
	for _, c := range b.calls {
		out = append(out, c.Op+" "+c.Key)
	}
	return out
}

func (b *secremRecorder) Put(_ context.Context, key string, r io.Reader, size int64) error {
	io.Copy(io.Discard, r)
	b.record("Put", key, size, 0)
	return nil
}

func (b *secremRecorder) Get(_ context.Context, key string) (io.ReadCloser, error) {
	b.record("Get", key, 0, 0)
	return io.NopCloser(strings.NewReader("")), nil
}

func (b *secremRecorder) Exists(_ context.Context, key string) (bool, error) {
	b.record("Exists", key, 0, 0)
	return false, nil
}

func (b *secremRecorder) List(_ context.Context, prefix string) ([]Object, error) {
	b.record("List", prefix, 0, 0)
	return b.list, nil
}

func (b *secremRecorder) SignPut(_ context.Context, key string, size int64, ttl time.Duration) (*SignedPut, error) {
	b.record("SignPut", key, size, ttl)
	return &SignedPut{URL: "https://storage.invalid/" + key, Method: http.MethodPut, Expires: time.Now().Add(ttl)}, nil
}

func (b *secremRecorder) Close() error { return nil }

// secremEscapes reports whether an underlying storage key, once the object
// store (or the filesystem) normalizes it, still lives under ns/.
func secremEscapes(ns, key string) bool {
	return !strings.HasPrefix(path.Clean("/"+key), "/"+ns+"/")
}

// A project id is a namespace, and Prefixed is the only thing enforcing it.
// Every key it forwards must still land under <prefix>/ AFTER the storage
// normalizes it — S3 and GCS both path.Join their own prefix on, and
// localBackend hands the key to filepath.Join, so ".." is not an inert
// character down there. A key that walks out of the namespace reads and
// writes another tenant's objects on the same storage root.
func TestSec_Prefixed_KeyCannotEscapeTheProjectNamespace(t *testing.T) {
	hostile := []string{
		"../victim/blobs/deadbeef",
		"blobs/../../victim/journal/d.jsonl",
		"../victim/blobs/x",
		"./../victim/blobs/x",
	}
	ctx := context.Background()
	for _, key := range hostile {
		t.Run(key, func(t *testing.T) {
			rec := &secremRecorder{}
			p := Prefixed(rec, "attacker")
			signer, ok := p.(PutSigner)
			if !ok {
				t.Fatal("Prefixed must keep the PutSigner capability")
			}
			p.Put(ctx, key, strings.NewReader("x"), 1)
			p.Get(ctx, key)
			p.Exists(ctx, key)
			signer.SignPut(ctx, key, 1, time.Minute)

			for _, c := range rec.calls {
				if secremEscapes("attacker", c.Key) {
					t.Errorf("%s(%q) reached storage as %q — outside the attacker/ namespace",
						c.Op, key, c.Key)
				}
			}
		})
	}
}

// The namespace has to survive the way OUT too. Prefixed filters List by
// string prefix and then trims it, so a stored key that merely starts with
// "<project>/" is handed back as an in-project key — and fed straight back to
// Get, it resolves somewhere else entirely. That is a round trip through the
// one containment primitive with no boundary left.
func TestSec_Prefixed_ListedKeysStayInsideTheNamespace(t *testing.T) {
	rec := &secremRecorder{list: []Object{
		{Key: "attacker/blobs/aa", Size: 1},
		{Key: "attacker/../victim/blobs/secret", Size: 2},
		{Key: "victim/blobs/secret", Size: 3},
	}}
	p := Prefixed(rec, "attacker")
	objs, err := p.List(context.Background(), "blobs/")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if o.Key != path.Clean(o.Key) || strings.HasPrefix(o.Key, "/") {
			t.Errorf("List handed back %q, which is not a clean relative key", o.Key)
		}
		// The real test: feed it back in, as every caller does (syncer.pull,
		// exportStore, RemoteSource.loadOps all Get what List named).
		rec.calls = nil
		p.Get(context.Background(), o.Key)
		for _, c := range rec.calls {
			if secremEscapes("attacker", c.Key) {
				t.Errorf("List returned %q; Get(%q) reached storage as %q — outside the namespace",
					o.Key, o.Key, c.Key)
			}
		}
	}
}

// A project id that is a prefix of another project's id must not see its
// objects. This is the multi-tenancy question in its plainest form.
func TestSec_Prefixed_SiblingWithAPrefixNameIsNotListed(t *testing.T) {
	root := t.TempDir()
	be, err := Open(context.Background(), "file://"+root)
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	ctx := context.Background()

	abc, abcd := Prefixed(be, "abc"), Prefixed(be, "abcd")
	if err := abc.Put(ctx, "blobs/aa", strings.NewReader("mine"), 4); err != nil {
		t.Fatal(err)
	}
	if err := abcd.Put(ctx, "blobs/bb", strings.NewReader("theirs"), 6); err != nil {
		t.Fatal(err)
	}
	objs, err := abc.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if o.Key != "blobs/aa" {
			t.Errorf("project abc listed %q, which belongs to abcd", o.Key)
		}
	}
	if ok, _ := abc.Exists(ctx, "blobs/bb"); ok {
		t.Error("project abc can see abcd's blob through Exists")
	}
}

// ---- localBackend: round 3 taught path() to refuse a key that climbs out of
// the root. That guard is lexical, and Get/Put both follow symlinks. ----

// A symlink planted anywhere inside the storage root turns every lexically
// valid key into a read or a write anywhere on the hub host. path() answers
// "does this string stay under root", which is a different question from
// "does this file".
func TestSec_Local_SymlinkInsideTheRootIsNotAWayOut(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("other tenant"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	be, err := Open(context.Background(), "file://"+root)
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	ctx := context.Background()

	if rc, err := be.Get(ctx, "escape/secret.txt"); err == nil {
		data, _ := io.ReadAll(rc)
		rc.Close()
		t.Errorf("Get read %q from outside the storage root through a symlink", data)
	}
	if err := be.Put(ctx, "escape/planted.txt", strings.NewReader("owned"), 5); err == nil {
		if _, err := os.Stat(filepath.Join(outside, "planted.txt")); err == nil {
			t.Error("Put wrote outside the storage root through a symlink")
		}
	}
}

// Round 3 fixed Get/Put. List and Exists take keys from the same callers, so
// they need the same answer.
func TestSec_Local_ListAndExistsCannotEscapeTheStorageRoot(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("no"), 0o600); err != nil {
		t.Fatal(err)
	}
	be, err := Open(context.Background(), "file://"+root)
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	ctx := context.Background()

	for _, key := range []string{"../" + filepath.Base(outside) + "/secret.txt", "/etc/hosts", "a/../../x"} {
		if ok, err := be.Exists(ctx, key); ok && err == nil {
			t.Errorf("Exists(%q) resolved outside the storage root", key)
		}
	}
	for _, prefix := range []string{"../", "/", "a/../../"} {
		objs, err := be.List(ctx, prefix)
		if err != nil {
			continue
		}
		for _, o := range objs {
			t.Errorf("List(%q) reported %q", prefix, o.Key)
		}
	}
}

// ---- httpBackend: the device as the client of a hostile hub ----

func secremHub(t *testing.T, h http.HandlerFunc) Backend {
	t.Helper()
	t.Setenv("BDRIVE_HOME", t.TempDir())
	t.Setenv("BDRIVE_TOKEN", "secret-device-token")
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	be, err := Open(context.Background(), ts.URL+"/p/p-0123abcd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { be.Close() })
	return be
}

// The hub names the keys; the device believes it. Those names become local
// journal paths (syncer.pull) and tar entry names (cmd/bdrive export writes
// o.Key straight into the archive header), so a key with ".." in it is a
// hostile hub writing to a spot on the victim's disk it was never given.
// Nothing downstream re-checks the shape, so this is the choke point.
func TestSec_HTTP_ListedKeysFromTheHubStayInTheKeySpace(t *testing.T) {
	be := secremHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"objects": []Object{
			{Key: "journal/good.jsonl", Size: 1},
			{Key: "../../../../../../tmp/pwned", Size: 2},
			{Key: "/etc/cron.d/pwned", Size: 3},
			{Key: "journal/../../.ssh/authorized_keys", Size: 4},
		}})
	})
	objs, err := be.List(context.Background(), "journal/")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if strings.HasPrefix(o.Key, "/") || o.Key != path.Clean(o.Key) || strings.HasPrefix(o.Key, "../") {
			t.Errorf("hub named %q and the backend passed it through", o.Key)
		}
	}
}

// The device token is scoped to the hub the user configured. Every endpoint
// this backend calls is the hub's own API, so a 3xx is never part of the
// contract — and net/http's redirect rules only strip Authorization when the
// HOSTNAME changes, ignoring scheme and port. A hub that answers with a
// redirect therefore hands the credential to whatever else listens on that
// name: another port on the same box, an https->http downgrade, a sibling
// subdomain. Nothing configures CheckRedirect here.
func TestSec_HTTP_BearerTokenIsNeverSentToAnotherOrigin(t *testing.T) {
	var got http.Header
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Write([]byte("{}"))
	}))
	defer elsewhere.Close()

	var hubURL string
	be := secremHub(t, func(w http.ResponseWriter, r *http.Request) {
		hubURL = "http://" + r.Host
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusFound)
	})
	rc, err := be.Get(context.Background(), "blobs/aa")
	if err == nil {
		rc.Close()
	}
	if got == nil {
		t.Fatal("redirect target was never reached; the test proves nothing")
	}
	if v := got.Get("Authorization"); v != "" {
		t.Errorf("configured hub %s redirected to %s and the device token went along: %q",
			hubURL, elsewhere.URL, v)
	}
}

// TLS is what keeps the device token off the wire. The backend must refuse a
// certificate it cannot verify rather than sync through it.
func TestSec_HTTP_UnverifiableTLSIsRefused(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	t.Setenv("BDRIVE_TOKEN", "secret-device-token")
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"objects":[]}`))
	}))
	defer ts.Close()
	be, err := Open(context.Background(), ts.URL+"/p/p-0123abcd")
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	if _, err := be.List(context.Background(), "journal/"); err == nil {
		t.Fatal("synced through an unverifiable TLS certificate")
	} else if !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "x509") {
		t.Fatalf("refused, but not for the certificate: %v", err)
	}
}

// ---- the PutSigner contract ----

// SignPut takes a size because the whole point of a presigned URL is that it
// grants ONE bounded write. A backend that accepts the parameter and drops it
// mints a URL that will take a body of any length — the hub quota-checked a
// number the signature never enforces, and the grant is unmetered for its
// whole TTL. S3 signs Content-Length (see TestS3SignPut); this asserts the
// same contract for every signer, since handleStoreSign/handleUploadInit
// cannot tell them apart.
//
// Signing is a local computation, so both arms run offline against synthetic
// credentials — no bucket and no network.
func TestSec_Sign_DeclaredSizeIsBoundIntoTheSignature(t *testing.T) {
	for name, open := range map[string]func(*testing.T) PutSigner{"s3": secremFakeS3, "gcs": secremFakeGCS} {
		t.Run(name, func(t *testing.T) {
			signer := open(t)
			sp, err := signer.SignPut(context.Background(), "blobs/aa", 42, 5*time.Minute)
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			if !secremBindsSize(sp, 42) {
				t.Errorf("%s presigned URL takes a body of ANY size — the 42 it was given is not in the signature: url=%s headers=%v",
					name, sp.URL, sp.Headers)
			}
		})
	}
}

func secremFakeS3(t *testing.T) PutSigner {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAFAKEFAKEFAKEFAKE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fakefakefakefakefakefakefakefakefakefake")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ENDPOINT_URL", "")
	be, err := Open(context.Background(), "s3://test-bucket/vol1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { be.Close() })
	return be.(PutSigner)
}

// A GCS service account whose private key is generated here: V4 signing needs
// a key it can sign bytes with, and nothing else.
func secremFakeGCS(t *testing.T) PutSigner {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	sa, err := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "p", "private_key_id": "k",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email": "signer@p.iam.gserviceaccount.com", "client_id": "1",
		"token_uri": "https://oauth2.googleapis.com/token",
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := gcs.NewClient(context.Background(), option.WithCredentialsJSON(sa))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &gcsBackend{client: c, bucket: c.Bucket("test-bucket"), prefix: "vol1"}
}

// secremBindsSize reports whether the size the caller declared is actually
// carried by the signed request (a signed Content-Length, or a length
// constraint in the URL's policy) rather than merely known to the server.
func secremBindsSize(sp *SignedPut, size int64) bool {
	want := fmt.Sprintf("%d", size)
	for k, v := range sp.Headers {
		if strings.EqualFold(k, "Content-Length") && v == want {
			return true
		}
	}
	return strings.Contains(sp.URL, "content-length-range") || strings.Contains(sp.URL, "x-goog-content-length")
}
