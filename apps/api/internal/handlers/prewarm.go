package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"time"
)

// prewarmModalTranscribe fires a best-effort warmup at the Modal transcription
// endpoint so a GPU container is spinning before the user's transcribe job
// arrives. It is triggered on a signal that predicts imminent load (upload
// completion) and is a no-op unless ORPHEUS_MODAL_WARMUP_URL/TOKEN are set.
//
// Run it in a goroutine: it blocks up to the timeout waiting for the container
// to come up (so the container reliably warms even though the caller does not
// wait), while the request handler returns immediately.
func prewarmModalTranscribe() {
	url := os.Getenv("ORPHEUS_MODAL_WARMUP_URL")
	token := os.Getenv("ORPHEUS_MODAL_WARMUP_TOKEN")
	if url == "" || token == "" {
		return
	}
	body, err := json.Marshal(map[string]any{"warmup": true, "token": token})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}
