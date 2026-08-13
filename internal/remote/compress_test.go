package remote

import (
	"bytes"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The probe consumes the bytes it judges, so the stream it hands back must be
// byte-identical to the one it was given — for a stream longer than the probe
// window, one shorter, and an empty one. A probe that eats bytes corrupts every
// push and pull that runs through it, and the damage surfaces as a sha
// mismatch nowhere near this file.
func TestCompressibleRejoinsTheStream(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"text past the window", []byte(strings.Repeat("package main // hello hello\n", 5000)), true},
		{"text under the window", []byte(strings.Repeat("hello beardrive\n", 100)), true},
		{"already compressed", randomBytes(200 << 10), false},
		{"tiny", []byte("hi"), false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, worth, err := Compressible(bytes.NewReader(c.in))
			if err != nil {
				t.Fatal(err)
			}
			if worth != c.want {
				t.Errorf("worth = %v, want %v", worth, c.want)
			}
			rejoined, err := io.ReadAll(got)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(rejoined, c.in) {
				t.Fatalf("rejoined stream is %d bytes, want the original %d", len(rejoined), len(c.in))
			}
		})
	}
}

func TestAcceptsGzip(t *testing.T) {
	cases := map[string]bool{
		"":                          false,
		"identity":                  false,
		"gzip":                      true,
		"deflate, gzip;q=1.0, *;q=0": true,
		"GZIP":                      true,
		"x-gzip":                    false, // a different token, not a prefix match
	}
	for hdr, want := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if hdr != "" {
			r.Header.Set("Accept-Encoding", hdr)
		}
		if got := AcceptsGzip(r); got != want {
			t.Errorf("AcceptsGzip(%q) = %v, want %v", hdr, got, want)
		}
	}
}

// randomBytes stands in for already-compressed content (JPEG, zip, model
// weights): incompressible by construction, which is the whole point.
func randomBytes(n int) []byte {
	b := make([]byte, n)
	rng := rand.New(rand.NewSource(1))
	rng.Read(b)
	return b
}
