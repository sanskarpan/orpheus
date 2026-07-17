// Package webhooks hosts the delivery service that POSTs events to
// subscriber URLs.
//
// Two paths can produce a `webhook_deliveries` row: a poll loop and a
// NATS subscription. The poll loop is the source of truth: it claims
// due rows with `FOR UPDATE SKIP LOCKED` and runs them through HMAC
// signing + exponential backoff. The NATS subscription is the fast
// enqueue path — it gets a row in front of the poll loop the next
// tick instead of waiting for the next outbox flush. The two are
// complementary: NATS keeps latency low on the happy path, the poll
// loop keeps the retry state durable across API restarts. Either
// alone would lose at least one of those properties.
package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"

	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/dbtx"
	"github.com/orpheus/api/internal/ssrfguard"
)

// claimVisibilityTimeout is how long a claimed ('delivering') row is
// hidden from other claimers before it can be re-claimed. It bounds the
// re-delivery window if the worker crashes between claim and terminal
// update.
const claimVisibilityTimeout = 60 * time.Second

const (
	defaultPollInterval = time.Second
	defaultBatch        = 32
	defaultMaxAttempts  = 24
	subjectPrefix       = "adkil."

	signatureHeader = "Orpheus-Signature"
	deliveryTimeout = 10 * time.Second
	maxResponseBody = 4 << 10
)

// defaultBackoff is the per-attempt retry interval. Index N (zero-based)
// is used for attempt N+1. After the last entry the schedule stays
// pinned to 24h.
var defaultBackoff = []time.Duration{
	1 * time.Minute,
	2 * time.Minute,
	4 * time.Minute,
	8 * time.Minute,
	16 * time.Minute,
	32 * time.Minute,
	1 * time.Hour,
	2 * time.Hour,
	4 * time.Hour,
	8 * time.Hour,
	16 * time.Hour,
	24 * time.Hour,
}

// DeliveryService is the background worker that signs and POSTs due
// webhook deliveries to their endpoints.
type DeliveryService struct {
	DB         *db.DB
	Logger     *slog.Logger
	NATS       *nats.Conn
	HTTPClient *http.Client

	PollInterval time.Duration
	Batch        int
	MaxAttempts  int
	Backoff      []time.Duration

	mu      sync.Mutex
	started bool
}

// New constructs a DeliveryService with sensible defaults. logger /
// httpClient may be nil for tests; the loop will fall back to
// slog.Default() and a 10s-timeout client.
func New(database *db.DB, logger *slog.Logger, nc *nats.Conn, httpClient *http.Client) *DeliveryService {
	if logger == nil {
		logger = slog.Default()
	}
	if httpClient == nil {
		// SSRF-safe client: the dialer refuses to connect to private /
		// link-local / metadata IPs and re-validates every redirect hop,
		// closing the DNS-rebind window that registration-time checks
		// alone cannot.
		httpClient = ssrfguard.SafeHTTPClient(deliveryTimeout)
	}
	return &DeliveryService{
		DB:           database,
		Logger:       logger,
		NATS:         nc,
		HTTPClient:   httpClient,
		PollInterval: defaultPollInterval,
		Batch:        defaultBatch,
		MaxAttempts:  defaultMaxAttempts,
		Backoff:      defaultBackoff,
	}
}

// Run blocks until ctx is cancelled, ticking every PollInterval and
// draining due deliveries on each tick.
func (s *DeliveryService) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = true
	s.mu.Unlock()

	if s.NATS != nil {
		go s.subscribeNATS(ctx)
	}

	interval := s.PollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.Logger.Info("webhooks.delivery.started",
		"interval", interval.String(),
		"batch", s.Batch,
		"nats", s.NATS != nil,
	)
	for {
		select {
		case <-ctx.Done():
			s.Logger.Info("webhooks.delivery.stopped")
			return nil
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

type claimed struct {
	ID           string
	OrgID        string
	EndpointID   string
	EventType    string
	EventID      string
	Payload      []byte
	AttemptCount int
}

func (s *DeliveryService) tick(ctx context.Context) {
	if s.DB == nil {
		return
	}
	batch, err := s.claim(ctx)
	if err != nil {
		s.Logger.Error("webhooks.delivery.claim_failed", "err", err)
		return
	}
	for _, d := range batch {
		s.deliverOne(ctx, d)
	}
}

// claim atomically grabs a batch of due deliveries. It runs in a single
// service-role transaction (webhook_deliveries has FORCE row-level
// security, so a bare-pool query sees zero rows) and marks the claimed
// rows 'delivering', bumps attempt_count, and pushes next_retry_at out by
// the visibility timeout — all in one UPDATE ... RETURNING. Holding the
// FOR UPDATE SKIP LOCKED rows inside the same tx that flips their status
// is what prevents a second delivery worker from double-sending: the
// commit both releases the locks and leaves the rows hidden (status
// 'delivering', next_retry_at in the future) until they are done or the
// visibility timeout lapses. The returned attempt_count is the new,
// authoritative value.
func (s *DeliveryService) claim(ctx context.Context) ([]claimed, error) {
	var batch []claimed
	err := s.withServiceTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE webhook_deliveries
			SET status = 'delivering',
			    attempt_count = attempt_count + 1,
			    next_retry_at = now() + make_interval(secs => $2)
			WHERE id IN (
				SELECT id FROM webhook_deliveries
				WHERE status IN ('pending','delivering') AND next_retry_at <= now()
				ORDER BY next_retry_at ASC
				LIMIT $1
				FOR UPDATE SKIP LOCKED
			)
			RETURNING id, org_id::text, endpoint_id::text, event_type, event_id::text, payload, attempt_count
		`, s.Batch, claimVisibilityTimeout.Seconds())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c claimed
			if err := rows.Scan(&c.ID, &c.OrgID, &c.EndpointID, &c.EventType, &c.EventID, &c.Payload, &c.AttemptCount); err != nil {
				return err
			}
			batch = append(batch, c)
		}
		return rows.Err()
	})
	return batch, err
}

// withServiceTx runs fn inside a transaction with app.is_service set
// transaction-locally. The delivery worker is a system component with no
// request principal, so it must present as the service role to read and
// write the FORCE-RLS webhook / audit tables across every org.
func (s *DeliveryService) withServiceTx(ctx context.Context, fn func(pgx.Tx) error) error {
	conn, err := s.DB.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.is_service','true',true)"); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

type endpointInfo struct {
	URL    string
	Secret string
}

func (s *DeliveryService) loadEndpoint(ctx context.Context, orgID, endpointID string) (endpointInfo, error) {
	var info endpointInfo
	err := s.withServiceTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT url, secret FROM webhook_endpoints
			WHERE id = $1 AND org_id = $2 AND active = true
		`, endpointID, orgID).Scan(&info.URL, &info.Secret)
	})
	return info, err
}

// deliverOne performs one delivery attempt. The row was already marked
// 'delivering' with attempt_count incremented at claim time, so
// d.AttemptCount is the authoritative attempt number. All state writes
// run through service-role transactions (FORCE-RLS tables).
func (s *DeliveryService) deliverOne(ctx context.Context, d claimed) {
	attempt := d.AttemptCount

	info, err := s.loadEndpoint(ctx, d.OrgID, d.EndpointID)
	if err != nil {
		// Endpoint is missing, deleted, or deactivated. Terminal.
		_ = s.finishDelivery(ctx, d, "failed", 0, "endpoint unavailable: "+err.Error(), time.Time{})
		_ = s.recordAudit(ctx, d.OrgID, d.EndpointID, "webhook.delivery_fail", map[string]any{
			"delivery_id": d.ID, "event_type": d.EventType, "reason": "endpoint_unavailable",
		})
		return
	}

	start := time.Now()
	statusCode, respBody, sigBase, postErr := s.post(ctx, info, d)
	durMs := int(time.Since(start).Milliseconds())

	// Record the per-attempt timeline + the signature base string and
	// response snippet for the debug view (PRD 03).
	s.recordAttempt(ctx, d, attempt, statusCode, durMs, failureReason(statusCode, postErr), sigBase, respBody)

	if postErr == nil && statusCode >= 200 && statusCode < 300 {
		if err := s.withServiceTx(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE webhook_deliveries
				SET status = 'delivered', response_status = $1, response_body = $2, delivered_at = now()
				WHERE id = $3
			`, statusCode, truncateBody(respBody), d.ID)
			return err
		}); err != nil {
			s.Logger.Error("webhooks.delivery.mark_delivered_failed", "err", err, "delivery_id", d.ID)
		}
		s.recordEndpointOutcome(ctx, d.EndpointID, true)
		_ = s.recordAudit(ctx, d.OrgID, d.EndpointID, "webhook.deliver", map[string]any{
			"delivery_id": d.ID, "event_type": d.EventType, "status_code": statusCode, "attempt": attempt,
		})
		return
	}

	reason := failureReason(statusCode, postErr)
	switch {
	case attempt >= s.MaxAttempts:
		_ = s.finishDelivery(ctx, d, "exhausted", statusCode, reason, time.Time{})
		s.recordEndpointOutcome(ctx, d.EndpointID, false)
		_ = s.recordAudit(ctx, d.OrgID, d.EndpointID, "webhook.delivery_exhausted", map[string]any{
			"delivery_id": d.ID, "event_type": d.EventType, "attempts": attempt,
		})
	case shouldRetryStatus(statusCode):
		nextAt := time.Now().Add(jitter(computeNextRetry(attempt, s.Backoff)))
		_ = s.finishDelivery(ctx, d, "pending", statusCode, reason, nextAt)
		_ = s.recordAudit(ctx, d.OrgID, d.EndpointID, "webhook.delivery_fail", map[string]any{
			"delivery_id": d.ID, "event_type": d.EventType, "status_code": statusCode, "attempt": attempt, "reason": reason,
		})
	default:
		_ = s.finishDelivery(ctx, d, "failed", statusCode, reason, time.Time{})
		s.recordEndpointOutcome(ctx, d.EndpointID, false)
		_ = s.recordAudit(ctx, d.OrgID, d.EndpointID, "webhook.delivery_fail", map[string]any{
			"delivery_id": d.ID, "event_type": d.EventType, "status_code": statusCode, "attempt": attempt, "reason": reason,
		})
	}
}

// autoDisableThreshold is how many consecutive terminal-failure deliveries
// deactivate an endpoint (PRD 03). Reset to 0 on any success.
const autoDisableThreshold = 100

// recordAttempt appends the per-attempt row and stashes the signature base
// string + a response snippet on the delivery for the debug view.
func (s *DeliveryService) recordAttempt(ctx context.Context, d claimed, attempt, statusCode, durMs int, errStr, sigBase string, respBody []byte) {
	if err := s.withServiceTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO webhook_delivery_attempts (id, delivery_id, org_id, attempt_no, attempted_at, status_code, duration_ms, error)
			VALUES (gen_random_uuid(), $1, $2, $3, now(), NULLIF($4,0), $5, NULLIF($6,''))
		`, d.ID, d.OrgID, attempt, statusCode, durMs, errStr); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE webhook_deliveries SET signature_base_string = $1, response_body_snippet = $2 WHERE id = $3
		`, sigBase, truncateBody(respBody), d.ID)
		return err
	}); err != nil {
		s.Logger.Error("webhooks.delivery.record_attempt_failed", "err", err, "delivery_id", d.ID)
	}
}

// recordEndpointOutcome resets the failure counter on success, or increments
// it and auto-disables the endpoint once it crosses the threshold. A manually
// disabled endpoint is never silently re-enabled.
func (s *DeliveryService) recordEndpointOutcome(ctx context.Context, endpointID string, success bool) {
	if err := s.withServiceTx(ctx, func(tx pgx.Tx) error {
		if success {
			_, err := tx.Exec(ctx, `UPDATE webhook_endpoints SET consecutive_failures = 0 WHERE id = $1`, endpointID)
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE webhook_endpoints
			SET consecutive_failures = consecutive_failures + 1,
			    active = CASE WHEN consecutive_failures + 1 >= $2 THEN false ELSE active END
			WHERE id = $1
		`, endpointID, autoDisableThreshold)
		return err
	}); err != nil {
		s.Logger.Error("webhooks.delivery.record_endpoint_outcome_failed", "err", err, "endpoint_id", endpointID)
	}
}

// finishDelivery writes the terminal (or retry-scheduled) state plus the
// failure detail in one service-role transaction. For status 'pending'
// nextRetry sets the next attempt time; for terminal states it is zero
// and left untouched.
func (s *DeliveryService) finishDelivery(ctx context.Context, d claimed, status string, statusCode int, reason string, nextRetry time.Time) error {
	return s.withServiceTx(ctx, func(tx pgx.Tx) error {
		if status == "pending" {
			_, err := tx.Exec(ctx, `
				UPDATE webhook_deliveries
				SET status = 'pending', next_retry_at = $1,
				    response_status = NULLIF($2, 0), response_body = $3
				WHERE id = $4
			`, nextRetry, statusCode, reason, d.ID)
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE webhook_deliveries
			SET status = $1::webhook_status,
			    response_status = NULLIF($2, 0), response_body = $3
			WHERE id = $4
		`, status, statusCode, reason, d.ID)
		return err
	})
}

// recordAudit inserts an audit_log row tagged as a system actor. The
// DeliveryService runs in the background with no request principal, so
// deliveries are attributed to the system actor type. audit_log is
// FORCE-RLS, hence the service-role transaction.
func (s *DeliveryService) recordAudit(ctx context.Context, orgID, resourceID, action string, metadata map[string]any) error {
	if s.DB == nil {
		return nil
	}
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	return s.withServiceTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO audit_log (id, org_id, user_id, actor_type, action, resource_type, resource_id, metadata, created_at)
			VALUES ($1, $2::uuid, NULL, 'system'::actor_type, $3::audit_action, 'webhook', $4, $5::jsonb, now())
		`, uuid.NewString(), orgID, action, resourceID, metaJSON)
		return err
	})
}

func (s *DeliveryService) post(ctx context.Context, info endpointInfo, d claimed) (int, []byte, string, error) {
	ts := time.Now().Unix()
	sig := signPayload(info.Secret, ts, d.Payload)
	header := fmt.Sprintf("t=%d,v1=%s", ts, sig)
	// The signature base string a receiver reconstructs to verify: "<ts>.<body>".
	sigBase := fmt.Sprintf("%d.%s", ts, d.Payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, info.URL, bytes.NewReader(d.Payload))
	if err != nil {
		return 0, nil, sigBase, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(signatureHeader, header)
	req.Header.Set("User-Agent", "Orpheus-Webhooks/1.0")

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return 0, nil, sigBase, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	return resp.StatusCode, body, sigBase, nil
}

// Enqueue inserts one pending webhook_deliveries row per active
// endpoint subscribed to eventType for orgID. A no-op when no
// endpoints match. The DB filter uses text[] containment so
// `subscribed_events = ['*']` matches every event type.
func (s *DeliveryService) Enqueue(ctx context.Context, orgID, eventType, eventID string, payload any) error {
	if s.DB == nil {
		return errors.New("webhooks: DB is nil")
	}
	if eventID == "" {
		eventID = uuid.NewString()
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("webhooks: marshal payload: %w", err)
	}

	return s.DB.WithTenant(ctx, orgID, func(ctx context.Context) error {
		// Use the dbtx helpers (not s.DB.Query/Exec): inside WithTenant
		// the org GUC lives on the transaction connection, and a bare
		// pool call would acquire a *different* connection with no tenant
		// context — RLS would then hide every endpoint and reject every
		// insert, so no delivery rows would ever be created.
		rows, err := dbtx.Query(ctx, s.DB, `
			SELECT id::text FROM webhook_endpoints
			WHERE org_id = $1 AND active = true
			  AND ($2 = ANY(subscribed_events) OR '*' = ANY(subscribed_events))
		`, orgID, eventType)
		if err != nil {
			return err
		}
		defer rows.Close()

		var endpointIDs []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			endpointIDs = append(endpointIDs, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		for _, id := range endpointIDs {
			if _, err := dbtx.Exec(ctx, s.DB, `
				INSERT INTO webhook_deliveries
				  (id, org_id, endpoint_id, event_type, event_id, payload, status, next_retry_at, attempt_count, max_attempts, created_at)
				VALUES
				  ($1, $2, $3, $4, $5, $6::jsonb, 'pending', now(), 0, $7, now())
			`, uuid.NewString(), orgID, id, eventType, eventID, payloadBytes, s.MaxAttempts); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *DeliveryService) subscribeNATS(ctx context.Context) {
	if s.NATS == nil {
		return
	}
	sub, err := s.NATS.Subscribe(subjectPrefix+">", func(msg *nats.Msg) {
		eventType := msg.Subject
		if len(eventType) <= len(subjectPrefix) {
			return
		}
		eventType = eventType[len(subjectPrefix):]
		orgID := msg.Header.Get("X-Org-Id")
		if orgID == "" {
			return
		}
		eventID := msg.Header.Get("X-Event-Id")
		if eventID == "" {
			eventID = uuid.NewString()
		}
		if err := s.Enqueue(ctx, orgID, eventType, eventID, json.RawMessage(msg.Data)); err != nil {
			s.Logger.Error("webhooks.delivery.nats_enqueue_failed", "err", err, "event_type", eventType)
		}
	})
	if err != nil {
		s.Logger.Error("webhooks.delivery.nats_subscribe_failed", "err", err)
		return
	}
	<-ctx.Done()
	_ = sub.Unsubscribe()
}

// signPayload produces the HMAC-SHA256 hex digest of "<ts>.<body>".
// The signed string format matches Stripe's convention so the same
// verification pattern works on the receiver side.
func signPayload(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%d.", ts)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// computeNextRetry maps an attempt count to a duration from base,
// capped at the last entry. attempt is 1-indexed (1 = first attempt).
func computeNextRetry(attempt int, base []time.Duration) time.Duration {
	if len(base) == 0 {
		return 0
	}
	idx := attempt - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(base) {
		idx = len(base) - 1
	}
	return base[idx]
}

// jitter applies ±10% multiplicative noise to a duration. Negative or
// zero values are returned unchanged.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	span := int64(d) / 10
	if span == 0 {
		return d
	}
	return d + time.Duration(rand.Int63n(2*span)-span)
}

// shouldRetryStatus mirrors the receiver-error / transport-error split
// from the OpenAPI spec. 2xx → success (handled before this).
// 408/429/5xx → transient, retry with backoff. Other 4xx → permanent.
func shouldRetryStatus(code int) bool {
	if code == http.StatusRequestTimeout || code == http.StatusTooManyRequests {
		return true
	}
	return code >= 500 && code <= 599
}

func failureReason(code int, err error) string {
	if err != nil {
		return "transport: " + err.Error()
	}
	return fmt.Sprintf("http %d", code)
}

func truncateBody(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		return b[:i]
	}
	if len(b) > 200 {
		return b[:200]
	}
	return b
}
