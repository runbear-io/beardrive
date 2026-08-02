package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Row 19 — the device as client of a hostile hub, driven from the client side.
//
// Every test in this file points the real https:// backend at an HTTP server
// that speaks the /api/p/<id>/store/* protocol and answers however it likes.
// The hub is the ONLY thing this backend trusts: it names the objects, declares
// their sizes, serves their bytes, and chooses where uploads go. Nothing here
// needs a compromised hub operator to be interesting — one injected JSON
// response is the same primitive.

// sechostHub is a hostile /store/* endpoint. Each field is one lever a real hub
// pulls honestly; a test sets the ones it wants to lie with.
type sechostHub struct {
	*httptest.Server
	list   func(prefix string) []Object            // what /store/list answers
	object func(key string, w http.ResponseWriter) // what /store/object serves
	sign   func(key string, w http.ResponseWriter) // what /store/sign answers
	exists func(key string, w http.ResponseWriter) // what /store/exists answers
}

func sechostServer(t *testing.T, h *sechostHub) *sechostHub {
	t.Helper()
	// Keep the device's credential lookup off the developer's real home.
	t.Setenv("BDRIVE_HOME", t.TempDir())
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		switch {
		case strings.HasSuffix(r.URL.Path, "/store/list") && h.list != nil:
			json.NewEncoder(w).Encode(map[string]any{"objects": h.list(r.URL.Query().Get("prefix"))})
		case strings.HasSuffix(r.URL.Path, "/store/object") && h.object != nil:
			h.object(key, w)
		case strings.HasSuffix(r.URL.Path, "/store/sign") && h.sign != nil:
			var req struct {
				Key string `json:"key"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			h.sign(req.Key, w)
		case strings.HasSuffix(r.URL.Path, "/store/exists") && h.exists != nil:
			h.exists(key, w)
		default:
			http.Error(w, "not served by this hostile hub", http.StatusNotFound)
		}
	}))
	t.Cleanup(h.Server.Close)
	return h
}

func sechostBackend(t *testing.T, h *sechostHub) Backend {
	t.Helper()
	be, err := Open(context.Background(), h.Server.URL+"/p/p-0123abcd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { be.Close() })
	return be
}

// sechostFlood writes chunk after chunk of JSON string filler until stop bytes
// have gone out or the client stops reading, and reports how many it wrote. It
// is how "the hub chooses how much the device allocates" is measured without
// needing the device to actually die of it.
func sechostFlood(w http.ResponseWriter, head string, stop int64) int64 {
	var served atomic.Int64
	if _, err := io.WriteString(w, head); err != nil {
		return served.Load()
	}
	w.(http.Flusher).Flush()
	chunk := bytes.Repeat([]byte("a"), 256<<10)
	for served.Load() < stop {
		n, err := w.Write(chunk)
		served.Add(int64(n))
		if err != nil {
			break
		}
		w.(http.Flusher).Flush()
	}
	return served.Load()
}

// sechostSignBound: /store/sign is decoded with a bare json.NewDecoder on the
// hub's body — no io.LimitReader anywhere on the path. It is called once per
// blob on every push, so the hub picks the device's allocation on a call the
// device makes on every cycle. Round 7 bounded List for exactly this reason and
// round 8 bounded the journal and blob bodies; sign (and exists) were missed.
//
// The ceiling asserted here is deliberately generous: a real sign response is
// under a kilobyte.
func TestSec_HostileHub_ASignedPlanCannotChooseTheDeviceAllocation(t *testing.T) {
	const flood = 72 << 20 // what the hub is willing to write
	const ceiling = 8 << 20
	// atomic: the handler goroutine writes it and the test body reads it.
	var served atomic.Int64
	h := sechostServer(t, &sechostHub{
		sign: func(key string, w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			served.Store(sechostFlood(w, `{"mode":"server","exists":false,"url":"`, flood))
		},
	})
	be := sechostBackend(t, h)
	be.Put(context.Background(), "blobs/"+strings.Repeat("a", 64), strings.NewReader("hi"), 2)
	if served.Load() > ceiling {
		t.Fatalf("the hub made this device read %d bytes of a sign response (ceiling %d): "+
			"sign decodes the hub's body with no io.LimitReader, on the call every blob push starts with", served.Load(), ceiling)
	}
}

// The same unbounded decode sits in Exists. No device-side caller reaches it
// today (only the hub calls Exists, on its own storage), but Backend is a public
// interface and the door is the same one.
func TestSec_HostileHub_AnExistsAnswerCannotChooseTheDeviceAllocation(t *testing.T) {
	const flood = 72 << 20
	const ceiling = 8 << 20
	// atomic: the handler goroutine writes it and the test body reads it.
	var served atomic.Int64
	h := sechostServer(t, &sechostHub{
		exists: func(key string, w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			served.Store(sechostFlood(w, `{"exists":true,"note":"`, flood))
		},
	})
	be := sechostBackend(t, h)
	be.Exists(context.Background(), "blobs/"+strings.Repeat("a", 64))
	if served.Load() > ceiling {
		t.Fatalf("the hub made this device read %d bytes of an exists response (ceiling %d)", served.Load(), ceiling)
	}
}

// Listed keys become local journal file paths (syncer.pull → store.JournalPath,
// which validates nothing) and tar member names (`bdrive export`). Round 5
// closed absolute/.. /non-Clean spellings; a key is still allowed to carry NUL
// and other control bytes, and to be arbitrarily long. journal.SafePath — the
// repo's own rule, already applied at both hub ingest doors and to every peer
// op path — refuses exactly these.
func TestSec_HostileHub_ListedKeysCannotCarryControlBytesOrUnboundedNames(t *testing.T) {
	long := "journal/" + strings.Repeat("d", 300) + ".jsonl"
	refuse := map[string]string{
		"NUL in the key":     "journal/a\x00b.jsonl",
		"newline in the key": "journal/a\nb.jsonl",
		"DEL in the key":     "journal/a\x7fb.jsonl",
		"a 300-byte name":    long,
		"empty":              "",
		"absolute":           "/journal/a.jsonl",
		"parent escape":      "../journal/a.jsonl",
		"a dot":              ".",
		"not Clean-stable":   "journal/./a.jsonl",
	}
	var all []Object
	for _, k := range refuse {
		all = append(all, Object{Key: k, Size: 1})
	}
	all = append(all, Object{Key: "journal/good.jsonl", Size: 1})

	h := sechostServer(t, &sechostHub{list: func(string) []Object { return all }})
	got, err := sechostBackend(t, h).List(context.Background(), "journal/")
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	for _, o := range got {
		kept[o.Key] = true
	}
	if !kept["journal/good.jsonl"] {
		t.Fatal("harness: the one legitimate key was dropped, so this test proves nothing")
	}
	for why, k := range refuse {
		if kept[k] {
			t.Errorf("%s: List kept %q — it becomes a path on this disk and a tar member name", why, k)
		}
	}
}

// A hub can also lie about how big an object is. Size is read as a memory bound
// (syncer.sizeBound) and written straight into a tar header by `bdrive export`;
// a negative one is not a size at all.
func TestSec_HostileHub_ListedSizesAreNotBelievedBlindly(t *testing.T) {
	h := sechostServer(t, &sechostHub{list: func(string) []Object {
		return []Object{{Key: "journal/a.jsonl", Size: -1}, {Key: "journal/b.jsonl", Size: 1 << 62}}
	}})
	got, err := sechostBackend(t, h).List(context.Background(), "journal/")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range got {
		if o.Size < 0 {
			t.Errorf("List passed through a negative size for %q (%d)", o.Key, o.Size)
		}
	}
}

// putDirect PUTs the file's bytes at whatever URL the hub names, with
// plan.Headers set verbatim and no check on the host or the scheme. Round 4
// judged this "not a new capability, the hub already holds the data" — but at
// the moment the hub names the destination it does NOT hold the data; that is
// what the upload is for. One injected sign response sends every file this
// device has to a third party the device never had any relationship with, and
// the hub never sees a byte of it.
func TestSec_HostileHub_ADirectUploadDoesNotGoToAnyHostTheHubNames(t *testing.T) {
	var stolen atomic.Value
	thirdParty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		stolen.Store(string(b))
		w.WriteHeader(http.StatusOK)
	}))
	defer thirdParty.Close()

	h := sechostServer(t, &sechostHub{sign: func(key string, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"mode":"direct","exists":false,"url":%q,"method":"PUT"}`, thirdParty.URL+"/steal")
	}})
	be := sechostBackend(t, h)
	const secret = "quarterly numbers, not for the internet"
	if err := be.Put(context.Background(), "blobs/"+strings.Repeat("a", 64), strings.NewReader(secret), int64(len(secret))); err != nil {
		t.Logf("put returned %v", err)
	}
	if got, _ := stolen.Load().(string); got == secret {
		t.Fatalf("the hub named %s as the upload target and this device delivered the file's bytes there verbatim; "+
			"the presigned-URL host is never checked against the hub's own origin or scheme", thirdParty.URL)
	}
}

// --- attacks the client already refuses (regression cover) ---

// A hub's 3xx must not become an unbounded chase, even entirely on its own
// origin. Round 5 refused the off-origin case; this pins the same-origin one.
func TestSec_HostileHub_ASameOriginRedirectChainIsBounded(t *testing.T) {
	var hops atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops.Add(1)
		http.Redirect(w, r, r.URL.Path+"?key=blobs/x&n="+r.URL.Query().Get("n")+"x", http.StatusFound)
	}))
	defer ts.Close()
	t.Setenv("BDRIVE_HOME", t.TempDir())
	be, err := Open(context.Background(), ts.URL+"/p/p-0123abcd")
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	if _, err := be.List(context.Background(), "journal/"); err == nil {
		t.Fatal("an endless same-origin redirect chain should fail the call")
	}
	if hops.Load() > 20 {
		t.Fatalf("followed %d same-origin redirects", hops.Load())
	}
}

// The hub's error text is quoted back to the user; the hub does not get to
// choose how much of it this device buffers.
func TestSec_HostileHub_AnErrorBodyIsBounded(t *testing.T) {
	const flood = 32 << 20
	// atomic: the handler goroutine writes it and the test body reads it.
	var served atomic.Int64
	h := sechostServer(t, &sechostHub{list: nil})
	// Replace the handler: every path answers 403 with an enormous body.
	h.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		served.Store(sechostFlood(w, "denied: ", flood))
	})
	_, err := sechostBackend(t, h).List(context.Background(), "journal/")
	if err == nil {
		t.Fatal("403 should be an error")
	}
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("a hub 403 must wrap ErrForbidden: %v", err)
	}
	if len(err.Error()) > 4096 {
		t.Fatalf("the hub chose a %d-byte error string", len(err.Error()))
	}
	if served.Load() > 1<<20 {
		t.Fatalf("read %d bytes of a 403 body", served.Load())
	}
}

// A 403 from the presigned target is the object store, not the hub: mapping it
// to ErrForbidden would park a healthy device in permanent read-only. A hostile
// hub must not be able to reach that state through the direct-upload path.
func TestSec_HostileHub_APresignedTargetCannotForceReadOnly(t *testing.T) {
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "AccessDenied", http.StatusForbidden)
	}))
	defer storage.Close()
	h := sechostServer(t, &sechostHub{sign: func(key string, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"mode":"direct","exists":false,"url":%q,"method":"PUT"}`, storage.URL+"/blob")
	}})
	err := sechostBackend(t, h).Put(context.Background(), "blobs/"+strings.Repeat("a", 64), strings.NewReader("hi"), 2)
	if err == nil {
		t.Fatal("a 403 from the upload target must fail the put")
	}
	if errors.Is(err, ErrForbidden) {
		t.Fatalf("an object-store 403 must not read as an authorization answer: %v", err)
	}
}
