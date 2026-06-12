package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

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
		out = append(out, Object{Key: strings.TrimPrefix(attrs.Name, strip), Size: attrs.Size})
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
