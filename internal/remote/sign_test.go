package remote

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

// file:// must NOT be a PutSigner: a filesystem path presigned to a browser
// is meaningless, so callers fall back to uploading through the server.
func TestLocalBackendCannotSign(t *testing.T) {
	be, err := Open(context.Background(), "file://"+t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	if _, ok := be.(PutSigner); ok {
		t.Fatal("file:// backend must not implement PutSigner")
	}
}

// Presigning is a local signature computation — no network needed — so it is
// unit-testable with static fake credentials.
func TestS3SignPut(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAFAKEFAKEFAKEFAKE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fakefakefakefakefakefakefakefakefakefake")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ENDPOINT_URL", "")

	be, err := Open(context.Background(), "s3://test-bucket/vol1")
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	signer, ok := be.(PutSigner)
	if !ok {
		t.Fatal("s3 backend must implement PutSigner")
	}

	ttl := 5 * time.Minute
	sp, err := signer.SignPut(context.Background(), "blobs/abc123", 42, ttl)
	if err != nil {
		t.Fatal(err)
	}
	if sp.Method != "PUT" {
		t.Fatalf("method = %q", sp.Method)
	}
	u, err := url.Parse(sp.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u.Host, "test-bucket") || !strings.HasSuffix(u.Path, "/vol1/blobs/abc123") {
		t.Fatalf("url = %s, want bucket + prefixed key", sp.URL)
	}
	q := u.Query()
	if q.Get("X-Amz-Signature") == "" {
		t.Fatalf("url not signed: %s", sp.URL)
	}
	if q.Get("X-Amz-Expires") != "300" {
		t.Fatalf("expires = %s, want 300s", q.Get("X-Amz-Expires"))
	}
	// the URL must be self-contained: no secret key material leaks into it
	if strings.Contains(sp.URL, "fakefakefake") {
		t.Fatal("secret key leaked into signed URL")
	}
	if sp.Expires.Before(time.Now()) || sp.Expires.After(time.Now().Add(ttl+time.Minute)) {
		t.Fatalf("expires = %v, want ~now+%v", sp.Expires, ttl)
	}
	// the signed content-length pins the upload to the declared size
	if got := sp.Headers["Content-Length"]; got != "42" {
		t.Fatalf("signed content-length = %q, want 42 (headers: %v)", got, sp.Headers)
	}
}
