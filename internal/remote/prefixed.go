package remote

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
)

// Prefixed namespaces a backend under prefix: every key is stored at
// <prefix>/<key>. Used by the web server to host many projects in one
// storage root. Closing the returned backend does not close the underlying
// one (it is shared across prefixes).
func Prefixed(be Backend, prefix string) Backend {
	prefix = strings.Trim(prefix, "/")
	p := &prefixed{be: be, prefix: prefix}
	if signer, ok := be.(PutSigner); ok {
		return &prefixedSigner{prefixed: p, signer: signer}
	}
	return p
}

type prefixed struct {
	be     Backend
	prefix string
}

// safeKey reports whether a key stays inside the namespace once the storage
// normalizes it. This is the whole containment check: p.key is string
// concatenation, and S3, GCS and the filesystem all resolve ".." for
// themselves, so one such segment crosses into another project's prefix on
// every operation. Nothing is normalized here — a key is either already safe
// or refused, because rewriting it would silently retarget a caller's object.
// A trailing slash is kept: List takes a directory-ish prefix.
func safeKey(key string) bool {
	if key == "" {
		return true // the whole namespace: List("")
	}
	if strings.HasPrefix(key, "/") {
		return false
	}
	trimmed := strings.TrimSuffix(key, "/")
	if trimmed == "" || trimmed == ".." || strings.HasPrefix(trimmed, "../") {
		return false
	}
	return path.Clean(trimmed) == trimmed
}

func (p *prefixed) key(key string) (string, error) {
	if !safeKey(key) {
		return "", fmt.Errorf("invalid key %q", key)
	}
	return p.prefix + "/" + key, nil
}

func (p *prefixed) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	k, err := p.key(key)
	if err != nil {
		return err
	}
	return p.be.Put(ctx, k, r, size)
}

func (p *prefixed) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	k, err := p.key(key)
	if err != nil {
		return nil, err
	}
	return p.be.Get(ctx, k)
}

func (p *prefixed) Exists(ctx context.Context, key string) (bool, error) {
	k, err := p.key(key)
	if err != nil {
		return false, err
	}
	return p.be.Exists(ctx, k)
}

func (p *prefixed) List(ctx context.Context, prefix string) ([]Object, error) {
	k, err := p.key(prefix)
	if err != nil {
		return nil, err
	}
	objs, err := p.be.List(ctx, k)
	if err != nil {
		return nil, err
	}
	strip := p.prefix + "/"
	out := make([]Object, 0, len(objs))
	for _, o := range objs {
		if !strings.HasPrefix(o.Key, strip) {
			continue
		}
		// The namespace has to survive the way out too: a stored key like
		// "<project>/../victim/x" passes the HasPrefix filter and comes back
		// looking in-project, and every caller feeds what List named straight
		// back to Get.
		rel := strings.TrimPrefix(o.Key, strip)
		if !safeKey(rel) {
			continue
		}
		out = append(out, Object{Key: rel, Size: o.Size})
	}
	return out, nil
}

func (p *prefixed) Close() error { return nil }

// prefixedSigner adds presigning when the underlying backend supports it, so
// direct uploads keep working through the namespace.
type prefixedSigner struct {
	*prefixed
	signer PutSigner
}

func (p *prefixedSigner) SignPut(ctx context.Context, key string, size int64, ttl time.Duration) (*SignedPut, error) {
	k, err := p.key(key)
	if err != nil {
		return nil, err
	}
	return p.signer.SignPut(ctx, k, size, ttl)
}
