package outbox

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/jobs"
	"github.com/orpheus/api/internal/metrics"
)

// tracer is the per-package handle to the global OTel tracer. Resolved
// at package init against whatever the global TracerProvider is at
// that moment (the observability.Init call in cmd/api is the prod
// wiring); the SDK is safe to call Tracer() before SetTracerProvider.
var tracer = otel.Tracer("orpheus-outbox")

// subjectPrefix is the routing key prefix every outbox-published
// message shares. Consumers subscribe to "adkil.>" to receive every
// event, or to "adkil.<aggregate>.*" to filter by domain.
const subjectPrefix = "adkil."

// Publisher drains the outbox table to NATS JetStream. A single
// Publisher instance is intended to run in its own goroutine; the
// caller's responsibility is to call [Run] with a context that is
// cancelled at shutdown.
type Publisher struct {
	DB      *db.DB
	JS      jobs.Publisher
	Metrics *metrics.Metrics
	Logger  *slog.Logger

	Interval time.Duration
	Batch    int

	mu      sync.Mutex
	started bool
}

// New constructs a Publisher with sensible defaults. database / js /
// logger may be nil for tests; the Run loop will skip its work in
// that case so unit tests can construct and discard a Publisher
// without panicking. metrics may be nil — tick records metrics
// defensively so a bare unit test can pass nil.
func New(database *db.DB, js jetstream.JetStream, m *metrics.Metrics, logger *slog.Logger) *Publisher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Publisher{
		DB:       database,
		JS:       js,
		Metrics:  m,
		Logger:   logger,
		Interval: time.Second,
		Batch:    100,
	}
}

// Run starts the publish loop and blocks until ctx is cancelled. It
// returns nil on a clean shutdown and an error only on misconfig.
//
// The loop is intentionally simple: a single ticker drains whatever
// the previous tick did not finish. For high-throughput deployments,
// run multiple Publisher instances in different processes — the
// `FOR UPDATE SKIP LOCKED` query in [tick] keeps them from
// double-publishing.
func (p *Publisher) Run(ctx context.Context) error {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return nil
	}
	p.started = true
	p.mu.Unlock()

	interval := p.Interval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	p.Logger.Info("outbox.publisher.started",
		"interval", interval.String(),
		"batch", p.Batch,
	)
	for {
		select {
		case <-ctx.Done():
			p.Logger.Info("outbox.publisher.stopped")
			return nil
		case <-ticker.C:
			p.tick(ctx)
		}
	}
}

// tick claims a batch of unpublished events, publishes each to the
// ORPHEUS_JOBS stream, and marks them published. On a publish or
// mark error it leaves the row unpublished so the next tick will
// retry — this is the "at-least-once" guarantee the outbox pattern
// is built around.
func (p *Publisher) tick(ctx context.Context) {
	if p.DB == nil || p.JS == nil {
		return
	}

	// The claim, publish, and mark-published all run inside ONE
	// transaction. This is load-bearing for two reasons:
	//
	//   1. FOR UPDATE SKIP LOCKED only prevents a concurrent publisher
	//      from grabbing the same rows while the locks are HELD — i.e.
	//      until this transaction commits. Claiming on the bare pool
	//      (implicit auto-commit) released the locks immediately, so two
	//      publishers could double-publish the same event.
	//   2. outbox has FORCE row-level security; a plain connection with
	//      no tenant/service context sees ZERO rows. We set the service
	//      GUC transaction-locally so the drain can read every org's
	//      rows (and reset is implicit at commit/rollback).
	tx, err := p.DB.Begin(ctx)
	if err != nil {
		p.Logger.Error("outbox.begin_failed", "err", err)
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.is_service', 'true', true)"); err != nil {
		p.Logger.Error("outbox.set_service_failed", "err", err)
		return
	}

	rows, err := tx.Query(ctx, `
		SELECT id, event_type, org_id, aggregate_id, payload, headers
		FROM outbox
		WHERE published_at IS NULL
		ORDER BY created_at ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, p.Batch)
	if err != nil {
		p.Logger.Error("outbox.claim_failed", "err", err)
		return
	}

	var batch []claimed
	for rows.Next() {
		var c claimed
		if err := rows.Scan(&c.id, &c.eventType, &c.orgID, &c.aggregateID, &c.payload, &c.headers); err != nil {
			p.Logger.Error("outbox.scan_failed", "err", err)
			continue
		}
		batch = append(batch, c)
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		p.Logger.Error("outbox.rows_iter", "err", rowsErr)
		return
	}
	if len(batch) == 0 {
		// Commit to release the (empty) transaction promptly.
		if err := tx.Commit(ctx); err == nil {
			committed = true
		}
		return
	}

	for _, c := range batch {
		func() {
			ctx, span := tracer.Start(ctx, "outbox.publish",
				trace.WithAttributes(
					attribute.String("outbox.event_id", c.id),
					attribute.String("outbox.event_type", c.eventType),
				),
			)
			defer span.End()

			carrier := propagation.MapCarrier{}
			otel.GetTextMapPropagator().Inject(ctx, carrier)

			env := envelope{
				EventID:     c.id,
				EventType:   c.eventType,
				OrgID:       c.orgID,
				AggregateID: c.aggregateID,
				Payload:     c.payload,
				Headers:     carrier,
			}
			envBytes, err := json.Marshal(env)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "marshal envelope")
				span.SetAttributes(attribute.String("outbox.result", "error"))
				p.Logger.Error("outbox.envelope_failed",
					"err", err,
					"event_id", c.id,
					"event_type", c.eventType,
				)
				return
			}

			start := time.Now()
			pubErr := jobs.Publish(ctx, p.JS, c.eventType, envBytes, nil)
			latency := time.Since(start).Seconds()
			result := "success"
			if pubErr != nil {
				result = "error"
				span.RecordError(pubErr)
				span.SetStatus(codes.Error, "publish failed")
				p.Logger.Error("outbox.publish_failed",
					"err", pubErr,
					"event_id", c.id,
					"event_type", c.eventType,
				)
			}
			span.SetAttributes(attribute.String("outbox.result", result))
			if p.Metrics != nil {
				p.Metrics.OutboxPublished.WithLabelValues(c.eventType, result).Inc()
				p.Metrics.OutboxPublishLatency.WithLabelValues(c.eventType).Observe(latency)
			}
			if pubErr != nil {
				return
			}
			if _, err := tx.Exec(ctx,
				`UPDATE outbox SET published_at = now() WHERE id = $1`, c.id,
			); err != nil {
				p.Logger.Error("outbox.mark_published_failed",
					"err", err,
					"event_id", c.id,
				)
			}
		}()
	}

	// Commit persists every mark-published done above and releases the
	// row locks. A commit failure rolls the whole batch back (via the
	// deferred Rollback): those events stay unpublished and are retried
	// next tick — at-least-once, never lost.
	if err := tx.Commit(ctx); err != nil {
		p.Logger.Error("outbox.commit_failed", "err", err)
		return
	}
	committed = true
}

// envelope is the JSON body the outbox publisher puts on the wire.
// It matches the contract the Python worker
// (apps/workers/orpheus_workers/worker.py) expects: the worker reads
// event_type at the top level and the inner job record under
// "payload". The raw outbox row is just the columns; this struct is
// the published shape.
type envelope struct {
	EventID     string            `json:"event_id"`
	EventType   string            `json:"event_type"`
	OrgID       string            `json:"org_id"`
	AggregateID string            `json:"aggregate_id"`
	Payload     json.RawMessage   `json:"payload"`
	Headers     map[string]string `json:"headers"`
}

// claimed is one outbox row held in memory between the claim query
// and the publish call. payload and headers are kept as raw bytes
// because they round-trip through jsonb unchanged: re-marshalling
// them would risk lossy ordering, escaping, or float precision.
type claimed struct {
	id          string
	eventType   string
	orgID       string
	aggregateID string
	payload     []byte
	headers     []byte
}

// buildEnvelope wraps a claimed outbox row into the published JSON
// body. The headers column is jsonb (Enqueue stores `{}` when nil);
// an empty/empty-object value decodes to a non-nil empty map so the
// wire field is always present.
func buildEnvelope(c claimed) ([]byte, error) {
	var headers map[string]string
	if len(c.headers) > 0 {
		if err := json.Unmarshal(c.headers, &headers); err != nil {
			return nil, err
		}
	}
	if headers == nil {
		headers = map[string]string{}
	}
	env := envelope{
		EventID:     c.id,
		EventType:   c.eventType,
		OrgID:       c.orgID,
		AggregateID: c.aggregateID,
		Payload:     c.payload,
		Headers:     headers,
	}
	return json.Marshal(env)
}

// Flush is an optional helper: it synchronously drains the outbox
// once. Useful in tests; production code should rely on Run.
func (p *Publisher) Flush(ctx context.Context) {
	p.tick(ctx)
}

// Subject returns the NATS subject a given event_type maps to. It's
// exported so the API's own subscribers can compute the right listen
// pattern without copying the prefix constant.
func Subject(eventType string) string {
	return subjectPrefix + eventType
}
