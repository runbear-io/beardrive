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
