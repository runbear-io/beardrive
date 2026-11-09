package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// s3Backend stores objects in Amazon S3 (or any S3-compatible store via the
// standard AWS_ENDPOINT_URL / AWS_PROFILE environment configuration).
type s3Backend struct {
	client *s3.Client
	bucket string
	prefix string
}

func newS3(ctx context.Context, bucket, prefix string) (*s3Backend, error) {
	if bucket == "" {
		return nil, fmt.Errorf("s3 remote needs a bucket: s3://bucket/prefix")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return &s3Backend{client: s3.NewFromConfig(cfg), bucket: bucket, prefix: prefix}, nil
}

func (b *s3Backend) key(key string) string {
	if b.prefix == "" {
		return key
	}
	return path.Join(b.prefix, key)
}

func (b *s3Backend) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(b.key(key)),
		Body:          r,
		ContentLength: aws.Int64(size),
	})
	return err
}

// PutIfAbsent is a create-only PutObject. 412 means the object exists. A
// store that does not support If-None-Match rejects the request, which the
// caller sees as an error rather than an overwrite.
func (b *s3Backend) PutIfAbsent(ctx context.Context, key string, r io.Reader, size int64) error {
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(b.key(key)),
		Body:          r,
		ContentLength: aws.Int64(size),
		IfNoneMatch:   aws.String("*"),
	})
	var ae smithy.APIError
	if errors.As(err, &ae) && (ae.ErrorCode() == "PreconditionFailed" || ae.ErrorCode() == "ConditionalRequestConflict") {
		return ErrVersionMismatch
	}
	return err
}

var _ CreateOnlyPutter = (*s3Backend)(nil)
var _ ConditionalPutter = (*s3Backend)(nil)

func (b *s3Backend) GetVersioned(ctx context.Context, key string) (io.ReadCloser, Version, error) {
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.key(key)),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		var nf *types.NotFound
		var ae smithy.APIError
		if errors.As(err, &nsk) || errors.As(err, &nf) || (errors.As(err, &ae) && (ae.ErrorCode() == "NoSuchKey" || ae.ErrorCode() == "NotFound" || ae.ErrorCode() == "404")) {
			return nil, VersionAbsent, os.ErrNotExist
		}
		return nil, VersionAbsent, err
	}
	return out.Body, Version(aws.ToString(out.ETag)), nil
}

func (b *s3Backend) PutIf(ctx context.Context, key string, r io.Reader, size int64, expect Version) (Version, error) {
	in := &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(b.key(key)),
		Body:          r,
		ContentLength: aws.Int64(size),
	}
	if expect == VersionAbsent {
		in.IfNoneMatch = aws.String("*")
	} else {
		in.IfMatch = aws.String(string(expect))
	}
	out, err := b.client.PutObject(ctx, in)
	if err != nil {
		var ae smithy.APIError
		if errors.As(err, &ae) && (ae.ErrorCode() == "PreconditionFailed" || ae.ErrorCode() == "ConditionalRequestConflict") {
			return VersionAbsent, ErrVersionMismatch
		}
		return VersionAbsent, err
	}
	return Version(aws.ToString(out.ETag)), nil
}

// SignPut presigns a PUT of exactly size bytes to the given key. Presigning
// is a local signature computation — no network round-trip, no credentials in
// the result.
func (b *s3Backend) SignPut(ctx context.Context, key string, size int64, ttl time.Duration) (*SignedPut, error) {
	req, err := s3.NewPresignClient(b.client).PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(b.key(key)),
		ContentLength: aws.Int64(size),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return nil, fmt.Errorf("presign s3 put: %w", err)
	}
	headers := make(map[string]string)
	for k, vs := range req.SignedHeader {
		if len(vs) > 0 && !strings.EqualFold(k, "Host") {
			headers[k] = vs[0]
		}
	}
	return &SignedPut{URL: req.URL, Method: req.Method, Headers: headers, Expires: time.Now().Add(ttl)}, nil
}

func (b *s3Backend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.key(key)),
	})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (b *s3Backend) List(ctx context.Context, prefix string) ([]Object, error) {
	full := b.key(prefix)
	var out []Object
	p := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(b.bucket),
		Prefix: aws.String(full),
	})
	strip := b.prefix
	if strip != "" {
		strip += "/"
	}
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			key := strings.TrimPrefix(aws.ToString(o.Key), strip)
			out = append(out, Object{Key: key, Size: aws.ToInt64(o.Size), Modified: aws.ToTime(o.LastModified)})
		}
	}
	return out, nil
}

func (b *s3Backend) Exists(ctx context.Context, key string) (bool, error) {
	_, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.key(key)),
	})
	if err == nil {
		return true, nil
	}
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return false, nil
	}
	var ae smithy.APIError
	if errors.As(err, &ae) && (ae.ErrorCode() == "NotFound" || ae.ErrorCode() == "404") {
		return false, nil
	}
	return false, err
}

func (b *s3Backend) Close() error { return nil }
