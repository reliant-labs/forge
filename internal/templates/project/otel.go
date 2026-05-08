//go:build ignore

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

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

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// setupOTel initializes OpenTelemetry trace and metric providers.
// A Prometheus exporter is always registered so /metrics is available.
// If OTEL_EXPORTER_OTLP_ENDPOINT is set, an OTLP/gRPC exporter is also
// configured for push-based collection.
//
// Configuration is driven by environment variables:
//   - OTEL_EXPORTER_OTLP_ENDPOINT: OTLP collector endpoint (e.g. "http://localhost:4317")
//   - OTEL_SERVICE_NAME: logical service name for traces and metrics
//
// Returns a shutdown function, an http.Handler for /metrics, and any error.
func setupOTel(ctx context.Context) (func(context.Context) error, http.Handler, error) {
	// Prometheus exporter is always enabled so /metrics works regardless of OTLP.
	promReader, err := promexporter.New()
	if err != nil {
		return nil, nil, fmt.Errorf("creating prometheus exporter: %w", err)
	}
	metricsHandler := promhttp.Handler()

	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		// No OTLP endpoint — set up Prometheus-only MeterProvider.
		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(promReader),
		)
		otel.SetMeterProvider(mp)
		return func(ctx context.Context) error { return mp.Shutdown(ctx) }, metricsHandler, nil
	}

	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "unknown"
	}

	attrs := []attribute.KeyValue{
		semconv.ServiceNameKey.String(serviceName),
	}
	if version != "" && version != "dev" {
		attrs = append(attrs, semconv.ServiceVersionKey.String(version))
	}
	if hostname, hostnameErr := os.Hostname(); hostnameErr == nil {
		attrs = append(attrs, semconv.ServiceInstanceIDKey.String(hostname))
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(attrs...),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("creating otel resource: %w", err)
	}

	traceExporter, err := otlptracegrpc.New(ctx)
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

	metricExporter, err := otlpmetricgrpc.New(ctx)
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