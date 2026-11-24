package remote

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// plainMem is a backend with no optional capabilities.
type plainMem struct{}

func (plainMem) Put(context.Context, string, io.Reader, int64) error { return nil }
func (plainMem) Get(context.Context, string) (io.ReadCloser, error)  { return nil, nil }
func (plainMem) Exists(context.Context, string) (bool, error)        { return false, nil }
func (plainMem) List(context.Context, string) ([]Object, error)      { return nil, nil }
func (plainMem) Close() error                                        { return nil }

// casMem can compare and swap, and records the keys it was asked for.
type casMem struct {
	plainMem
	keys []string
}

func (m *casMem) GetVersioned(_ context.Context, key string) (io.ReadCloser, Version, error) {
	m.keys = append(m.keys, key)
	return io.NopCloser(bytes.NewReader(nil)), "1", nil
}
func (m *casMem) PutIf(_ context.Context, key string, _ io.Reader, _ int64, _ Version) (Version, error) {
	m.keys = append(m.keys, key)
	return "2", nil
}

type casSigner struct{ casMem }

func (s *casSigner) SignPut(_ context.Context, key string, _ int64, _ time.Duration) (*SignedPut, error) {
	return &SignedPut{URL: "https://store/" + key}, nil
}

// Every hub project is a Prefixed view of the storage root. A capability the
// view drops is silently a slower path: the journal append looked for
// compare-and-swap on it, never found it, and fell back to last-writer-wins
// even on GCS, where two hub processes then erase each other's ops.
func TestPrefixedKeepsCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	for name, root := range map[string]Backend{"cas": &casMem{}, "cas+sign": &casSigner{}} {
		be := Prefixed(root, "proj")
		cp, ok := be.(ConditionalPutter)
		if !ok {
			t.Fatalf("%s: Prefixed dropped compare-and-swap", name)
		}
		if _, _, err := cp.GetVersioned(ctx, "journal/hub.jsonl"); err != nil {
			t.Fatal(err)
		}
		if _, err := cp.PutIf(ctx, "journal/hub.jsonl", strings.NewReader("x"), 1, "1"); err != nil {
			t.Fatal(err)
		}
		if _, err := cp.PutIf(ctx, "../other/journal/hub.jsonl", strings.NewReader("x"), 1, "1"); err == nil {
			t.Fatalf("%s: a key escaping the namespace was written", name)
		}
		var keys []string
		switch r := root.(type) {
		case *casMem:
			keys = r.keys
		case *casSigner:
			keys = r.keys
			signer, ok := be.(PutSigner)
			if !ok {
				t.Fatal("cas+sign: Prefixed dropped presigning")
			}
			sp, err := signer.SignPut(ctx, "blobs/abc", 1, time.Minute)
			if err != nil || sp.URL != "https://store/proj/blobs/abc" {
				t.Fatalf("cas+sign: signed %v (%v), want the prefixed key", sp, err)
			}
		}
		for _, k := range keys {
			if k != "proj/journal/hub.jsonl" {
				t.Fatalf("%s: root saw key %q, want it under the project prefix", name, k)
			}
		}
	}
	if _, ok := Prefixed(&plainMem{}, "proj").(ConditionalPutter); ok {
		t.Fatal("Prefixed invented compare-and-swap for a root without it")
	}
}

type rangeMem struct{ plainMem }

func (rangeMem) GetRange(context.Context, string, int64, int64) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

type metaMem struct {
	casMem
	metaKeys []string
}

func (m *metaMem) GetRange(_ context.Context, key string, _, _ int64) (io.ReadCloser, error) {
	m.metaKeys = append(m.metaKeys, key)
	return io.NopCloser(bytes.NewReader(nil)), nil
}

func (m *metaMem) GetVersionedRange(_ context.Context, key string, _, _ int64) (io.ReadCloser, Version, error) {
	m.metaKeys = append(m.metaKeys, key)
	return io.NopCloser(bytes.NewReader(nil)), "1", nil
}

func (m *metaMem) GetVersionedWithMeta(_ context.Context, key string) (io.ReadCloser, ObjectMeta, error) {
	m.metaKeys = append(m.metaKeys, key)
	return io.NopCloser(bytes.NewReader(nil)), ObjectMeta{Version: "1"}, nil
}

func (m *metaMem) PutIfWithMeta(_ context.Context, key string, _ io.Reader, size int64, _ Version, meta map[string]string) (ObjectMeta, error) {
	m.metaKeys = append(m.metaKeys, key)
	return ObjectMeta{Version: "2", Size: size, Metadata: meta}, nil
}

func (m *metaMem) GetVersionedRangeWithMeta(_ context.Context, key string, _, _ int64) (io.ReadCloser, ObjectMeta, error) {
	m.metaKeys = append(m.metaKeys, key)
	return io.NopCloser(bytes.NewReader(nil)), ObjectMeta{Version: "1"}, nil
}

type metaSignerMem struct{ metaMem }

func (*metaSignerMem) SignPut(_ context.Context, key string, _ int64, _ time.Duration) (*SignedPut, error) {
	return &SignedPut{URL: "https://store/" + key}, nil
}

type versionedListMem struct{ plainMem }

func (versionedListMem) List(context.Context, string) ([]Object, error) {
	return []Object{{
		Key:              "proj/journal/d.jsonl",
		Size:             12,
		Version:          "42",
		ImmutableVersion: true,
		CRC32C:           0xdeadbeef,
	}}, nil
}

func TestPrefixedCapabilitiesAndContainment(t *testing.T) {
	ctx := context.Background()

	// casMem (without RangeGetter/VersionedRangeGetter) must NOT falsely satisfy them.
	casOnly := Prefixed(&casMem{}, "proj")
	if _, ok := casOnly.(RangeGetter); ok {
		t.Fatal("Prefixed invented RangeGetter on casMem")
	}
	if _, ok := casOnly.(VersionedRangeGetter); ok {
		t.Fatal("Prefixed invented VersionedRangeGetter on casMem")
	}
	if _, ok := casOnly.(VersionedMetaBackend); ok {
		t.Fatal("Prefixed invented VersionedMetaBackend on casMem")
	}

	// RangeGetter on a non-conditional backend must still be forwarded.
	rgOnly := Prefixed(rangeMem{}, "proj")
	if _, ok := rgOnly.(RangeGetter); !ok {
		t.Fatal("Prefixed dropped RangeGetter on non-conditional backend")
	}
	if _, ok := rgOnly.(ConditionalPutter); ok {
		t.Fatal("Prefixed invented ConditionalPutter on rangeMem")
	}

	// List must preserve internal Version, ImmutableVersion, and CRC32C.
	objs, err := Prefixed(versionedListMem{}, "proj").List(ctx, "journal/")
	if err != nil || len(objs) != 1 {
		t.Fatalf("List = (%+v, %v), want 1 object", objs, err)
	}
	if objs[0].Version != "42" || !objs[0].ImmutableVersion || objs[0].CRC32C != 0xdeadbeef {
		t.Fatalf("Prefixed.List dropped revision fields: %+v", objs[0])
	}

	// VersionedMetaBackend + PutSigner forwarding and ../ containment across every new method.
	for name, raw := range map[string]Backend{"meta": &metaMem{}, "meta+sign": &metaSignerMem{}} {
		be := Prefixed(raw, "proj")
		mb, ok := be.(VersionedMetaBackend)
		if !ok {
			t.Fatalf("%s: Prefixed dropped VersionedMetaBackend", name)
		}
		if _, sign := be.(PutSigner); sign != (name == "meta+sign") {
			t.Fatalf("%s: PutSigner mismatch got=%v", name, sign)
		}
		if _, err := mb.GetRange(ctx, "../victim/journal/a.jsonl", 0, -1); err == nil {
			t.Fatalf("%s: GetRange accepted escape key", name)
		}
		if _, _, err := mb.GetVersionedRange(ctx, "../victim/journal/a.jsonl", 0, -1); err == nil {
			t.Fatalf("%s: GetVersionedRange accepted escape key", name)
		}
		if _, _, err := mb.GetVersionedWithMeta(ctx, "../victim/journal/a.jsonl"); err == nil {
			t.Fatalf("%s: GetVersionedWithMeta accepted escape key", name)
		}
		if _, err := mb.PutIfWithMeta(ctx, "../victim/journal/a.jsonl", strings.NewReader("x"), 1, VersionAbsent, nil); err == nil {
			t.Fatalf("%s: PutIfWithMeta accepted escape key", name)
		}
		if _, _, err := mb.GetVersionedRangeWithMeta(ctx, "../victim/journal/a.jsonl", 0, -1); err == nil {
			t.Fatalf("%s: GetVersionedRangeWithMeta accepted escape key", name)
		}
		if rc, _, err := mb.GetVersionedRangeWithMeta(ctx, "journal/a.jsonl", 0, 1); err != nil {
			t.Fatalf("%s: valid GetVersionedRangeWithMeta failed: %v", name, err)
		} else {
			_ = rc.Close()
		}
		var seen []string
		switch r := raw.(type) {
		case *metaMem:
			seen = r.metaKeys
		case *metaSignerMem:
			seen = r.metaKeys
		}
		if len(seen) != 1 || seen[0] != "proj/journal/a.jsonl" {
			t.Fatalf("%s: unexpected keys seen by underlying backend: %v", name, seen)
		}
	}
}
