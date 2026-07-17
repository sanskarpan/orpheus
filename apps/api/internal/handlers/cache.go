// Package handlers — content-addressed result cache (PRD 01).
//
// The cache key derivation and lookup live here; the job Create handler
// (jobs.go) calls into these helpers. The key is derived only in Go so the
// read side (Create) and the write side (worker, which copies the stored
// cache_meta) never disagree on canonicalization.
package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/dbtx"
)

// cacheParamsHashVersion versions the params canonicalization so a future
// change to the hashing scheme cannot silently reuse stale entries.
const cacheParamsHashVersion = "v1"

// canonicalParamsHash returns the hex sha256 of the canonicalized params
// JSON. Canonical form = the params object with the reserved `_processor`
// marker removed, re-marshaled by encoding/json (which sorts map keys). A
// nil/empty params hashes as the empty object.
func canonicalParamsHash(params json.RawMessage) (string, error) {
	m := map[string]any{}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &m); err != nil {
			return "", err
		}
	}
	delete(m, processorKey)
	canon, err := json.Marshal(m) // map keys are marshaled in sorted order
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(cacheParamsHashVersion+":"), canon...))
	return hex.EncodeToString(sum[:]), nil
}

// computeCacheKey derives the org-scoped cache key from the three inputs.
// NUL separators keep the concatenation unambiguous.
func computeCacheKey(inputHash, paramsHash, modelVersionID string) []byte {
	h := sha256.New()
	h.Write([]byte(inputHash))
	h.Write([]byte{0})
	h.Write([]byte(paramsHash))
	h.Write([]byte{0})
	h.Write([]byte(modelVersionID))
	return h.Sum(nil)
}

// cacheLookup returns a prior cached result for (org, cacheKey) if present.
// It must run inside a WithTenant scope so RLS confines the read to the org.
func cacheLookup(ctx context.Context, database *db.DB, cacheKey []byte) (result json.RawMessage, sourceJobID string, found bool, err error) {
	var res []byte
	err = dbtx.QueryRow(ctx, database, `
		SELECT result, source_job_id::text
		FROM job_result_cache
		WHERE cache_key = $1
	`, cacheKey).Scan(&res, &sourceJobID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", false, nil
		}
		return nil, "", false, err
	}
	return json.RawMessage(res), sourceJobID, true, nil
}

// CacheHandler serves the cache admin endpoints (stats + invalidation).
type CacheHandler struct {
	DB *db.DB
}

// CacheStats is the response for GET /v1/cache/stats.
type CacheStats struct {
	Entries       int     `json:"entries"`
	Hits          int     `json:"hits"`
	Misses        int     `json:"misses"`
	HitRate       float64 `json:"hit_rate"`
	EstSavingsUSD float64 `json:"est_savings_usd"`
	Period        string  `json:"period"`
}

// Stats handles GET /v1/cache/stats. Hits/misses are counted from the jobs
// table over the current billing month (cache_hit vs. a cacheable miss —
// a job that carried cache_meta but was not itself a hit). Estimated
// savings = summed cost of the source jobs that hits reused.
func (h *CacheHandler) Stats(w http.ResponseWriter, r *http.Request) {
	p, err := auth.PrincipalFromContext(r.Context())
	if err != nil {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "Unauthorized")
		return
	}
	var s CacheStats
	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		if err := dbtx.QueryRow(ctx, h.DB, `SELECT COUNT(*)::int FROM job_result_cache`).Scan(&s.Entries); err != nil {
			return err
		}
		return dbtx.QueryRow(ctx, h.DB, `
			SELECT
				COUNT(*) FILTER (WHERE cache_hit)::int,
				COUNT(*) FILTER (WHERE NOT cache_hit AND cache_meta IS NOT NULL)::int,
				COALESCE(SUM(
					CASE WHEN cache_hit THEN (
						SELECT COALESCE(src.cost_usd, 0)
						FROM jobs src WHERE src.id = jobs.cached_from_job_id
					) ELSE 0 END
				), 0)::float8
			FROM jobs
			WHERE created_at >= date_trunc('month', now())
		`).Scan(&s.Hits, &s.Misses, &s.EstSavingsUSD)
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to get cache stats")
		return
	}
	if total := s.Hits + s.Misses; total > 0 {
		s.HitRate = float64(s.Hits) / float64(total)
	}
	s.Period = time.Now().UTC().Format("2006-01")
	writeJSON(w, http.StatusOK, s)
}

// Invalidate handles DELETE /v1/cache?model_version_id=... — purge cache
// entries for a model version (e.g. a defective model). Org-scoped via RLS.
func (h *CacheHandler) Invalidate(w http.ResponseWriter, r *http.Request) {
	p, err := auth.PrincipalFromContext(r.Context())
	if err != nil {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "Unauthorized")
		return
	}
	mv := r.URL.Query().Get("model_version_id")
	if mv == "" {
		writeProblem(w, http.StatusBadRequest, "validation", "model_version_id query param required")
		return
	}
	var deleted int64
	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		tag, e := dbtx.Exec(ctx, h.DB, `DELETE FROM job_result_cache WHERE model_version_id = $1`, mv)
		if e != nil {
			return e
		}
		deleted = tag.RowsAffected()
		return nil
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to invalidate cache")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted, "model_version_id": mv})
}
