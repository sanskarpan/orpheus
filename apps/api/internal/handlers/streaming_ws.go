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
	"encoding/xml"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/dbtx"
)

const streamTokenTTL = 2 * time.Minute

// Server-side audio metering: the browser sends PCM16 mono @ 16 kHz, so the
// billable duration is (bytes received) / (sampleRate * bytesPerSample). Metering
// here — not trusting the client's finalize payload — makes billing accurate and
// unspoofable.
const (
	streamSampleRate     = 16000
	streamBytesPerSample = 2
)

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

// workerConverseURL is the worker's full-duplex speech-to-speech socket
// (ASR→LLM→TTS). Derived from the transcribe URL by swapping the path unless
// overridden, so a single ORPHEUS_STREAMING_WS_URL configures both.
func workerConverseURL() string {
	if u := os.Getenv("ORPHEUS_STREAMING_CONVERSE_WS_URL"); u != "" {
		return u
	}
	return swapWSPath(workerStreamURL(), "/v1/stream/converse")
}

// workerTelephonyURL is the worker's Twilio Media Streams bridge socket.
func workerTelephonyURL() string {
	if u := os.Getenv("ORPHEUS_STREAMING_TELEPHONY_WS_URL"); u != "" {
		return u
	}
	return swapWSPath(workerStreamURL(), "/v1/telephony/twilio")
}

// swapWSPath replaces the path of a ws:// URL, keeping scheme+host.
func swapWSPath(base, path string) string {
	if i := strings.Index(base, "://"); i >= 0 {
		if j := strings.Index(base[i+3:], "/"); j >= 0 {
			return base[:i+3+j] + path
		}
	}
	return base + path
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
	h.relaySession(w, r, workerStreamURL())
}

// StreamConverse relays a browser connection to the worker's full-duplex
// speech-to-speech socket (ASR→LLM→TTS). Same token/session auth as transcribe.
func (h *StreamingHandler) StreamConverse(w http.ResponseWriter, r *http.Request) {
	h.relaySession(w, r, workerConverseURL())
}

func (h *StreamingHandler) relaySession(w http.ResponseWriter, r *http.Request, workerURL string) {
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
	worker, wresp, err := websocket.DefaultDialer.DialContext(dialCtx, workerURL, nil)
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

	// Pump both directions; the first error tears down both sockets. Count the
	// PCM bytes flowing browser→worker so the session is billed on audio
	// actually received, not on a duration the client reports at finalize.
	var once sync.Once
	var pcmBytes int64
	done := make(chan struct{})
	stop := func() { once.Do(func() { close(done) }) }
	relay := func(src, dst *websocket.Conn, count *int64) {
		defer stop()
		for {
			mt, data, err := src.ReadMessage()
			if err != nil {
				return
			}
			if count != nil && mt == websocket.BinaryMessage {
				atomic.AddInt64(count, int64(len(data)))
			}
			if err := dst.WriteMessage(mt, data); err != nil {
				return
			}
		}
	}
	go relay(client, worker, &pcmBytes) // browser → worker (start frame + PCM16)
	go relay(worker, client, nil)       // worker → browser (partial/final/done)
	<-done

	// Send a normal close frame both ways so peers see a clean 1000, not an
	// abnormal 1006.
	deadline := time.Now().Add(time.Second)
	closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
	_ = client.WriteControl(websocket.CloseMessage, closeMsg, deadline)
	_ = worker.WriteControl(websocket.CloseMessage, closeMsg, deadline)

	// Persist the server-metered audio duration; Finalize bills on it.
	if metered := float64(atomic.LoadInt64(&pcmBytes)) / float64(streamSampleRate*streamBytesPerSample); metered > 0 {
		h.setStreamMeteredSeconds(context.Background(), orgID, sessionID, metered)
	}

	// Leave the session in a finalize-able state; the client POSTs
	// /finalize with the accumulated transcript, which sets status=closed.
	h.setStreamStatus(context.Background(), orgID, sessionID, "closing", "")
}

const telephonyTokenTTL = 365 * 24 * time.Hour // baked into a tenant's TwiML

// MintTelephonyToken returns a long-lived org-scoped token the tenant embeds in
// their Twilio <Stream url="wss://…/telephony/twilio?token=…"> TwiML. Unlike the
// per-session stream token, telephony has no prior session (Twilio dials from a
// static webhook), so the token carries only the org.
func MintTelephonyToken(orgID string) string {
	payload := fmt.Sprintf("tel:%s:%d", orgID, time.Now().Add(telephonyTokenTTL).Unix())
	mac := hmac.New(sha256.New, streamTokenSecret())
	mac.Write([]byte(payload))
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(payload)) + "." + enc.EncodeToString(mac.Sum(nil))
}

func verifyTelephonyToken(token string) (orgID string, ok bool) {
	enc := base64.RawURLEncoding
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return "", false
	}
	payload, err := enc.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	gotSig, err := enc.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, streamTokenSecret())
	mac.Write(payload)
	if !hmac.Equal(gotSig, mac.Sum(nil)) {
		return "", false
	}
	fields := strings.Split(string(payload), ":")
	if len(fields) != 3 || fields[0] != "tel" {
		return "", false
	}
	exp, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	return fields[1], true
}

// TelephonyConfig returns the per-org telephony token + the Stream path a tenant
// bakes into their Twilio TwiML. Authenticated (streaming:write).
func (h *StreamingHandler) TelephonyConfig(w http.ResponseWriter, r *http.Request) {
	p, err := auth.PrincipalFromContext(r.Context())
	if err != nil {
		writeProblem(w, http.StatusUnauthorized, "unauthorized", "missing principal")
		return
	}
	token := MintTelephonyToken(p.OrgID)
	writeJSON(w, http.StatusOK, map[string]string{
		"token":   token,
		"ws_path": "/telephony/twilio?token=" + token,
		"twiml_hint": `<Response><Connect><Stream url="wss://YOUR_HOST/telephony/twilio?token=` +
			token + `"/></Connect></Response>`,
	})
}

// TelephonyTwiML returns the TwiML a Twilio number's Voice webhook fetches when a
// call comes in: it tells Twilio to open a bidirectional Media Stream back to this
// relay. The per-org token (query param) is echoed into the Stream URL. Mounted
// top-level (Twilio fetches it unauthenticated); the token is the credential for
// the subsequent stream. The wss host is taken from the request Host so it works
// behind a tunnel/load balancer.
func (h *StreamingHandler) TelephonyTwiML(w http.ResponseWriter, r *http.Request) {
	// Escape the reflected, attacker-controllable values so a crafted token/host
	// cannot restructure the returned TwiML (this endpoint is public).
	token := xmlEscape(r.URL.Query().Get("token"))
	host := xmlEscape(r.Host)
	twiml := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<Response><Connect><Stream url="wss://` + host + `/telephony/twilio?token=` + token +
		`"/></Connect></Response>`
	w.Header().Set("Content-Type", "text/xml")
	_, _ = w.Write([]byte(twiml))
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// TelephonyTwilio relays a Twilio Media Streams WebSocket to the worker's
// telephony bridge. Mounted OUTSIDE the X-API-Key middleware; authenticates via
// the per-org telephony token in the query string (Twilio can't send headers).
func (h *StreamingHandler) TelephonyTwilio(w http.ResponseWriter, r *http.Request) {
	orgID, ok := verifyTelephonyToken(r.URL.Query().Get("token"))
	if !ok || orgID == "" {
		http.Error(w, "invalid or expired telephony token", http.StatusUnauthorized)
		return
	}
	dialCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	worker, wresp, err := websocket.DefaultDialer.DialContext(dialCtx, workerTelephonyURL(), nil)
	if err != nil {
		if wresp != nil {
			_ = wresp.Body.Close()
		}
		http.Error(w, "telephony service unavailable", http.StatusBadGateway)
		return
	}
	defer func() { _ = worker.Close() }()

	client, err := streamUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = client.Close() }()

	var once sync.Once
	done := make(chan struct{})
	stop := func() { once.Do(func() { close(done) }) }
	pump := func(src, dst *websocket.Conn) {
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
	go pump(client, worker) // Twilio → worker (start/media/dtmf/stop frames)
	go pump(worker, client) // worker → Twilio (media/clear frames)
	<-done

	deadline := time.Now().Add(time.Second)
	closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
	_ = client.WriteControl(websocket.CloseMessage, closeMsg, deadline)
	_ = worker.WriteControl(websocket.CloseMessage, closeMsg, deadline)
}

// setStreamMeteredSeconds records the server-measured billable audio duration on
// a live session, so Finalize can bill on it instead of a client-reported value.
func (h *StreamingHandler) setStreamMeteredSeconds(ctx context.Context, orgID, id string, seconds float64) {
	_ = h.DB.WithTenant(ctx, orgID, func(ctx context.Context) error {
		_, e := dbtx.Exec(ctx, h.DB,
			`UPDATE streaming_sessions SET audio_seconds = $3
			 WHERE id = $1 AND org_id = $2 AND status <> 'closed'`,
			id, orgID, seconds)
		return e
	})
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
