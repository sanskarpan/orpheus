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
	// WSURL is the (relative) path the client opens for the streaming
	// WebSocket, including the short-lived auth token. WSToken is the same
	// token exposed separately for clients that build their own URL.
	WSURL   string `json:"ws_url,omitempty"`
	WSToken string `json:"ws_token,omitempty"`
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
	token := MintStreamToken(id, p.OrgID)
	s.WSToken = token
	s.WSURL = "/stream/transcribe?session_id=" + id + "&token=" + token
	writeJSON(w, http.StatusCreated, s)
}

// Get returns one session.
func (h *StreamingHandler) Get(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Session not found")
		return
	}
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
	writeList(w, http.StatusOK, out, false, "")
}

// Finalize closes a session and persists the final transcript + billable
// duration + cost. Idempotent: finalizing an already-closed session is a no-op
// that returns the stored result.
func (h *StreamingHandler) Finalize(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Session not found")
		return
	}
	var req finalizeStreamingSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	// req.AudioSeconds is deliberately ignored: billing is derived only from the
	// server-metered audio_seconds the relay persisted, which a client cannot
	// spoof. Accepting a client duration here is what allowed a $0 finalize.

	var found, conflict bool
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		var tag string
		// Bill strictly on the server-metered audio_seconds recorded by the relay;
		// the client-reported value is never used. Finalize is only permitted once
		// the relay has finished the session — status 'closing' (metered) or
		// 'failed' (never streamed). A still-'connecting'/'live' session is not
		// finalize-able, so a mid-stream finalize cannot close it or zero out the
		// bill before the relay has metered the audio.
		e := dbtx.QueryRow(ctx, h.DB,
			`UPDATE streaming_sessions
			 SET status = 'closed', ended_at = now(), transcript = $2,
			     cost_usd = COALESCE(audio_seconds, 0) * $3
			 WHERE id = $1 AND org_id = $4 AND status IN ('closing', 'failed')
			 RETURNING 'ok'`,
			id, req.Transcript, streamingCostPerAudioSecond, p.OrgID,
		).Scan(&tag)
		if errors.Is(e, pgx.ErrNoRows) {
			// No row finalized: the session is missing, already closed (idempotent
			// replay), or still in flight (not yet finalize-able) — distinguish by
			// reading its current status.
			var status string
			if e2 := dbtx.QueryRow(ctx, h.DB,
				`SELECT status FROM streaming_sessions WHERE id = $1 AND org_id = $2`, id, p.OrgID,
			).Scan(&status); e2 == nil {
				if status == "closed" {
					found = true // already finalized — return the stored result
					return nil
				}
				conflict = true // 'connecting'/'live' — relay hasn't finished metering
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
	if errors.Is(err, pgx.ErrNoRows) || (!found && !conflict) {
		writeProblem(w, http.StatusNotFound, "not_found", "Session not found")
		return
	}
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to finalize session")
		return
	}
	if conflict {
		writeProblem(w, http.StatusConflict, "conflict", "Session is still streaming; finalize is not available until the relay has closed it")
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
