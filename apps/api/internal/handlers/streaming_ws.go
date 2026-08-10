// Package handlers — streaming ASR WebSocket relay (Phase 8).
//
// The realtime transcription server lives in the Python worker (a FastAPI
// WebSocket at /v1/stream/transcribe that consumes PCM16 mono frames and emits
// ready/partial/final/done events). This relay is the platform's front door to
// it: the browser opens a WebSocket to the API, the API authenticates the
// connection, marks the session live, dials the worker's WebSocket, and pumps
// frames both ways. Keeping the relay here (rather than exposing the worker
// directly) preserves auth, tenancy, and session/billing state.
//
// Auth: a browser cannot send X-API-Key on a WebSocket handshake, and the key
// must stay server-side (BFF), so Create mints a short-lived HMAC token bound
// to (session_id, org). This relay validates that token — it is the only
// credential on the socket, scoped to one session for a couple of minutes.
package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/orpheus/api/internal/dbtx"
)

const streamTokenTTL = 2 * time.Minute

func streamTokenSecret() []byte {
	if s := os.Getenv("ORPHEUS_STREAM_TOKEN_SECRET"); s != "" {
		return []byte(s)
	}
	return []byte("orpheus-dev-stream-token-secret-change-me")
}

func workerStreamURL() string {
	if u := os.Getenv("ORPHEUS_STREAMING_WS_URL"); u != "" {
		return u
	}
	return "ws://127.0.0.1:8082/v1/stream/transcribe"
}

// MintStreamToken returns a signed, short-lived token binding a WebSocket
// connection to one session + org. Format: base64url(payload).base64url(sig)
// where payload = "sid:org:expUnix".
func MintStreamToken(sessionID, orgID string) string {
	payload := fmt.Sprintf("%s:%s:%d", sessionID, orgID, time.Now().Add(streamTokenTTL).Unix())
	mac := hmac.New(sha256.New, streamTokenSecret())
	mac.Write([]byte(payload))
	sig := mac.Sum(nil)
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(payload)) + "." + enc.EncodeToString(sig)
}

func verifyStreamToken(token string) (sessionID, orgID string, ok bool) {
	enc := base64.RawURLEncoding
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	payload, err := enc.DecodeString(parts[0])
	if err != nil {
		return "", "", false
	}
	gotSig, err := enc.DecodeString(parts[1])
	if err != nil {
		return "", "", false
	}
	mac := hmac.New(sha256.New, streamTokenSecret())
	mac.Write(payload)
	if !hmac.Equal(gotSig, mac.Sum(nil)) {
		return "", "", false
	}
	fields := strings.Split(string(payload), ":")
	if len(fields) != 3 {
		return "", "", false
	}
	exp, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", "", false
	}
	return fields[0], fields[1], true
}

var streamUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// The token is the credential, not the Origin; a same-origin check would
	// wrongly reject the BFF-hosted browser client. (Restrict for prod.)
	CheckOrigin: func(*http.Request) bool { return true },
}

// StreamTranscribe upgrades a browser connection and relays it to the worker's
// streaming ASR WebSocket. Mounted OUTSIDE the X-API-Key middleware; it
// authenticates via the query-string token minted at session creation.
func (h *StreamingHandler) StreamTranscribe(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sessionID := q.Get("session_id")
	token := q.Get("token")
	sid, orgID, ok := verifyStreamToken(token)
	if !ok || sid == "" || sid != sessionID {
		http.Error(w, "invalid or expired stream token", http.StatusUnauthorized)
		return
	}

	// The session must exist for this org and not already be closed.
	var status string
	err := h.DB.WithTenant(r.Context(), orgID, func(ctx context.Context) error {
		return dbtx.QueryRow(ctx, h.DB,
			`SELECT status FROM streaming_sessions WHERE id = $1 AND org_id = $2`, sessionID, orgID).Scan(&status)
	})
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if status == "closed" {
		http.Error(w, "session already closed", http.StatusConflict)
		return
	}

	// Dial the worker's streaming WS before upgrading the browser, so a worker
	// outage surfaces as a clean HTTP error instead of a dead socket.
	dialCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	worker, wresp, err := websocket.DefaultDialer.DialContext(dialCtx, workerStreamURL(), nil)
	if err != nil {
		if wresp != nil {
			_ = wresp.Body.Close()
		}
		h.setStreamStatus(r.Context(), orgID, sessionID, "failed", "worker streaming service unavailable")
		http.Error(w, "streaming service unavailable", http.StatusBadGateway)
		return
	}
	defer func() { _ = worker.Close() }()

	client, err := streamUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote the error
	}
	defer func() { _ = client.Close() }()

	h.setStreamStatus(r.Context(), orgID, sessionID, "live", "")

	// Pump both directions; the first error tears down both sockets.
	var once sync.Once
	done := make(chan struct{})
	stop := func() { once.Do(func() { close(done) }) }
	relay := func(src, dst *websocket.Conn) {
		defer stop()
		for {
			mt, data, err := src.ReadMessage()
			if err != nil {
				return
			}
			if err := dst.WriteMessage(mt, data); err != nil {
				return
			}
		}
	}
	go relay(client, worker) // browser → worker (start frame + PCM)
	go relay(worker, client) // worker → browser (partial/final/done)
	<-done

	// Leave the session in a finalize-able state; the client POSTs
	// /finalize with the accumulated transcript, which sets status=closed.
	h.setStreamStatus(context.Background(), orgID, sessionID, "closing", "")
}

// setStreamStatus best-effort updates a session's status (never on an
// already-closed session, so it can't clobber a finalize).
func (h *StreamingHandler) setStreamStatus(ctx context.Context, orgID, id, status, errMsg string) {
	_ = h.DB.WithTenant(ctx, orgID, func(ctx context.Context) error {
		var errArg any
		if errMsg != "" {
			errArg = errMsg
		}
		_, e := dbtx.Exec(ctx, h.DB,
			`UPDATE streaming_sessions SET status = $3, error = COALESCE($4, error)
			 WHERE id = $1 AND org_id = $2 AND status <> 'closed'`,
			id, orgID, status, errArg)
		return e
	})
}
