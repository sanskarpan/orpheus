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
package outbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/orpheus/api/internal/db"
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
// The caller is responsible for invoking this inside the same
// transaction as the business write — typically via
// `db.WithTenant(ctx, orgID, fn)` and a follow-up outbox insert in
// `fn`. The function does not accept a tx handle directly; in Phase 1
// it relies on a single connection per WithTenant call (see
// internal/db/db.go). Phase 2 will thread a pgx.Tx through ctx.
func Enqueue(ctx context.Context, database *db.DB, e Event) error {
	// Validate inputs first so the caller gets a specific error
	// before we touch the database. The nil-DB check is last; a
	// missing DB is a wiring bug, not a data problem.
	if e.EventType == "" {
		return fmt.Errorf("outbox.enqueue: empty event_type")
	}
	if e.AggregateType == "" || e.AggregateID == "" {
		return fmt.Errorf("outbox.enqueue: aggregate (%q, %q) is incomplete",
			e.AggregateType, e.AggregateID)
	}
	if database == nil {
		return fmt.Errorf("outbox.enqueue: nil db")
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

	const q = `
		INSERT INTO outbox (id, org_id, aggregate_type, aggregate_id, event_type, payload, headers)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`
	if _, err := database.Exec(ctx, q,
		e.ID, e.OrgID, e.AggregateType, e.AggregateID, e.EventType, payload, headerJSON,
	); err != nil {
		return fmt.Errorf("outbox.enqueue.insert: %w", err)
	}
	return nil
}

// newEventID returns a 16-byte hex string. We use crypto/rand rather
// than uuid.New() because (a) the outbox row's PK is uuid but the
// producer doesn't have to think in uuids, and (b) we already need
// hex for NATS subjects in a few places.
func newEventID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
