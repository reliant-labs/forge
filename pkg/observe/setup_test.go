package observe

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestResourceFromConfig(t *testing.T) {
	res, err := resourceFromConfig(context.Background(), Config{
		ServiceName:           "api",
		ServiceVersion:        "1.2.3",
		InstanceID:            "instance-1",
		DeploymentEnvironment: "staging",
	})
	if err != nil {
		t.Fatalf("resourceFromConfig returned error: %v", err)
	}

	for key, want := range map[attribute.Key]string{
		"service.name":                "api",
		"service.version":             "1.2.3",
		"service.instance.id":         "instance-1",
		"deployment.environment.name": "staging",
	} {
		got, ok := res.Set().Value(key)
		if !ok || got.AsString() != want {
			t.Errorf("resource attribute %q = %q, %t; want %q, true", key, got.AsString(), ok, want)
		}
	}
}

func TestResourceFromConfig_OmitsEmptyOptionalAttributes(t *testing.T) {
	res, err := resourceFromConfig(context.Background(), Config{ServiceName: "api"})
	if err != nil {
		t.Fatalf("resourceFromConfig returned error: %v", err)
	}

	for _, key := range []attribute.Key{"service.version", "service.instance.id", "deployment.environment.name"} {
		if got, ok := res.Set().Value(key); ok {
			t.Errorf("resource unexpectedly contains %q = %q", key, got.AsString())
		}
	}
}

// Setup must NEVER read os.Getenv / use resource.WithFromEnv. We assert that by
// confirming behaviour is governed entirely by Config: with no endpoint we get
// the Prometheus-only path even if the OTLP env var is set.
func TestSetup_NoOTLPEndpoint_PrometheusOnly(t *testing.T) {
	// Set the env var the OLD code read; the new lib must ignore it.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://should-be-ignored:4317")
	t.Setenv("OTEL_SERVICE_NAME", "should-be-ignored")

	shutdown, handler, err := Setup(context.Background(), Config{
		ServiceName: "test-svc",
		// OTLPEndpoint intentionally empty.
	})
	if err != nil {
		t.Fatalf("Setup returned error: %v", err)
	}
	if handler == nil {
		t.Fatal("expected non-nil metrics handler")
	}
	if shutdown == nil {
		t.Fatal("expected non-nil shutdown func")
	}

	// No OTLP endpoint => the global TracerProvider must remain the no-op
	// default (Setup only installs a TracerProvider on the OTLP path). This is
	// what proves the env var was ignored.
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); ok {
		t.Fatal("did not expect an SDK TracerProvider to be installed when OTLPEndpoint is empty")
	}

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown returned error: %v", err)
	}
}

func TestSetup_WithOTLPEndpoint(t *testing.T) {
	// otlptracegrpc/otlpmetricgrpc are non-blocking by default: New does not
	// dial, so an unreachable endpoint still constructs successfully.
	shutdown, handler, err := Setup(context.Background(), Config{
		ServiceName:    "test-svc",
		ServiceVersion: "1.2.3",
		InstanceID:     "host-abc",
		OTLPEndpoint:   "http://127.0.0.1:4317",
	})
	if err != nil {
		t.Fatalf("Setup returned error: %v", err)
	}
	if handler == nil {
		t.Fatal("expected non-nil metrics handler")
	}

	// On the OTLP path an SDK TracerProvider is installed globally.
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Fatalf("expected an SDK TracerProvider, got %T", otel.GetTracerProvider())
	}

	// shutdown is expected to attempt a final export; against an unreachable
	// collector it returns an export error. Construction success is what we
	// assert here. Use a short, already-cancelled-style ctx so the test does
	// not block on the exporter's default 10s timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = shutdown(ctx)
}

// "dev" version is treated as "no version" — behaviour-preserving with the old
// generated code. We can't easily read the resource back, so we just exercise
// the path to ensure it constructs.
func TestSetup_DevVersionTreatedAsUnset(t *testing.T) {
	shutdown, _, err := Setup(context.Background(), Config{
		ServiceName:    "test-svc",
		ServiceVersion: "dev",
		OTLPEndpoint:   "http://127.0.0.1:4317",
	})
	if err != nil {
		t.Fatalf("Setup returned error: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = shutdown(ctx)
}
