package testv1

import "testing"

// Test functions in _test.go files should never be flagged by the linter.
// No diagnostics are expected from this file.

func TestCreateItem(t *testing.T) {
	t.Log("test")
}

func TestGetItem(t *testing.T) {
	t.Log("test")
}

func TestExportedHelper(t *testing.T) {
	t.Log("test")
}
