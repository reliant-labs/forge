package svcerr_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"
	connectv2 "connectrpc.com/connect/v2"

	"github.com/reliant-labs/forge/pkg/svcerr"
)

var (
	errJobDeleted = svcerr.Sentinel(svcerr.ErrNotFound, "job was deleted")
	errLeaseLost  = svcerr.Sentinel(svcerr.ErrFailedPrecondition, "the lease was lost to another replica")
	// A sentinel refining another: it matches both.
	errJobPurged = svcerr.Sentinel(errJobDeleted, "job was deleted and its history purged")
)

// ownSentinel is the hand-rolled shape the Ranger build used (forge friction
// #37): its own text never reaches the client, because svcerr cannot tell it
// from a driver error that happens to unwrap to a kind.
type ownSentinel struct {
	msg  string
	kind error
}

func (e *ownSentinel) Error() string { return e.msg }
func (e *ownSentinel) Unwrap() error { return e.kind }

func TestSentinelCarriesItsMessageToTheWire(t *testing.T) {
	t.Parallel()
	cause := errors.New("pq: row 42 on shard db-7.internal")
	for _, tc := range []struct {
		name     string
		err      error
		sentinel error
		kind     error
		code     connect.Code
		msg      string
	}{
		{"bare", errJobDeleted, errJobDeleted, svcerr.ErrNotFound, connect.CodeNotFound, "job was deleted"},
		{"wrapped with operator context", fmt.Errorf("watch job %s: %w", "ns/a", errJobDeleted), errJobDeleted, svcerr.ErrNotFound, connect.CodeNotFound, "job was deleted"},
		{"with a cause", svcerr.WithCause(errLeaseLost, cause), errLeaseLost, svcerr.ErrFailedPrecondition, connect.CodeFailedPrecondition, "the lease was lost to another replica"},
		{"refined", errJobPurged, errJobDeleted, svcerr.ErrNotFound, connect.CodeNotFound, "job was deleted and its history purged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wrapped := svcerr.Wrap(tc.err)
			var ce *connect.Error
			if !errors.As(wrapped, &ce) {
				t.Fatalf("Wrap(%v) is not a *connect.Error", tc.err)
			}
			if ce.Code() != tc.code || ce.Message() != tc.msg {
				t.Errorf("wire = %v %q, want %v %q", ce.Code(), ce.Message(), tc.code, tc.msg)
			}
			if !errors.Is(wrapped, tc.sentinel) || !errors.Is(wrapped, tc.kind) {
				t.Errorf("Wrap lost the sentinel's identity: Is(sentinel)=%v Is(kind)=%v", errors.Is(wrapped, tc.sentinel), errors.Is(wrapped, tc.kind))
			}
			if svcerr.Code(tc.err) != tc.code {
				t.Errorf("Code = %v, want %v", svcerr.Code(tc.err), tc.code)
			}
			if !svcerr.IsClassified(tc.err) {
				t.Error("a Sentinel must be classified, so WrapOr keeps it")
			}
			if strings.Contains(ce.Message(), "shard") || strings.Contains(ce.Message(), "ns/a") {
				t.Errorf("operator context leaked to the wire: %q", ce.Message())
			}
			v2 := svcerr.ToConnectV2(tc.err)
			if v2.Code() != connectv2.Code(tc.code) || v2.Message() != tc.msg {
				t.Errorf("v2 wire = %v %q, want %v %q", v2.Code(), v2.Message(), tc.code, tc.msg)
			}
		})
	}
}

func TestSentinelsAreDistinctValues(t *testing.T) {
	t.Parallel()
	other := svcerr.Sentinel(svcerr.ErrNotFound, "job was deleted")
	if errors.Is(errJobDeleted, other) || errors.Is(other, errJobDeleted) {
		t.Error("two Sentinel calls with the same text must be different identities")
	}
	if errors.Is(errJobDeleted, errJobPurged) {
		t.Error("a sentinel must not match one that refines it")
	}
	if errors.Is(errLeaseLost, svcerr.ErrNotFound) {
		t.Error("a sentinel matched a kind it does not carry")
	}
}

// TestHandRolledSentinelTextIsWithheld pins the behaviour Sentinel exists to
// give a supported spelling for: a type of the application's own that unwraps
// to a kind keeps its code, and its text stays server-side.
func TestHandRolledSentinelTextIsWithheld(t *testing.T) {
	t.Parallel()
	ce := svcerr.ToConnect(&ownSentinel{msg: "job was deleted", kind: svcerr.ErrNotFound})
	if ce.Code() != connect.CodeNotFound || ce.Message() != svcerr.ErrNotFound.Error() {
		t.Fatalf("hand-rolled sentinel = %v %q, want not_found %q", ce.Code(), ce.Message(), svcerr.ErrNotFound.Error())
	}
}

func TestSentinelRefusesWhatItCannotClassify(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		kind error
		msg  string
		want string
	}{
		{"empty message", svcerr.ErrNotFound, "", "empty message"},
		{"nil kind", nil, "x", "kind must be an svcerr kind"},
		{"unclassified kind", errors.New("boom"), "x", "not an error svcerr classifies"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("Sentinel did not panic")
				}
				if !strings.Contains(fmt.Sprint(r), tc.want) {
					t.Errorf("panic %q does not say %q", r, tc.want)
				}
			}()
			_ = svcerr.Sentinel(tc.kind, tc.msg)
		})
	}
}

func TestSentinelAcceptsEveryKind(t *testing.T) {
	t.Parallel()
	for _, kind := range []error{
		context.Canceled, svcerr.ErrCanceled, svcerr.ErrUnknown, svcerr.ErrInvalidArgument, svcerr.ErrDeadlineExceeded,
		svcerr.ErrNotFound, svcerr.ErrAlreadyExists, svcerr.ErrPermissionDenied, svcerr.ErrResourceExhausted,
		svcerr.ErrFailedPrecondition, svcerr.ErrAborted, svcerr.ErrOutOfRange, svcerr.ErrUnimplemented,
		svcerr.ErrInternal, svcerr.ErrUnavailable, svcerr.ErrDataLoss, svcerr.ErrUnauthenticated,
		svcerr.ErrPlanLimit, svcerr.ErrInsufficientBalance, svcerr.ErrExpired, svcerr.ErrServerDraining,
	} {
		s := svcerr.Sentinel(kind, "copy")
		if got, want := svcerr.Code(s), svcerr.Code(kind); got != want {
			t.Errorf("Sentinel(%v).Code = %v, want %v", kind, got, want)
		}
		if svcerr.ToConnect(s).Message() != "copy" {
			t.Errorf("Sentinel(%v) message = %q, want %q", kind, svcerr.ToConnect(s).Message(), "copy")
		}
	}
}
