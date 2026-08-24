// Package observability owns the cross-cutting telemetry setup for
// the Orpheus API: OpenTelemetry TracerProvider, exporters, and the
// global TextMapPropagator. The rest of the binary calls
// [Init] once at startup and uses the global helpers
// (otel.Tracer, otel.GetTextMapPropagator) to instrument code.
package observability

import (
	"context"
	"fmt"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/trace"
)

// Init wires the global OTel pipeline. The span exporter is selected by the
// ORPHEUS_OTEL_TRACES_EXPORTER environment variable:
//
//   - "stdout" (the default when unset): a batched SDK provider writing spans
//     to the process stdout as pretty-printed OTLP/JSON. Convenient for local
//     dev — the operator can grep the API process's stdout for spans without
//     standing up a collector.
//   - "none" (also "noop"/"off"/"disabled"): no exporter and NeverSample, so a
//     production deploy does not flood its logs with per-request span JSON.
//
// The TextMapPropagator (W3C TraceContext + Baggage) is installed regardless of
// the exporter: the TraceContext inject/read pair is what the otelhttp server
// middleware uses to continue an incoming trace, and what the outbox publisher
// injects into the JetStream envelope headers so the Python worker can pick it
// up on the other side — trace-context propagation must survive even when local
// span recording is off.
//
// The returned function flushes the batcher and shuts the provider down. The
// caller MUST invoke it at process exit (the signal handler in cmd/api) —
// otherwise spans queued in the batcher are lost on shutdown.
func Init(ctx context.Context) (func(context.Context) error, error) {
	var tp *trace.TracerProvider
	switch exporterKind() {
	case "none", "noop", "off", "disabled":
		// No exporter, never sample: no span output and negligible overhead.
		tp = trace.NewTracerProvider(trace.WithSampler(trace.NeverSample()))
	default: // "stdout"
		exporter, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return nil, fmt.Errorf("observability.init.exporter: %w", err)
		}
		tp = trace.NewTracerProvider(trace.WithBatcher(exporter))
	}
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	return tp.Shutdown, nil
}

// exporterKind returns the configured span exporter name, lowercased and
// trimmed, defaulting to "stdout" when ORPHEUS_OTEL_TRACES_EXPORTER is unset.
func exporterKind() string {
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("ORPHEUS_OTEL_TRACES_EXPORTER"))); v != "" {
		return v
	}
	return "stdout"
}
