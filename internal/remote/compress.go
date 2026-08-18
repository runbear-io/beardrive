package remote

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
)

// Transport compression for the sync wire. Nothing on it was compressed:
// blobs and journals crossed as raw application/octet-stream in both
// directions, while the corpus they carry is 5–10 KB markdown and source
// files that gzip ~3.4x (TestCompressionTextCorpusRatio measures it).
//
// This is a TRANSPORT concern only, and the whole feature depends on that
// staying true: content addressing is over the UNCOMPRESSED bytes, storage
// holds uncompressed objects, and the journal format is untouched. A hub that
// stored a gzip body under the sha256 of its plaintext would have broken
// every device's blob check, so the hub inflates before it hashes
// (handleStorePut) and never after.
//
// The codec is gzip rather than zstd for one decisive reason: net/http already
// sends `Accept-Encoding: gzip` on every request whose caller did not set that
// header itself, and transparently inflates the response. httpBackend.do does
// not set it, so the entire pull leg compresses for binaries built before this
// existed, with no client change at all. zstd would forfeit that.

// probeWindow is how much of a stream is sampled to decide whether the rest is
// worth compressing. Big enough to see past a file header, small enough that
// the sample is a buffer rather than a spool.
const probeWindow = 64 << 10

// probeMargin is the share of the sample compression has to save before it is
// worth paying for. Already-compressed content (JPEG, zip, model weights) gets
// ~0.1% BIGGER under gzip, so the margin is really a sign test with slack.
const probeMargin = 0.9

// Compressible reports whether a stream is worth gzipping, by compressing its
// first probeWindow bytes and checking that the sample actually shrank.
//
// It returns the stream REJOINED — the sampled bytes followed by whatever is
// left — because the probe has to consume the bytes it judges. That identity is
// the property this helper lives or dies on: a probe that eats bytes silently
// corrupts every push and every pull that runs through it, and the failure
// shows up as a sha mismatch far from here. compress_test.go asserts it for a
// stream longer than the window, one shorter, and an empty one.
func Compressible(r io.Reader) (io.Reader, bool, error) {
	sample, err := io.ReadAll(io.LimitReader(r, probeWindow))
	rejoined := io.MultiReader(bytes.NewReader(sample), r)
	if err != nil {
		return rejoined, false, err
	}
	if len(sample) == 0 {
		return rejoined, false, nil
	}
	var n countingSink
	gz := gzip.NewWriter(&n)
	if _, err := gz.Write(sample); err != nil {
		return rejoined, false, err
	}
	if err := gz.Close(); err != nil {
		return rejoined, false, err
	}
	return rejoined, float64(n) < float64(len(sample))*probeMargin, nil
}

type countingSink int

func (c *countingSink) Write(p []byte) (int, error) { *c += countingSink(len(p)); return len(p), nil }

// AcceptsGzip reports whether a request's Accept-Encoding allows a gzipped
// answer. Go's own transport sets that header on every request this package
// makes, which is why old devices get the compressed pull leg for free.
func AcceptsGzip(r *http.Request) bool {
	for _, enc := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if name, _, _ := strings.Cut(enc, ";"); strings.EqualFold(strings.TrimSpace(name), "gzip") {
			return true
		}
	}
	return false
}
