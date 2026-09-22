package remote

import (
	"context"
	"errors"
	"fmt"
	"google.golang.org/api/googleapi"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// gcsBackend stores objects in Google Cloud Storage using Application
// Default Credentials (gcloud auth application-default login, or a service
// account via GOOGLE_APPLICATION_CREDENTIALS).
type gcsBackend struct {
	client *gcs.Client
	bucket *gcs.BucketHandle
	prefix string
}

func newGCS(ctx context.Context, bucket, prefix string) (*gcsBackend, error) {
	if bucket == "" {
		return nil, fmt.Errorf("gs remote needs a bucket: gs://bucket/prefix")
	}
	client, err := gcs.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create GCS client: %w", err)
	}
	return &gcsBackend{client: client, bucket: client.Bucket(bucket), prefix: prefix}, nil
}

func (b *gcsBackend) key(key string) string {
	if b.prefix == "" {
		return key
	}
	return path.Join(b.prefix, key)
}

func (b *gcsBackend) Put(ctx context.Context, key string, r io.Reader, _ int64) error {
	w := b.bucket.Object(b.key(key)).NewWriter(ctx)
	if _, err := io.Copy(w, r); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

// SignPut mints a V4 signed PUT URL. Signing needs credentials that can sign
// bytes (a service account key, or iam.serviceAccounts.signBlob rights);
// plain end-user ADC cannot, in which case callers fall back to uploading
// through the server.
// SignPut mints a presigned PUT. The size is signed into the request, not just
// known to the hub: a presigned URL is a grant, and a grant that takes a body
// of any length is unmetered for its whole TTL — the hub quota-checked a number
// nothing then enforces. (The S3 arm signs Content-Length the same way.)
func (b *gcsBackend) SignPut(_ context.Context, key string, size int64, ttl time.Duration) (*SignedPut, error) {
	expires := time.Now().Add(ttl)
	length := strconv.FormatInt(size, 10)
	u, err := b.bucket.SignedURL(b.key(key), &gcs.SignedURLOptions{
		Scheme:  gcs.SigningSchemeV4,
		Method:  http.MethodPut,
		Expires: expires,
		Headers: []string{"Content-Length:" + length},
	})
	if err != nil {
		return nil, fmt.Errorf("sign gcs put: %w", err)
	}
	return &SignedPut{
		URL: u, Method: http.MethodPut, Expires: expires,
		Headers: map[string]string{"Content-Length": length},
	}, nil
}

func (b *gcsBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return b.bucket.Object(b.key(key)).NewReader(ctx)
}

func (b *gcsBackend) List(ctx context.Context, prefix string) ([]Object, error) {
	it := b.bucket.Objects(ctx, &gcs.Query{Prefix: b.key(prefix)})
	strip := b.prefix
	if strip != "" {
		strip += "/"
	}
	var out []Object
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		out = append(out, Object{Key: strings.TrimPrefix(attrs.Name, strip), Size: attrs.Size, Modified: attrs.Updated})
	}
	return out, nil
}

func (b *gcsBackend) Exists(ctx context.Context, key string) (bool, error) {
	_, err := b.bucket.Object(b.key(key)).Attrs(ctx)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, gcs.ErrObjectNotExist) {
		return false, nil
	}
	return false, err
}

func (b *gcsBackend) Close() error { return b.client.Close() }

// Compile-time: this is the one backend that offers compare-and-swap, and the
// hub's journal append silently falls back to last-writer-wins without it.
var _ ConditionalPutter = (*gcsBackend)(nil)

/* Compare-and-swap, which GCS gives us for free through object generations.

   A generation is GCS's own revision number for an object: every successful
   write produces a new one, and a precondition on it is evaluated by the
   storage service rather than by us. That is the whole point — two hub
   processes appending to one journal cannot be serialised by anything either
   of them holds, so the only place the check can be honest is the store.

   DoesNotExist is the VersionAbsent case rather than GenerationMatch(0),
   which GCS would read as "match generation zero" and refuse everything. */

func (b *gcsBackend) GetVersioned(ctx context.Context, key string) (io.ReadCloser, Version, error) {
	obj := b.bucket.Object(b.key(key))
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		return nil, VersionAbsent, err
	}
	// Read the generation we just looked at, not "latest": between Attrs and
	// NewReader another process may have written, and a body from a different
	// revision than the version we report is the exact bug this prevents.
	rc, err := obj.Generation(attrs.Generation).NewReader(ctx)
	if err != nil {
		return nil, VersionAbsent, err
	}
	return rc, gcsVersion(attrs.Generation), nil
}

func (b *gcsBackend) PutIf(ctx context.Context, key string, r io.Reader, _ int64, expect Version) (Version, error) {
	obj := b.bucket.Object(b.key(key))
	if expect == VersionAbsent {
		obj = obj.If(gcs.Conditions{DoesNotExist: true})
	} else {
		gen, err := strconv.ParseInt(string(expect), 10, 64)
		if err != nil {
			return VersionAbsent, fmt.Errorf("bad version %q: %w", expect, err)
		}
		obj = obj.If(gcs.Conditions{GenerationMatch: gen})
	}
	w := obj.NewWriter(ctx)
	if _, err := io.Copy(w, r); err != nil {
		w.Close()
		return VersionAbsent, err
	}
	if err := w.Close(); err != nil {
		// 412 is the precondition failing, which is not an error so much as
		// "somebody else got there" — the caller re-reads and retries.
		var apiErr *googleapi.Error
		if errors.As(err, &apiErr) && apiErr.Code == http.StatusPreconditionFailed {
			return VersionAbsent, ErrVersionMismatch
		}
		return VersionAbsent, err
	}
	return gcsVersion(w.Attrs().Generation), nil
}

func gcsVersion(gen int64) Version { return Version(strconv.FormatInt(gen, 10)) }
