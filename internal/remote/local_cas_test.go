package remote

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestLocalPutIfIsACompareAndSwapAcrossProcesses(t *testing.T) {
	root := t.TempDir()
	a, err := newLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	b, err := newLocal(root) // a second process on the same directory
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const key = "journal/dev.jsonl"
	read := func(be *localBackend) (string, Version) {
		t.Helper()
		rc, v, err := be.GetVersioned(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		body, _ := io.ReadAll(rc)
		return string(body), v
	}

	if _, _, err := a.GetVersioned(ctx, key); !IsNotExist(err) {
		t.Fatalf("absent object: %v", err)
	}
	if _, err := a.PutIf(ctx, key, strings.NewReader("one\n"), 4, VersionAbsent); err != nil {
		t.Fatal(err)
	}
	if _, err := b.PutIf(ctx, key, strings.NewReader("two\n"), 4, VersionAbsent); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("second create-if-absent: %v", err)
	}

	// b reads, a sneaks a write in, b's conditional write must be refused.
	body, v := read(b)
	if body != "one\n" {
		t.Fatalf("read %q", body)
	}
	if err := a.Put(ctx, key, strings.NewReader("one\nsneak\n"), 10); err != nil {
		t.Fatal(err)
	}
	if _, err := b.PutIf(ctx, key, strings.NewReader("one\ntwo\n"), 8, v); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("write over a moved object: %v", err)
	}
	body, v = read(b)
	if _, err := b.PutIf(ctx, key, strings.NewReader(body+"two\n"), 0, v); err != nil {
		t.Fatal(err)
	}
	if got, _ := read(a); got != "one\nsneak\ntwo\n" {
		t.Fatalf("after the retry: %q", got)
	}

	// Twenty appenders across both "processes", each retrying on a
	// mismatch: every line lands exactly once.
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			be := a
			if i%2 == 1 {
				be = b
			}
			line := "line" + string(rune('a'+i)) + "\n"
			for {
				rc, v, err := be.GetVersioned(ctx, key)
				if err != nil {
					t.Error(err)
					return
				}
				cur, _ := io.ReadAll(rc)
				rc.Close()
				_, err = be.PutIf(ctx, key, strings.NewReader(string(cur)+line), 0, v)
				if err == nil {
					return
				}
				if !errors.Is(err, ErrVersionMismatch) {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	final, _ := read(a)
	for i := range 20 {
		line := "line" + string(rune('a'+i)) + "\n"
		if n := strings.Count(final, line); n != 1 {
			t.Errorf("%q appears %d times", line, n)
		}
	}
}
