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
	"github.com/orpheus/api/internal/dbtx"
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
//
// The flow is reserve-then-execute so that two concurrent requests with
// the same key do NOT both run the handler (which would double-apply the
// side effect): we atomically INSERT an `in_progress` row before calling
// the handler. The loser of that race sees the existing row and either
// replays the completed response, reports a body/endpoint conflict, or
// (if the winner is still running) returns 409.
//
// The request hash covers method + path + body, so reusing one key
// across two different endpoints is a conflict rather than a wrong-endpoint
// replay.
//
// All DB access runs inside WithTenant: idempotency_keys has FORCE
// row-level security, so a bare-pool query would see zero rows / be
// rejected on insert, silently disabling idempotency entirely.
func (m *Middleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(HeaderName)
		// No key, key too long, or idempotency disabled (nil DB) → no-op.
		if key == "" || len(key) > MaxKeyLen || m.DB == nil {
			next.ServeHTTP(w, r)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))

		// Scope the hash to method+path+body so a key reused on a
		// different endpoint (or with a different body) is a conflict,
		// not a replay of the wrong response.
		h := sha256.New()
		_, _ = io.WriteString(h, r.Method+"\n"+r.URL.Path+"\n")
		_, _ = h.Write(body)
		reqHash := hex.EncodeToString(h.Sum(nil))

		p, err := auth.PrincipalFromContext(r.Context())
		if err != nil || p == nil || p.OrgID == "" {
			next.ServeHTTP(w, r)
			return
		}
		orgID := p.OrgID

		// Reserve the key BEFORE running the handler.
		var (
			reserved bool
			existing *keyRecord
		)
		if derr := m.DB.WithTenant(r.Context(), orgID, func(ctx context.Context) error {
			var rerr error
			reserved, existing, rerr = m.reserve(ctx, orgID, key, reqHash)
			return rerr
		}); derr != nil {
			// Best-effort: a DB error here shouldn't hard-fail the
			// request. Proceed without idempotency protection.
			next.ServeHTTP(w, r)
			return
		}

		if !reserved {
			switch {
			case existing == nil:
				next.ServeHTTP(w, r)
			case existing.RequestHash != reqHash:
				writeProblem(w, http.StatusConflict,
					"https://docs.orpheus.dev/errors/idempotency-conflict",
					"Idempotency conflict",
					"key reused with a different request",
				)
			case existing.Status == "completed":
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set(replayHeader, "true")
				w.WriteHeader(int(existing.ResponseStatus))
				_, _ = w.Write(existing.ResponseBody)
			default: // in_progress: the original request is still running
				writeProblem(w, http.StatusConflict,
					"https://docs.orpheus.dev/errors/idempotency-in-progress",
					"Request in progress",
					"a request with this Idempotency-Key is still being processed",
				)
			}
			return
		}

		// We own the reservation: run the handler and persist the result.
		rec := newRecorder(w)
		next.ServeHTTP(rec, r)

		respBody := rec.body.Bytes()
		if len(respBody) == 0 {
			respBody = nil
		}
		_ = m.DB.WithTenant(r.Context(), orgID, func(ctx context.Context) error {
			return m.complete(ctx, orgID, key, int32(rec.status), respBody)
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
	Status         string
	ResponseStatus int32
	ResponseBody   []byte
}

// reserve atomically claims (org_id, key). On the first request it
// INSERTs an `in_progress` row and returns reserved=true. On a
// concurrent/repeat request the INSERT hits the unique constraint, does
// nothing, and reserve returns reserved=false plus the existing row so
// the caller can replay / conflict / 409-in-progress. Runs on the tx
// carried by ctx (WithTenant) so RLS is satisfied.
func (m *Middleware) reserve(ctx context.Context, orgID, key, reqHash string) (reserved bool, existing *keyRecord, err error) {
	if m.DB == nil {
		return false, nil, nil
	}
	const insertQ = `
		INSERT INTO idempotency_keys (id, org_id, key, request_hash, status, expires_at)
		VALUES ($1, $2, $3, $4, 'in_progress', now() + ($5::text || ' seconds')::interval)
		ON CONFLICT (org_id, key) DO NOTHING
		RETURNING id
	`
	var id string
	row := dbtx.QueryRow(ctx, m.DB, insertQ, uuid.NewString(), orgID, key, reqHash, fmt.Sprintf("%d", int(ResponseTTL.Seconds())))
	switch scanErr := row.Scan(&id); {
	case scanErr == nil:
		return true, nil, nil
	case errors.Is(scanErr, pgx.ErrNoRows):
		// Conflict: fetch the existing row.
		const selQ = `
			SELECT id, org_id, key, request_hash, status::text,
			       COALESCE(response_status, 0),
			       COALESCE(response_body, '{}'::jsonb)
			FROM idempotency_keys
			WHERE org_id = $1 AND key = $2
		`
		var rec keyRecord
		if err := dbtx.QueryRow(ctx, m.DB, selQ, orgID, key).Scan(
			&rec.ID, &rec.OrgID, &rec.Key, &rec.RequestHash, &rec.Status,
			&rec.ResponseStatus, &rec.ResponseBody,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil, nil
			}
			return false, nil, err
		}
		return false, &rec, nil
	default:
		return false, nil, scanErr
	}
}

// complete transitions the reserved row to `completed` with the captured
// response. Runs on the WithTenant tx so RLS accepts the UPDATE.
func (m *Middleware) complete(ctx context.Context, orgID, key string, status int32, body []byte) error {
	if m.DB == nil {
		return nil
	}
	const q = `
		UPDATE idempotency_keys
		SET status = 'completed', response_status = $3, response_body = $4
		WHERE org_id = $1 AND key = $2
	`
	_, err := dbtx.Exec(ctx, m.DB, q, orgID, key, status, body)
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
