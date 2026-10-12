package telemetry

import (
	"context"
	"errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"time"
)

func Setup(ctx context.Context, enabled bool) (func(context.Context) error, error) {
	if !enabled {
		return func(context.Context) error { return nil }, nil
	}
	res := resource.NewWithAttributes("", attribute.String("service.name", "agent-project-controller"))
	traces, e := otlptracehttp.New(ctx, otlptracehttp.WithTimeout(2*time.Second))
	if e != nil {
		return nil, e
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(.1))), sdktrace.WithBatcher(traces, sdktrace.WithMaxQueueSize(256), sdktrace.WithExportTimeout(2*time.Second), sdktrace.WithBatchTimeout(2*time.Second)))
	metrics, e := otlpmetrichttp.New(ctx, otlpmetrichttp.WithTimeout(2*time.Second))
	if e != nil {
		_ = tp.Shutdown(ctx)
		return nil, e
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metrics, sdkmetric.WithInterval(30*time.Second), sdkmetric.WithTimeout(2*time.Second))))
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	return func(ctx context.Context) error { return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx)) }, nil
}
