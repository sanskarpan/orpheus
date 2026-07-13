// Package observability owns the cross-cutting telemetry setup for
// the Orpheus API: OpenTelemetry TracerProvider, exporters, and the
// global TextMapPropagator. The rest of the binary calls
// [Init] once at startup and uses the global helpers
// (otel.Tracer, otel.GetTextMapPropagator) to instrument code.
package observability

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/trace"
)

// Init wires the global OTel pipeline:
//
//   - TracerProvider: a batched SDK provider writing to a stdout
//     exporter in OTLP/JSON form. The stdout target is the right
//     default for Phase 3.2 — the operator can grep the API
//     process's stdout for spans without standing up a collector.
//     A follow-up swaps the exporter behind an env var so prod can
//     ship to an OTLP/HTTP endpoint without a code change.
//   - TextMapPropagator: W3C TraceContext + Baggage. The
//     TraceContext inject/read pair is what the otelhttp server
//     middleware uses to continue an incoming trace, and what the
//     outbox publisher injects into the JetStream envelope headers
//     so the Python worker can pick it up on the other side.
//
// The returned function flushes the batcher and shuts the provider
// down. The caller MUST invoke it at process exit (the signal
// handler in cmd/api) — otherwise spans queued in the batcher are
// lost on shutdown.
func Init(ctx context.Context) (func(context.Context) error, error) {
	exporter, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
	if err != nil {
		return nil, fmt.Errorf("observability.init.exporter: %w", err)
	}
	tp := trace.NewTracerProvider(trace.WithBatcher(exporter))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	return tp.Shutdown, nil
}
