package main

import (
	"reflect"
	"testing"
)

func TestParseFlag(t *testing.T) {
	for _, tc := range []struct {
		arg       string
		name, val string
		hasValue  bool
	}{
		{"-exclude=a,b", "exclude", "a,b", true},
		{"--exclude=a", "exclude", "a", true},
		{"-exclude", "exclude", "", false},
		{"--exclude", "exclude", "", false},
		{"-exclude=", "exclude", "", true},
		{"-contract.exclude=x=y", "contract.exclude", "x=y", true}, // only the first = splits
		{"./...", "", "", false},
		{"internal/foo", "", "", false},
		{"-", "", "", false},
		{"--", "", "", false},
		{"", "", "", false},
	} {
		name, val, has := parseFlag(tc.arg)
		if name != tc.name || val != tc.val || has != tc.hasValue {
			t.Errorf("parseFlag(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.arg, name, val, has, tc.name, tc.val, tc.hasValue)
		}
	}
}

func TestSplitExcludes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"internal/naming", []string{"internal/naming"}},
		{"internal/naming,internal/assets", []string{"internal/naming", "internal/assets"}},
		{" internal/naming , internal/assets ", []string{"internal/naming", "internal/assets"}},
		{"a,,b,", []string{"a", "b"}},
		{",", []string{}},
	} {
		got := splitExcludes(tc.in)
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitExcludes(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestExtractExcludeFlag(t *testing.T) {
	for _, tc := range []struct {
		name         string
		args         []string
		wantExcludes []string
		wantRest     []string
	}{
		{"no flags", []string{"./..."}, nil, []string{"./..."}},
		{"no args", nil, nil, []string{}},
		{"equals form", []string{"-exclude=a,b", "./..."}, []string{"a", "b"}, []string{"./..."}},
		{"double dash equals form", []string{"--exclude=a", "./..."}, []string{"a"}, []string{"./..."}},
		{"space form consumes the next arg", []string{"-exclude", "a,b", "./..."}, []string{"a", "b"}, []string{"./..."}},
		{"flag after the packages", []string{"./...", "-exclude=a"}, []string{"a"}, []string{"./..."}},
		{"other flags are kept in place", []string{"-json", "-exclude=a", "-fix", "./..."}, []string{"a"}, []string{"-json", "-fix", "./..."}},
		{"per-analyzer exclude is not the top-level one", []string{"-contract.exclude=a", "./..."}, nil, []string{"-contract.exclude=a", "./..."}},
		{"every occurrence is consumed and they accumulate", []string{"-exclude=a", "-exclude=b", "./..."}, []string{"a", "b"}, []string{"./..."}},
		{"bare -exclude at the end clears and is consumed", []string{"./...", "-exclude"}, nil, []string{"./..."}},
		{"empty value excludes nothing", []string{"-exclude=", "./..."}, nil, []string{"./..."}},
		{"after -- everything is positional", []string{"-exclude=a", "--", "-exclude=b"}, []string{"a"}, []string{"--", "-exclude=b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex, rest := extractExcludeFlag(tc.args)
			if len(ex) != len(tc.wantExcludes) || (len(ex) > 0 && !reflect.DeepEqual(ex, tc.wantExcludes)) {
				t.Errorf("excludes = %q, want %q", ex, tc.wantExcludes)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("remaining = %q, want %q", rest, tc.wantRest)
			}
		})
	}
}

// extractExcludeFlag must not write through to the caller's slice.
func TestExtractExcludeFlagDoesNotMutateItsInput(t *testing.T) {
	args := []string{"-exclude=a", "./..."}
	extractExcludeFlag(args)
	if !reflect.DeepEqual(args, []string{"-exclude=a", "./..."}) {
		t.Errorf("input mutated: %q", args)
	}
}
