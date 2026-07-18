// Package handlers — realtime streaming session endpoints (Phase 8).
//
// These are the control plane for streaming ASR sessions: the actual
// transcription happens over a WebSocket in the worker streaming service;
// this REST surface creates a session, lets clients inspect/list it, and
// finalizes it (persisting the final transcript, billable audio duration,
// and cost). Sessions are org-scoped and RLS-enforced.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/dbtx"
)

// streamingCostPerAudioSecond prices a streaming session by its billable
// audio duration. A coarse rate; GPU/tier pricing refines it later.
const streamingCostPerAudioSecond = 0.0001

type StreamingHandler struct {
	DB    *db.DB
	Audit *audit.Recorder
}

type StreamingSession struct {
	ID             string     `json:"id"`
	Status         string     `json:"status"`
	ModelVersionID *string    `json:"model_version_id,omitempty"`
	StartedAt      time.Time  `json:"started_at"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
	AudioSeconds   *float64   `json:"audio_seconds,omitempty"`
	Transcript     *string    `json:"transcript,omitempty"`
	CostUSD        float64    `json:"cost_usd"`
	Error          string     `json:"error,omitempty"`
	// WSURL is a hint for where the client opens the streaming WebSocket.
	WSURL string `json:"ws_url,omitempty"`
}

type createStreamingSessionRequest struct {
	ModelVersionID string `json:"model_version_id,omitempty"`
}

type finalizeStreamingSessionRequest struct {
	Transcript   string  `json:"transcript"`
	AudioSeconds float64 `json:"audio_seconds"`
}

// Create opens a streaming session (status=connecting).
func (h *StreamingHandler) Create(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	var req createStreamingSessionRequest
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req)
	}
	var modelVersion any
	if req.ModelVersionID != "" {
		modelVersion = req.ModelVersionID
	}

	id := uuid.NewString()
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		_, e := dbtx.Exec(ctx, h.DB,
			`INSERT INTO streaming_sessions (id, org_id, status, model_version_id) VALUES ($1, $2, 'connecting', $3)`,
			id, p.OrgID, modelVersion,
		)
		return e
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to create session")
		return
	}
	s, err := h.load(r.Context(), p.OrgID, id)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to load session")
		return
	}
	s.WSURL = "/v1/stream/transcribe?session_id=" + id
	writeJSON(w, http.StatusCreated, s)
}

// Get returns one session.
func (h *StreamingHandler) Get(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id := chi.URLParam(r, "id")
	s, err := h.load(r.Context(), p.OrgID, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, http.StatusNotFound, "not_found", "Session not found")
		return
	}
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to load session")
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// List returns the org's sessions, most recent first.
func (h *StreamingHandler) List(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	out := []StreamingSession{}
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		rows, err := dbtx.Query(ctx, h.DB,
			`SELECT id, status, model_version_id, started_at, ended_at, audio_seconds, cost_usd, COALESCE(error,'')
			 FROM streaming_sessions WHERE org_id = $1 ORDER BY started_at DESC LIMIT 100`, p.OrgID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s StreamingSession
			if err := rows.Scan(&s.ID, &s.Status, &s.ModelVersionID, &s.StartedAt, &s.EndedAt,
				&s.AudioSeconds, &s.CostUSD, &s.Error); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to list sessions")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// Finalize closes a session and persists the final transcript + billable
// duration + cost. Idempotent: finalizing an already-closed session is a no-op
// that returns the stored result.
func (h *StreamingHandler) Finalize(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id := chi.URLParam(r, "id")
	var req finalizeStreamingSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	if req.AudioSeconds < 0 {
		writeProblem(w, http.StatusBadRequest, "validation", "audio_seconds must be >= 0")
		return
	}
	cost := req.AudioSeconds * streamingCostPerAudioSecond

	var found bool
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		var tag string
		e := dbtx.QueryRow(ctx, h.DB,
			`UPDATE streaming_sessions
			 SET status = 'closed', ended_at = now(), transcript = $2, audio_seconds = $3, cost_usd = $4
			 WHERE id = $1 AND org_id = $5 AND status <> 'closed'
			 RETURNING 'ok'`,
			id, req.Transcript, req.AudioSeconds, cost, p.OrgID,
		).Scan(&tag)
		if errors.Is(e, pgx.ErrNoRows) {
			// Either not found, or already closed (idempotent) — distinguish below.
			var exists bool
			if e2 := dbtx.QueryRow(ctx, h.DB,
				`SELECT true FROM streaming_sessions WHERE id = $1 AND org_id = $2`, id, p.OrgID,
			).Scan(&exists); e2 == nil {
				found = true // already closed
				return nil
			}
			return pgx.ErrNoRows
		}
		if e != nil {
			return e
		}
		found = true
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) || !found {
		writeProblem(w, http.StatusNotFound, "not_found", "Session not found")
		return
	}
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to finalize session")
		return
	}
	s, err := h.load(r.Context(), p.OrgID, id)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to load session")
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (h *StreamingHandler) load(ctx context.Context, orgID, id string) (StreamingSession, error) {
	var s StreamingSession
	err := h.DB.WithTenant(ctx, orgID, func(ctx context.Context) error {
		return dbtx.QueryRow(ctx, h.DB,
			`SELECT id, status, model_version_id, started_at, ended_at, audio_seconds, transcript, cost_usd, COALESCE(error,'')
			 FROM streaming_sessions WHERE id = $1 AND org_id = $2`, id, orgID,
		).Scan(&s.ID, &s.Status, &s.ModelVersionID, &s.StartedAt, &s.EndedAt,
			&s.AudioSeconds, &s.Transcript, &s.CostUSD, &s.Error)
	})
	return s, err
}
