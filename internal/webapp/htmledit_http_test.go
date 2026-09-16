package webapp

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The editable view is a VIEW. Every test here is really one assertion from a
// different angle: asking for it must never change what the file is, who may
// read it, or what any other door answers.

func TestEditViewStampsHTML(t *testing.T) {
	h := dirServer(t, map[string]string{"page.html": "<body><p>Hello</p></body>"})

	rec := get(t, h, "/api/file?path=page.html&edit=1")
	if rec.Code != 200 {
		t.Fatalf("edit view: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, srcAttr+`="`) {
		t.Fatalf("nothing stamped:\n%s", body)
	}
	if !strings.Contains(body, `<script src="`+editScriptURL+`"`) {
		t.Fatalf("bootstrap missing:\n%s", body)
	}
	// A wrong length truncates the bootstrap right back off, which is exactly
	// the bug the print view already had to learn about.
	if cl := rec.Header().Get("Content-Length"); cl != "" {
		if n, _ := strconv.Atoi(cl); n != len(body) {
			t.Fatalf("Content-Length %s but body is %d bytes", cl, len(body))
		}
	}
}

// The sandbox is the reason synced HTML can be rendered at all. Editing must
// not buy itself a single extra flag — allow-same-origin here would hand every
// synced page the hub's session.
func TestEditViewKeepsTheSandbox(t *testing.T) {
	h := dirServer(t, map[string]string{"page.html": "<p>hi</p>"})

	plain := get(t, h, "/api/file?path=page.html").Header().Get("Content-Security-Policy")
	edit := get(t, h, "/api/file?path=page.html&edit=1").Header().Get("Content-Security-Policy")
	if edit != plain {
		t.Fatalf("edit CSP %q differs from the plain render's %q", edit, plain)
	}
	if strings.Contains(edit, "allow-same-origin") {
		t.Fatalf("edit view handed synced HTML the hub's own origin: %q", edit)
	}
}

// Both views come off the same blob. Sharing an ETag would let a cache answer
// an ?edit=1 request with unstamped bytes, and the page would have nothing to
// click for no reason anyone could see.
func TestEditViewHasItsOwnETag(t *testing.T) {
	h := dirServer(t, map[string]string{"page.html": "<p>hi</p>"})

	plain := get(t, h, "/api/file?path=page.html").Header().Get("ETag")
	edit := get(t, h, "/api/file?path=page.html&edit=1").Header().Get("ETag")
	if plain == "" || edit == "" {
		t.Fatalf("missing ETag: plain=%q edit=%q", plain, edit)
	}
	if plain == edit {
		t.Fatalf("both views answer with ETag %q", plain)
	}
	// A conditional request carrying the plain tag must not be answered 304
	// with the edit view in mind, or vice versa.
	req := httptest.NewRequest("GET", "/api/file?path=page.html&edit=1", nil)
	req.Header.Set("If-None-Match", plain)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotModified {
		t.Fatal("edit view answered 304 to the plain render's ETag")
	}
}

// A download is saved, not rendered. Markers in a file on someone's disk would
// be the one thing this feature promises never to do.
func TestEditViewNeverTouchesADownload(t *testing.T) {
	const src = "<body><p>Hello</p></body>"
	h := dirServer(t, map[string]string{"page.html": src})

	rec := get(t, h, "/api/download?path=page.html&edit=1")
	if body := rec.Body.String(); body != src {
		t.Fatalf("download was rewritten:\n got %q\nwant %q", body, src)
	}
	if strings.Contains(rec.Body.String(), srcAttr) {
		t.Fatal("download carries edit markers")
	}
}

// Stamping cannot stream, so it reads ahead — and past the ceiling it has to
// give those bytes BACK. Serving the prefix it happened to buffer would hand a
// reader a truncated document that looks like a corrupted file, which is a far
// worse answer than "this one is not clickable".
func TestEditViewTooLargeServesTheWholeFile(t *testing.T) {
	// One byte over the cap, and shaped so a truncation is unmistakable: the
	// closing tag only exists in the tail that read-ahead consumed.
	filler := strings.Repeat("<p>x</p>\n", (maxEditableHTML/9)+1)
	src := "<body>\n" + filler + "<p>LAST</p></body>"
	if len(src) <= maxEditableHTML {
		t.Fatalf("fixture is only %d bytes, not over the %d cap", len(src), maxEditableHTML)
	}
	h := dirServer(t, map[string]string{"big.html": src})

	rec := get(t, h, "/api/file?path=big.html&edit=1")
	if rec.Code != 200 {
		t.Fatalf("edit view on a large file: %d", rec.Code)
	}
	if got := rec.Body.String(); got != src {
		t.Fatalf("body is %d bytes, want the whole %d — truncated at the read-ahead",
			len(got), len(src))
	}
	if strings.Contains(rec.Body.String(), srcAttr) {
		t.Error("a file past the cap was stamped anyway")
	}
}

// Only HTML has a DOM to click. Anything else must fall straight through, or a
// stray ?edit=1 would corrupt a markdown or CSV render.
func TestEditViewIgnoresNonHTML(t *testing.T) {
	files := map[string]string{
		"plan.md":   "# heading\n\ntext",
		"data.csv":  "a,b\n1,2",
		"notes.txt": "plain",
	}
	h := dirServer(t, files)
	for name, want := range files {
		rec := get(t, h, "/api/file?path="+name+"&edit=1")
		if got := rec.Body.String(); got != want {
			t.Errorf("%s was rewritten:\n got %q\nwant %q", name, got, want)
		}
	}
}

// The print view predates this one and has its own contract. Neither may
// quietly become the other.
func TestEditAndPrintViewsStaySeparate(t *testing.T) {
	h := dirServer(t, map[string]string{"page.html": "<p>hi</p>"})

	pr := get(t, h, "/api/file?path=page.html&print=1").Body.String()
	if strings.Contains(pr, srcAttr) {
		t.Fatal("print view carries edit markers")
	}
	if !strings.HasSuffix(pr, printSuffix) {
		t.Fatal("print view stopped self-printing")
	}
	ed := get(t, h, "/api/file?path=page.html&edit=1").Body.String()
	if strings.Contains(ed, printSuffix) {
		t.Fatal("edit view self-prints")
	}
}

// Without ?edit=1 nothing changes for anybody. This is the regression that
// would otherwise be found by a reader noticing their page looks different.
func TestPlainRenderIsUntouched(t *testing.T) {
	const src = "<!DOCTYPE html>\n<body>\n  <p>Hello</p>\n</body>"
	h := dirServer(t, map[string]string{"page.html": src})

	if got := get(t, h, "/api/file?path=page.html").Body.String(); got != src {
		t.Fatalf("plain render changed:\n got %q\nwant %q", got, src)
	}
}

// ---- who is offered the editable view ----

// editHub is permHub with an HTML file in it, in the project root and inside a
// subfolder, so a folder rule has something to be a rule about.
func editHub(t *testing.T) (http.Handler, *Server, map[string]*http.Cookie, Project) {
	t.Helper()
	h, srv, c, p, root := permHubAt(t)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.put("seed", "page.html", "<body><p>Hello</p></body>")
	f.put("seed", "docs/page.html", "<body><p>Docs</p></body>")
	return h, srv, c, p
}

func editable(t *testing.T, h http.Handler, p Project, path string, c *http.Cookie) bool {
	t.Helper()
	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/file?path="+path+"&edit=1", nil, c)
	if rec.Code != 200 {
		t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
	}
	return strings.Contains(rec.Body.String(), srcAttr+`="`)
}

// A reader asking for ?edit=1 gets the file, not a 403 — the Edit button is
// never offered to them, so reaching this is a stale tab or a typed URL, and
// refusing to render a file someone may read would be the wrong answer to it.
// What they must not get is something to click.
func TestEditViewIsOfferedOnlyToWriters(t *testing.T) {
	h, srv, c, p := editHub(t)

	if !editable(t, h, p, "page.html", c["bob"]) {
		t.Error("a member with write is not offered the editable view")
	}
	if err := srv.Projects.SetPerm(p.ID, "carol@x.io", PermRead); err != nil {
		t.Fatal(err)
	}
	if editable(t, h, p, "page.html", c["carol"]) {
		t.Error("a read-only member was handed editable markers")
	}
	// An outsider is refused by the route itself, long before any of this.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/file?path=page.html&edit=1", nil, c["dave"]); rec.Code == 200 {
		t.Errorf("an outsider read the file at all: %d %s", rec.Code, rec.Body)
	}
}

// Folder rules are the case the Edit button itself already respects. The view
// behind it has to agree, or a read-only subtree shows clickable text that
// 403s on the first save.
func TestEditViewRespectsFolderRules(t *testing.T) {
	h, srv, c, p := editHub(t)

	rule := map[string]any{"prefix": "docs", "default": PermRead}
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", rule, c["alice"]); rec.Code != 200 {
		t.Fatalf("set rule: %d %s", rec.Code, rec.Body)
	}
	if editable(t, h, p, "docs/page.html", c["bob"]) {
		t.Error("a read-only FOLDER still offered the editable view")
	}
	if !editable(t, h, p, "page.html", c["bob"]) {
		t.Error("a rule on docs/ stopped the project root being editable")
	}
	// An org owner is never lockable out of a subtree — same break-glass
	// every other door gives them.
	if !editable(t, h, p, "docs/page.html", c["alice"]) {
		t.Error("the org owner was locked out of a read-only folder")
	}
	_ = srv
}
