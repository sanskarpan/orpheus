// Package handlers — usage time-series analytics (PRD 07).
package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/dbtx"
)

// UsageTimeseriesHandler serves GET /v1/usage/timeseries.
type UsageTimeseriesHandler struct {
	DB *db.DB
}

// TimeseriesPoint is one bucket of the series.
type TimeseriesPoint struct {
	Bucket         time.Time `json:"bucket"`
	Group          string    `json:"group,omitempty"`
	Jobs           int       `json:"jobs"`
	ComputeSeconds float64   `json:"compute_seconds"`
	CostUSD        float64   `json:"cost_usd"`
}

// GetTimeseries handles GET /v1/usage/timeseries?granularity=day&from=..&to=..&group_by=processor.
func (h *UsageTimeseriesHandler) GetTimeseries(w http.ResponseWriter, r *http.Request) {
	p, err := auth.PrincipalFromContext(r.Context())
	if err != nil {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "Unauthorized")
		return
	}
	q := r.URL.Query()
	granularity := q.Get("granularity")
	if granularity == "" {
		granularity = "day"
	}
	if granularity != "day" && granularity != "hour" {
		writeProblem(w, http.StatusBadRequest, "validation", "granularity must be day or hour")
		return
	}
	dimension := "total"
	switch q.Get("group_by") {
	case "", "total":
		dimension = "total"
	case "processor":
		dimension = "processor"
	case "status":
		dimension = "status"
	default:
		writeProblem(w, http.StatusBadRequest, "validation", "group_by must be processor, status, or total")
		return
	}
	// Default window: last 30 days.
	to := time.Now().UTC()
	from := to.AddDate(0, 0, -30)
	if v := q.Get("from"); v != "" {
		if t, e := time.Parse("2006-01-02", v); e == nil {
			from = t
		} else if t, e := time.Parse(time.RFC3339, v); e == nil {
			from = t
		}
	}
	if v := q.Get("to"); v != "" {
		if t, e := time.Parse("2006-01-02", v); e == nil {
			to = t.AddDate(0, 0, 1)
		} else if t, e := time.Parse(time.RFC3339, v); e == nil {
			to = t
		}
	}

	var series []TimeseriesPoint
	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		rows, e := dbtx.Query(ctx, h.DB, `
			SELECT date_trunc($2, hour) AS bucket, dimension_value,
			       sum(jobs)::int, sum(compute_seconds)::float8, sum(cost_usd)::float8
			FROM usage_rollup_hourly
			WHERE org_id=$1 AND dimension=$3 AND hour >= $4 AND hour < $5
			GROUP BY 1, 2
			ORDER BY 1, 2
		`, p.OrgID, granularity, dimension, from, to)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var pt TimeseriesPoint
			var group string
			if e := rows.Scan(&pt.Bucket, &group, &pt.Jobs, &pt.ComputeSeconds, &pt.CostUSD); e != nil {
				return e
			}
			if dimension != "total" {
				pt.Group = group
			}
			series = append(series, pt)
		}
		return rows.Err()
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to query usage")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"granularity": granularity, "group_by": dimension,
		"from": from, "to": to, "series": series,
	})
}
