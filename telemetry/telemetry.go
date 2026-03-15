package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Config holds configuration for the OpenTelemetry SDK initialization.
type Config struct {
	// ServiceName is the service.name resource attribute.
	ServiceName string

	// ServiceVersion is the service.version resource attribute.
	ServiceVersion string

	// SpanExporter overrides the default OTLP gRPC trace exporter.
	// Used in tests to inject an in-memory exporter.
	SpanExporter sdktrace.SpanExporter

	// MetricReader overrides the default OTLP gRPC metric reader.
	// Used in tests to inject a manual reader.
	MetricReader sdkmetric.Reader

	// ResourceAttributes provides additional resource attributes beyond
	// service.name and service.version (e.g. cloud.provider, cloud.region,
	// deployment.environment.name). The caller is responsible for setting
	// any deployment-specific attributes.
	ResourceAttributes []attribute.KeyValue
}

// Initialize sets up the OpenTelemetry TracerProvider, MeterProvider, and
// LoggerProvider with OTLP gRPC exporters and W3C TraceContext propagation.
//
// Configuration is primarily driven by standard OTEL_* environment variables
// (OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_EXPORTER_OTLP_HEADERS, etc.).
//
// Returns a shutdown function that must be called to flush pending telemetry.
func Initialize(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	attrs := []attribute.KeyValue{
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
	}
	attrs = append(attrs, cfg.ResourceAttributes...)

	res := resource.NewWithAttributes("", attrs...)

	// Traces
	spanExporter := cfg.SpanExporter
	if spanExporter == nil {
		var err error
		spanExporter, err = otlptracegrpc.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to create OTLP trace exporter: %w", err)
		}
	}

	tpOptions := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
	}
	// Use synchronous export when a custom exporter is provided (tests),
	// otherwise batch for production use.
	if cfg.SpanExporter != nil {
		tpOptions = append(tpOptions, sdktrace.WithSyncer(spanExporter))
	} else {
		tpOptions = append(tpOptions, sdktrace.WithBatcher(spanExporter))
	}

	tp := sdktrace.NewTracerProvider(tpOptions...)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// Metrics
	metricReader := cfg.MetricReader
	if metricReader == nil {
		metricExporter, mErr := otlpmetricgrpc.New(ctx,
			otlpmetricgrpc.WithTemporalitySelector(sdkmetric.DefaultTemporalitySelector),
		)
		if mErr != nil {
			return nil, fmt.Errorf("failed to create OTLP metric exporter: %w", mErr)
		}
		metricReader = sdkmetric.NewPeriodicReader(metricExporter)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(metricReader),
	)

	otel.SetMeterProvider(mp)

	// Logs
	logExporter, err := otlploggrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP log exporter: %w", err)
	}

	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
	)

	global.SetLoggerProvider(lp)

	shutdown := func(ctx context.Context) error {
		// Flush logs before traces so log records referencing spans are exported first.
		if lErr := lp.Shutdown(ctx); lErr != nil {
			return fmt.Errorf("failed to shutdown LoggerProvider: %w", lErr)
		}
		if mErr := mp.Shutdown(ctx); mErr != nil {
			return fmt.Errorf("failed to shutdown MeterProvider: %w", mErr)
		}
		return tp.Shutdown(ctx)
	}

	return shutdown, nil
}
