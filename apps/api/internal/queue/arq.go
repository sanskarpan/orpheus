// Package queue bridges the Go API's outbox events into the Python
// worker plane's queue (Arq on Redis). The flow is:
//
//	handler -> outbox row (in same tx as business write)
//	outbox publisher -> NATS (adkil.<event_type>)
//	ArqEnqueuer -> Redis (arq:result:queue list)
//	Python arq worker -> dispatch_job (Phase 2; routes to extract-metadata etc.)
package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
)

const (
	arqQueueKey         = "arq:result:queue"
	arqFunctionDispatch = "dispatch_job"
	noopJobNatsSubj     = "adkil.job.queued"
)

type arqJob struct {
	TaskID      string         `json:"task_id"`
	Function    string         `json:"function"`
	Args        []any          `json:"args"`
	Kwargs      map[string]any `json:"kwargs"`
	EnqueueTime int64          `json:"enqueue_time"`
}

// redisWriter is the subset of the go-redis client surface that the
// enqueuer uses. A small interface keeps the production code focused
// (one method, one responsibility) and gives tests a stub seam
// without pulling in a mock library.
type redisWriter interface {
	LPush(ctx context.Context, key string, values ...any) error
}

// redisLPush adapts *redis.Client to redisWriter by collapsing the
// *IntCmd result into an error. It is set on the enqueuer at
// construction time in main.go; tests can substitute a stub.
func redisLPush(c *redis.Client) redisWriter {
	if c == nil {
		return nil
	}
	return redisClientLPush{c: c}
}

type redisClientLPush struct{ c *redis.Client }

func (r redisClientLPush) LPush(ctx context.Context, key string, values ...any) error {
	return r.c.LPush(ctx, key, values...).Err()
}

// ArqEnqueuer subscribes to NATS and writes Arq-shaped jobs to
// Redis. It is the bridge between the Go API's outbox events and the
// Python worker plane's queue (arq on Redis).
//
// In Phase 2 the only event type this cares about is
// `adkil.job.queued`. The enqueued function is `dispatch_job`,
// which looks up the job's processor from the row and routes to the
// right processor module (extract-metadata, transcribe, etc.).
type ArqEnqueuer struct {
	NC     *nats.Conn
	Writer redisWriter
	Logger *slog.Logger
}

// NewArqEnqueuer constructs an ArqEnqueuer with the supplied
// dependencies. rdb may be nil; the worker logs a warning and
// returns without subscribing in that case (dev-mode fallback).
// logger may be nil; the default slog logger is used.
func NewArqEnqueuer(nc *nats.Conn, rdb *redis.Client, logger *slog.Logger) *ArqEnqueuer {
	if logger == nil {
		logger = slog.Default()
	}
	return &ArqEnqueuer{NC: nc, Writer: redisLPush(rdb), Logger: logger}
}

// Run subscribes to noopJobNatsSubj and translates each message
// into an arq job in Redis. It blocks until ctx is cancelled and
// returns nil on a clean shutdown.
//
// When Writer is nil (i.e. no Redis client was wired) the worker
// logs a warning and returns without subscribing. NATS being nil is
// a no-op for the same reason.
func (a *ArqEnqueuer) Run(ctx context.Context) error {
	if a.NC == nil {
		a.Logger.Warn("queue.arq.no_nats")
		return nil
	}
	if a.Writer == nil {
		a.Logger.Warn("queue.arq.no_redis")
		<-ctx.Done()
		return nil
	}

	sub, err := a.NC.QueueSubscribe(noopJobNatsSubj, "arq-enqueuer", a.handle)
	if err != nil {
		return fmt.Errorf("queue.arq.subscribe: %w", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	a.Logger.Info("queue.arq.started", "subject", noopJobNatsSubj, "redis_key", arqQueueKey)
	<-ctx.Done()
	a.Logger.Info("queue.arq.stopped")
	return nil
}

// handle decodes the outbox payload, builds an arq job, and LPUSHes
// it. The two error modes are distinct:
//
//   - Malformed payload (bad JSON, missing job_id): log + ack. The
//     outbox row will not be re-published, but redelivery would only
//     keep failing. A bad message is durable in the outbox table
//     already; no need to loop on it.
//   - Writer error (LPush failed): log + nack. With JetStream the
//     message is redelivered. With plain NATS, Nak is a no-op and
//     the message is lost; the outbox row is already published_at
//     set, so the job is dropped on the floor. Redis being down is
//     the operator's problem; we log loudly so they notice.
func (a *ArqEnqueuer) handle(msg *nats.Msg) {
	var payload map[string]any
	if err := json.Unmarshal(msg.Data, &payload); err != nil {
		a.Logger.Warn("queue.arq.malformed_payload",
			"subject", msg.Subject,
			"err", err,
		)
		_ = msg.Ack()
		return
	}
	jobID, ok := payload["job_id"].(string)
	if !ok || jobID == "" {
		a.Logger.Warn("queue.arq.missing_job_id",
			"subject", msg.Subject,
		)
		_ = msg.Ack()
		return
	}

	job := arqJob{
		TaskID:      uuid.NewString(),
		Function:    arqFunctionDispatch,
		Args:        []any{jobID},
		Kwargs:      map[string]any{},
		EnqueueTime: time.Now().Unix(),
	}
	blob, err := json.Marshal(job)
	if err != nil {
		a.Logger.Error("queue.arq.marshal_failed", "err", err)
		_ = msg.Ack()
		return
	}

	if err := a.Writer.LPush(context.Background(), arqQueueKey, blob); err != nil {
		a.Logger.Error("queue.arq.lpush_failed",
			"err", err,
			"job_id", jobID,
		)
		_ = msg.Nak()
		return
	}
	_ = msg.Ack()
}
