package observe

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Config is the explicit, typed configuration for Setup. Every field that
// influences exporter behaviour is named here; Setup itself never calls
// os.Getenv. The resource additionally merges the standard OTEL_SERVICE_NAME and
// OTEL_RESOURCE_ATTRIBUTES variables through the SDK (resource.WithFromEnv), which
// is how the deployment environment reaches it.
type Config struct {
	// ServiceName is the logical service name reported on traces and metrics
	// (semconv service.name). Empty falls back to "unknown".
	ServiceName string

	// ServiceVersion is reported as semconv service.version when non-empty and
	// not "dev". The "dev" sentinel is treated as "no version" to match the
	// prior env-driven behaviour.
	ServiceVersion string

	// OTLPEndpoint is the OTLP/gRPC collector endpoint (e.g.
	// "http://localhost:4317" or "collector:4317"). Empty means no OTLP
	// exporter is configured: only the always-on Prometheus reader is wired,
	// so /metrics still works.
	OTLPEndpoint string

	// InstanceID is reported as semconv service.instance.id when non-empty.
	// Callers typically pass os.Hostname() (the env-free input stays in the
	// app, not in this library).
	InstanceID string
}

// Setup initializes OpenTelemetry trace and metric providers from an explicit
// Config and installs them as the global providers. A Prometheus exporter is
// always registered so the returned metricsHandler (mount at /metrics) is
// available regardless of OTLP. When Config.OTLPEndpoint is non-empty, an
// OTLP/gRPC trace exporter and an OTLP/gRPC metric exporter are also configured
// for push-based collection, the global text-map propagator is set to
// TraceContext+Baggage, and a resource describing the service is attached.
//
// The OTLP endpoint, service name/version and instance id come from Config.
// Resource identity beyond that (notably deployment.environment.name, which is the
// forge env name) arrives through the standard OTEL_RESOURCE_ATTRIBUTES variable,
// merged by the SDK. This is the library form of the code that forge previously
// generated into each app's cmd/otel.go.
//
// It returns a shutdown function (flushes/stops the providers), an http.Handler
// for /metrics, and any error.
func Setup(ctx context.Context, cfg Config) (func(context.Context) error, http.Handler, error) {
	// Prometheus exporter is always enabled so /metrics works regardless of OTLP.
	promReader, err := promexporter.New()
	if err != nil {
		return nil, nil, fmt.Errorf("creating prometheus exporter: %w", err)
	}
	metricsHandler := promhttp.Handler()

	if cfg.OTLPEndpoint == "" {
		// No OTLP endpoint — set up Prometheus-only MeterProvider.
		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(promReader),
		)
		otel.SetMeterProvider(mp)
		return func(ctx context.Context) error { return mp.Shutdown(ctx) }, metricsHandler, nil
	}

	res, err := resourceFromConfig(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("creating otel resource: %w", err)
	}

	// Endpoint is passed explicitly (WithEndpointURL), not auto-read from
	// OTEL_EXPORTER_OTLP_ENDPOINT by the exporter's env defaults.
	traceExporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpointURL(cfg.OTLPEndpoint),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("creating trace exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	metricExporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpointURL(cfg.OTLPEndpoint),
	)
	if err != nil {
		_ = tp.Shutdown(ctx)
		return nil, nil, fmt.Errorf("creating metric exporter: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(promReader),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	return func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}, metricsHandler, nil
}

// resourceFromConfig constructs the OpenTelemetry resource for Setup. It is
// separate from exporter setup so resource identity is testable without a
// collector or global SDK providers.
func resourceFromConfig(ctx context.Context, cfg Config) (*resource.Resource, error) {
	serviceName := cfg.ServiceName
	if serviceName == "" {
		serviceName = "unknown"
	}

	attrs := []attribute.KeyValue{
		semconv.ServiceNameKey.String(serviceName),
	}
	if cfg.ServiceVersion != "" && cfg.ServiceVersion != "dev" {
		attrs = append(attrs, semconv.ServiceVersionKey.String(cfg.ServiceVersion))
	}
	if cfg.InstanceID != "" {
		attrs = append(attrs, semconv.ServiceInstanceIDKey.String(cfg.InstanceID))
	}

	// WithFromEnv is listed after the Config-derived attributes so the standard
	// OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES variables win over them. The
	// deployment environment is never typed config: the platform renders it as
	// OTEL_RESOURCE_ATTRIBUTES=deployment.environment.name=<forge env name>.
	return resource.New(ctx,
		resource.WithAttributes(attrs...),
		resource.WithFromEnv(),
	)
}
