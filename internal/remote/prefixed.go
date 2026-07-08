package remote

import (
	"context"
	"io"
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

func (p *prefixed) key(key string) string { return p.prefix + "/" + key }

func (p *prefixed) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	return p.be.Put(ctx, p.key(key), r, size)
}

func (p *prefixed) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return p.be.Get(ctx, p.key(key))
}

func (p *prefixed) Exists(ctx context.Context, key string) (bool, error) {
	return p.be.Exists(ctx, p.key(key))
}

func (p *prefixed) List(ctx context.Context, prefix string) ([]Object, error) {
	objs, err := p.be.List(ctx, p.key(prefix))
	if err != nil {
		return nil, err
	}
	strip := p.prefix + "/"
	out := make([]Object, 0, len(objs))
	for _, o := range objs {
		if !strings.HasPrefix(o.Key, strip) {
			continue
		}
		out = append(out, Object{Key: strings.TrimPrefix(o.Key, strip), Size: o.Size})
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
	return p.signer.SignPut(ctx, p.key(key), size, ttl)
}
