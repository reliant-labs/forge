package observe

import (
	"context"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// The constructors below pass an endpoint option only when the caller set
// Config.OTLPEndpoint. Otherwise the exporter reads OTEL_EXPORTER_OTLP_*
// itself — endpoint (and its per-signal variants), headers, timeout,
// compression and TLS material — which is how headers reach the wire.

func newTraceExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	proto, err := protocolFor(signalTraces)
	if err != nil {
		return nil, err
	}
	url, typed, err := typedURL(cfg, proto, signalTraces)
	if err != nil {
		return nil, err
	}
	if proto == ProtocolGRPC {
		var opts []otlptracegrpc.Option
		if typed {
			opts = append(opts, otlptracegrpc.WithEndpointURL(url))
		}
		return otlptracegrpc.New(ctx, opts...)
	}
	var opts []otlptracehttp.Option
	if typed {
		opts = append(opts, otlptracehttp.WithEndpointURL(url))
	}
	return otlptracehttp.New(ctx, opts...)
}

func newMetricExporter(ctx context.Context, cfg Config) (sdkmetric.Exporter, error) {
	proto, err := protocolFor(signalMetrics)
	if err != nil {
		return nil, err
	}
	url, typed, err := typedURL(cfg, proto, signalMetrics)
	if err != nil {
		return nil, err
	}
	if proto == ProtocolGRPC {
		var opts []otlpmetricgrpc.Option
		if typed {
			opts = append(opts, otlpmetricgrpc.WithEndpointURL(url))
		}
		return otlpmetricgrpc.New(ctx, opts...)
	}
	var opts []otlpmetrichttp.Option
	if typed {
		opts = append(opts, otlpmetrichttp.WithEndpointURL(url))
	}
	return otlpmetrichttp.New(ctx, opts...)
}

func newLogExporter(ctx context.Context, cfg Config) (sdklog.Exporter, error) {
	proto, err := protocolFor(signalLogs)
	if err != nil {
		return nil, err
	}
	url, typed, err := typedURL(cfg, proto, signalLogs)
	if err != nil {
		return nil, err
	}
	if proto == ProtocolGRPC {
		var opts []otlploggrpc.Option
		if typed {
			opts = append(opts, otlploggrpc.WithEndpointURL(url))
		}
		return otlploggrpc.New(ctx, opts...)
	}
	var opts []otlploghttp.Option
	if typed {
		opts = append(opts, otlploghttp.WithEndpointURL(url))
	}
	return otlploghttp.New(ctx, opts...)
}

func typedURL(cfg Config, proto string, s signal) (url string, typed bool, err error) {
	if cfg.OTLPEndpoint == "" {
		return "", false, nil
	}
	url, err = typedEndpointURL(cfg.OTLPEndpoint, proto, s)
	return url, err == nil, err
}
