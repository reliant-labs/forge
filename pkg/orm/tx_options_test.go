package orm

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// Every opener rejects options that leave a decision to a default, before it
// touches the database (the zero Client here has none), and names the line
// that asked.
func TestTxOptions_UnsetLevelIsRejectedNamingTheCallSite(t *testing.T) {
	ctx := context.Background()
	var c Client
	var tx Tx
	ran := false
	fn := func(context.Context) error { ran = true; return nil }
	handleFn := func(Context) error { ran = true; return nil }

	type opener struct {
		name string
		run  func(TxOptions) (error, int)
	}
	line := func() int { _, _, l, _ := runtime.Caller(1); return l }
	openers := []opener{
		{"Client.RunTx", func(o TxOptions) (error, int) { return c.RunTx(ctx, o, fn), line() }},
		{"Client.RunTransaction", func(o TxOptions) (error, int) { return c.RunTransaction(ctx, o, handleFn), line() }},
		{"Client.BeginDetached", func(o TxOptions) (error, int) { _, err := c.BeginDetached(ctx, o); return err, line() }},
		{"Tx.RunTx (a join)", func(o TxOptions) (error, int) { return tx.RunTx(ctx, o, fn), line() }},
		{"Tx.RunTransaction (a join)", func(o TxOptions) (error, int) { return tx.RunTransaction(ctx, o, handleFn), line() }},
	}
	bad := []struct {
		name string
		opts TxOptions
		want string
	}{
		{"zero value", TxOptions{}, "names no isolation level"},
		{"read only, no level", TxOptions{ReadOnly: true, MaxAttempts: 1}, "names no isolation level"},
		{"unknown level", TxOptions{Isolation: 9, MaxAttempts: 1}, "unknown isolation level 9"},
		{"level without attempts", TxOptions{Isolation: IsolationReadCommitted}, "sets no attempt budget"},
		{"negative attempts", Serializable.WithMaxAttempts(-1), "sets no attempt budget"},
	}
	for _, op := range openers {
		for _, b := range bad {
			t.Run(op.name+"/"+b.name, func(t *testing.T) {
				ran = false
				err, at := op.run(b.opts)
				if !errors.Is(err, ErrInvalidTxOptions) {
					t.Fatalf("err = %v, want ErrInvalidTxOptions", err)
				}
				if !strings.Contains(err.Error(), b.want) {
					t.Errorf("err = %q, want it to say %q", err, b.want)
				}
				if site := fmt.Sprintf("tx_options_test.go:%d", at); !strings.Contains(err.Error(), site) {
					t.Errorf("err = %q, want it to name the call site %s", err, site)
				}
				if ran {
					t.Error("fn ran despite invalid options")
				}
			})
		}
	}

	// BeginDetached hands the transaction to the caller and cannot retry:
	// a budget above one is a promise nobody keeps.
	_, err := c.BeginDetached(ctx, Serializable)
	if !errors.Is(err, ErrInvalidTxOptions) || !strings.Contains(err.Error(), "orm.Serializable.WithMaxAttempts(1)") {
		t.Errorf("BeginDetached(Serializable) = %v, want ErrInvalidTxOptions naming orm.Serializable.WithMaxAttempts(1)", err)
	}
}

// The three levels carry the budgets the package documents, and the
// modifiers derive variants without touching them.
func TestTxOptions_LevelsAndModifiers(t *testing.T) {
	cases := []struct {
		opts TxOptions
		want string
	}{
		{Serializable, "serializable, 7 attempts"},
		{RepeatableRead, "repeatable read, 7 attempts"},
		{ReadCommitted, "read committed, 1 attempt"},
		{Serializable.WithMaxAttempts(1), "serializable, 1 attempt"},
		{Serializable.WithReadOnly(), "serializable read only deferrable, 7 attempts"},
		{ReadCommitted.WithReadOnly().WithMaxAttempts(3), "read committed read only, 3 attempts"},
		{TxOptions{}, "unset, 0 attempts"},
	}
	for _, tc := range cases {
		if got := tc.opts.String(); got != tc.want {
			t.Errorf("%#v.String() = %q, want %q", tc.opts, got, tc.want)
		}
	}
	if Serializable.MaxAttempts != DefaultTxMaxAttempts || Serializable.ReadOnly || Serializable.Isolation != IsolationSerializable {
		t.Errorf("a modifier changed Serializable: %#v", Serializable)
	}
	for _, o := range []TxOptions{Serializable, RepeatableRead, ReadCommitted} {
		if err := o.validate("RunTx", false); err != nil {
			t.Errorf("%v rejected: %v", o, err)
		}
	}
	if err := ReadCommitted.validate("BeginDetached", true); err != nil {
		t.Errorf("ReadCommitted rejected by BeginDetached: %v", err)
	}
}
