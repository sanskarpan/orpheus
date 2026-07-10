// Package outbox implements the transactional outbox pattern: events
// are written to the `outbox` table inside the same database
// transaction as the business state change that produced them, and a
// background [Publisher] drains the table to NATS.
//
// Why this pattern:
//
//   - The business write and the "publish event" step either both
//     happen or neither does. There is no window where the DB has
//     committed but the message is lost, or where the message is in
//     flight but the DB rolled back.
//   - The publish step is retried until success (or a max-age trip
//     wire). The cost of retries is bounded by the publisher's poll
//     interval, not by client request latency.
//   - Consumers see events in causal order: rows in `outbox` are
//     published in `created_at` order, so a consumer of `job.create`
//     for a given aggregate will always see the `job.update` that
//     followed.
//
// Call sites for [Enqueue] are expected to be inside a
// `db.WithTenant(ctx, orgID, fn)` block: the dbtx package threads the
// `pgx.Tx` through ctx, and Enqueue picks it up via dbtx.FromContext
// so the outbox row commits or rolls back atomically with the
// business write. Callers outside a WithTenant block fall back to
// the supplied pool; that path is here for background workers and
// tests, not for the request hot path.
package outbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orpheus/api/internal/dbtx"
)

// Event is the in-memory representation of an outbox row. Construct
// one with the fields populated, then pass to [Enqueue] inside the
// same database transaction as the business write.
type Event struct {
	// ID is the event's UUID. If empty, [Enqueue] generates a fresh
	// one. Set it explicitly when the producer wants idempotent
	// consumers (the same ID will be used on retry).
	ID string

	// OrgID scopes the event to a tenant. The row is RLS-scoped on
	// insert; the publisher runs as the service role.
	OrgID string

	// AggregateType + AggregateID are the domain noun + primary key
	// the event is about, e.g. ("upload", "<uuid>"). They let
	// consumers filter by aggregate without parsing the payload.
	AggregateType string
	AggregateID   string

	// EventType is the short verb-noun string, e.g. "upload.complete".
	// It is the suffix appended to the NATS subject: "adkil.<event>".
	EventType string

	// Payload is the JSON body. It is marshalled once and stored as
	// jsonb. Pass any value json.Marshal accepts; a struct is the
	// usual choice.
	Payload any

	// Headers is an arbitrary string -> string map stored as jsonb.
	// Consumers can use it for trace ids, idempotency keys, etc. nil
	// is encoded as `{}`.
	Headers map[string]string
}

// Enqueue inserts an event into the outbox table.
//
// database is the fallback Querier used when no `pgx.Tx` is attached
// to ctx — typically the *db.DB pool. When ctx carries a tx
// (i.e. the caller is inside a `db.WithTenant` block), Enqueue runs
// the INSERT on that tx so the outbox row is in the same database
// transaction as the business write. This is the load-bearing
// guarantee of the outbox pattern: either both commit or both roll
// back.
func Enqueue(ctx context.Context, database dbtx.Querier, e Event) error {
	if e.EventType == "" {
		return fmt.Errorf("outbox.enqueue: empty event_type")
	}
	if e.AggregateType == "" || e.AggregateID == "" {
		return fmt.Errorf("outbox.enqueue: aggregate (%q, %q) is incomplete",
			e.AggregateType, e.AggregateID)
	}
	var q dbtx.Querier
	if tx := dbtx.FromContext(ctx); tx != nil {
		q = tx
	} else {
		q = database
		if q == nil {
			return fmt.Errorf("outbox.enqueue: nil db and no tx in ctx")
		}
	}
	if e.ID == "" {
		e.ID = newEventID()
	}

	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return fmt.Errorf("outbox.enqueue.marshal_payload: %w", err)
	}
	headers := e.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	headerJSON, err := json.Marshal(headers)
	if err != nil {
		return fmt.Errorf("outbox.enqueue.marshal_headers: %w", err)
	}

	const sql = `
		INSERT INTO outbox (id, org_id, aggregate_type, aggregate_id, event_type, payload, headers)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`
	if _, err := q.Exec(ctx, sql,
		e.ID, e.OrgID, e.AggregateType, e.AggregateID, e.EventType, payload, headerJSON,
	); err != nil {
		return fmt.Errorf("outbox.enqueue.insert: %w", err)
	}
	return nil
}

// Compile-time assertion that the pool satisfies dbtx.Querier, so
// handlers can keep passing `h.DB` (which embeds *pgxpool.Pool) into
// Enqueue's signature. The embedded pool's methods are promoted onto
// *db.DB at compile time, but a concrete check is cheap insurance
// against a future refactor that drops the embed.
var _ dbtx.Querier = (*pgxpool.Pool)(nil)

// newEventID returns a 16-byte hex string. We use crypto/rand rather
// than uuid.New() because (a) the outbox row's PK is uuid but the
// producer doesn't have to think in uuids, and (b) we already need
// hex for NATS subjects in a few places.
func newEventID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
