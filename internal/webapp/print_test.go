package webapp

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The app shell cannot print synced HTML: it renders in a cross-origin frame,
// and a cross-origin frame prints clipped to its box. So the Print button
// opens a top-level print view instead, and this is the contract it relies on
// — the document self-prints, and it does so with EXACTLY one extra sandbox
// flag. The moment allow-same-origin appears here, synced markup is running
// with the hub origin's session and every wall in this package is moot.
func TestPrintViewSelfPrintsInsideTheSandbox(t *testing.T) {
	h := dirServer(t, map[string]string{
		"page.html": "<h1>hi</h1>",
		"plan.md":   "# md",
		"pic.svg":   "<svg xmlns='http://www.w3.org/2000/svg'/>",
	})

	rec := get(t, h, "/api/file?path=page.html&print=1")
	if rec.Code != 200 {
		t.Fatalf("print view: %d %s", rec.Code, rec.Body)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp != "sandbox allow-scripts allow-modals" {
		t.Fatalf("print CSP = %q, want the inline sandbox plus allow-modals only", csp)
	}
	if strings.Contains(rec.Header().Get("Content-Security-Policy"), "allow-same-origin") {
		t.Fatal("print view handed synced HTML the hub's own origin")
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "<h1>hi</h1>") {
		t.Fatalf("print view must serve the stored bytes first: %q", body)
	}
	if !strings.HasSuffix(body, printSuffix) {
		t.Fatalf("print view never presses Print: %q", body)
	}
	// A promised length measured on the source would truncate the script
	// straight back off the response.
	if cl := rec.Header().Get("Content-Length"); cl != "" {
		n, _ := strconv.Atoi(cl)
		if n != len(body) {
			t.Fatalf("Content-Length %s but body is %d bytes — the print script is cut off", cl, len(body))
		}
	}

	// Everything else is untouched: no flag, no suffix. print=1 on a type the
	// suffix would corrupt (or that the app prints in place anyway) is inert.
	for _, u := range []string{
		"/api/file?path=page.html",             // the ordinary render
		"/api/file?path=plan.md&print=1",       // markdown prints from the app's own DOM
		"/api/file?path=pic.svg&print=1",       // not text/html
		"/api/download?path=page.html&print=1", // an attachment is saved, not rendered
	} {
		rec := get(t, h, u)
		if rec.Code != 200 {
			t.Fatalf("%s: %d", u, rec.Code)
		}
		if csp := rec.Header().Get("Content-Security-Policy"); strings.Contains(csp, "allow-modals") {
			t.Errorf("%s: CSP = %q, want no print relaxation", u, csp)
		}
		if strings.Contains(rec.Body.String(), "print()") {
			t.Errorf("%s: body carries the print script: %q", u, rec.Body)
		}
	}
}

// The version door serves the same bytes as the live one, so it must not
// differ in its wall — and printing a pinned ?v= has to print THAT version,
// not quietly fall back to the current bytes.
func TestPrintViewOnAPinnedVersion(t *testing.T) {
	srv, p, root := newHub(t, false, nil)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.put("dev1", "report.html", "<p>v1</p>")
	f.put("dev1", "report.html", "<p>v2</p>")

	h := srv.Handler()
	base := "/api/p/" + p.ID + "/"
	sum := sha256.Sum256([]byte("<p>v1</p>"))
	old := hex.EncodeToString(sum[:])

	rec := do(t, h, "GET", base+"blob?sha="+old+"&name=report.html&print=1", nil)
	if rec.Code != 200 {
		t.Fatalf("pinned print view: %d %s", rec.Code, rec.Body)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp != "sandbox allow-scripts allow-modals" {
		t.Fatalf("pinned print CSP = %q", csp)
	}
	if body := rec.Body.String(); !strings.HasPrefix(body, "<p>v1</p>") || !strings.HasSuffix(body, printSuffix) {
		t.Fatalf("pinned print view = %q, want the OLD version plus the print script", body)
	}

	// Saving that version must still save the file, not the file plus a script.
	rec = do(t, h, "GET", base+"blob?sha="+old+"&name=report.html&download=1&print=1", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "<p>v1</p>" {
		t.Fatalf("download variant = %d %q, want the stored bytes verbatim", rec.Code, rec.Body)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); strings.Contains(csp, "allow-modals") {
		t.Fatalf("download variant relaxed the sandbox: %q", csp)
	}
}
