package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
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
var _ VersionedMetaBackend = (*s3Backend)(nil)

func mapS3NotExist(err error) error {
	if err == nil {
		return nil
	}
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	var ae smithy.APIError
	if errors.As(err, &nsk) || errors.As(err, &nf) || (errors.As(err, &ae) && (ae.ErrorCode() == "NoSuchKey" || ae.ErrorCode() == "NotFound" || ae.ErrorCode() == "404")) {
		return os.ErrNotExist
	}
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound {
		return os.ErrNotExist
	}
	return err
}

func isS3InvalidRange(err error) bool {
	if err == nil {
		return false
	}
	var ae smithy.APIError
	if errors.As(err, &ae) && (ae.ErrorCode() == "InvalidRange" || ae.ErrorCode() == "416") {
		return true
	}
	var re *smithyhttp.ResponseError
	return errors.As(err, &re) && re.HTTPStatusCode() == http.StatusRequestedRangeNotSatisfiable
}

func parseContentRange(cr string) (start, end, total int64, ok bool) {
	rest, hasBytes := strings.CutPrefix(strings.TrimSpace(cr), "bytes ")
	if !hasBytes {
		return 0, 0, 0, false
	}
	rangePart, totalPart, hasSlash := strings.Cut(rest, "/")
	if !hasSlash || strings.HasPrefix(rangePart, "*") || totalPart == "*" {
		return 0, 0, 0, false
	}
	startStr, endStr, hasDash := strings.Cut(rangePart, "-")
	if !hasDash {
		return 0, 0, 0, false
	}
	s, err1 := strconv.ParseInt(startStr, 10, 64)
	e, err2 := strconv.ParseInt(endStr, 10, 64)
	t, err3 := strconv.ParseInt(totalPart, 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || s < 0 || e < s || e >= t {
		return 0, 0, 0, false
	}
	return s, e, t, true
}

func (b *s3Backend) GetVersioned(ctx context.Context, key string) (io.ReadCloser, Version, error) {
	rc, meta, err := b.GetVersionedWithMeta(ctx, key)
	return rc, meta.Version, err
}

func (b *s3Backend) GetVersionedWithMeta(ctx context.Context, key string) (io.ReadCloser, ObjectMeta, error) {
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.key(key)),
	})
	if err != nil {
		return nil, ObjectMeta{}, mapS3NotExist(err)
	}
	meta := ObjectMeta{
		Version:  Version(aws.ToString(out.ETag)),
		Size:     aws.ToInt64(out.ContentLength),
		Metadata: normalizeMeta(out.Metadata),
	}
	return out.Body, meta, nil
}

func (b *s3Backend) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	rc, _, err := b.GetVersionedRangeWithMeta(ctx, key, offset, length)
	return rc, err
}

func (b *s3Backend) GetVersionedRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, Version, error) {
	rc, meta, err := b.GetVersionedRangeWithMeta(ctx, key, offset, length)
	return rc, meta.Version, err
}

func (b *s3Backend) GetVersionedRangeWithMeta(ctx context.Context, key string, offset, length int64) (io.ReadCloser, ObjectMeta, error) {
	if offset < 0 {
		return nil, ObjectMeta{}, fmt.Errorf("invalid range offset %d", offset)
	}
	fullKey := b.key(key)
	headZero := func() (ObjectMeta, error) {
		head, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(b.bucket),
			Key:    aws.String(fullKey),
		})
		if err != nil {
			return ObjectMeta{}, mapS3NotExist(err)
		}
		return ObjectMeta{
			Version:  Version(aws.ToString(head.ETag)),
			Size:     aws.ToInt64(head.ContentLength),
			Metadata: normalizeMeta(head.Metadata),
		}, nil
	}
	if length == 0 {
		meta, err := headZero()
		if err != nil {
			return nil, ObjectMeta{}, err
		}
		if offset > meta.Size {
			return nil, ObjectMeta{}, fmt.Errorf("range offset %d out of bounds for size %d", offset, meta.Size)
		}
		return io.NopCloser(bytes.NewReader(nil)), meta, nil
	}
	if length > 0 && offset > math.MaxInt64-length {
		return nil, ObjectMeta{}, fmt.Errorf("range offset %d + length %d overflows", offset, length)
	}
	var rangeHeader string
	switch {
	case length > 0:
		rangeHeader = fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
	case offset > 0:
		rangeHeader = fmt.Sprintf("bytes=%d-", offset)
	}
	for attempt := range 2 {
		in := &s3.GetObjectInput{
			Bucket: aws.String(b.bucket),
			Key:    aws.String(fullKey),
		}
		if rangeHeader != "" {
			in.Range = aws.String(rangeHeader)
		}
		out, err := b.client.GetObject(ctx, in)
		if err != nil {
			if isS3InvalidRange(err) {
				meta, herr := headZero()
				if herr != nil {
					return nil, ObjectMeta{}, herr
				}
				if offset == meta.Size {
					return io.NopCloser(bytes.NewReader(nil)), meta, nil
				}
				if offset > meta.Size {
					return nil, ObjectMeta{}, fmt.Errorf("range offset %d out of bounds for size %d", offset, meta.Size)
				}
				if attempt == 0 {
					continue
				}
			}
			return nil, ObjectMeta{}, mapS3NotExist(err)
		}
		total := aws.ToInt64(out.ContentLength)
		if rangeHeader != "" {
			if out.ContentRange == nil {
				_ = out.Body.Close()
				return nil, ObjectMeta{}, fmt.Errorf("range response missing Content-Range header")
			}
			start, end, crTotal, ok := parseContentRange(aws.ToString(out.ContentRange))
			if !ok || start != offset || (length > 0 && end > offset+length-1) {
				_ = out.Body.Close()
				return nil, ObjectMeta{}, fmt.Errorf("unexpected Content-Range %q for offset %d length %d", aws.ToString(out.ContentRange), offset, length)
			}
			total = crTotal
		}
		return boundedReadCloser(out.Body, length), ObjectMeta{
			Version:  Version(aws.ToString(out.ETag)),
			Size:     total,
			Metadata: normalizeMeta(out.Metadata),
		}, nil
	}
	return nil, ObjectMeta{}, fmt.Errorf("range read failed after retry")
}

func (b *s3Backend) PutIf(ctx context.Context, key string, r io.Reader, size int64, expect Version) (Version, error) {
	meta, err := b.PutIfWithMeta(ctx, key, r, size, expect, nil)
	return meta.Version, err
}

func (b *s3Backend) PutIfWithMeta(ctx context.Context, key string, r io.Reader, size int64, expect Version, meta map[string]string) (ObjectMeta, error) {
	normMeta := normalizeMeta(meta)
	in := &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(b.key(key)),
		Body:          r,
		ContentLength: aws.Int64(size),
		Metadata:      normMeta,
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
			return ObjectMeta{}, ErrVersionMismatch
		}
		var re *smithyhttp.ResponseError
		if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusPreconditionFailed {
			return ObjectMeta{}, ErrVersionMismatch
		}
		return ObjectMeta{}, err
	}
	return ObjectMeta{
		Version:  Version(aws.ToString(out.ETag)),
		Size:     size,
		Metadata: normMeta,
	}, nil
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
			out = append(out, Object{
				Key:              key,
				Size:             aws.ToInt64(o.Size),
				Modified:         aws.ToTime(o.LastModified),
				Version:          Version(aws.ToString(o.ETag)),
				ImmutableVersion: false,
			})
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
