// Package idempotency provides an HTTP middleware that recognises the
// Idempotency-Key header on POSTs and replays the original response
// when the same key is presented again with the same body.
//
// Semantics:
//
//   - Header missing or empty: the middleware is a no-op.
//   - Key + same body: cached response is replayed verbatim, with the
//     `Idempotent-Replay: true` header attached.
//   - Key + different body: 409 with a problem+json body, so the client
//     learns that the key was reused for a different request.
//   - Response status >= 400: the row is stored as "completed" so a
//     later retry with the same key + same body replays the failure.
//     5xx responses are also cached — the alternative (always re-try)
//     is a footgun for non-idempotent downstream calls.
//
// The middleware MUST be installed after the auth middleware so the
// org-scoped lookup has a principal to read.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/db"
)

const (
	// HeaderName is the HTTP header clients set to opt in.
	HeaderName = "Idempotency-Key"

	// MaxKeyLen caps the key length to the size of a typical UUID +
	// some room for a client prefix. Longer values are treated as if
	// the header were absent.
	MaxKeyLen = 255

	// ResponseTTL is how long a completed entry lives in
	// idempotency_keys before the cleanup job (out of scope for Phase
	// 1) is allowed to remove it.
	ResponseTTL = 24 * time.Hour

	// replayHeader is set on responses that came from the cache so
	// clients and test harnesses can tell replays apart from live
	// responses.
	replayHeader = "Idempotent-Replay"
)

// Middleware is the chi-compatible HTTP middleware. It is safe to share
// across requests; the *db.DB pool is goroutine-safe and the middleware
// holds no per-request state.
type Middleware struct {
	DB *db.DB
}

// New constructs a Middleware. database may be nil for tests that
// short-circuit before reaching the DB.
func New(database *db.DB) *Middleware {
	return &Middleware{DB: database}
}

// Handler returns the http.Handler middleware.
func (m *Middleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(HeaderName)
		if key == "" || len(key) > MaxKeyLen {
			next.ServeHTTP(w, r)
			return
		}

		// Read and buffer the body so we can hash it. Reset r.Body to
		// a fresh reader so the downstream handler still sees the
		// payload.
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))

		sum := sha256.Sum256(body)
		bodyHash := hex.EncodeToString(sum[:])

		// No principal = auth not applied yet. Skip caching; the auth
		// middleware (mounted after us in the chain) will respond.
		p, err := auth.PrincipalFromContext(r.Context())
		if err != nil || p == nil || p.OrgID == "" {
			next.ServeHTTP(w, r)
			return
		}
		orgID := p.OrgID

		// Cached entry?
		cached, err := m.getKey(r.Context(), orgID, key)
		if err == nil && cached != nil {
			if cached.RequestHash != bodyHash {
				writeProblem(w, http.StatusConflict,
					"https://docs.orpheus.dev/errors/idempotency-conflict",
					"Idempotency conflict",
					"key reused with different request body",
				)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set(replayHeader, "true")
			w.WriteHeader(int(cached.ResponseStatus))
			_, _ = w.Write(cached.ResponseBody)
			return
		}

		// Wrap the writer so we can capture status + body for caching.
		rec := newRecorder(w)
		next.ServeHTTP(rec, r)

		// Cache the response. status 2xx with a body is the typical
		// case; 4xx/5xx are also stored so a retry replays the
		// failure. The (org_id, key) unique constraint collapses
		// concurrent first-time requests onto one row.
		_ = m.completeKey(r.Context(), completeParams{
			ID:             uuid.NewString(),
			OrgID:          orgID,
			Key:            key,
			RequestHash:    bodyHash,
			ResponseStatus: int32(rec.status),
			ResponseBody:   rec.body.Bytes(),
		})
	})
}

// keyRecord is the projection of an idempotency_keys row that the
// middleware needs to do its job.
type keyRecord struct {
	ID             string
	OrgID          string
	Key            string
	RequestHash    string
	ResponseStatus int32
	ResponseBody   []byte
}

// getKey fetches a row by (org_id, key). A missing row is not an error:
// it returns (nil, nil) so the caller can fall through to the
// "first-time request" path.
func (m *Middleware) getKey(ctx context.Context, orgID, key string) (*keyRecord, error) {
	if m.DB == nil {
		return nil, nil
	}
	const q = `
		SELECT id, org_id, key, request_hash,
		       COALESCE(response_status, 0),
		       COALESCE(response_body, '{}'::jsonb)
		FROM idempotency_keys
		WHERE org_id = $1 AND key = $2
	`
	row := m.DB.QueryRow(ctx, q, orgID, key)
	var r keyRecord
	if err := row.Scan(&r.ID, &r.OrgID, &r.Key, &r.RequestHash, &r.ResponseStatus, &r.ResponseBody); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

// completeParams is the input to the upsert. The (org_id, key) UNIQUE
// constraint means a concurrent first-time request for the same key
// will lose the race; that's fine, both invocations are equivalent.
type completeParams struct {
	ID             string
	OrgID          string
	Key            string
	RequestHash    string
	ResponseStatus int32
	ResponseBody   []byte
}

// completeKey stores the response. We set status='completed' and an
// expires_at in the future; the table's partial index on
// response_status IS NULL doesn't help us here, so the cleanup job
// (Phase 2) should target `expires_at < now()`.
func (m *Middleware) completeKey(ctx context.Context, p completeParams) error {
	if m.DB == nil {
		return nil
	}
	const q = `
		INSERT INTO idempotency_keys (
			id, org_id, key, request_hash, response_status, response_body,
			status, expires_at
		)
		VALUES (
			$1, $2, $3, $4, $5, $6,
			'completed', now() + ($7::text || ' seconds')::interval
		)
		ON CONFLICT (org_id, key) DO NOTHING
	`
	_, err := m.DB.Exec(ctx, q,
		p.ID, p.OrgID, p.Key, p.RequestHash,
		p.ResponseStatus, p.ResponseBody,
		fmt.Sprintf("%d", int(ResponseTTL.Seconds())),
	)
	return err
}

// recorder is a tiny http.ResponseWriter wrapper that captures the
// status code and body for caching. It does NOT buffer Content-Type or
// other headers — the spec only stores status + body, and replay
// always rewrites Content-Type to application/json.
type recorder struct {
	http.ResponseWriter
	body   *bytes.Buffer
	status int
}

func newRecorder(w http.ResponseWriter) *recorder {
	return &recorder{ResponseWriter: w, body: &bytes.Buffer{}, status: http.StatusOK}
}

func (r *recorder) Header() http.Header { return r.ResponseWriter.Header() }

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

// writeProblem emits a problem+json response. The shape mirrors RFC
// 7807: `type` is a URI that documents the error, `title` is a short
// summary, `status` mirrors the HTTP code, and `detail` is the
// human-readable message.
func writeProblem(w http.ResponseWriter, status int, typeURI, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	body := fmt.Sprintf(
		`{"type":%q,"title":%q,"status":%d,"detail":%q}`,
		typeURI, title, status, detail,
	)
	_, _ = io.WriteString(w, body)
}
