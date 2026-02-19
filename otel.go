package main

import (
	"context"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

// tracer is the package-level tracer for cogos-kernel.
// When OTEL is not configured, this uses the global noop tracer,
// so callers can always call tracer.Start() without nil checks.
var tracer = otel.Tracer("cogos-kernel")

// initTracer initializes an OpenTelemetry TracerProvider with an OTLP gRPC exporter.
// If OTEL_EXPORTER_OTLP_ENDPOINT is not set, it returns (nil, nil) and the global
// tracer remains the default noop — zero overhead, no errors.
func initTracer() (*sdktrace.TracerProvider, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("cogos-kernel"),
			semconv.ServiceVersion(Version),
		),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tp)

	// Enable W3C Trace Context propagation so upstream services
	// (e.g. OpenClaw) can be trace parents.
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// Re-bind the package-level tracer so it uses the real provider.
	tracer = tp.Tracer("cogos-kernel")

	return tp, nil
}

// otelMiddleware wraps an http.HandlerFunc with an OTEL tracing span.
// It extracts W3C Trace Context from incoming headers (enabling distributed
// tracing with upstream callers), creates a span for the request, and
// attaches basic HTTP attributes. Reusable for any endpoint.
func otelMiddleware(name string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Extract propagated trace context from upstream (e.g. OpenClaw).
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))

		ctx, span := tracer.Start(ctx, name,
			trace.WithAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.url", r.URL.Path),
			),
		)
		defer span.End()

		// Pass the span context down so child spans can be created.
		next.ServeHTTP(w, r.WithContext(ctx))
	}
}

// shutdownTracer flushes and shuts down the TracerProvider with a 5-second timeout.
func shutdownTracer(tp *sdktrace.TracerProvider) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tp.Shutdown(ctx)
}
