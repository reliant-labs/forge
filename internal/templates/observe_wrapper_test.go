package templates

import (
	"go/format"
	"strings"
	"testing"
)

// observePkgData mirrors the template data runPackageNew builds for the
// shared internal-package templates (internal/cli/package.go).
type observePkgData struct {
	Name       string
	ImportPath string
	Module     string
	Flavor     string
}

// observeSeamData mirrors the template data runPackageNew builds for the
// owned observe_chain.go seam (internal/cli/package.go): the shared package
// fields plus the resolved success-log level.
type observeSeamData struct {
	Name       string
	ImportPath string
	Module     string
	Flavor     string
	LogLevel   string
}

// TestObserveChainSeamTemplate renders the owned observe_chain.go extension
// seam and asserts it is gofmt-valid Go carrying the newObserveChain builder
// (what the generated decorator calls), the per-package tracer/meter identity,
// and the configured success-log level. The seam is flavor-independent — the
// per-method wrappers now live in the GENERATED decorator, derived from the
// interface, so one seam shape serves every flavor.
func TestObserveChainSeamTemplate(t *testing.T) {
	out, err := InternalPkgTemplates().Render("observe_chain.go.tmpl", observeSeamData{
		Name:       "stripe",
		ImportPath: "stripe",
		Module:     "example.com/proj",
		Flavor:     "service",
		LogLevel:   "slog.LevelDebug",
	})
	if err != nil {
		t.Fatalf("render observe_chain.go.tmpl: %v", err)
	}
	src := string(out)

	for _, want := range []string{
		"func newObserveChain() *observe.ComponentChain {",
		`scope := "example.com/proj/internal/stripe"`,
		"observe.RecoverMiddleware(logger)",
		"observe.TraceMiddleware(otel.Tracer(scope))",
		`observe.MetricsMiddleware(otel.Meter(scope), "stripe")`,
		"observe.LogMiddleware(logger, slog.LevelDebug)",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("missing %q:\n%s", want, src)
		}
	}
	// Owned-file banner: the file must never be overwritten by forge.
	if !strings.HasPrefix(src, "// yours: scaffolded once") {
		t.Fatalf("missing owned-scaffold banner:\n%s", src)
	}
	if _, ferr := format.Source(out); ferr != nil {
		t.Fatalf("rendered observe_chain.go is not gofmt-valid: %v\n%s", ferr, src)
	}
}

// TestProvidersTemplate_DefaultClient renders providers.go.tmpl in all
// database shapes and asserts the DefaultClient method + shared instrumented
// base transport are present and the output is gofmt-valid.
//
// The transport() indirection is load-bearing, not style: http.Client reads a
// nil Transport as http.DefaultTransport, so a DefaultClient that reached for
// the field directly would keep serving outbound calls with no OTel span, no
// trace propagation, and no request ID — working, silent, and invisible in
// traces. The negative assertion below is what keeps that state unreachable.
func TestProvidersTemplate_DefaultClient(t *testing.T) {
	for _, tc := range []struct {
		name string
		data any
	}{
		{"no-db", struct {
			Module      string
			HasDatabase bool
			OrmEnabled  bool
		}{Module: "example.com/proj"}},
		{"db", struct {
			Module      string
			HasDatabase bool
			OrmEnabled  bool
		}{Module: "example.com/proj", HasDatabase: true}},
		{"db-orm", struct {
			Module      string
			HasDatabase bool
			OrmEnabled  bool
		}{Module: "example.com/proj", HasDatabase: true, OrmEnabled: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ProjectTemplates().Render("providers.go.tmpl", tc.data)
			if err != nil {
				t.Fatalf("render providers.go.tmpl: %v", err)
			}
			src := string(out)
			for _, want := range []string{
				"func (i *Infra) DefaultClient() *http.Client {",
				"Transport: i.transport(),",
				"func (i *Infra) transport() http.RoundTripper {",
				"i.httpBase = otelhttp.NewTransport(observe.NewRequestIDTransport(http.DefaultTransport))",
				"httpBaseOnce sync.Once",
				"infra.transport()",
				`"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"`,
				`"github.com/reliant-labs/forge/pkg/observe"`,
			} {
				if !strings.Contains(src, want) {
					t.Fatalf("missing %q:\n%s", want, src)
				}
			}
			if strings.Contains(src, "Transport: i.httpBase") {
				t.Fatalf("DefaultClient reads httpBase directly — a nil field would silently degrade to uninstrumented HTTP:\n%s", src)
			}
			if _, ferr := format.Source(out); ferr != nil {
				t.Fatalf("rendered providers.go is not gofmt-valid: %v\n%s", ferr, src)
			}
		})
	}
}

// TestAdapterAndClientTemplates_InjectableHTTPClient guards the fix for the
// client scaffold's hardcoded bare &http.Client{}: both outbound scaffolds
// must accept Deps.HTTPClient with a nil fallback, and render gofmt-valid.
func TestAdapterAndClientTemplates_InjectableHTTPClient(t *testing.T) {
	data := observePkgData{Name: "stripe", ImportPath: "stripe", Module: "example.com/proj", Flavor: "client"}

	clientOut, err := InternalPkgKindTemplates("client").Render("client.go.tmpl", data)
	if err != nil {
		t.Fatalf("render client.go.tmpl: %v", err)
	}
	if !strings.Contains(string(clientOut), "HTTPClient *http.Client") {
		t.Fatalf("client Deps must expose HTTPClient:\n%s", clientOut)
	}
	if !strings.Contains(string(clientOut), "if httpClient == nil {") {
		t.Fatalf("client New must default a nil HTTPClient:\n%s", clientOut)
	}
	if _, ferr := format.Source(clientOut); ferr != nil {
		t.Fatalf("rendered client.go is not gofmt-valid: %v\n%s", ferr, clientOut)
	}

	adapterOut, err := InternalPkgKindTemplates("adapter").Render("adapter.go.tmpl", data)
	if err != nil {
		t.Fatalf("render adapter.go.tmpl: %v", err)
	}
	if !strings.Contains(string(adapterOut), "HTTPClient *http.Client") {
		t.Fatalf("adapter Deps must expose HTTPClient:\n%s", adapterOut)
	}
	if _, ferr := format.Source(adapterOut); ferr != nil {
		t.Fatalf("rendered adapter.go is not gofmt-valid: %v\n%s", ferr, adapterOut)
	}
}
