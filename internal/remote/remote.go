// Package remote abstracts the cloud object store a volume syncs through.
// sfs is provider-agnostic: any backend that can put/get/list immutable
// objects works. Built-in schemes:
//
//	file:///abs/path      local or network-drive directory (also used in tests)
//	s3://bucket/prefix    Amazon S3 (or S3-compatible via AWS_ENDPOINT_URL)
//	gs://bucket/prefix    Google Cloud Storage
//
// Remote layout: blobs/<sha256> for content, journal/<device>.jsonl for op
// logs. Each device writes only its own journal, so there are no concurrent
// writers per object and no server-side coordination is needed.
package remote

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
)

type Object struct {
	Key  string
	Size int64
}

type Backend interface {
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	List(ctx context.Context, prefix string) ([]Object, error)
	Exists(ctx context.Context, key string) (bool, error)
	Close() error
}

// Open creates a backend from a remote URL.
func Open(ctx context.Context, raw string) (Backend, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid remote %q: %w", raw, err)
	}
	switch u.Scheme {
	case "file":
		return newLocal(u.Path)
	case "s3":
		return newS3(ctx, u.Host, strings.Trim(u.Path, "/"))
	case "gs":
		return newGCS(ctx, u.Host, strings.Trim(u.Path, "/"))
	default:
		return nil, fmt.Errorf("unsupported remote scheme %q (supported: file://, s3://, gs://)", u.Scheme)
	}
}
