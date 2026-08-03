package webapp

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// secRegisterDevice is how a device becomes known to the hub: it SIGNS IN, and
// the hub binds the id it names to the account it just authenticated
// (DeviceRegistry.Bind, from BuiltinAuth.finishLogin).
//
// It used to push an empty journal instead, because a first journal write was
// the only thing that could claim an unowned id. That arm is gone: it admitted
// `!known && journalNames(dev, ops)`, a check whose evidence is a field the
// writer writes, so any member with write on any project could take any id that
// had not yet pushed — including every device of every read-only member, which
// can never reach that door to claim its own id at all. The helper follows the
// registration path, so it moved with it. Every assertion in its 13 callers is
// unchanged.
//
// The cookie is the browser session approving the sign-in, which is exactly
// what the loopback flow asks of the person at the keyboard.
func secRegisterDevice(t *testing.T, h http.Handler, projectID string, c *http.Cookie, id, name, os string) *httptest.ResponseRecorder {
	t.Helper()
	_ = projectID // registration is hub-wide now, not per project
	verifier := "0f1e2d3c4b5a69788796a5b4c3d2e1f0"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	approve := httptest.NewRequest("POST", "/auth/cli?redirect="+
		url.QueryEscape("http://127.0.0.1:53123/callback")+"&state=s"+
		"&code_challenge="+challenge+"&code_challenge_method=S256", nil)
	approve.AddCookie(c)
	arec := httptest.NewRecorder()
	h.ServeHTTP(arec, approve)
	if arec.Code != http.StatusSeeOther {
		t.Fatalf("secRegisterDevice: approving the sign-in failed: %d %s", arec.Code, arec.Body)
	}
	loc, err := url.Parse(arec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("secRegisterDevice: bad callback %q: %v", arec.Header().Get("Location"), err)
	}
	body, _ := json.Marshal(map[string]string{
		"code": loc.Query().Get("code"), "device": name, "code_verifier": verifier,
	})
	req := httptest.NewRequest("POST", "/api/auth/exchange", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Bdrive-Device", id)
	req.Header.Set("X-Bdrive-Device-Name", name)
	req.Header.Set("X-Bdrive-Os", os)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
