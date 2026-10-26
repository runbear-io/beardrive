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
