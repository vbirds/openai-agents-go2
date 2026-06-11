package main

import (
	"reflect"
	"testing"
)

func TestSvnRevisionArgs(t *testing.T) {
	cases := []struct {
		target  string
		want    []string
		wantErr bool
	}{
		{"wc", nil, false},
		{"100:105", []string{"-r", "100:105"}, false},
		{"BASE:HEAD", []string{"-r", "BASE:HEAD"}, false},
		{"12345", []string{"-c", "12345"}, false},
		{"HEAD", nil, true},     // single keyword is ambiguous
		{"abc", nil, true},      // not a revision
		{"", nil, true},         // empty
		{"1.5", nil, true},      // not a valid revision number
	}
	for _, tc := range cases {
		got, err := svnRevisionArgs(tc.target)
		if tc.wantErr {
			if err == nil {
				t.Errorf("svnRevisionArgs(%q): expected error", tc.target)
			}
			continue
		}
		if err != nil {
			t.Errorf("svnRevisionArgs(%q): %v", tc.target, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("svnRevisionArgs(%q) = %v, want %v", tc.target, got, tc.want)
		}
	}
}

func TestParseSvnSummarize(t *testing.T) {
	out := "M       src/server.go\n" +
		"A       src/new_file.go\n" +
		"D       src/removed.go\n" +
		" M      props_only_change.go\n" +
		"MM      src/text and props.go\n" +
		"\n"
	got := parseSvnSummarize(out)
	want := []string{
		"src/server.go",
		"src/new_file.go",
		"props_only_change.go",
		"src/text and props.go",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseSvnSummarize = %v, want %v", got, want)
	}

	if got := parseSvnSummarize(""); got != nil {
		t.Errorf("parseSvnSummarize(empty) = %v, want nil", got)
	}
}
