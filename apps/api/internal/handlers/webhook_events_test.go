package handlers

import "testing"

// TestAllowedEventsCoversEmittedEvents guards against the regression where the
// system emits an event that no webhook can subscribe to (so it never
// delivers). Every event type the API/worker actually enqueues MUST be an
// allowed subscription target. See the emitters in workers/worker.py and
// handlers/jobs.go / bundles.go.
func TestAllowedEventsCoversEmittedEvents(t *testing.T) {
	emitted := []string{
		"job.queued",      // handlers/jobs.go, workflows.go, bundles.go
		"job.completed",   // workers/worker.py, handlers/jobs.go (cache hit)
		"job.retry",       // workers/worker.py
		"job.dead_letter", // workers/worker.py
		"job.canceled",    // handlers/jobs.go
		"bundle.ready",    // workers/processors/export_bundle.py
		"bundle.failed",   // workers/processors/export_bundle.py
		"batch.completed", // internal/batching
	}
	for _, e := range emitted {
		if _, ok := allowedEvents[e]; !ok {
			t.Errorf("emitted event %q is not in allowedEvents — a subscription to it is rejected, so it can never deliver", e)
		}
	}
}
