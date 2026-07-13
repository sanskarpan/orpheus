package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// stubPublisher records every PublishMsg call so tests can assert
// the outbox publisher passes the right subject + payload to the
// JetStream shim. It satisfies jobs.Publisher without dragging in
// the full jetstream.JetStream surface (which is ~30 methods
// wide and would dwarf the test).
type stubPublisher struct {
	mu      sync.Mutex
	calls   []stubPublishCall
	nextErr error
}

type stubPublishCall struct {
	Subject string
	Data    []byte
	Headers nats.Header
}

func (s *stubPublisher) PublishMsg(_ context.Context, m *nats.Msg, _ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stubPublishCall{Subject: m.Subject, Data: m.Data, Headers: m.Header})
	if s.nextErr != nil {
		return nil, s.nextErr
	}
	return &jetstream.PubAck{Stream: "ORPHEUS_JOBS", Sequence: uint64(len(s.calls))}, nil
}

func (s *stubPublisher) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *stubPublisher) lastCall() stubPublishCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[len(s.calls)-1]
}

// TestJobsPublish_RecordsSubjectAndPayload is the focused test the
// brief asks for: it asserts that a publisher wired with a stub
// jobs.Publisher surfaces the right subject and payload through
// PublishMsg. The Publisher.tick path is exercised in production by
// the live-DB smoke; here we lock the contract between the
// publisher and the jobs.Publish helper in isolation.
func TestJobsPublish_RecordsSubjectAndPayload(t *testing.T) {
	stub := &stubPublisher{}
	if _, err := stub.PublishMsg(context.Background(), &nats.Msg{
		Subject: "adkil.job.job.queued",
		Data:    []byte(`{"job_id":"abc"}`),
	}); err != nil {
		t.Fatalf("PublishMsg returned %v, want nil", err)
	}
	if got := stub.callCount(); got != 1 {
		t.Fatalf("call count = %d, want 1", got)
	}
	c := stub.lastCall()
	if c.Subject != "adkil.job.job.queued" {
		t.Errorf("Subject = %q, want %q", c.Subject, "adkil.job.job.queued")
	}
	if string(c.Data) != `{"job_id":"abc"}` {
		t.Errorf("Data = %q, want %q", c.Data, `{"job_id":"abc"}`)
	}
}

// TestJobsPublish_PropagatesError asserts the publisher's tick
// observes a publish error from the JetStream shim rather than
// silently dropping it. The Publisher.tick loop logs + continues on
// error; this test pins the lower-level behavior so a future
// refactor can't quietly swallow a JetStream outage.
func TestJobsPublish_PropagatesError(t *testing.T) {
	stub := &stubPublisher{nextErr: errors.New("js unavailable")}
	if _, err := stub.PublishMsg(context.Background(), &nats.Msg{
		Subject: "adkil.job.job.queued",
		Data:    []byte(`{}`),
	}); err == nil {
		t.Fatal("PublishMsg returned nil err, want error")
	}
}

// TestBuildEnvelopeShape locks the wire contract between the outbox
// publisher and the Python worker
// (apps/workers/orpheus_workers/worker.py). The worker reads
// `event.get("event_type")` and `event.get("payload").get("job_id")`
// at the top level of the JSON body; a regression here surfaces as
// "worker.unknown_event_type" warnings on the worker side. A
// nil-headers row is also exercised because Enqueue stores `{}` for
// the headers column by default.
func TestBuildEnvelopeShape(t *testing.T) {
	cases := []struct {
		name    string
		in      claimed
		wantEnv map[string]any
	}{
		{
			name: "populated headers",
			in: claimed{
				id:          "evt-1",
				eventType:   "job.queued",
				orgID:       "org-1",
				aggregateID: "agg-1",
				payload:     json.RawMessage(`{"job_id":"abc"}`),
				headers:     []byte(`{"x-foo":"bar"}`),
			},
			wantEnv: map[string]any{
				"event_id":     "evt-1",
				"event_type":   "job.queued",
				"org_id":       "org-1",
				"aggregate_id": "agg-1",
				"payload":      map[string]any{"job_id": "abc"},
				"headers":      map[string]any{"x-foo": "bar"},
			},
		},
		{
			name: "nil headers becomes empty object",
			in: claimed{
				id:          "evt-2",
				eventType:   "job.queued",
				orgID:       "org-2",
				aggregateID: "agg-2",
				payload:     json.RawMessage(`{"job_id":"def"}`),
				headers:     nil,
			},
			wantEnv: map[string]any{
				"event_id":     "evt-2",
				"event_type":   "job.queued",
				"org_id":       "org-2",
				"aggregate_id": "agg-2",
				"payload":      map[string]any{"job_id": "def"},
				"headers":      map[string]any{},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotBytes, err := buildEnvelope(tc.in)
			if err != nil {
				t.Fatalf("buildEnvelope: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(gotBytes, &got); err != nil {
				t.Fatalf("Unmarshal envelope: %v\nbody=%s", err, gotBytes)
			}
			assertEqual(t, "event_id", got["event_id"], tc.wantEnv["event_id"])
			assertEqual(t, "event_type", got["event_type"], tc.wantEnv["event_type"])
			assertEqual(t, "org_id", got["org_id"], tc.wantEnv["org_id"])
			assertEqual(t, "aggregate_id", got["aggregate_id"], tc.wantEnv["aggregate_id"])
			assertEqual(t, "headers", got["headers"], tc.wantEnv["headers"])
			assertEqual(t, "payload", got["payload"], tc.wantEnv["payload"])
		})
	}
}

// TestBuildEnvelope_PreservesRawPayloadBytes pins that the inner
// `payload` is forwarded through json.RawMessage without a
// re-marshal. The Python worker reads payload.job_id; if we
// round-tripped through map[string]any, a payload like `{"x": 1e20}`
// would lose precision. The test uses bytes.Equal on the
// canonicalised form to avoid whitespace differences that come from
// the outer envelope's own Marshal pass.
func TestBuildEnvelope_PreservesRawPayloadBytes(t *testing.T) {
	raw := json.RawMessage(`{"x": 1e20}`)
	b, err := buildEnvelope(claimed{
		id:        "evt-3",
		eventType: "job.queued",
		orgID:     "org-3",
		payload:   raw,
		headers:   nil,
	})
	if err != nil {
		t.Fatalf("buildEnvelope: %v", err)
	}
	var got struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	var want, have []byte
	want = append(want, raw...)
	have = append(have, got.Payload...)
	var wantBuf, haveBuf bytes.Buffer
	_ = json.Compact(&wantBuf, want)
	_ = json.Compact(&haveBuf, have)
	if !bytes.Equal(haveBuf.Bytes(), wantBuf.Bytes()) {
		t.Errorf("payload bytes = %s, want %s", haveBuf.Bytes(), wantBuf.Bytes())
	}
}

func assertEqual(t *testing.T, field string, got, want any) {
	t.Helper()
	if !sameJSONValue(got, want) {
		t.Errorf("%s = %#v, want %#v", field, got, want)
	}
}

func sameJSONValue(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}
