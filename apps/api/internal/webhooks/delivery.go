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
	"github.com/nats-io/nats.go"

	"github.com/orpheus/api/internal/db"
)

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
		httpClient = &http.Client{Timeout: deliveryTimeout}
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
	rows, err := s.DB.Query(ctx, `
		SELECT id, org_id::text, endpoint_id::text, event_type, event_id::text, payload, attempt_count
		FROM webhook_deliveries
		WHERE status IN ('pending','delivering') AND next_retry_at <= now()
		ORDER BY next_retry_at ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, s.Batch)
	if err != nil {
		s.Logger.Error("webhooks.delivery.claim_failed", "err", err)
		return
	}
	defer rows.Close()

	var batch []claimed
	for rows.Next() {
		var c claimed
		if err := rows.Scan(&c.ID, &c.OrgID, &c.EndpointID, &c.EventType, &c.EventID, &c.Payload, &c.AttemptCount); err != nil {
			s.Logger.Error("webhooks.delivery.scan_failed", "err", err)
			continue
		}
		batch = append(batch, c)
	}
	if err := rows.Err(); err != nil {
		s.Logger.Error("webhooks.delivery.rows_iter", "err", err)
		return
	}
	if len(batch) == 0 {
		return
	}

	for _, d := range batch {
		s.deliverOne(ctx, d)
	}
}

type endpointInfo struct {
	URL    string
	Secret string
}

func (s *DeliveryService) loadEndpoint(ctx context.Context, orgID, endpointID string) (endpointInfo, error) {
	var info endpointInfo
	err := s.DB.QueryRow(ctx, `
		SELECT url, secret FROM webhook_endpoints
		WHERE id = $1 AND org_id = $2 AND active = true
	`, endpointID, orgID).Scan(&info.URL, &info.Secret)
	return info, err
}

func (s *DeliveryService) deliverOne(ctx context.Context, d claimed) {
	if _, err := s.DB.Exec(ctx, `
		UPDATE webhook_deliveries
		SET status = 'delivering', attempt_count = attempt_count + 1
		WHERE id = $1
	`, d.ID); err != nil {
		s.Logger.Error("webhooks.delivery.mark_delivering_failed",
			"err", err, "delivery_id", d.ID,
		)
		return
	}

	newAttempt := d.AttemptCount + 1

	info, err := s.loadEndpoint(ctx, d.OrgID, d.EndpointID)
	if err != nil {
		// Endpoint is missing, deleted, or deactivated. Treat as a
		// terminal failure: no point retrying.
		s.markFailed(ctx, d, newAttempt, 0, "endpoint unavailable: "+err.Error())
		_ = recordAudit(ctx, s.DB, d.OrgID, d.EndpointID, "webhook.delivery_fail", map[string]any{
			"delivery_id": d.ID,
			"event_type":  d.EventType,
			"reason":      "endpoint_unavailable",
		})
		return
	}

	statusCode, respBody, postErr := s.post(ctx, info, d)

	success := postErr == nil && statusCode >= 200 && statusCode < 300
	if success {
		_, err := s.DB.Exec(ctx, `
			UPDATE webhook_deliveries
			SET status = 'delivered',
			    response_status = $1,
			    response_body = $2,
			    delivered_at = now()
			WHERE id = $3
		`, statusCode, truncateBody(respBody), d.ID)
		if err != nil {
			s.Logger.Error("webhooks.delivery.mark_delivered_failed",
				"err", err, "delivery_id", d.ID,
			)
		}
		_ = recordAudit(ctx, s.DB, d.OrgID, d.EndpointID, "webhook.deliver", map[string]any{
			"delivery_id": d.ID,
			"event_type":  d.EventType,
			"status_code": statusCode,
			"attempt":     newAttempt,
		})
		return
	}

	reason := failureReason(statusCode, postErr)
	s.markFailed(ctx, d, newAttempt, statusCode, reason)

	switch {
	case newAttempt >= s.MaxAttempts:
		_, _ = s.DB.Exec(ctx, `
			UPDATE webhook_deliveries SET status = 'exhausted' WHERE id = $1
		`, d.ID)
		_ = recordAudit(ctx, s.DB, d.OrgID, d.EndpointID, "webhook.delivery_exhausted", map[string]any{
			"delivery_id": d.ID,
			"event_type":  d.EventType,
			"attempts":    newAttempt,
		})
	case shouldRetryStatus(statusCode):
		nextAt := time.Now().Add(jitter(computeNextRetry(newAttempt, s.Backoff)))
		_, err := s.DB.Exec(ctx, `
			UPDATE webhook_deliveries
			SET status = 'pending', next_retry_at = $1
			WHERE id = $2
		`, nextAt, d.ID)
		if err != nil {
			s.Logger.Error("webhooks.delivery.schedule_retry_failed",
				"err", err, "delivery_id", d.ID,
			)
		}
	default:
		_, _ = s.DB.Exec(ctx, `
			UPDATE webhook_deliveries SET status = 'failed' WHERE id = $1
		`, d.ID)
	}

	_ = recordAudit(ctx, s.DB, d.OrgID, d.EndpointID, "webhook.delivery_fail", map[string]any{
		"delivery_id": d.ID,
		"event_type":  d.EventType,
		"status_code": statusCode,
		"attempt":     newAttempt,
		"reason":      reason,
	})
}

// recordAudit inserts an audit_log row tagged as a system actor.
// The DeliveryService runs in the background with no request principal,
// so all deliveries are attributed to the system actor type.
func recordAudit(ctx context.Context, db *db.DB, orgID, resourceID, action string, metadata map[string]any) error {
	if db == nil {
		return nil
	}
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `
		INSERT INTO audit_log (id, org_id, user_id, actor_type, action, resource_type, resource_id, metadata, created_at)
		VALUES ($1, $2::uuid, NULL, 'system'::actor_type, $3::audit_action, 'webhook', $4, $5::jsonb, now())
	`, uuid.NewString(), orgID, action, resourceID, metaJSON)
	return err
}

func (s *DeliveryService) markFailed(ctx context.Context, d claimed, attempt, statusCode int, reason string) {
	_, _ = s.DB.Exec(ctx, `
		UPDATE webhook_deliveries
		SET response_status = NULLIF($1, 0), response_body = $2
		WHERE id = $3
	`, statusCode, reason, d.ID)
}

func (s *DeliveryService) post(ctx context.Context, info endpointInfo, d claimed) (int, []byte, error) {
	ts := time.Now().Unix()
	sig := signPayload(info.Secret, ts, d.Payload)
	header := fmt.Sprintf("t=%d,v1=%s", ts, sig)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, info.URL, bytes.NewReader(d.Payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(signatureHeader, header)
	req.Header.Set("User-Agent", "Orpheus-Webhooks/1.0")

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	return resp.StatusCode, body, nil
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
		// Select active endpoints whose subscribed_events contains
		// either the concrete event type or the wildcard '*'. The
		// text[] operator $1 = ANY(...) handles both cases.
		rows, err := s.DB.Query(ctx, `
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
			if _, err := s.DB.Exec(ctx, `
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
