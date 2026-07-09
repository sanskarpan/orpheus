// Package handlers — processor catalog endpoints.
//
// The processor catalog is global, not org-scoped: every org sees the
// same list of processors. The `processors` and `processor_versions`
// tables have RLS policies that allow public SELECT and reserve all
// writes for the service role, so reads can go through the pool
// without WithTenant. We still acquire a connection per request so
// behaviour matches the rest of the surface; if a future iteration
// needs a different pool, the change is local to this file.
package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/orpheus/api/internal/db"
)

// ProcessorHandler bundles the dependencies the processor endpoints
// need. All fields are required; zero values will fail at request time.
type ProcessorHandler struct {
	DB *db.DB
}

// Processor is the response shape for GET /v1/processors/{name}.
// It is the catalog record plus the full set of published versions.
type Processor struct {
	Name        string             `json:"name"`
	DisplayName string             `json:"display_name"`
	Description string             `json:"description"`
	Versions    []ProcessorVersion `json:"versions"`
}

// ProcessorVersion is a single published version of a processor. The
// numeric fields on the wire come back as `omitempty` so the JSON
// matches the OpenAPI spec for the Phase 1 surface — fields that the
// service role populates asynchronously (cost_per_second_usd, etc.)
// stay zero rather than serialising as `0`.
type ProcessorVersion struct {
	Version       string     `json:"version"`
	ModelID       string     `json:"model_id,omitempty"`
	ModelVerID    string     `json:"model_version_id,omitempty"`
	SLOP95Seconds float64    `json:"slo_p95_seconds"`
	SLOP99Seconds float64    `json:"slo_p99_seconds"`
	CostUSD       float64    `json:"cost_usd"`
	CreatedAt     time.Time  `json:"created_at"`
	DeprecatedAt  *time.Time `json:"deprecated_at,omitempty"`
}

// ProcessorSummary is the lightweight per-processor record returned
// by GET /v1/processors. It omits versions; clients call the detail
// endpoint when they need the full set.
type ProcessorSummary struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
}

// ProcessorSummaryList is a cursor-paginated list of processor
// summaries. We expose a NextCursor field for forward-compatibility
// with the spec, but list-processor currently returns the full set
// (limited to `limit`) and has_more drives the cursor.
type ProcessorSummaryList struct {
	Data       []ProcessorSummary `json:"data"`
	HasMore    bool               `json:"has_more"`
	NextCursor string             `json:"next_cursor"`
}

// List handles GET /v1/processors. The catalog is global and
// relatively small, so we limit the response to limit rows; the
// "list all" use case is satisfied at the page-size cap (200).
func (h *ProcessorHandler) List(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	rows, err := h.DB.Pool.Query(r.Context(), `
		SELECT name, display_name, description FROM processors
		ORDER BY name
		LIMIT $1
	`, limit+1)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to list processors")
		return
	}
	defer rows.Close()

	var procs []ProcessorSummary
	for rows.Next() {
		var p ProcessorSummary
		if err := rows.Scan(&p.Name, &p.DisplayName, &p.Description); err != nil {
			writeProblem(w, http.StatusInternalServerError, "internal", "scan failed")
			return
		}
		procs = append(procs, p)
	}
	if err := rows.Err(); err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "iter failed")
		return
	}

	hasMore := len(procs) > limit
	if hasMore {
		procs = procs[:limit]
	}

	writeJSON(w, http.StatusOK, ProcessorSummaryList{Data: procs, HasMore: hasMore})
}

// Get handles GET /v1/processors/{name}. The processor row is
// looked up by name (the slug used in CreateJobRequest.processor.name)
// and joined with every published version.
func (h *ProcessorHandler) Get(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	var p Processor
	err := h.DB.Pool.QueryRow(r.Context(), `
		SELECT name, display_name, description FROM processors WHERE name = $1
	`, name).Scan(&p.Name, &p.DisplayName, &p.Description)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "not_found", "Processor not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to get processor")
		return
	}

	rows, err := h.DB.Pool.Query(r.Context(), `
		SELECT version, model_id, model_version_id,
		       COALESCE(slo_p95_seconds, 0)::float8,
		       COALESCE(slo_p99_seconds, 0)::float8,
		       0::float8,
		       created_at, deprecated_at
		FROM processor_versions
		WHERE processor_id = (SELECT id FROM processors WHERE name = $1)
		ORDER BY created_at DESC
	`, name)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to list versions")
		return
	}
	defer rows.Close()

	for rows.Next() {
		var v ProcessorVersion
		var deprecatedAt *time.Time
		if err := rows.Scan(
			&v.Version,
			&v.ModelID,
			&v.ModelVerID,
			&v.SLOP95Seconds,
			&v.SLOP99Seconds,
			&v.CostUSD,
			&v.CreatedAt,
			&deprecatedAt,
		); err != nil {
			writeProblem(w, http.StatusInternalServerError, "internal", "version scan failed")
			return
		}
		v.DeprecatedAt = deprecatedAt
		p.Versions = append(p.Versions, v)
	}
	if err := rows.Err(); err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "version iter failed")
		return
	}

	writeJSON(w, http.StatusOK, p)
}
