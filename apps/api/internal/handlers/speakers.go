// Package handlers — speaker voiceprint (enrolled-profile) management (PRD 13).
//
// Voiceprints are BIOMETRIC data enrolled via the speaker.enroll processor and
// stored in the org-scoped, FORCE-RLS `speaker_profiles` table. This surface lets
// a tenant list and delete its own profiles — the delete path is what makes GDPR
// erasure of biometric data possible from the API. The embedding vector itself is
// never returned over the wire.
package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/dbtx"
)

// SpeakerHandler serves /v1/speakers.
type SpeakerHandler struct {
	DB    *db.DB
	Audit *audit.Recorder
}

// SpeakerProfile is the wire shape (no embedding vector — biometric).
type SpeakerProfile struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Dim            int       `json:"dim"`
	SampleCount    int       `json:"sample_count"`
	ModelVersionID string    `json:"model_version_id"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// List returns the org's enrolled speaker voiceprints (speakers:read).
func (h *SpeakerHandler) List(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	out := []SpeakerProfile{}
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		rows, err := dbtx.Query(ctx, h.DB,
			`SELECT id::text, name, dim, sample_count, model_version_id, created_at, updated_at
			 FROM speaker_profiles WHERE org_id = $1 ORDER BY name`, p.OrgID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s SpeakerProfile
			if err := rows.Scan(&s.ID, &s.Name, &s.Dim, &s.SampleCount,
				&s.ModelVersionID, &s.CreatedAt, &s.UpdatedAt); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to list speaker profiles")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"speaker_profiles": out})
}

// Delete removes one enrolled voiceprint — GDPR erasure of biometric data
// (speakers:write). Idempotent.
func (h *SpeakerHandler) Delete(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		_, e := dbtx.Exec(ctx, h.DB, `DELETE FROM speaker_profiles WHERE id = $1`, id)
		return e
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to delete speaker profile")
		return
	}
	if h.Audit != nil {
		_ = h.Audit.Record(r.Context(), audit.Entry{
			Action:       "speaker_profile.delete",
			ResourceType: "speaker_profile",
			ResourceID:   id,
		})
	}
	w.WriteHeader(http.StatusNoContent)
}
