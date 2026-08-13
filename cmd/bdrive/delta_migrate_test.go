package main

// Row C8 of the delta-sync goal: a chunked-only project — content that exists
// only as chunks/ + manifests/, no whole blobs — survives export → import with
// full fidelity. Missing this silently is the known trap: an archive without
// the new key classes looks complete and has lost the file content.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDelta_Migrate_RoundTrip(t *testing.T) {
	ctx := context.Background()
	src := openFileBackend(t)

	// A chunked file: two chunks and a manifest keyed by the whole file's
	// sha. No blobs/ entry for it — that is the point. Plus a normal small
	// blob and a journal, so all four key classes travel together.
	c1, c2 := strings.Repeat("x", 300), strings.Repeat("y", 400)
	whole := c1 + c2
	put(t, src, "chunks/"+strings.TrimPrefix(blobKey(c1), "blobs/"), c1)
	put(t, src, "chunks/"+strings.TrimPrefix(blobKey(c2), "blobs/"), c2)
	fileSha := strings.TrimPrefix(blobKey(whole), "blobs/")
	man := `{"v":1,"size":700,"chunks":[]}`
	put(t, src, "manifests/"+fileSha, man)
	put(t, src, blobKey("small"), "small")
	put(t, src, "journal/dev-1.jsonl", `{"path":"a.md"}`+"\n")

	var buf bytes.Buffer
	blobs, journals, _, err := exportStore(ctx, src, &buf,
		exportManifest{Project: "wiki", ExportedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	// chunks and manifests count with blobs: 2 chunks + 1 manifest + 1 blob.
	if blobs != 4 || journals != 1 {
		t.Fatalf("export counted %d blobs, %d journals; want 4, 1", blobs, journals)
	}

	gz, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	_, first, err := readManifest(tr)
	if err != nil {
		t.Fatal(err)
	}
	dst := openFileBackend(t)
	if _, _, _, err := importStore(ctx, dst, tr, first, false); err != nil {
		t.Fatal(err)
	}

	for _, prefix := range []string{"journal/", "blobs/", "chunks/", "manifests/"} {
		objs, err := src.List(ctx, prefix)
		if err != nil {
			t.Fatal(err)
		}
		if len(objs) == 0 && prefix != "journal/" {
			t.Fatalf("source lost its %s keys", prefix)
		}
		for _, o := range objs {
			if got, want := read(t, dst, o.Key), read(t, src, o.Key); got != want {
				t.Errorf("%s: imported %q, want %q", o.Key, got, want)
			}
		}
	}
}

// TestDelta_Import_RefusesIncompleteArchive: an archive whose journal names
// content the archive does not hold is refused, naming the missing path.
// This is what a pre-delta `bdrive export` produces against a delta-sync hub
// (it enumerates only journal/ and blobs/, so chunked large files vanish) —
// importing it used to succeed and silently lose every large file.
func TestDelta_Import_RefusesIncompleteArchive(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	mb, err := json.MarshalIndent(exportManifest{Project: "wiki", ExportedAt: time.Now().UTC()}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTarFile(tw, manifestName, mb); err != nil {
		t.Fatal(err)
	}
	// The journal references a blob for docs/big.bin; the archive holds
	// neither blobs/<sha> nor manifests/<sha> for it — the old-export shape.
	missing := strings.TrimPrefix(blobKey("the large file the old export dropped"), "blobs/")
	op := `{"seq":1,"lamport":1,"device":"dev-1","kind":"put","path":"docs/big.bin","blob":"` + missing + `","size":9000000}`
	if err := writeTarFile(tw, "journal/dev-1.jsonl", []byte(op+"\n")); err != nil {
		t.Fatal(err)
	}
	if err := writeTarFile(tw, blobKey("present"), []byte("present")); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()

	zr, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	_, first, err := readManifest(tr)
	if err != nil {
		t.Fatal(err)
	}
	dst := openFileBackend(t)
	_, _, _, err = importStore(ctx, dst, tr, first, false)
	if err == nil {
		t.Fatal("an archive missing referenced content was imported")
	}
	if !strings.Contains(err.Error(), "docs/big.bin") {
		t.Fatalf("refusal does not name the missing path: %v", err)
	}
}

// TestDelta_Import_RefusesManifestNamingAbsentChunks: a manifest in an
// archive must bring its chunks along — one indirection deeper than the
// blob-completeness check, and the same silent-loss shape if missed.
func TestDelta_Import_RefusesManifestNamingAbsentChunks(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	mb, err := json.MarshalIndent(exportManifest{Project: "wiki", ExportedAt: time.Now().UTC()}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTarFile(tw, manifestName, mb); err != nil {
		t.Fatal(err)
	}
	fileSha := strings.TrimPrefix(blobKey("whole file"), "blobs/")
	absent := strings.TrimPrefix(blobKey("chunk not in archive"), "blobs/")
	man := `{"v":1,"size":20,"chunks":[{"h":"` + absent + `","n":20}]}`
	if err := writeTarFile(tw, "manifests/"+fileSha, []byte(man)); err != nil {
		t.Fatal(err)
	}
	op := `{"seq":1,"lamport":1,"device":"dev-1","kind":"put","path":"docs/big.bin","blob":"` + fileSha + `","size":20}`
	if err := writeTarFile(tw, "journal/dev-1.jsonl", []byte(op+"\n")); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()

	zr, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	_, first, err := readManifest(tr)
	if err != nil {
		t.Fatal(err)
	}
	dst := openFileBackend(t)
	if _, _, _, err := importStore(ctx, dst, tr, first, false); err == nil {
		t.Fatal("an archive whose manifest names absent chunks was imported")
	}
}

// TestDelta_Import_RejectsCorruptChunk: a chunk is content-addressed, so
// import applies the same key-equals-hash check blobs get.
func TestDelta_Import_RejectsCorruptChunk(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	mb, err := json.MarshalIndent(exportManifest{Project: "wiki", ExportedAt: time.Now().UTC()}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTarFile(tw, manifestName, mb); err != nil {
		t.Fatal(err)
	}
	// A chunk whose key does not match its content.
	key := "chunks/" + strings.TrimPrefix(blobKey("honest"), "blobs/")
	if err := writeTarFile(tw, key, []byte("hostile")); err != nil {
		t.Fatal(err)
	}
	if err := writeTarFile(tw, "journal/dev-1.jsonl", []byte(`{"path":"a.md"}`+"\n")); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()

	zr, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	_, first, err := readManifest(tr)
	if err != nil {
		t.Fatal(err)
	}
	dst := openFileBackend(t)
	if _, _, _, err := importStore(ctx, dst, tr, first, false); err == nil {
		t.Fatal("a chunk that does not hash to its key was imported")
	}
}
