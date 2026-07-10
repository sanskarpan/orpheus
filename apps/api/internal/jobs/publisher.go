// Package jobs owns the JetStream stream and publish helpers used
// by the outbox publisher to fan out job-lifecycle events to
// workers. The stream and the publish path are kept here so the rest
// of the API only needs to import a single thin wrapper instead of
// the full nats.go/jetstream surface.
//
// Contract with the Python worker (apps/workers/orpheus_workers/):
//
//	stream    = ORPHEUS_JOBS
//	subjects  = adkil.job.>
//	subject   = adkil.<event_type>  (e.g. "adkil.job.queued")
//	payload   = JSON: {"event_id", "event_type", "org_id",
//	                    "aggregate_id", "payload", "headers"}
package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	StreamName           = "ORPHEUS_JOBS"
	SubjectPrefix        = "adkil.job."
	DefaultRetentionDays = 7
	DefaultReplicas      = 1
)

// EnsureStream creates the ORPHEUS_JOBS stream when it does not
// already exist. A no-op return on duplicates keeps the call
// idempotent so main.go can call it on every boot.
func EnsureStream(js jetstream.JetStream, retentionDays int) error {
	if js == nil {
		return fmt.Errorf("jobs.ensure_stream: nil jetstream context")
	}
	if retentionDays <= 0 {
		retentionDays = DefaultRetentionDays
	}
	_, err := js.CreateStream(context.Background(), jetstream.StreamConfig{
		Name:      StreamName,
		Subjects:  []string{SubjectPrefix + ">"},
		Retention: jetstream.LimitsPolicy,
		MaxAge:    time.Duration(retentionDays) * 24 * time.Hour,
		Storage:   jetstream.FileStorage,
		Replicas:  DefaultReplicas,
	})
	if err != nil && !errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
		return fmt.Errorf("jobs.ensure_stream: %w", err)
	}
	return nil
}

// Publish writes a message to the ORPHEUS_JOBS stream. The subject
// is "adkil.job.<event_type>" (e.g. "adkil.job.queued"); the payload
// is forwarded verbatim — the outbox row's `payload` column is
// already a JSON byte slice. headers is unused for now; the Python
// worker pulls what it needs from the JSON body. Kept in the
// signature so a future caller (trace ids, idempotency keys) can
// wire NATS headers without changing the call site.
func Publish(ctx context.Context, js jetstream.JetStream, eventType string, payload []byte, headers map[string]string) error {
	if js == nil {
		return fmt.Errorf("jobs.publish: nil jetstream context")
	}
	subj := SubjectPrefix + eventType
	msg := &nats.Msg{
		Subject: subj,
		Data:    payload,
	}
	if len(headers) > 0 {
		msg.Header = nats.Header{}
		for k, v := range headers {
			msg.Header.Set(k, v)
		}
	}
	if _, err := js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("jobs.publish: %w", err)
	}
	return nil
}
