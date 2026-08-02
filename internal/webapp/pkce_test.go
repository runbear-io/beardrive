package webapp

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The loopback sign-in flow with PKCE, end to end: the CLI's own happy path
// must keep working (nothing else in the suite exercises a flow that carries a
// code_challenge), a code minted for a flow that bound itself is redeemable
// only with that flow's verifier, and a pre-PKCE CLI on a new hub still works.
func TestCLIBrowserLoginPKCERoundTrip(t *testing.T) {
	srv, _, _ := authHub(t, true)
	h := srv.Handler()
	cookie := signupAndSession(t, h, "dev@x.io", "Dev", "password1")

	const verifier = "0f1e2d3c4b5a69788796a5b4c3d2e1f0"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	// approve runs the browser half and returns the one-time code the CLI's
	// loopback listener would receive. extra is appended to the query.
	approve := func(extra string) string {
		t.Helper()
		u := "/auth/cli?redirect=" + url.QueryEscape("http://127.0.0.1:53123/callback") + "&state=s" + extra
		req := httptest.NewRequest("POST", u, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("approve: %d %s", rec.Code, rec.Body)
		}
		cb, err := url.Parse(rec.Header().Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		return cb.Query().Get("code")
	}
	exchange := func(code, v string) *httptest.ResponseRecorder {
		body := map[string]string{"code": code, "device": "laptop"}
		if v != "" {
			body["code_verifier"] = v
		}
		return do(t, h, "POST", "/api/auth/exchange", body)
	}

	// The real CLI's flow: challenge in, verifier out.
	if got := exchange(approve("&code_challenge="+challenge+"&code_challenge_method=S256"), verifier); got.Code != 200 ||
		!strings.Contains(got.Body.String(), "dev@x.io") {
		t.Fatalf("the CLI's own sign-in no longer completes: %d %s", got.Code, got.Body)
	}
	// The wrong verifier buys nothing.
	if got := exchange(approve("&code_challenge="+challenge), "not-the-verifier"); got.Code != http.StatusUnauthorized {
		t.Fatalf("a wrong code_verifier was accepted: %d %s", got.Code, got.Body)
	}
	// A code minted by a flow that bound nothing — what a local attacker who
	// only read the printed URL can arrange — is refused for a CLI that did.
	if got := exchange(approve(""), verifier); got.Code != http.StatusUnauthorized {
		t.Fatalf("a challenge-less code was redeemed by a PKCE client: %d %s", got.Code, got.Body)
	}
	// A pre-PKCE CLI on a new hub still signs in.
	if got := exchange(approve(""), ""); got.Code != 200 {
		t.Fatalf("an older CLI can no longer sign in: %d %s", got.Code, got.Body)
	}
}
