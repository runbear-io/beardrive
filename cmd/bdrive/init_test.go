package main

import (
	"reflect"
	"testing"
)

func TestScopeRemove(t *testing.T) {
	for _, tc := range []struct {
		include []string
		args    []string
		want    []string
		err     bool
	}{
		{include: []string{"/wiki/", "/docs/", "*.md"}, args: []string{"docs"}, want: []string{"/wiki/", "*.md"}},
		{include: []string{"/wiki/", "/docs/", "*.md"}, args: []string{"docs/"}, want: []string{"/wiki/", "*.md"}}, // normalized match
		{include: []string{"/wiki/", "/docs/", "*.md"}, args: []string{"*.md"}, want: []string{"/wiki/", "/docs/"}}, // literal pattern match
		{include: []string{"/wiki/", "/docs/", "*.md"}, args: []string{"wiki", "docs"}, want: []string{"*.md"}},
		{include: []string{"/wiki/", "/docs/", "*.md"}, args: []string{"notes"}, err: true}, // not in scope
		// A config written before include entries were anchored: the
		// unanchored form must still be removable.
		{include: []string{"wiki/", "docs/"}, args: []string{"wiki"}, want: []string{"docs/"}},
	} {
		got, err := scopeRemove(tc.include, tc.args)
		if tc.err != (err != nil) {
			t.Errorf("scopeRemove(%q, %q) err = %v, want err %v", tc.include, tc.args, err, tc.err)
			continue
		}
		if !tc.err && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("scopeRemove(%q, %q) = %q, want %q", tc.include, tc.args, got, tc.want)
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
		{in: []string{"wiki"}, want: []string{"/wiki/"}}, // anchored: only the root-level wiki/
		{in: []string{"wiki", "docs"}, want: []string{"/wiki/", "/docs/"}},
		{in: []string{" wiki ", "./docs/", "wiki"}, want: []string{"/wiki/", "/docs/"}}, // trimmed, cleaned, deduped
		{in: []string{"a/b"}, want: []string{"/a/b/"}},
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
