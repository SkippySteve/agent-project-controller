package telemetry

import (
	"context"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUnavailableCollectorDoesNotBlockOperations(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unavailable", 503) }))
	defer server.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", server.URL+"/v1/traces")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", server.URL+"/v1/metrics")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "")
	shutdown, err := Setup(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	counter, err := otel.Meter("qualification").Int64Counter("operations")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	for i := 0; i < 10000; i++ {
		ctx, span := otel.Tracer("qualification").Start(context.Background(), "operation")
		counter.Add(ctx, 1)
		span.SetAttributes(attribute.String("outcome", "success"))
		span.End()
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("unavailable collector blocked operation recording")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = shutdown(ctx)
}
