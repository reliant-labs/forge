package asserted

import (
	"errors"
	"testing"

	"connectrpc.com/connect"
)

func codeOf(t *testing.T, err error) connect.Code {
	t.Helper()
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Fatalf("want a connect error, got %v", err)
	}
	return ce.Code()
}

func TestRefuse(t *testing.T) {
	if got := codeOf(t, Refuse()); got != connect.CodePermissionDenied {
		t.Fatalf("Refuse: %v", got)
	}
}
