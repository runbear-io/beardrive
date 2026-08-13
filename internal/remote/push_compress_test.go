package remote

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The mixed-fleet contract for the push leg, both directions, at the one place
// it is decided: what sign() advertised.
//
// New client ↔ OLD hub is the dangerous half. An old hub does not inflate, so a
// gzipped body would be stored verbatim under the sha256 of its plaintext —
// rejected outright for a blob, silently mis-stored for a journal. The old hub
// says nothing about encodings, so the client must send raw.
func TestPushCompressesOnlyWhenTheHubAdvertisesIt(t *testing.T) {
	payload := strings.Repeat("# notes\nthe corpus is markdown and source, which gzips well\n", 500)

	for _, tc := range []struct {
		name       string
		signAnswer string
		wantGzip   bool
	}{
		{"old hub says nothing", `{"mode":"server"}`, false},
		{"old hub with an empty list", `{"mode":"server","accept_encoding":[]}`, false},
		{"hub speaks another codec", `{"mode":"server","accept_encoding":["zstd"]}`, false},
		{"new hub", `{"mode":"server","accept_encoding":["gzip"]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody []byte
			var gotEncoding string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/store/sign") {
					w.Header().Set("Content-Type", "application/json")
					w.Write([]byte(tc.signAnswer))
					return
				}
				gotEncoding = r.Header.Get("Content-Encoding")
				gotBody, _ = io.ReadAll(r.Body)
				w.Write([]byte(`{"ok":true}`))
			}))
			defer ts.Close()

			be, err := Open(context.Background(), ts.URL+"/p/p-0123abcd")
			if err != nil {
				t.Fatal(err)
			}
			defer be.Close()
			if err := be.Put(context.Background(), "blobs/"+strings.Repeat("a", 64),
				strings.NewReader(payload), int64(len(payload))); err != nil {
				t.Fatal(err)
			}

			if !tc.wantGzip {
				if gotEncoding != "" {
					t.Fatalf("Content-Encoding = %q, want none", gotEncoding)
				}
				if string(gotBody) != payload {
					t.Fatal("body is not the plaintext it was handed")
				}
				return
			}
			if gotEncoding != "gzip" {
				t.Fatalf("Content-Encoding = %q, want gzip", gotEncoding)
			}
			if len(gotBody) >= len(payload) {
				t.Fatalf("compressed body is %d bytes, larger than the %d it started as", len(gotBody), len(payload))
			}
			gz, err := gzip.NewReader(bytes.NewReader(gotBody))
			if err != nil {
				t.Fatal(err)
			}
			plain, err := io.ReadAll(gz)
			if err != nil {
				t.Fatal(err)
			}
			// Content addressing is over the UNCOMPRESSED bytes: what the hub
			// inflates has to be exactly what the key names.
			if string(plain) != payload {
				t.Fatal("the body does not inflate to what was pushed")
			}
		})
	}
}

// Incompressible content must cross untouched even against a hub that offers
// gzip — chunked large files are mostly already-compressed binary, where gzip
// pays CPU to make the payload ~0.1% bigger.
func TestPushSkipsIncompressibleContent(t *testing.T) {
	payload := randomBytes(300 << 10)
	var gotEncoding string
	var gotLen int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/store/sign") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"mode":"server","accept_encoding":["gzip"]}`))
			return
		}
		gotEncoding = r.Header.Get("Content-Encoding")
		body, _ := io.ReadAll(r.Body)
		gotLen = len(body)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	be, err := Open(context.Background(), ts.URL+"/p/p-0123abcd")
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	if err := be.Put(context.Background(), "blobs/"+strings.Repeat("b", 64),
		bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	if gotEncoding != "" {
		t.Fatalf("Content-Encoding = %q on incompressible content, want none", gotEncoding)
	}
	if gotLen != len(payload) {
		t.Fatalf("wire carried %d bytes for a %d-byte payload", gotLen, len(payload))
	}
}
