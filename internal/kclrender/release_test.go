package kclrender

import (
	"context"
	"reflect"
	"testing"
)

func TestReleaseDArgs(t *testing.T) {
	if got := ReleaseDArgs(context.Background()); got != nil {
		t.Fatalf("unbound render must emit no release binding, got %v", got)
	}
	if got := ReleaseDArgs(WithRelease(context.Background(), "")); got != nil {
		t.Fatalf("an empty release must stay unbound, got %v", got)
	}
	// Quoted, so an all-digit version stays a KCL str (cf. image_tag).
	want := []string{`release_version="20261010"`}
	if got := ReleaseDArgs(WithRelease(context.Background(), "20261010")); !reflect.DeepEqual(got, want) {
		t.Fatalf("ReleaseDArgs = %v, want %v", got, want)
	}
}
