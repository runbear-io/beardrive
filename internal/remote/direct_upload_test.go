package remote

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Presigned uploads never share one HTTP/2 connection, even against storage
// that offers it: a write error on a shared connection force-closes it and
// fails every parallel upload riding on it.
func TestDirectUploadsUseHTTP1(t *testing.T) {
	var proto string
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proto = r.Proto
	}))
	ts.EnableHTTP2 = true
	ts.StartTLS()
	defer ts.Close()

	tr := directUploadTransport.Clone()
	tr.TLSClientConfig = ts.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	b := &httpBackend{base: "https://hub.example", direct: &http.Client{Transport: tr}}
	plan := putPlan{Mode: "direct", URL: ts.URL + "/bucket/blobs/x", Method: http.MethodPut}
	if err := b.putDirect(t.Context(), plan, strings.NewReader("abc"), 3); err != nil {
		t.Fatal(err)
	}
	if proto != "HTTP/1.1" {
		t.Fatalf("direct upload used %s, want HTTP/1.1", proto)
	}
}
