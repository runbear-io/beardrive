package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/webapp"
)

// orgHub builds an auth+org hub and returns two device tokens (alice, bob)
// plus alice's project. Bob is in his own org, so alice's project must be a
// wall for his devices.
func orgHub(t *testing.T, storage remote.Backend) (ts *httptest.Server, aliceTok, bobTok string, pa webapp.Project) {
	t.Helper()
	dir := t.TempDir()
	db, err := webapp.OpenProjectDB(filepath.Join(dir, "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := webapp.OpenBuiltinAuth(filepath.Join(dir, "auth.json"), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	orgs, err := webapp.OpenOrgDB(filepath.Join(dir, "orgs.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &webapp.Server{
		Root: storage, Projects: db, Refresh: 0,
		Upload: webapp.UploadConfig{Enabled: true},
		Auth:   auth, Dir: webapp.LocalDirectory{OrgDB: orgs},
	}
	ts = httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	aliceTok = signupDeviceToken(t, ts, "alice@x.io", "Alice")
	bobTok = signupDeviceToken(t, ts, "bob@x.io", "Bob")

	// alice creates her project (she lands in a fresh org of her own)
	req, _ := http.NewRequest("POST", ts.URL+"/api/projects", strings.NewReader(`{"name":"wiki"}`))
	req.Header.Set("Authorization", "Bearer "+aliceTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Project webapp.Project `json:"project"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return ts, aliceTok, bobTok, out.Project
}

// signupDeviceToken drives the real browser + CLI-callback flow: signup for
// a session cookie, /auth/cli mints a one-time code, /api/auth/exchange
// trades it for a device token.
func signupDeviceToken(t *testing.T, ts *httptest.Server, email, name string) string {
	t.Helper()
	jarless := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	form := url.Values{"email": {email}, "name": {name}, "password": {"password1"}}
	resp, err := jarless.PostForm(ts.URL+"/auth/signup", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "bdrive_session" {
			session = c
		}
	}
	if session == nil {
		t.Fatalf("signup(%s) set no session cookie: %d", email, resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", ts.URL+"/auth/cli?redirect="+url.QueryEscape("http://127.0.0.1:1/cb")+"&state=s", nil)
	req.AddCookie(session)
	resp, err = jarless.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Query().Get("code") == "" {
		t.Fatalf("cli login redirect = %q", resp.Header.Get("Location"))
	}

	body, _ := json.Marshal(map[string]string{"code": loc.Query().Get("code"), "device": "test"})
	resp, err = http.Post(ts.URL+"/api/auth/exchange", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.Token == "" {
		t.Fatalf("exchange: %v %+v", err, tok)
	}
	return tok.Token
}

// A device signed in to the wrong org can neither push into nor pull from a
// project: sync degrades to offline (never partial access) and no data
// crosses the wall in either direction.
func TestOrgWallsDeviceSync(t *testing.T) {
	storage := sharedRemote(t)
	ts, aliceTok, bobTok, pa := orgHub(t, storage)

	// alice's device syncs her project and shares a file
	os.Setenv("BDRIVE_TOKEN", aliceTok)
	defer os.Unsetenv("BDRIVE_TOKEN")
	viaA, err := remote.Open(context.Background(), ts.URL+"/p/"+pa.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer viaA.Close()
	a := newDevice(t, "alicedev", viaA)
	write(t, a.Folder, "secret.md", "org A only")
	if res := cycle(t, a); res.Offline {
		t.Fatalf("alice's own project must sync: %v", res.OfflineErr)
	}

	// bob's device pointed at alice's project: pull gets nothing
	os.Setenv("BDRIVE_TOKEN", bobTok)
	viaB, err := remote.Open(context.Background(), ts.URL+"/p/"+pa.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer viaB.Close()
	b := newDevice(t, "bobdev", viaB)
	res, err := b.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Offline {
		t.Fatal("cross-org sync must degrade to offline, not succeed")
	}
	if _, err := os.Stat(filepath.Join(b.Folder, "secret.md")); err == nil {
		t.Fatal("org A's file leaked to a device in org B")
	}

	// ...and his local edits never reach the store
	time.Sleep(10 * time.Millisecond)
	write(t, b.Folder, "intruder.md", "should not land")
	if _, err := b.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	direct := newDevice(t, "directdev", remote.Prefixed(storage, pa.ID))
	cycle(t, direct)
	if _, err := os.Stat(filepath.Join(direct.Folder, "intruder.md")); err == nil {
		t.Fatal("a non-member's write crossed into the project")
	}
	if got := read(t, direct.Folder, "secret.md"); got != "org A only" {
		t.Fatalf("direct device should still converge on alice's data, got %q", got)
	}
}
