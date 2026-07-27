package config

import (
	"reflect"
	"testing"
)

func TestNormalizeInclude(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want []string
	}{
		{in: nil, want: nil},
		{in: []string{"wiki/"}, want: []string{"/wiki/"}}, // legacy config, anchored on read
		{in: []string{"wiki"}, want: []string{"/wiki"}},
		{in: []string{"/wiki/"}, want: []string{"/wiki/"}}, // already anchored
		{in: []string{"a/b/"}, want: []string{"a/b/"}},     // compile() anchors these already
		{in: []string{"*.md"}, want: []string{"*.md"}},     // deliberate pattern, left alone
		{in: []string{"!keep"}, want: []string{"!keep"}},
		{in: []string{"wiki/", "*.md"}, want: []string{"/wiki/", "*.md"}},
	} {
		in := append([]string(nil), tc.in...)
		if got := normalizeInclude(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("normalizeInclude(%q) = %q, want %q", in, got, tc.want)
		}
	}
}
