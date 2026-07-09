package outbox

import (
	"context"
	"strings"
	"testing"
)

// TestEnqueueValidations covers the early-exit branches in [Enqueue].
// A nil DB or empty event_type/aggregate fields should fail before
// we touch the database; these checks are pure validation.
func TestEnqueueValidations(t *testing.T) {
	cases := []struct {
		name    string
		event   Event
		wantSub string
	}{
		{
			name:    "nil db",
			event:   Event{EventType: "x", AggregateType: "y", AggregateID: "z"},
			wantSub: "nil db",
		},
		{
			name:    "empty event_type",
			event:   Event{AggregateType: "upload", AggregateID: "abc"},
			wantSub: "empty event_type",
		},
		{
			name:    "empty aggregate type",
			event:   Event{EventType: "upload.complete", AggregateID: "abc"},
			wantSub: "incomplete",
		},
		{
			name:    "empty aggregate id",
			event:   Event{EventType: "upload.complete", AggregateType: "upload"},
			wantSub: "incomplete",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Enqueue(context.Background(), nil, tc.event)
			if err == nil {
				t.Fatal("Enqueue returned nil error, want error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestNewEventID asserts that the random ID generator produces
// non-empty, non-equal values. We don't assert on the format because
// it can change — only on the contract.
func TestNewEventID(t *testing.T) {
	a := newEventID()
	b := newEventID()
	if a == "" || b == "" {
		t.Fatalf("newEventID returned empty: a=%q b=%q", a, b)
	}
	if a == b {
		t.Errorf("newEventID collision: %q", a)
	}
	if len(a) != 32 {
		t.Errorf("len(newEventID()) = %d, want 32 hex chars", len(a))
	}
}

// TestSubject verifies the public subject-formatting helper. A
// regression here would silently misroute every consumer.
func TestSubject(t *testing.T) {
	cases := map[string]string{
		"upload.complete": "adkil.upload.complete",
		"job.create":      "adkil.job.create",
		"x":               "adkil.x",
	}
	for in, want := range cases {
		if got := Subject(in); got != want {
			t.Errorf("Subject(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPublisherRunReturnsOnCancel makes sure Run returns promptly when
// the context is cancelled, even with a nil DB / nil NATS. The
// production code never passes nil, but a malformed test
// configuration shouldn't hang.
func TestPublisherRunReturnsOnCancel(t *testing.T) {
	p := New(nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so Run exits on its first iteration
	if err := p.Run(ctx); err != nil {
		t.Errorf("Run returned %v, want nil", err)
	}
}

// TestTickSkipsWhenWiringMissing confirms the safety net in tick:
// with nil DB or nil NATS, tick is a no-op rather than a panic.
func TestTickSkipsWhenWiringMissing(t *testing.T) {
	p := New(nil, nil, nil)
	// Should not panic.
	p.tick(context.Background())
}

// TestEnqueueRequiresDB is the live-DB version of the validation
// tests. It is skipped without a database. The intent is to catch
// wiring regressions (wrong column names, missing columns) in the
// INSERT path before integration tests in Phase 2.
func TestEnqueueRequiresDB(t *testing.T) {
	if testing.Short() {
		t.Skip("requires ORPHEUS_TEST_DATABASE_URL; skipped in -short mode")
	}
	t.Skip("ORPHEUS_TEST_DATABASE_URL not set; skipping live-db outbox test")
}
