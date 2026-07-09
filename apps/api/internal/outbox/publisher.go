package outbox

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/orpheus/api/internal/db"
)

// subjectPrefix is the routing key prefix every outbox-published
// message shares. Consumers subscribe to "adkil.>" to receive every
// event, or to "adkil.<aggregate>.*" to filter by domain.
const subjectPrefix = "adkil."

// Publisher drains the outbox table to NATS. A single Publisher
// instance is intended to run in its own goroutine; the caller's
// responsibility is to call [Run] with a context that is cancelled
// at shutdown.
type Publisher struct {
	DB       *db.DB
	NATS     *nats.Conn
	Logger   *slog.Logger
	Interval time.Duration
	Batch    int

	mu      sync.Mutex
	started bool
}

// New constructs a Publisher with sensible defaults. database / nc /
// logger may be nil for tests; the Run loop will skip its work in
// that case so unit tests can construct and discard a Publisher
// without panicking.
func New(database *db.DB, nc *nats.Conn, logger *slog.Logger) *Publisher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Publisher{
		DB:       database,
		NATS:     nc,
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

// tick claims a batch of unpublished events, publishes each to NATS,
// and marks them published. On a publish or mark error it leaves the
// row unpublished so the next tick will retry — this is the
// "at-least-once" guarantee the outbox pattern is built around.
func (p *Publisher) tick(ctx context.Context) {
	if p.DB == nil || p.NATS == nil {
		return
	}

	rows, err := p.DB.Query(ctx, `
		SELECT id, event_type, payload
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
	defer rows.Close()

	type claimed struct {
		id        string
		eventType string
		payload   []byte
	}
	var batch []claimed
	for rows.Next() {
		var c claimed
		if err := rows.Scan(&c.id, &c.eventType, &c.payload); err != nil {
			p.Logger.Error("outbox.scan_failed", "err", err)
			continue
		}
		batch = append(batch, c)
	}
	if err := rows.Err(); err != nil {
		p.Logger.Error("outbox.rows_iter", "err", err)
		return
	}
	if len(batch) == 0 {
		return
	}

	for _, c := range batch {
		subject := subjectPrefix + c.eventType
		if err := p.NATS.Publish(subject, c.payload); err != nil {
			p.Logger.Error("outbox.publish_failed",
				"err", err,
				"event_id", c.id,
				"subject", subject,
			)
			continue
		}
		if _, err := p.DB.Exec(ctx,
			`UPDATE outbox SET published_at = now() WHERE id = $1`, c.id,
		); err != nil {
			p.Logger.Error("outbox.mark_published_failed",
				"err", err,
				"event_id", c.id,
			)
		}
	}
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
