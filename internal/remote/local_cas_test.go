package remote

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const localCASTestGuard = 3 * time.Second

func holdLocalCASLock(t *testing.T, root string) (*os.File, func()) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(root, localLockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		})
	}
	t.Cleanup(release)
	return f, release
}

func assertLocalCASLockBlocked(t *testing.T, f *os.File) {
	t.Helper()
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("third descriptor acquired held CAS lock: %v", err)
	}
}

func assertLocalCASLockFree(t *testing.T, f *os.File) {
	t.Helper()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("third descriptor could not acquire CAS lock after release: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
}

func waitLocalCASCall(t *testing.T, done <-chan error, release func()) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(localCASTestGuard):
		release()
		select {
		case <-done:
		case <-time.After(localCASTestGuard):
		}
		t.Fatal("CAS operation did not observe context cancellation while the external lock was held")
		return nil
	}
}

func TestLocalCASLockCancellationPreservesHolderAndBytes(t *testing.T) {
	for _, operation := range []string{"GetVersioned", "PutIf"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			be, err := newLocal(root)
			if err != nil {
				t.Fatal(err)
			}
			const key = "journal/dev.jsonl"
			const original = "original\n"
			if err := be.Put(context.Background(), key, strings.NewReader(original), int64(len(original))); err != nil {
				t.Fatal(err)
			}

			var expect Version
			if operation == "PutIf" {
				rc, version, err := be.GetVersioned(context.Background(), key)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := io.ReadAll(rc); err != nil {
					t.Fatal(err)
				}
				if err := rc.Close(); err != nil {
					t.Fatal(err)
				}
				expect = version
			}

			_, release := holdLocalCASLock(t, root)
			probe, err := os.OpenFile(filepath.Join(root, localLockName), os.O_CREATE|os.O_RDWR, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			defer probe.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if operation == "GetVersioned" {
					rc, _, err := be.GetVersioned(ctx, key)
					if rc != nil {
						_ = rc.Close()
					}
					done <- err
					return
				}
				_, err := be.PutIf(ctx, key, strings.NewReader("replacement\n"), int64(len("replacement\n")), expect)
				done <- err
			}()

			err = waitLocalCASCall(t, done, release)
			if ctx.Err() != context.DeadlineExceeded || !errors.Is(err, ctx.Err()) {
				t.Fatalf("%s error = %v, context error = %v, want context deadline exceeded", operation, err, ctx.Err())
			}
			assertLocalCASLockBlocked(t, probe)
			gotWhileHeld, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(key)))
			if err != nil {
				t.Fatal(err)
			}
			if string(gotWhileHeld) != original {
				t.Fatalf("cancelled %s changed bytes while the external lock was held: %q", operation, gotWhileHeld)
			}

			release()
			assertLocalCASLockFree(t, probe)

			rc, _, err := be.GetVersioned(context.Background(), key)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(rc)
			if closeErr := rc.Close(); err != nil {
				t.Fatal(err)
			} else if closeErr != nil {
				t.Fatal(closeErr)
			}
			if string(got) != original {
				t.Fatalf("cancelled %s changed bytes to %q", operation, got)
			}
		})
	}
}

func TestLocalCASAlreadyCanceledContextDoesNotWriteOrHoldLock(t *testing.T) {
	for _, operation := range []string{"GetVersioned", "PutIf"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			be, err := newLocal(root)
			if err != nil {
				t.Fatal(err)
			}
			const key = "journal/dev.jsonl"
			const original = "original\n"
			if err := be.Put(context.Background(), key, strings.NewReader(original), int64(len(original))); err != nil {
				t.Fatal(err)
			}

			var expect Version
			if operation == "PutIf" {
				rc, version, err := be.GetVersioned(context.Background(), key)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := io.ReadAll(rc); err != nil {
					t.Fatal(err)
				}
				if err := rc.Close(); err != nil {
					t.Fatal(err)
				}
				expect = version
			}

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if operation == "GetVersioned" {
				rc, _, err := be.GetVersioned(ctx, key)
				if rc != nil {
					_ = rc.Close()
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("%s error = %v, want context canceled", operation, err)
				}
			} else {
				_, err := be.PutIf(ctx, key, strings.NewReader("replacement\n"), int64(len("replacement\n")), expect)
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("%s error = %v, want context canceled", operation, err)
				}
			}

			probe, err := os.OpenFile(filepath.Join(root, localLockName), os.O_CREATE|os.O_RDWR, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			defer probe.Close()
			assertLocalCASLockFree(t, probe)

			rc, _, err := be.GetVersioned(context.Background(), key)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(rc)
			if closeErr := rc.Close(); err != nil {
				t.Fatal(err)
			} else if closeErr != nil {
				t.Fatal(closeErr)
			}
			if string(got) != original {
				t.Fatalf("cancelled %s changed bytes to %q", operation, got)
			}
		})
	}
}

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

func TestLocalBackendRangeAndVersionedRange(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	be, err := newLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	const key = "journal/dev.jsonl"
	const v1Body = "line1\nline2\n"
	if err := be.Put(ctx, key, strings.NewReader(v1Body), int64(len(v1Body))); err != nil {
		t.Fatal(err)
	}
	rcFull, fullVer, err := be.GetVersioned(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	_ = rcFull.Close()

	// Exact tail read.
	rc, ver, err := be.GetVersionedRange(ctx, key, 6, -1)
	if err != nil {
		t.Fatal(err)
	}
	if ver != fullVer {
		_ = rc.Close()
		t.Fatalf("version = %q, want %q", ver, fullVer)
	}

	// While holding the open tail reader, a concurrent PutIf rename must NOT block
	// (flock was released) and the open reader must still read v1's tail from its inode.
	done := make(chan error, 1)
	go func() {
		_, putErr := be.PutIf(ctx, key, strings.NewReader(v1Body+"line3\n"), int64(len(v1Body)+6), fullVer)
		done <- putErr
	}()
	select {
	case putErr := <-done:
		if putErr != nil {
			_ = rc.Close()
			t.Fatalf("concurrent PutIf while tail reader open failed: %v", putErr)
		}
	case <-time.After(2 * time.Second):
		_ = rc.Close()
		t.Fatal("PutIf blocked while caller held GetVersionedRange reader (flock held too long)")
	}
	gotTail, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(gotTail) != "line2\n" {
		t.Fatalf("open tail reader after rename = (%q, %v), want %q", gotTail, err, "line2\n")
	}

	// offset == size and length == 0 return empty body + latest version.
	rcFull2, ver2, err := be.GetVersioned(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	full2Bytes, _ := io.ReadAll(rcFull2)
	_ = rcFull2.Close()

	for _, tc := range []struct {
		name   string
		off    int64
		length int64
	}{
		{"offset_eq_size", int64(len(full2Bytes)), -1},
		{"length_zero", 6, 0},
	} {
		r, v, err := be.GetVersionedRange(ctx, key, tc.off, tc.length)
		if err != nil {
			t.Fatalf("%s GetVersionedRange: %v", tc.name, err)
		}
		b, _ := io.ReadAll(r)
		_ = r.Close()
		if len(b) != 0 || v != ver2 {
			t.Fatalf("%s = (%q, %q), want empty & %q", tc.name, b, v, ver2)
		}
		r2, err := be.GetRange(ctx, key, tc.off, tc.length)
		if err != nil {
			t.Fatalf("%s GetRange: %v", tc.name, err)
		}
		b2, _ := io.ReadAll(r2)
		_ = r2.Close()
		if len(b2) != 0 {
			t.Fatalf("%s GetRange = %q, want empty", tc.name, b2)
		}
	}

	// Out-of-bounds offsets fail closed on both GetRange and GetVersionedRange.
	for _, off := range []int64{-1, int64(len(full2Bytes)) + 1} {
		if r, err := be.GetRange(ctx, key, off, -1); err == nil {
			_ = r.Close()
			t.Fatalf("GetRange(%d) succeeded, want error", off)
		}
		if r, _, err := be.GetVersionedRange(ctx, key, off, -1); err == nil {
			_ = r.Close()
			t.Fatalf("GetVersionedRange(%d) succeeded, want error", off)
		}
	}
}
