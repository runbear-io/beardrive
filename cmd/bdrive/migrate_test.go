package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/remote"
)

func openFileBackend(t *testing.T) remote.Backend {
	t.Helper()
	be, err := remote.Open(context.Background(), "file://"+t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { be.Close() })
	return be
}

func put(t *testing.T, be remote.Backend, key, content string) {
	t.Helper()
	if err := be.Put(context.Background(), key, strings.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func blobKey(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "blobs/" + hex.EncodeToString(sum[:])
}

func TestExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := openFileBackend(t)

	// A project as two devices left it: two journals, two blobs.
	blobA, blobB := "hello from device one", "and from device two"
	put(t, src, blobKey(blobA), blobA)
	put(t, src, blobKey(blobB), blobB)
	put(t, src, "journal/dev-1.jsonl", `{"path":"a.md"}`+"\n")
	put(t, src, "journal/dev-2.jsonl", `{"path":"b.md"}`+"\n")

	var buf bytes.Buffer
	man := exportManifest{Project: "wiki", Remote: "https://old.example/p/p-00000000", ExportedAt: time.Now().UTC()}
	blobs, journals, _, err := exportStore(ctx, src, &buf, man)
	if err != nil {
		t.Fatal(err)
	}
	if blobs != 2 || journals != 2 {
		t.Fatalf("export counted %d blobs, %d journals; want 2, 2", blobs, journals)
	}

	// The manifest comes first and names the project.
	gz, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	gotMan, first, err := readManifest(tr)
	if err != nil {
		t.Fatal(err)
	}
	if first != nil || gotMan.Project != "wiki" {
		t.Fatalf("manifest = %+v (first=%v), want project wiki consumed first", gotMan, first)
	}

	dst := openFileBackend(t)
	blobs, journals, _, err = importStore(ctx, dst, tr, first)
	if err != nil {
		t.Fatal(err)
	}
	if blobs != 2 || journals != 2 {
		t.Fatalf("import counted %d blobs, %d journals; want 2, 2", blobs, journals)
	}

	// Every key round-trips byte-identical.
	for _, prefix := range []string{"journal/", "blobs/"} {
		objs, err := src.List(ctx, prefix)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range objs {
			want := read(t, src, o.Key)
			got := read(t, dst, o.Key)
			if want != got {
				t.Errorf("%s: imported %q, want %q", o.Key, got, want)
			}
		}
	}
}

func TestImportRejectsCorruptBlob(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// Content that does not hash to its key.
	writeTarFile(tw, blobKey("original"), []byte("tampered"))
	writeTarFile(tw, "journal/dev-1.jsonl", []byte("{}\n"))
	tw.Close()
	gz.Close()

	dst := openFileBackend(t)
	if _, _, _, err := importStore(context.Background(), dst, openTar(t, buf.Bytes()), nil); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("err = %v, want corrupt-archive error", err)
	}
}

func TestImportRejectsForeignEntries(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	writeTarFile(tw, "../../etc/passwd", []byte("nope"))
	tw.Close()
	gz.Close()

	dst := openFileBackend(t)
	if _, _, _, err := importStore(context.Background(), dst, openTar(t, buf.Bytes()), nil); err == nil || !strings.Contains(err.Error(), "unexpected entry") {
		t.Fatalf("err = %v, want unexpected-entry error", err)
	}
}

func TestImportRequiresJournals(t *testing.T) {
	content := "just a blob"
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	writeTarFile(tw, blobKey(content), []byte(content))
	tw.Close()
	gz.Close()

	dst := openFileBackend(t)
	if _, _, _, err := importStore(context.Background(), dst, openTar(t, buf.Bytes()), nil); err == nil || !strings.Contains(err.Error(), "no journals") {
		t.Fatalf("err = %v, want no-journals error", err)
	}
}

func openTar(t *testing.T, b []byte) *tar.Reader {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return tar.NewReader(gz)
}

func read(t *testing.T, be remote.Backend, key string) string {
	t.Helper()
	rc, err := be.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
