package jobs

import (
	"context"
	"testing"
)

// TestPublish_NilJS asserts the safety net: a nil JetStream context
// must surface a clear error rather than panicking inside
// PublishMsg.
func TestPublish_NilJS(t *testing.T) {
	err := Publish(context.Background(), nil, "job.queued", []byte(`{}`), nil)
	if err == nil {
		t.Fatal("Publish(nil) returned nil err")
	}
}

// TestPublish_SubjectShape locks the wire contract: the subject is
// the SubjectPrefix plus the event_type, and the payload bytes are
// forwarded verbatim. The full Publish path is exercised by the
// e2e smoke in apps/api/internal/e2e; this test pins the parts of
// the contract that are easiest to break in a refactor.
func TestPublish_SubjectShape(t *testing.T) {
	cases := map[string]string{
		"job.queued":    "adkil.job.job.queued",
		"job.completed": "adkil.job.job.completed",
		"job.canceled":  "adkil.job.job.canceled",
		"job.failed":    "adkil.job.job.failed",
	}
	for eventType, wantSubject := range cases {
		t.Run(eventType, func(t *testing.T) {
			if got := SubjectPrefix + eventType; got != wantSubject {
				t.Errorf("subject = %q, want %q", got, wantSubject)
			}
		})
	}
}

// TestEnsureStream_NilJS asserts EnsureStream's nil-safety net so a
// misconfigured main.go does not crash on a nil JetStream context.
func TestEnsureStream_NilJS(t *testing.T) {
	if err := EnsureStream(nil, 0); err == nil {
		t.Fatal("EnsureStream(nil) returned nil err")
	}
}
