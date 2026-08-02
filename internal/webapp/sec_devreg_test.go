package webapp

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// secRegisterDevice is how a device becomes known to the hub: it pushes its
// own journal. Nothing else claims an id — a read grants nothing, so it may
// claim nothing (TestSec_Row5_NoReadRouteRegistersADeviceItHasNeverSeen), and
// a blob put says nothing about who a device is.
//
// The body is empty, which is an honest first sync: the ops are what a first
// claim is checked against (journalNames), and a device that has committed
// nothing yet has none. Fixtures written before the read doors stopped
// claiming used a GET /store/list for this; they call this instead.
func secRegisterDevice(t *testing.T, h http.Handler, projectID string, c *http.Cookie, id, name, os string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/api/p/"+projectID+"/store/object?key=journal/"+id+".jsonl", nil)
	req.Header.Set("X-Bdrive-Device", id)
	req.Header.Set("X-Bdrive-Device-Name", name)
	req.Header.Set("X-Bdrive-Os", os)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
