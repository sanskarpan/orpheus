// Phase 4.2 transcribe-long end-to-end smoke.
//
// Proves the user-facing transcribe-long workflow: the user
// uploads an audio file, POSTs /v1/workflows/transcribe-long
// with the artifact_id, the API creates a workflows row + a
// transcribe jobs row + an outbox event, the worker subscribes
// to JetStream, downloads the artifact, runs faster-whisper,
// writes the transcript into jobs.result, and updates the
// workflows row to status='completed' with the same result.
// The user then polls GET /v1/workflows/{id} and sees the
// completed workflow + transcript.
//
// Flow:
//
//	seed org + user + api_key + processor (transcribe 1.0.0) + artifact
//	upload a WAV to MinIO at artifact.s3_key
//	start the Go API in-process
//	start the Python JetStream worker subprocess (uses ffmpeg + faster-whisper)
//	POST /v1/workflows/transcribe-long {artifact_id}
//	  → workflows row (queued) + jobs row (queued) + outbox → JetStream
//	  → worker → jobs.status = 'completed' + workflows.status = 'completed'
//	poll GET /v1/workflows/{id} until status = 'completed'
//	assert workflows.result.text is non-empty
//
// Gated on:
//
//	ORPHEUS_E2E=1
//	ORPHEUS_TEST_DATABASE_URL
//	ORPHEUS_TEST_NATS_URL
//	ORPHEUS_TEST_S3_ENDPOINT
//	ORPHEUS_TEST_S3_BUCKET
//	ORPHEUS_TEST_S3_ACCESS_KEY
//	ORPHEUS_TEST_S3_SECRET_KEY
//	ORPHEUS_TEST_WHISPER_MODEL (optional; defaults to tiny.en)
//
// The test generates the source audio with espeak-ng if
// available (so the transcript can be asserted to contain the
// phrase "the quick brown fox jumps over the lazy dog"), or
// falls back to an ffmpeg-generated 1 kHz sine tone for
// environments without a TTS binary installed. With ffmpeg
// fallback the test only asserts that the transcript is
// non-empty — a sine wave does not produce a meaningful
// transcript from whisper.
//
// Hermetic on 127.0.0.1:18085 — distinct from TestE2E_Health
// (18080), TestE2E_ExtractMetadata (18082), TestE2E_Probe
// (18083), TestE2E_Slice (18084).
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orpheus/api/internal/e2e/setup"
)

const (
	tlTestProcessorName    = "transcribe"
	tlTestProcessorVersion = "1.0.0"
	tlTestS3Key            = "e2e/transcribe-long/test.wav"
	tlTestAPIPort          = 18085
)

func TestE2E_TranscribeLong(t *testing.T) {
	if os.Getenv("ORPHEUS_E2E") != "1" {
		t.Skip("set ORPHEUS_E2E=1 to run")
	}
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	natsURL := os.Getenv("ORPHEUS_TEST_NATS_URL")
	s3Endpoint := os.Getenv("ORPHEUS_TEST_S3_ENDPOINT")
	s3Bucket := os.Getenv("ORPHEUS_TEST_S3_BUCKET")
	s3AccessKey := os.Getenv("ORPHEUS_TEST_S3_ACCESS_KEY")
	s3SecretKey := os.Getenv("ORPHEUS_TEST_S3_SECRET_KEY")
	whisperDir := os.Getenv("ORPHEUS_TEST_WHISPER_DIR")
	whisperModel := os.Getenv("ORPHEUS_TEST_WHISPER_MODEL")
	if whisperModel == "" {
		whisperModel = "tiny.en"
	}
	if dsn == "" || natsURL == "" ||
		s3Endpoint == "" || s3Bucket == "" ||
		s3AccessKey == "" || s3SecretKey == "" {
		t.Skip("missing one of ORPHEUS_TEST_{DATABASE,NATS,S3}_*_URL")
	}

	if whisperDir == "" {
		if _, err := os.Stat("/models"); err == nil {
			whisperDir = "/models"
		}
	}
	if whisperDir != "" {
		t.Setenv("ORPHEUS_WORKER_WHISPER_DIR", whisperDir)
		t.Setenv("ORPHEUS_WORKER_WHISPER_MODEL", whisperModel)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	setup.RunMigrations(t, ctx, dsn)
	pool := setup.ServicePool(t, ctx, dsn)
	setup.StartWorker(t, ctx, natsURL, dsn, s3Endpoint, s3Bucket, s3AccessKey, s3SecretKey)
	apiURL, apiStop := setup.StartAPI(t, ctx, pool, natsURL, tlTestAPIPort)
	t.Cleanup(apiStop)

	orgID := uuid.NewString()
	setup.SeedOrg(t, ctx, pool, orgID)
	setup.SeedUser(t, ctx, pool, orgID)
	setup.CleanupOrgData(t, pool, orgID)
	processorID, processorVersionID := setup.SeedProcessor(t, ctx, pool, tlTestProcessorName, tlTestProcessorVersion)

	phrase := "the quick brown fox jumps over the lazy dog"
	wavPath := filepath.Join(t.TempDir(), "test.wav")
	sourceKind, err := generateTestWAV(wavPath, phrase)
	if err != nil {
		t.Skipf("could not generate test wav: %v (espeak-ng or ffmpeg required)", err)
	}
	wavBytes, err := os.ReadFile(wavPath)
	if err != nil {
		t.Fatalf("read generated wav: %v", err)
	}
	setup.EnsureBucket(t, ctx, s3Endpoint, s3Bucket, s3AccessKey, s3SecretKey)
	setup.UploadS3(t, ctx, s3Endpoint, s3Bucket, tlTestS3Key, wavBytes, s3AccessKey, s3SecretKey)
	t.Cleanup(func() {
		setup.DeleteS3(t, ctx, s3Endpoint, s3Bucket, tlTestS3Key, s3AccessKey, s3SecretKey)
	})
	artifactID := setup.SeedArtifact(t, ctx, pool, orgID, s3Bucket, tlTestS3Key, len(wavBytes))
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanCancel()
		_, _ = pool.Exec(cleanCtx, `DELETE FROM artifacts WHERE id = $1`, artifactID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM processor_versions WHERE id = $1`, processorVersionID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM processors WHERE id = $1`, processorID)
	})

	apiKey := setup.SeedAPIKey(t, ctx, pool, orgID)

	body, err := json.Marshal(map[string]any{"artifact_id": artifactID})
	if err != nil {
		t.Fatalf("marshal workflow request: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/v1/workflows/transcribe-long", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest POST transcribe-long: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/workflows/transcribe-long: %v", err)
	}
	respBody, _ := readAll(resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/workflows/transcribe-long status = %d, want 201; body=%s", resp.StatusCode, respBody)
	}
	var created struct {
		ID           string `json:"id"`
		CurrentJobID string `json:"current_job_id"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil {
		t.Fatalf("decode workflow create: %v (body=%s)", err, respBody)
	}
	if created.ID == "" {
		t.Fatalf("workflow id is empty; body=%s", respBody)
	}
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanCancel()
		_, _ = pool.Exec(cleanCtx, `DELETE FROM workflows WHERE id = $1`, created.ID)
		if created.CurrentJobID != "" {
			_, _ = pool.Exec(cleanCtx, `DELETE FROM jobs WHERE id = $1`, created.CurrentJobID)
		}
	})

	deadline := time.Now().Add(60 * time.Second)
	var workflow struct {
		ID     string          `json:"id"`
		Status string          `json:"status"`
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/v1/workflows/"+created.ID, nil)
		if err != nil {
			t.Fatalf("NewRequest GET workflow: %v", err)
		}
		req.Header.Set("X-API-Key", apiKey)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /v1/workflows/%s: %v", created.ID, err)
		}
		wfBody, _ := readAll(r)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("GET /v1/workflows/%s status = %d, want 200; body=%s", created.ID, r.StatusCode, wfBody)
		}
		if err := json.Unmarshal(wfBody, &workflow); err != nil {
			t.Fatalf("decode workflow: %v (body=%s)", err, wfBody)
		}
		if workflow.Status == "completed" || workflow.Status == "failed" {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if workflow.Status != "completed" {
		t.Fatalf("workflow %s last status = %s, want completed; error=%s; result=%s",
			created.ID, workflow.Status, workflow.Error, string(workflow.Result))
	}

	var result struct {
		Text            string  `json:"text"`
		Language        string  `json:"language"`
		DurationSeconds float64 `json:"duration_seconds"`
	}
	if err := json.Unmarshal(workflow.Result, &result); err != nil {
		t.Fatalf("decode workflow.result: %v (raw=%s)", err, string(workflow.Result))
	}
	if result.Text == "" {
		t.Fatalf("workflow.result.text is empty; raw=%s", string(workflow.Result))
	}
	if sourceKind == "espeak" {
		if !strings.Contains(strings.ToLower(result.Text), "fox") {
			t.Errorf("expected transcript to contain phrase containing 'fox' (espeak-ng source); got %q",
				result.Text)
		}
	}
}

// generateTestWAV writes a 16kHz mono WAV at path. It prefers
// espeak-ng so the transcript can be asserted to contain a known
// phrase. If espeak-ng is not on PATH it falls back to an
// ffmpeg-generated 1 kHz sine tone. Returns the kind of source
// ("espeak" or "ffmpeg") so the caller can decide whether to
// assert on the transcript content.
func generateTestWAV(path, phrase string) (string, error) {
	if _, err := exec.LookPath("espeak-ng"); err == nil {
		cmd := exec.Command("espeak-ng", "-w", path, phrase)
		if runErr := cmd.Run(); runErr == nil {
			return "espeak", nil
		}
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return "", fmt.Errorf("neither espeak-ng nor ffmpeg available: %w", err)
	}
	cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi",
		"-i", "sine=frequency=1000:duration=2:sample_rate=16000",
		"-ac", "1", path)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffmpeg generate wav: %w", err)
	}
	return "ffmpeg", nil
}

func readAll(resp *http.Response) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(resp.Body)
}
