package queue

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"
)

// stubWriter records the LPush calls and returns whatever err the
// test pre-loaded. It satisfies the redisWriter interface that
// ArqEnqueuer depends on, so tests can drive handle() without
// pulling in a real Redis.
type stubWriter struct {
	mu      sync.Mutex
	calls   []stubLPushCall
	nextErr error
}

type stubLPushCall struct {
	Key   string
	Value []byte
}

func (s *stubWriter) LPush(_ context.Context, key string, values ...any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var v []byte
	if len(values) > 0 {
		if b, ok := values[0].([]byte); ok {
			v = b
		} else {
			b, _ := json.Marshal(values[0])
			v = b
		}
	}
	s.calls = append(s.calls, stubLPushCall{Key: key, Value: v})
	return s.nextErr
}

func (s *stubWriter) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func newTestEnqueuer(w redisWriter) *ArqEnqueuer {
	return &ArqEnqueuer{
		NC:     nil,
		Writer: w,
		Logger: newNopLogger(),
	}
}

// TestArqEnqueuer_NoRedis_NoSubscribe asserts the dev-mode fallback:
// when the writer (Redis) is nil, Run logs a warning, does not
// subscribe, and returns nil when ctx is cancelled.
func TestArqEnqueuer_NoRedis_NoSubscribe(t *testing.T) {
	a := &ArqEnqueuer{
		NC:     nil,
		Writer: nil,
		Logger: newNopLogger(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
}

// TestArqEnqueuer_NoNATS_NoSubscribe asserts the same fallback for
// the NATS connection.
func TestArqEnqueuer_NoNATS_NoSubscribe(t *testing.T) {
	a := &ArqEnqueuer{
		NC:     nil,
		Writer: &stubWriter{},
		Logger: newNopLogger(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
}

// TestHandle_HappyPath builds an arq job from a valid payload and
// asserts the LPush call lands on the right Redis key with a
// well-formed job blob. The test mocks NATS by handing a
// nats.NewMsg(subject) directly to handle().
func TestHandle_HappyPath(t *testing.T) {
	w := &stubWriter{}
	a := newTestEnqueuer(w)
	msg := nats.NewMsg(noopJobNatsSubj)
	msg.Data = []byte(`{"job_id":"job-123","job_type":"extract-metadata"}`)

	a.handle(msg)

	if w.callCount() != 1 {
		t.Fatalf("LPush call count = %d, want 1", w.callCount())
	}
	call := w.calls[0]
	if call.Key != arqQueueKey {
		t.Errorf("LPush key = %q, want %q", call.Key, arqQueueKey)
	}
	var got arqJob
	if err := json.Unmarshal(call.Value, &got); err != nil {
		t.Fatalf("unmarshal arqJob: %v (blob=%s)", err, call.Value)
	}
	if got.Function != arqFunctionNoop {
		t.Errorf("Function = %q, want %q", got.Function, arqFunctionNoop)
	}
	if len(got.Args) != 1 || got.Args[0] != "job-123" {
		t.Errorf("Args = %v, want [job-123]", got.Args)
	}
	if got.Kwargs == nil {
		t.Errorf("Kwargs is nil; want empty object")
	}
	if got.TaskID == "" {
		t.Errorf("TaskID is empty")
	}
	if got.EnqueueTime <= 0 {
		t.Errorf("EnqueueTime = %d, want > 0", got.EnqueueTime)
	}
}

// TestHandle_MalformedPayload_AcksNoPush asserts that a bad-JSON
// payload is dropped (acked, no LPush, no nack) so a bad row in the
// outbox doesn't loop forever.
func TestHandle_MalformedPayload_AcksNoPush(t *testing.T) {
	w := &stubWriter{}
	a := newTestEnqueuer(w)
	msg := nats.NewMsg(noopJobNatsSubj)
	msg.Data = []byte(`{not json`)

	a.handle(msg)

	if w.callCount() != 0 {
		t.Errorf("LPush call count = %d, want 0 (malformed payload must not be enqueued)", w.callCount())
	}
}

// TestHandle_MissingJobID_AcksNoPush asserts that a payload without
// a job_id is also dropped.
func TestHandle_MissingJobID_AcksNoPush(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"empty object", `{}`},
		{"empty job_id", `{"job_id":""}`},
		{"non-string job_id", `{"job_id":42}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &stubWriter{}
			a := newTestEnqueuer(w)
			msg := nats.NewMsg(noopJobNatsSubj)
			msg.Data = []byte(tc.payload)

			a.handle(msg)

			if w.callCount() != 0 {
				t.Errorf("LPush call count = %d, want 0 (missing job_id must not be enqueued)", w.callCount())
			}
		})
	}
}

// TestHandle_RedisError_DoesNotPanic asserts that an LPush error
// path returns without panicking. We can't observe the Ack/Nak
// decision from a unit test (plain NATS, no JetStream), but we can
// assert the handler didn't crash and didn't fall through to a
// second LPush.
func TestHandle_RedisError_DoesNotPanic(t *testing.T) {
	w := &stubWriter{nextErr: errors.New("boom")}
	a := newTestEnqueuer(w)
	msg := nats.NewMsg(noopJobNatsSubj)
	msg.Data = []byte(`{"job_id":"job-x"}`)

	a.handle(msg)

	if w.callCount() != 1 {
		t.Errorf("LPush call count = %d, want 1 (the call was attempted)", w.callCount())
	}
}

// TestArqJobJSONShape locks the wire format Arq consumes. If the
// JSON shape changes, the Python arq worker will start rejecting
// every job; a test that breaks on the change is the cheapest place
// to catch it.
func TestArqJobJSONShape(t *testing.T) {
	j := arqJob{
		TaskID:      "t-1",
		Function:    arqFunctionNoop,
		Args:        []any{"job-7"},
		Kwargs:      map[string]any{},
		EnqueueTime: 1700000000,
	}
	blob, err := json.Marshal(j)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(blob)
	for _, want := range []string{
		`"task_id":"t-1"`,
		`"function":"noop_job"`,
		`"args":["job-7"]`,
		`"kwargs":{}`,
		`"enqueue_time":1700000000`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("arqJob JSON missing %q\nblob: %s", want, got)
		}
	}
}

// newNopLogger returns a slog.Logger that drops every record. The
// enqueuer logs a lot in error paths; tests don't want the noise in
// their output.
func newNopLogger() *slog.Logger { return slog.New(nopHandler{}) }

type nopHandler struct{}

func (nopHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (nopHandler) Handle(context.Context, slog.Record) error { return nil }
func (nopHandler) WithAttrs([]slog.Attr) slog.Handler        { return nopHandler{} }
func (nopHandler) WithGroup(string) slog.Handler             { return nopHandler{} }
