package observe

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	goruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Config is the typed configuration for Setup. The standard OTEL_* environment
// variables are the DEFAULT source for everything exporter-related (see
// otelenv.go and the package doc); a non-zero field here overrides its variable.
type Config struct {
	// ServiceName is the compile-time default service.name. OTEL_SERVICE_NAME,
	// when set, wins over it. Empty falls back to "unknown".
	ServiceName string

	// ServiceVersion is reported as semconv service.version when non-empty and
	// not "dev". The "dev" sentinel is treated as "no version".
	ServiceVersion string

	// OTLPEndpoint overrides OTEL_EXPORTER_OTLP_ENDPOINT (e.g.
	// "http://localhost:4318" or "collector:4318"). It is a BASE endpoint: for
	// http/protobuf the per-signal path (/v1/traces, ...) is appended. When both
	// this field and every endpoint variable are empty, no OTLP exporter is
	// configured and only the always-on Prometheus reader is wired, so /metrics
	// still works. The protocol, headers, timeout and TLS settings always come
	// from the environment.
	OTLPEndpoint string

	// OTLPLogs turns the OTLP logs exporter on, like OTEL_LOGS_EXPORTER=otlp. It
	// is off by default and only takes effect with an endpoint. It is a second
	// shipping path for logs: do not enable it where stdout/file collection
	// already ships the same lines (see NewLogHandler).
	OTLPLogs bool

	// InstanceID is reported as semconv service.instance.id when non-empty.
	// Callers typically pass os.Hostname().
	InstanceID string
}

// Setup initializes OpenTelemetry from Config plus the standard OTEL_*
// environment and installs the providers globally.
//
// Always, even with export off: the W3C TraceContext+Baggage propagator is
// installed (so an incoming traceparent is honoured and forwarded by every
// observe client), and a Prometheus reader backs the returned /metrics handler.
//
// When an OTLP endpoint is configured and OTEL_SDK_DISABLED is not "true":
// traces and metrics are exported over OTLP, http/protobuf by default or grpc
// when OTEL_EXPORTER_OTLP_PROTOCOL says so, with OTEL_EXPORTER_OTLP_HEADERS
// (and the per-signal variants) applied by the exporter; Go runtime metrics are
// recorded; and, only when explicitly switched on, logs are exported too.
//
// It returns a shutdown function (flushes/stops the providers), an http.Handler
// for /metrics, and any error.
func Setup(ctx context.Context, cfg Config) (func(context.Context) error, http.Handler, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	promReader, err := promexporter.New()
	if err != nil {
		return nil, nil, fmt.Errorf("creating prometheus exporter: %w", err)
	}
	metricsHandler := promhttp.Handler()

	wantTraces := exportEnabled(signalTraces, cfg)
	wantMetrics := exportEnabled(signalMetrics, cfg)
	wantLogs := exportEnabled(signalLogs, cfg)

	if !wantTraces && !wantMetrics && !wantLogs {
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(promReader))
		otel.SetMeterProvider(mp)
		return func(ctx context.Context) error { return mp.Shutdown(ctx) }, metricsHandler, nil
	}

	res, err := resourceFromConfig(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("creating otel resource: %w", err)
	}

	var shutdowns []func(context.Context) error
	fail := func(err error) (func(context.Context) error, http.Handler, error) {
		for _, f := range shutdowns {
			_ = f(ctx)
		}
		return nil, nil, err
	}

	if wantTraces {
		exp, err := newTraceExporter(ctx, cfg)
		if err != nil {
			return fail(fmt.Errorf("creating trace exporter: %w", err))
		}
		tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
		otel.SetTracerProvider(tp)
		shutdowns = append(shutdowns, tp.Shutdown)
	}

	mpOpts := []sdkmetric.Option{sdkmetric.WithReader(promReader), sdkmetric.WithResource(res)}
	if wantMetrics {
		exp, err := newMetricExporter(ctx, cfg)
		if err != nil {
			return fail(fmt.Errorf("creating metric exporter: %w", err))
		}
		mpOpts = append(mpOpts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)))
	}
	mp := sdkmetric.NewMeterProvider(mpOpts...)
	otel.SetMeterProvider(mp)
	shutdowns = append(shutdowns, mp.Shutdown)
	if wantMetrics {
		if err := goruntime.Start(goruntime.WithMeterProvider(mp)); err != nil {
			return fail(fmt.Errorf("starting go runtime metrics: %w", err))
		}
	}

	if wantLogs {
		exp, err := newLogExporter(ctx, cfg)
		if err != nil {
			return fail(fmt.Errorf("creating log exporter: %w", err))
		}
		lp := sdklog.NewLoggerProvider(
			sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
			sdklog.WithResource(res),
		)
		otel.SetLoggerProvider(lp)
		shutdowns = append(shutdowns, lp.Shutdown)
	}

	return func(ctx context.Context) error {
		errs := make([]error, 0, len(shutdowns))
		for _, f := range shutdowns {
			errs = append(errs, f(ctx))
		}
		return errors.Join(errs...)
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
