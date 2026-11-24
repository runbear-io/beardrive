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
//
// Only optional capabilities actually implemented by be are exposed on the
// returned wrapper, so type assertions never succeed against unsupported stores.
func Prefixed(be Backend, prefix string) Backend {
	prefix = strings.Trim(prefix, "/")
	p := &prefixed{be: be, prefix: prefix}
	signer, signs := be.(PutSigner)
	if meta, ok := be.(VersionedMetaBackend); ok {
		pc := &pCond{p: p, cond: meta}
		pr := &pRange{p: p, rg: meta}
		pv := &pVRange{p: p, vrg: meta}
		pm := &pMeta{p: p, meta: meta}
		if signs {
			return struct {
				*prefixed
				*pSigner
				*pCond
				*pRange
				*pVRange
				*pMeta
			}{p, &pSigner{p: p, signer: signer}, pc, pr, pv, pm}
		}
		return struct {
			*prefixed
			*pCond
			*pRange
			*pVRange
			*pMeta
		}{p, pc, pr, pv, pm}
	}
	cond, hasCond := be.(ConditionalPutter)
	rg, hasRange := be.(RangeGetter)
	vrg, hasVRange := be.(VersionedRangeGetter)
	var ps *pSigner
	var pc *pCond
	var pr *pRange
	var pv *pVRange
	if signs {
		ps = &pSigner{p: p, signer: signer}
	}
	if hasCond {
		pc = &pCond{p: p, cond: cond}
	}
	if hasRange {
		pr = &pRange{p: p, rg: rg}
	}
	if hasVRange {
		pv = &pVRange{p: p, vrg: vrg}
	}
	mask := 0
	if signs {
		mask |= 8
	}
	if hasCond {
		mask |= 4
	}
	if hasRange {
		mask |= 2
	}
	if hasVRange {
		mask |= 1
	}
	switch mask {
	case 0:
		return p
	case 1:
		return struct {
			*prefixed
			*pVRange
		}{p, pv}
	case 2:
		return struct {
			*prefixed
			*pRange
		}{p, pr}
	case 3:
		return struct {
			*prefixed
			*pRange
			*pVRange
		}{p, pr, pv}
	case 4:
		return struct {
			*prefixed
			*pCond
		}{p, pc}
	case 5:
		return struct {
			*prefixed
			*pCond
			*pVRange
		}{p, pc, pv}
	case 6:
		return struct {
			*prefixed
			*pCond
			*pRange
		}{p, pc, pr}
	case 7:
		return struct {
			*prefixed
			*pCond
			*pRange
			*pVRange
		}{p, pc, pr, pv}
	case 8:
		return struct {
			*prefixed
			*pSigner
		}{p, ps}
	case 9:
		return struct {
			*prefixed
			*pSigner
			*pVRange
		}{p, ps, pv}
	case 10:
		return struct {
			*prefixed
			*pSigner
			*pRange
		}{p, ps, pr}
	case 11:
		return struct {
			*prefixed
			*pSigner
			*pRange
			*pVRange
		}{p, ps, pr, pv}
	case 12:
		return struct {
			*prefixed
			*pSigner
			*pCond
		}{p, ps, pc}
	case 13:
		return struct {
			*prefixed
			*pSigner
			*pCond
			*pVRange
		}{p, ps, pc, pv}
	case 14:
		return struct {
			*prefixed
			*pSigner
			*pCond
			*pRange
		}{p, ps, pc, pr}
	default: // 15
		return struct {
			*prefixed
			*pSigner
			*pCond
			*pRange
			*pVRange
		}{p, ps, pc, pr, pv}
	}
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
	// "." is Clean-stable, so the check below accepts it — and it is not a
	// key: filepath.Join collapses "<project>/." to the project DIRECTORY on
	// file://, while S3 and GCS store a literal object by that name. One key,
	// three meanings.
	if trimmed == "" || trimmed == "." || trimmed == ".." || strings.HasPrefix(trimmed, "../") {
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
		out = append(out, Object{
			Key:              rel,
			Size:             o.Size,
			Modified:         o.Modified,
			Version:          o.Version,
			ImmutableVersion: o.ImmutableVersion,
			CRC32C:           o.CRC32C,
		})
	}
	return out, nil
}

func (p *prefixed) Close() error { return nil }

type pSigner struct {
	p      *prefixed
	signer PutSigner
}

func (s *pSigner) SignPut(ctx context.Context, key string, size int64, ttl time.Duration) (*SignedPut, error) {
	k, err := s.p.key(key)
	if err != nil {
		return nil, err
	}
	return s.signer.SignPut(ctx, k, size, ttl)
}

type pCond struct {
	p    *prefixed
	cond ConditionalPutter
}

func (c *pCond) GetVersioned(ctx context.Context, key string) (io.ReadCloser, Version, error) {
	k, err := c.p.key(key)
	if err != nil {
		return nil, VersionAbsent, err
	}
	return c.cond.GetVersioned(ctx, k)
}

func (c *pCond) PutIf(ctx context.Context, key string, r io.Reader, size int64, expect Version) (Version, error) {
	k, err := c.p.key(key)
	if err != nil {
		return VersionAbsent, err
	}
	return c.cond.PutIf(ctx, k, r, size, expect)
}

type pRange struct {
	p  *prefixed
	rg RangeGetter
}

func (r *pRange) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	k, err := r.p.key(key)
	if err != nil {
		return nil, err
	}
	return r.rg.GetRange(ctx, k, offset, length)
}

type pVRange struct {
	p   *prefixed
	vrg VersionedRangeGetter
}

func (v *pVRange) GetVersionedRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, Version, error) {
	k, err := v.p.key(key)
	if err != nil {
		return nil, VersionAbsent, err
	}
	return v.vrg.GetVersionedRange(ctx, k, offset, length)
}

type pMeta struct {
	p    *prefixed
	meta VersionedMetaBackend
}

func (m *pMeta) GetVersionedWithMeta(ctx context.Context, key string) (io.ReadCloser, ObjectMeta, error) {
	k, err := m.p.key(key)
	if err != nil {
		return nil, ObjectMeta{}, err
	}
	return m.meta.GetVersionedWithMeta(ctx, k)
}

func (m *pMeta) PutIfWithMeta(ctx context.Context, key string, r io.Reader, size int64, expect Version, meta map[string]string) (ObjectMeta, error) {
	k, err := m.p.key(key)
	if err != nil {
		return ObjectMeta{}, err
	}
	return m.meta.PutIfWithMeta(ctx, k, r, size, expect, meta)
}

func (m *pMeta) GetVersionedRangeWithMeta(ctx context.Context, key string, offset, length int64) (io.ReadCloser, ObjectMeta, error) {
	k, err := m.p.key(key)
	if err != nil {
		return nil, ObjectMeta{}, err
	}
	return m.meta.GetVersionedRangeWithMeta(ctx, k, offset, length)
}
