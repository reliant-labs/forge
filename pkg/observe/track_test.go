package observe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/authn"
)

func trackLine(t *testing.T, ctx context.Context, name string, attrs ...slog.Attr) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	ctx = WithLogger(ctx, slog.New(slog.NewJSONHandler(&buf, nil)))
	Track(ctx, name, attrs...)
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("bad log line %q: %v", buf.String(), err)
	}
	return m
}

func TestTrackShape(t *testing.T) {
	SetAppVersion("1.2.3")
	t.Cleanup(func() { SetAppVersion("") })
	ctx := authn.ContextWithClaims(context.Background(), &authn.Claims{UserID: "u-1"})
	m := trackLine(t, ctx, "project_created", slog.String("template", "go"), slog.Int("repos", 2))
	want := map[string]any{
		"level": "INFO", "event.name": "product.project_created", "user.id": "u-1",
		"app.client": "backend", "app.version": "1.2.3",
		"product.template": "go", "product.repos": "2",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v (line %v)", k, m[k], v, m)
		}
	}
}

func TestProductEventsOn(t *testing.T) {
	for v, want := range map[string]bool{
		"": true, "on": true, "true": true, "1": true, "anything": true,
		"off": false, "OFF": false, " off ": false, "false": false, "0": false,
	} {
		if got := productEventsOn(v); got != want {
			t.Errorf("productEventsOn(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestTrackAnonymousHasNoUserID(t *testing.T) {
	m := trackLine(t, context.Background(), "page_viewed")
	if _, ok := m["user.id"]; ok {
		t.Errorf("anonymous event carries user.id: %v", m)
	}
	if m["event.name"] != "product.page_viewed" || m["app.client"] != "backend" {
		t.Errorf("unexpected line: %v", m)
	}
}

func TestTrackRejectsBadNames(t *testing.T) {
	for _, n := range []string{"", "Project", "project-created", "1abc", "a b", strings.Repeat("a", 65), "product.x"} {
		m := trackLine(t, context.Background(), n)
		if _, ok := m["event.name"]; ok {
			t.Errorf("name %q was emitted as a product event", n)
		}
		if m["level"] != "WARN" {
			t.Errorf("name %q: want a WARN, got %v", n, m)
		}
	}
}

func TestTrackBoundsProps(t *testing.T) {
	attrs := make([]slog.Attr, 0, 40)
	for i := 0; i < 40; i++ {
		attrs = append(attrs, slog.String(fmt.Sprintf("k%02d", i), "v"))
	}
	m := trackLine(t, context.Background(), "many", attrs...)
	n := 0
	for k := range m {
		if strings.HasPrefix(k, "product.") {
			n++
		}
	}
	if n != MaxProductProps {
		t.Errorf("kept %d props, want %d", n, MaxProductProps)
	}
	if _, ok := m["product.k39"]; ok {
		t.Error("prop past the limit was kept")
	}

	m = trackLine(t, context.Background(), "long", slog.String("v", strings.Repeat("é", 300)), slog.String("short", "ok"))
	if got := m["product.v"].(string); got != strings.Repeat("é", MaxProductPropValueLen) {
		t.Errorf("value len = %d runes, want %d", len([]rune(got)), MaxProductPropValueLen)
	}
	if m["product.short"] != "ok" {
		t.Errorf("short value mangled: %v", m["product.short"])
	}
}

func TestTrackSystemMarksTheEvent(t *testing.T) {
	signedIn := authn.ContextWithClaims(context.Background(), &authn.Claims{UserID: "caller"})
	many := make([]slog.Attr, 0, MaxProductProps+1)
	for i := 0; i < MaxProductProps; i++ {
		many = append(many, slog.String(fmt.Sprintf("k%02d", i), "v"))
	}
	for _, tc := range []struct {
		name     string
		emit     func(ctx context.Context)
		wantUser string // "" = no user.id
		wantMark bool
	}{
		{"Track is the user's own activity", func(ctx context.Context) { Track(ctx, "project_created") }, "caller", false},
		{"TrackSystem names its user, not the caller", func(ctx context.Context) { TrackSystem(ctx, "owner-1", "billing_state") }, "owner-1", true},
		{"TrackSystem with no user", func(ctx context.Context) { TrackSystem(ctx, "", "nightly_rollup") }, "", true},
		{"SystemActivity marks a Track", func(ctx context.Context) { Track(ctx, "billing_state", SystemActivity()) }, "caller", true},
		{"the marker survives the property cap", func(ctx context.Context) { Track(ctx, "billing_state", append(many, SystemActivity())...) }, "caller", true},
		{"a marked context marks Track", func(ctx context.Context) { Track(WithSystemActivity(ctx), "order_created") }, "caller", true},
		{"a marked context marks TrackAuth", func(ctx context.Context) {
			TrackAuth(WithSystemActivity(ctx), AuthSignedIn, "canary", WithKind(AuthKindCredential), WithMethod(AuthMethodPassword))
		}, "canary", true},
		{"TrackSystem under a marked context is marked once", func(ctx context.Context) { TrackSystem(WithSystemActivity(ctx), "o", "billing_state") }, "o", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.emit(WithLogger(signedIn, slog.New(slog.NewJSONHandler(&buf, nil))))
			var m map[string]any
			if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
				t.Fatalf("bad log line %q: %v", buf.String(), err)
			}
			if got, _ := m[AttrUserID].(string); got != tc.wantUser {
				t.Errorf("user.id = %q, want %q", got, tc.wantUser)
			}
			mark, ok := m[AttrPropPrefix+PropActivity]
			if tc.wantMark != ok || (ok && mark != ActivitySystem) {
				t.Errorf("product.%s = %v (present %v), want marked %v: %v", PropActivity, mark, ok, tc.wantMark, m)
			}
		})
	}
}

func TestTrackFlattensGroups(t *testing.T) {
	m := trackLine(t, context.Background(), "grouped", slog.Group("plan", slog.String("from", "free"), slog.String("to", "pro")))
	if m["product.plan.from"] != "free" || m["product.plan.to"] != "pro" {
		t.Errorf("group not flattened: %v", m)
	}
}
