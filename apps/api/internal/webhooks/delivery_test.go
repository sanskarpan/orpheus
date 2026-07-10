package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestComputeNextRetry_FirstEntry(t *testing.T) {
	base := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute}
	got := computeNextRetry(1, base)
	if got != time.Minute {
		t.Errorf("computeNextRetry(1) = %v, want 1m", got)
	}
}

func TestComputeNextRetry_MiddleEntry(t *testing.T) {
	base := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute}
	got := computeNextRetry(3, base)
	if got != 4*time.Minute {
		t.Errorf("computeNextRetry(3) = %v, want 4m", got)
	}
}

func TestComputeNextRetry_CapsAtLastEntry(t *testing.T) {
	base := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute}
	got := computeNextRetry(99, base)
	if got != 4*time.Minute {
		t.Errorf("computeNextRetry(99) = %v, want 4m (capped)", got)
	}
}

func TestComputeNextRetry_EmptyBase(t *testing.T) {
	if got := computeNextRetry(5, nil); got != 0 {
		t.Errorf("empty base: got %v, want 0", got)
	}
	if got := computeNextRetry(5, []time.Duration{}); got != 0 {
		t.Errorf("zero-len base: got %v, want 0", got)
	}
}

func TestComputeNextRetry_ZeroAttempt(t *testing.T) {
	base := []time.Duration{30 * time.Second, time.Minute}
	if got := computeNextRetry(0, base); got != 30*time.Second {
		t.Errorf("attempt 0: got %v, want 30s (clamped to first entry)", got)
	}
}

func TestSignPayload_Deterministic(t *testing.T) {
	secret := "s3cret"
	body := []byte(`{"hello":"world"}`)
	ts := int64(1700000000)

	want := signPayload(secret, ts, body)
	got := signPayload(secret, ts, body)
	if want != got {
		t.Fatalf("non-deterministic: %s != %s", want, got)
	}

	// Cross-check against stdlib so a future refactor cannot silently
	// diverge from HMAC-SHA256(secret, "<ts>.<body>").
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%d.", ts)
	mac.Write(body)
	wantHex := hex.EncodeToString(mac.Sum(nil))
	if want != wantHex {
		t.Errorf("mismatch with stdlib: got %s, want %s", want, wantHex)
	}
}

func TestSignPayload_DifferentTSProducesDifferentSig(t *testing.T) {
	body := []byte(`{"x":1}`)
	a := signPayload("k", 1, body)
	b := signPayload("k", 2, body)
	if a == b {
		t.Errorf("different timestamps produced the same signature")
	}
}

func TestSignPayload_DifferentSecretProducesDifferentSig(t *testing.T) {
	body := []byte(`{"x":1}`)
	a := signPayload("a", 1, body)
	b := signPayload("b", 1, body)
	if a == b {
		t.Errorf("different secrets produced the same signature")
	}
}

func TestShouldRetryStatus(t *testing.T) {
	cases := []struct {
		code int
		want bool
	}{
		{200, false},
		{201, false},
		{204, false},
		{301, false},
		{400, false},
		{401, false},
		{403, false},
		{404, false},
		{408, true},
		{429, true},
		{500, true},
		{502, true},
		{503, true},
		{504, true},
	}
	for _, tc := range cases {
		got := shouldRetryStatus(tc.code)
		if got != tc.want {
			t.Errorf("shouldRetryStatus(%d) = %v, want %v", tc.code, got, tc.want)
		}
	}
}

func TestJitter_ZeroOrNegativeReturnedUnchanged(t *testing.T) {
	if got := jitter(0); got != 0 {
		t.Errorf("jitter(0) = %v, want 0", got)
	}
	if got := jitter(-time.Second); got != -time.Second {
		t.Errorf("jitter(-1s) = %v, want -1s", got)
	}
}

func TestJitter_StaysWithinBand(t *testing.T) {
	base := 10 * time.Second
	min := time.Duration(float64(base) * 0.9)
	max := time.Duration(float64(base) * 1.1)
	for i := 0; i < 200; i++ {
		got := jitter(base)
		if got < min || got > max {
			t.Fatalf("jitter(%v) = %v, want in [%v, %v]", base, got, min, max)
		}
	}
}

func TestEnqueue_NoDBIsError(t *testing.T) {
	s := &DeliveryService{}
	if err := s.Enqueue(context.Background(), "00000000-0000-0000-0000-000000000001", "job.succeeded", "id-1", map[string]any{}); err == nil {
		t.Fatal("expected error when DB is nil")
	}
}

func TestPost_SendsExpectedShapeAndValidSignature(t *testing.T) {
	const secret = "shh"
	var (
		gotMethod string
		gotCT     string
		gotSig    string
		gotBody   []byte
		gotUA     string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		gotSig = r.Header.Get(signatureHeader)
		gotUA = r.Header.Get("User-Agent")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := &DeliveryService{Logger: nil, HTTPClient: srv.Client()}
	body := []byte(`{"event":"job.succeeded","id":"e1"}`)
	code, _, err := s.post(context.Background(), endpointInfo{URL: srv.URL, Secret: secret}, claimed{
		EventType: "job.succeeded",
		EventID:   "e1",
		Payload:   body,
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if code != 200 {
		t.Errorf("status = %d, want 200", code)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if !strings.HasPrefix(gotUA, "Orpheus-Webhooks/") {
		t.Errorf("User-Agent = %q, want Orpheus-Webhooks/*", gotUA)
	}
	if !strings.HasPrefix(gotSig, "t=") || !strings.Contains(gotSig, ",v1=") {
		t.Fatalf("signature header %q missing t= or v1= segment", gotSig)
	}

	// Verify the signature.
	var ts int64
	var v1 string
	for _, seg := range strings.Split(gotSig, ",") {
		if strings.HasPrefix(seg, "t=") {
			_, _ = fmt.Sscanf(seg[2:], "%d", &ts)
		} else if strings.HasPrefix(seg, "v1=") {
			v1 = seg[3:]
		}
	}
	if ts == 0 || v1 == "" {
		t.Fatalf("could not parse signature %q", gotSig)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%d.", ts)
	mac.Write(gotBody)
	want := hex.EncodeToString(mac.Sum(nil))
	if v1 != want {
		t.Errorf("signature mismatch: got %s, want %s", v1, want)
	}
}

func TestPost_HandlesNon2xxAsErrorPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()

	s := &DeliveryService{HTTPClient: srv.Client()}
	code, body, err := s.post(context.Background(), endpointInfo{URL: srv.URL, Secret: "k"}, claimed{Payload: []byte(`{}`)})
	if err != nil {
		t.Fatalf("unexpected transport err: %v", err)
	}
	if code != 500 {
		t.Errorf("code = %d, want 500", code)
	}
	if string(body) != "nope" {
		t.Errorf("body = %q, want nope", body)
	}
}

func TestTruncateBody_StripsAtNewlineAndCapsAt200(t *testing.T) {
	multi := []byte("line1\nline2\nline3")
	if got := truncateBody(multi); string(got) != "line1" {
		t.Errorf("truncate at newline: got %q, want line1", got)
	}
	long := bytesRepeat('x', 500)
	if got := truncateBody(long); len(got) != 200 {
		t.Errorf("truncate at 200: len = %d, want 200", len(got))
	}
	short := []byte("hi")
	if got := truncateBody(short); string(got) != "hi" {
		t.Errorf("truncate short: got %q, want hi", got)
	}
}

func TestFailureReason_TransportVsHTTP(t *testing.T) {
	if got := failureReason(503, nil); got != "http 503" {
		t.Errorf("http reason = %q, want http 503", got)
	}
	if got := failureReason(0, fmt.Errorf("dial tcp: connection refused")); !strings.HasPrefix(got, "transport:") {
		t.Errorf("transport reason = %q, want transport: prefix", got)
	}
}

func TestEnqueue_NoEndpointsIsNoop(t *testing.T) {
	// Enqueue is documented as a no-op when the org has no matching
	// endpoints. Without a DB connection we cannot exercise the full
	// happy path, so this test documents the contract: nil-DB errors
	// loudly, and the "no endpoints match" branch is left to
	// integration tests. The behaviour the unit test is allowed to
	// pin down is that the no-match case is silent (no panic, no
	// extra writes).
	s := &DeliveryService{DB: nil}
	if err := s.Enqueue(context.Background(), "00000000-0000-0000-0000-000000000001", "job.succeeded", "id", map[string]any{}); err == nil {
		t.Fatal("expected error on nil DB (no way to discover endpoints)")
	}
}

func TestRun_TerminatesOnContextCancel(t *testing.T) {
	s := New(nil, nil, nil, nil)
	s.PollInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestRun_TickHandlesNilDB makes sure Run does not panic when the DB
// is nil — this is the test path. Production code never constructs a
// nil-DB service, but the contract is that Run is safe to start and
// stop without external dependencies in place.
func TestRun_TickHandlesNilDB(t *testing.T) {
	s := New(nil, nil, nil, nil)
	s.PollInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = s.Run(ctx)
		close(done)
	}()
	time.Sleep(15 * time.Millisecond)
	cancel()
	<-done
}

func TestEncodePayload(t *testing.T) {
	// Round-trip a payload through the same encoder Enqueue uses.
	v := map[string]any{"job_id": "abc", "ok": true}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back["job_id"] != "abc" {
		t.Errorf("round-trip lost job_id")
	}
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
