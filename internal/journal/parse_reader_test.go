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
	oversizedValid, err := Marshal([]Op{secpkgOp(99, "huge/"+strings.Repeat("x", MaxLineBytes+1024)+".md")})
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
		"oversized-skip": bytes.Join([][]byte{good1, oversizedValid, good2}, nil),
		"forged-pathraw": bytes.Join([][]byte{good1, []byte(forgedPathRaw), good2}, nil),
		"unknown-kind":   bytes.Join([][]byte{good1, []byte(unknownKind), good2}, nil),
		"unterminated":   append(append([]byte(nil), good1...), unterminated...),
		"unterm-oversized": append(append([]byte(nil), good1...),
			bytes.TrimSuffix(oversizedValid, []byte("\n"))...),
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
				t.Fatalf("ParseReader mismatch vs Parse:\ngot:  %+v\nwant: %+v", got, want)
			}
		})
	}

	t.Run("oversized-valid-op-is-skipped-by-both", func(t *testing.T) {
		ops1, _ := Parse(oversizedValid)
		ops2, err := ParseReader(bytes.NewReader(oversizedValid))
		if err != nil {
			t.Fatalf("ParseReader oversized error: %v", err)
		}
		if len(ops1) != 0 || len(ops2) != 0 {
			t.Fatalf("expected oversized valid op (> %d) to be skipped by both: Parse=%d ParseReader=%d", MaxLineBytes, len(ops1), len(ops2))
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
}
