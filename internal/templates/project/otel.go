//go:build ignore

package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// setupOTel initializes OpenTelemetry trace and metric providers using OTLP/gRPC.
// Configuration is driven by environment variables:
//   - OTEL_EXPORTER_OTLP_ENDPOINT: OTLP collector endpoint (e.g. "http://localhost:4317")
//   - OTEL_SERVICE_NAME: logical service name for traces and metrics
//
// If OTEL_EXPORTER_OTLP_ENDPOINT is not set, no-op providers are used and the
// returned shutdown function is a no-op.
func setupOTel(ctx context.Context) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }

	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return noop, nil
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
	if hostname, err := os.Hostname(); err == nil {
		attrs = append(attrs, semconv.ServiceInstanceIDKey.String(hostname))
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(attrs...),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, fmt.Errorf("creating otel resource: %w", err)
	}

	traceExporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating trace exporter: %w", err)
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
		return nil, fmt.Errorf("creating metric exporter: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	return func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}, nil
}
