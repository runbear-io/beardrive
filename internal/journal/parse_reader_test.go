package journal

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

func TestParseReaderParityCapAndFailClosed(t *testing.T) {
	good1, err := Marshal([]Op{secpkgOp(1, "a.md"), secpkgOp(2, "b.md")})
	if err != nil {
		t.Fatal(err)
	}
	good2, err := Marshal([]Op{secpkgOp(3, "c.md"), secpkgOp(4, "notes/caf\xe9.md"), secpkgOp(5, "notes/caf\xff.md")})
	if err != nil {
		t.Fatal(err)
	}
	largeNote := secpkgOp(99, "notes/large-note.md")
	largeNote.Note = strings.Repeat("x", (16<<20)+1024)
	largeLine, err := Marshal([]Op{largeNote})
	if err != nil {
		t.Fatal(err)
	}
	forgedPathRaw := `{"seq":6,"kind":"put","path":"safe.md","path_raw":"ZmlsZS5tZA=="}` + "\n"
	unknownKind := `{"seq":7,"kind":"rename","path":"z.md"}` + "\n"
	unterminated, err := Marshal([]Op{secpkgOp(8, "last.md")})
	if err != nil {
		t.Fatal(err)
	}
	unterminated = bytes.TrimSuffix(unterminated, []byte("\n"))

	cases := map[string][]byte{
		"empty":          nil,
		"blank-lines":    []byte("\n   \n\t\n"),
		"normal+raw":     append(append([]byte(nil), good1...), good2...),
		"corrupt-middle": bytes.Join([][]byte{good1, []byte("{corrupt\n"), good2}, nil),
		"large-note>16m": bytes.Join([][]byte{good1, largeLine, good2}, nil),
		"forged-pathraw": bytes.Join([][]byte{good1, []byte(forgedPathRaw), good2}, nil),
		"unknown-kind":   bytes.Join([][]byte{good1, []byte(unknownKind), good2}, nil),
		"unterminated":   append(append([]byte(nil), good1...), unterminated...),
		"unterm-large":   append(append([]byte(nil), good1...), bytes.TrimSuffix(largeLine, []byte("\n"))...),
	}

	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			want, err := Parse(data)
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}
			got, err := ParseReader(iotest.OneByteReader(bytes.NewReader(data)))
			if err != nil {
				t.Fatalf("ParseReader error: %v", err)
			}
			if len(got) == 0 && len(want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ParseReader mismatch vs Parse:\ngot:  %d ops\nwant: %d ops", len(got), len(want))
			}
		})
	}

	t.Run("large-op-over-16MiB-preserved", func(t *testing.T) {
		ops1, _ := Parse(largeLine)
		ops2, err := ParseReader(bytes.NewReader(largeLine))
		if err != nil {
			t.Fatalf("ParseReader error: %v", err)
		}
		if len(ops1) != 1 || len(ops2) != 1 || ops1[0].Note != largeNote.Note || ops2[0].Note != largeNote.Note {
			t.Fatalf("large >16MiB op not preserved: Parse=%d ParseReader=%d", len(ops1), len(ops2))
		}
	})

	t.Run("mid-stream-io-error-fails-closed", func(t *testing.T) {
		r := io.MultiReader(bytes.NewReader(good1), iotest.ErrReader(io.ErrUnexpectedEOF))
		got, err := ParseReader(r)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("expected ErrUnexpectedEOF, got %v (ops=%d)", err, len(got))
		}
		if len(got) != 0 {
			t.Fatalf("expected no partial ops on error, got %d", len(got))
		}
	})

	t.Run("multi-buffer-line", func(t *testing.T) {
		multiBuf, err := Marshal([]Op{secpkgOp(10, "notes/"+strings.Repeat("a", 128<<10)+".md")})
		if err != nil {
			t.Fatal(err)
		}
		want, err := Parse(multiBuf)
		if err != nil || len(want) != 1 {
			t.Fatalf("Parse multiBuf: len=%d err=%v", len(want), err)
		}
		got, err := ParseReader(bytes.NewReader(multiBuf))
		if err != nil {
			t.Fatalf("ParseReader multiBuf: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ParseReader multiBuf mismatch: got=%+v want=%+v", got, want)
		}
	})
}
