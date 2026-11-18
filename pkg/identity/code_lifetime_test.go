package identity

import (
	"context"
	"testing"
	"time"
)

// forge#2265: a declared code lifetime converges the instance's
// password-reset code generator — expiry only, length and alphabet kept —
// once, and a minted code's ExpiresAt follows it. Zero declares nothing.
func TestReconcile_CodeLifetime(t *testing.T) {
	const gen = "SECRET_GENERATOR_TYPE_PASSWORD_RESET_CODE"
	f := newFake(t)
	f.mu.Lock()
	f.generators[gen] = map[string]any{"length": float64(8), "expiry": "3600s", "includeUpperLetters": true, "includeDigits": true}
	f.mu.Unlock()
	r, _ := newRec(t, f, func(o *Options) { o.SignInCodes = true })
	ctx := context.Background()
	spec := Spec{Project: ProjectSpec{Name: "p"}, Roles: []Role{{Key: "member", Declared: true, Members: []string{"new@example.com"}}}}

	if _, err := r.Reconcile(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if got := f.generators[gen]["expiry"]; got != "3600s" {
		t.Fatalf("no lifetime declared, yet the generator was written: %v", f.generators[gen])
	}

	spec.CodeLifetime = 24 * time.Hour
	before := f.now()
	rep, err := r.Reconcile(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	g := f.generators[gen]
	if g["expiry"] != "86400s" || g["length"] != float64(8) || g["includeUpperLetters"] != true || g["includeDigits"] != true || g["includeLowerLetters"] != false {
		t.Fatalf("generator = %v, want expiry 86400s with the length and alphabet kept", g)
	}
	for _, c := range rep.SignInCodes {
		if c.ExpiresAt.Before(before.Add(24 * time.Hour)) {
			t.Errorf("ExpiresAt %v ignores the declared lifetime", c.ExpiresAt)
		}
	}
	again, err := r.Reconcile(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range again.Changes {
		if ch.Kind == "secret generator" {
			t.Errorf("a converged generator was written again: %+v", ch)
		}
	}

	for _, d := range []time.Duration{4 * time.Minute, 8 * 24 * time.Hour} {
		if err := (Spec{Project: ProjectSpec{Name: "p"}, CodeLifetime: d}).Validate(); err == nil {
			t.Errorf("CodeLifetime %s validated", d)
		}
	}
}
