package webapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/remote"
)

// Two doors, one request each, and the header the browser sets for each.
// Sec-Fetch-* is the whole discriminator, so every test below drives it
// explicitly rather than relying on httptest's bare request.
type imgDoor struct {
	name string
	hdr  map[string]string
}

// imgSubresource: <img src>, srcset, CSS url(), a favicon. dest: image is set
// by the browser and cannot be set by a page (fetch() forces `empty`).
func imgSubresource() imgDoor {
	return imgDoor{"image subresource", map[string]string{
		"Sec-Fetch-Dest": "image", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Site": "same-origin",
	}}
}

// imgInDocument: an <image href> inside an inline SVG, which is where the
// nested image load comes from. No site header at all — the opaque origin has
// no site relationship to report.
func imgInDocument() imgDoor {
	return imgDoor{"image inside a document", map[string]string{
		"Sec-Fetch-Dest": "image", "Sec-Fetch-Mode": "no-cors",
	}}
}

// navigation: the file opened as a page — the viewer, a share link, a past
// version. This is the door the sandbox exists for.
func navigation() imgDoor {
	return imgDoor{"navigation", map[string]string{
		"Sec-Fetch-Dest": "document", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "same-origin",
	}}
}

// unsupported: a client that sends no Sec-Fetch-* at all. Meant to be the
// browser that predates the header; really it is curl.
func noSecFetch() imgDoor {
	return imgDoor{"no Sec-Fetch headers", nil}
}

func getAsDoor(t *testing.T, h http.Handler, path string, d imgDoor) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	if r.URL.RawQuery == "" {
		t.Fatalf("%s: query was stripped, the test would prove nothing", path)
	}
	for k, v := range d.hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func seedUpload(t *testing.T, h http.Handler, path, body string) {
	t.Helper()
	if rec := do(t, h, "PUT", "/api/upload/content?path="+path, []byte(body)); rec.Code != 200 {
		t.Fatalf("seed %s: %d %s", path, rec.Code, rec.Body)
	}
}

func imageTestHub(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	be, err := remote.Open(context.Background(), "file://"+dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { be.Close() })
	srv := &Server{
		Source: &RemoteSource{Backend: be, Device: webDevice},
		Volume: "imgtest", Refresh: 0,
		Upload: UploadConfig{Enabled: true},
	}
	return srv.Handler()
}

// The bug: a PNG or SVG embedded in a markdown doc was served with the same
// wall as the document it was embedded IN. A sandboxed image is not a
// sandboxed image — it is a broken one, which is what a reader saw wherever a
// screenshot should have been.
func TestImageSubresourceRendersInTheDocument(t *testing.T) {
	const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="8" height="8"><rect width="8" height="8"/></svg>`
	h := imageTestHub(t)
	seedUpload(t, h, "assets%2Ficon.svg", svg)

	for _, d := range []imgDoor{imgSubresource(), imgInDocument()} {
		rec := getAsDoor(t, h, "/api/file?path=assets/icon.svg&name=icon.svg", d)
		if rec.Code != 200 {
			t.Fatalf("%s: %d", d.name, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "image/svg+xml") {
			t.Errorf("%s: Content-Type = %q, want image/svg+xml — a demoted type is a broken picture too", d.name, got)
		}
		if csp := rec.Header().Get("Content-Security-Policy"); csp != "" {
			t.Errorf("%s: Content-Security-Policy = %q, want none — the sandbox turns the image into a broken icon", d.name, csp)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: nosniff dropped; it is what stops a browser sniffing stored bytes into a document", d.name)
		}
	}
}

// The wall the test above must not have punched: the same bytes, opened as a
// page, are still a sandboxed document.
func TestNavigatedSVGsAreStillSandboxed(t *testing.T) {
	const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="8" height="8"><script>alert(1)</script></svg>`
	h := imageTestHub(t)
	seedUpload(t, h, "assets%2Fevil.svg", svg)

	for _, d := range []imgDoor{navigation(), noSecFetch()} {
		rec := getAsDoor(t, h, "/api/file?path=assets/evil.svg&name=evil.svg", d)
		if csp := rec.Header().Get("Content-Security-Policy"); csp != inlineSandbox {
			t.Errorf("%s: Content-Security-Policy = %q, want %q", d.name, csp, inlineSandbox)
		}
	}
}

// A type the hub cannot vouch for gets no exemption even from the image door:
// the allowlist fails closed, so a content type nobody has listed yet stays a
// sandboxed document rather than becoming a script context. Content-Type is
// what the allowlist decides on, so this drives the helper directly for a type
// no upload path can even produce, then checks the header the route emits.
func TestUnknownImageTypeIsNotExempt(t *testing.T) {
	h := imageTestHub(t)
	seedUpload(t, h, "assets%2Ficon.svg", "<svg/>")

	rec := getAsDoor(t, h, "/api/file?path=assets/icon.svg&name=plot.svg", imgSubresource())
	if rec.Code != 200 {
		t.Fatalf("svg: %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "image/svg") {
		t.Fatalf("premise gone: %q — the name did not set the type", got)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp != "" {
		t.Errorf("known type: Content-Security-Policy = %q, want none", csp)
	}

	for _, ct := range []string{"image/svg+xmlz", "application/xml", "text/html", "image/unknown"} {
		if safeRaster(ct) {
			t.Errorf("safeRaster(%q) = true — the allowlist must fail closed", ct)
		}
	}
	if !safeRaster("image/svg+xml; charset=utf-8") {
		t.Errorf("safeRaster lost the parameter-stripping: a typed SVG would be sandboxed again")
	}
}

// An attachment is not rendered, and TestInlineHTMLIsSandboxed pins that the
// download answer carries a disposition INSTEAD of a CSP. Nothing here may
// change that.
func TestImageDownloadKeepsItsDisposition(t *testing.T) {
	h := imageTestHub(t)
	seedUpload(t, h, "assets%2Ficon.svg", "<svg/>")

	rec := getAsDoor(t, h, "/api/download?path=assets/icon.svg&name=icon.svg", navigation())
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Disposition"), "icon.svg") {
		t.Errorf("download: %d %q", rec.Code, rec.Header().Get("Content-Disposition"))
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp != "" {
		t.Errorf("download: Content-Security-Policy = %q, want none", csp)
	}
}

// The share page serves stored bytes too, and a public link to an SVG is a
// page somebody opens, not an image anything embeds. It keeps the wall — a
// request WITHOUT a share token can only prove the route refuses, so this
// drives the same bytes through the inline door a /s/ page uses.
func TestSharePageBytesKeepTheWall(t *testing.T) {
	h := imageTestHub(t)
	seedUpload(t, h, "assets%2Fshared.svg", "<svg/>")

	rec := getAsDoor(t, h, "/api/file?path=assets/shared.svg&name=shared.svg", navigation())
	if csp := rec.Header().Get("Content-Security-Policy"); csp != inlineSandbox {
		t.Errorf("inline svg as a page: Content-Security-Policy = %q, want %q", csp, inlineSandbox)
	}
}
