package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/remote"
)

// Security tests for `bdrive export` / `bdrive import`. Import's input is a
// tar.gz the user obtained from somewhere — a colleague, a download, a hub
// that is not theirs — so every archive member is attacker-controlled. Export's
// input is a hub's object listing, which is attacker-controlled the moment the
// hub is not yours ("works against any hub in either direction").

// secpkgTar builds a tar.gz from the given headers+bodies, manifest first.
func secpkgTar(t *testing.T, withManifest bool, entries ...func(*tar.Writer)) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if withManifest {
		body := []byte(`{"project":"wiki"}`)
		if err := tw.WriteHeader(&tar.Header{Name: manifestName, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		tw.Write(body)
	}
	for _, e := range entries {
		e(tw)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

// secpkgEntry writes one member with a literal body.
func secpkgEntry(t *testing.T, h *tar.Header, body string) func(*tar.Writer) {
	return func(tw *tar.Writer) {
		if h.Size == 0 && body != "" {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Logf("writer refused header %q: %v", h.Name, err)
			return
		}
		tw.Write([]byte(body))
	}
}

// secpkgReadArchive returns a tar.Reader positioned past the manifest, the
// way importCmd does it.
func secpkgReadArchive(t *testing.T, r io.Reader) (*tar.Reader, *tar.Header) {
	t.Helper()
	gz, err := gzip.NewReader(r)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	_, first, err := readManifest(tr)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	return tr, first
}

// TestSec_Migrate_ArchiveEntryCannotEscapeTheStorePrefix asserts the refused
// case: every classic tar-extraction trick is rejected by importStore's key
// allowlist, and nothing lands outside journal/ + blobs/ in the target store
// or anywhere on the local filesystem.
func TestSec_Migrate_ArchiveEntryCannotEscapeTheStorePrefix(t *testing.T) {
	ctx := context.Background()
	hostile := []*tar.Header{
		{Name: "../../../../etc/cron.d/pwn", Mode: 0o644},
		{Name: "/etc/cron.d/pwn", Mode: 0o644},
		{Name: "journal/../../../../etc/pwn.jsonl", Mode: 0o644},
		{Name: "blobs/../../../../etc/pwn", Mode: 0o644},
		{Name: "./../.ssh/authorized_keys", Mode: 0o644},
		{Name: "C:\\Windows\\System32\\pwn.jsonl", Mode: 0o644},
		{Name: "journal/evil.jsonl\x00/../../pwn", Mode: 0o644},
		{Name: "journal/pwn.jsonl", Mode: 0o4755}, // setuid bits on a store key
		{Name: "blobs/" + strings.Repeat("A", 64), Mode: 0o644},
		{Name: "journal/dev.JSONL", Mode: 0o644},
		{Name: "journal/dev.jsonl", Typeflag: tar.TypeSymlink, Linkname: "../../../../etc/passwd"},
		{Name: "journal/hard.jsonl", Typeflag: tar.TypeLink, Linkname: "/etc/passwd"},
		{Name: "journal/fifo.jsonl", Typeflag: tar.TypeFifo},
		{Name: "journal/dev0.jsonl", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3},
	}
	for _, h := range hostile {
		t.Run(strings.ReplaceAll(h.Name, "/", "_"), func(t *testing.T) {
			root := t.TempDir()
			be, err := remote.Open(ctx, "file://"+filepath.Join(root, "store"))
			if err != nil {
				t.Fatal(err)
			}
			defer be.Close()
			hdr := *h
			r := secpkgTar(t, true, secpkgEntry(t, &hdr, "planted"))
			tr, first := secpkgReadArchive(t, r)
			_, _, _, _ = importStore(ctx, be, tr, first)

			for _, p := range secpkgFilesUnder(t, root) {
				rel, err := filepath.Rel(filepath.Join(root, "store"), p)
				if err != nil || strings.HasPrefix(rel, "..") {
					t.Errorf("member %q created %q, outside the store", h.Name, p)
					continue
				}
				slash := filepath.ToSlash(rel)
				if !strings.HasPrefix(slash, "journal/") && !strings.HasPrefix(slash, "blobs/") {
					t.Errorf("member %q created store key %q, outside journal/ and blobs/", h.Name, slash)
				}
				if fi, err := os.Stat(p); err == nil && fi.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
					t.Errorf("member %q produced %q with mode %v", h.Name, p, fi.Mode())
				}
			}
		})
	}
}

// TestSec_Migrate_CorruptBlobNeverLandsInTheTargetStore: importStore streams
// each blob straight into be.Put and only then compares the hash it teed off.
// By the time the mismatch is noticed the object is already stored under a key
// that promises different content — in a project the user has just created on
// their hub, alongside the journals that reference it (journals are written
// first). Every device that later connects fails its pull with "blob corrupt
// on remote" and never recovers.
func TestSec_Migrate_CorruptBlobNeverLandsInTheTargetStore(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	be, err := remote.Open(ctx, "file://"+root)
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()

	honest := "the content this key promises"
	sum := sha256.Sum256([]byte(honest))
	key := "blobs/" + hex.EncodeToString(sum[:])

	r := secpkgTar(t, true,
		secpkgEntry(t, &tar.Header{Name: "journal/dev-1.jsonl", Mode: 0o644}, `{"path":"a.md"}`+"\n"),
		secpkgEntry(t, &tar.Header{Name: key, Mode: 0o644}, "SUBSTITUTED CONTENT"),
	)
	tr, first := secpkgReadArchive(t, r)
	if _, _, _, err := importStore(ctx, be, tr, first); err == nil {
		t.Fatal("importStore accepted a blob whose content does not match its key")
	}

	rc, err := be.Get(ctx, key)
	if err != nil {
		return // not stored: the secure outcome
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != honest {
		t.Errorf("after the refused import, %s still holds %q — a content-addressed key serving content that does not hash to it", key, b)
	}
}

// secpkgHostileBackend is a hub that answers List with keys of its own
// choosing. A hub is not trusted to name its own objects: export copies each
// key into the archive verbatim as a tar member name.
type secpkgHostileBackend struct{ keys []string }

func (b *secpkgHostileBackend) Put(context.Context, string, io.Reader, int64) error { return nil }
func (b *secpkgHostileBackend) Get(_ context.Context, key string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("x")), nil
}
func (b *secpkgHostileBackend) List(_ context.Context, prefix string) ([]remote.Object, error) {
	var out []remote.Object
	for _, k := range b.keys {
		if strings.HasPrefix(k, prefix) || prefix == "journal/" && strings.HasPrefix(k, "journal") {
			out = append(out, remote.Object{Key: k, Size: 1})
		}
	}
	return out, nil
}
func (b *secpkgHostileBackend) Exists(context.Context, string) (bool, error) { return true, nil }
func (b *secpkgHostileBackend) Close() error                                 { return nil }

// TestSec_Migrate_ExportOnlyEmitsStoreKeys: exportStore trusts the hub's
// listing and writes o.Key as the tar member name with no check that it is a
// store key. A hostile (or compromised) hub therefore hands the user an
// archive whose members are ../../.ssh/authorized_keys — and the export's own
// printed advice is to pass that file around. bdrive import rejects it, but
// `tar xzf` does not.
func TestSec_Migrate_ExportOnlyEmitsStoreKeys(t *testing.T) {
	ctx := context.Background()
	be := &secpkgHostileBackend{keys: []string{
		"journal/../../../../.ssh/authorized_keys",
		"journal/dev-1.jsonl",
		"blobs/../../../../etc/cron.d/pwn",
	}}
	var buf bytes.Buffer
	if _, _, _, err := exportStore(ctx, be, &buf, exportManifest{Project: "wiki", ExportedAt: time.Now().UTC()}); err != nil {
		return // refusing the hub's key is the secure outcome
	}
	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == manifestName {
			continue
		}
		clean := filepath.ToSlash(filepath.Clean(h.Name))
		if strings.HasPrefix(clean, "..") || strings.HasPrefix(h.Name, "/") ||
			(!strings.HasPrefix(clean, "journal/") && !strings.HasPrefix(clean, "blobs/")) {
			t.Errorf("export wrote archive member %q, which extracts outside the store layout", h.Name)
		}
	}
}

// secpkgFilesUnder lists every regular file under root.
func secpkgFilesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		out = append(out, p)
		return nil
	})
	sort.Strings(out)
	return out
}
