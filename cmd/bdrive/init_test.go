package main

import (
	"reflect"
	"testing"
)

func TestScopeRemove(t *testing.T) {
	include := []string{"wiki/", "docs/", "*.md"}
	for _, tc := range []struct {
		args []string
		want []string
		err  bool
	}{
		{args: []string{"docs"}, want: []string{"wiki/", "*.md"}},
		{args: []string{"docs/"}, want: []string{"wiki/", "*.md"}},   // normalized match
		{args: []string{"*.md"}, want: []string{"wiki/", "docs/"}},   // literal pattern match
		{args: []string{"wiki", "docs"}, want: []string{"*.md"}},
		{args: []string{"notes"}, err: true}, // not in scope
	} {
		got, err := scopeRemove(include, tc.args)
		if tc.err != (err != nil) {
			t.Errorf("scopeRemove(%q) err = %v, want err %v", tc.args, err, tc.err)
			continue
		}
		if !tc.err && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("scopeRemove(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestCleanShared(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want []string
		err  bool
	}{
		{in: nil, want: nil},
		{in: []string{"wiki"}, want: []string{"wiki/"}},
		{in: []string{"wiki", "docs"}, want: []string{"wiki/", "docs/"}},
		{in: []string{" wiki ", "./docs/", "wiki"}, want: []string{"wiki/", "docs/"}}, // trimmed, cleaned, deduped
		{in: []string{"a/b"}, want: []string{"a/b/"}},
		{in: []string{""}, err: true},
		{in: []string{"wiki", ""}, err: true}, // "wiki,,docs" typo must not half-apply
		{in: []string{"."}, err: true},        // would silently mean whole-folder sync
		{in: []string{"../up"}, err: true},
	} {
		got, err := cleanShared(tc.in)
		if tc.err != (err != nil) {
			t.Errorf("cleanShared(%q) err = %v, want err %v", tc.in, err, tc.err)
			continue
		}
		if !tc.err && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("cleanShared(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
