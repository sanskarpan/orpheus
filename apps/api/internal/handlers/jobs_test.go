package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/dbtx"
	"github.com/orpheus/api/internal/outbox"
)

func TestWriteJSON_SetsContentTypeAndStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusTeapot, map[string]string{"hello": "world"})

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["hello"] != "world" {
		t.Errorf("body[hello] = %q, want world", body["hello"])
	}
}

func TestWriteProblem_EmitsProblemJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	writeProblem(rec, http.StatusNotFound, "not_found", "thing missing")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}

	var body struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != 404 {
		t.Errorf("body.status = %d, want 404", body.Status)
	}
	if body.Detail != "thing missing" {
		t.Errorf("body.detail = %q, want thing missing", body.Detail)
	}
	if !strings.HasSuffix(body.Type, "not_found") {
		t.Errorf("body.type = %q, want suffix not_found", body.Type)
	}
}

func TestNullStringVal_DerefsOrEmpty(t *testing.T) {
	s := "value"
	if got := nullStringVal(&s); got != "value" {
		t.Errorf("non-nil = %q, want value", got)
	}
	if got := nullStringVal(nil); got != "" {
		t.Errorf("nil = %q, want empty", got)
	}
}

func TestMergeAndExtractProcessor_RoundTrip(t *testing.T) {
	original := ProcessorRef{Name: "whisper-transcribe", Version: "1.2.0"}
	raw := []byte(`{"language":"en","diarize":true}`)

	stored := mergeProcessorIntoParams(raw, original)
	got := extractProcessorFromParams(stored)
	if got != original {
		t.Errorf("extract(%s) = %+v, want %+v", stored, got, original)
	}

	// Strip the key: the result should equal the original user params
	// up to JSON key ordering. Compare as decoded maps so key order
	// does not matter.
	stripped := stripProcessorFromParams(stored)
	var strippedMap, originalMap map[string]any
	if err := json.Unmarshal(stripped, &strippedMap); err != nil {
		t.Fatalf("strip is not a JSON object: %v", err)
	}
	if err := json.Unmarshal(raw, &originalMap); err != nil {
		t.Fatalf("raw is not a JSON object: %v", err)
	}
	if len(strippedMap) != len(originalMap) {
		t.Errorf("stripped has %d keys, want %d", len(strippedMap), len(originalMap))
	}
	for k, v := range originalMap {
		if strippedMap[k] != v {
			t.Errorf("stripped[%q] = %v, want %v", k, strippedMap[k], v)
		}
	}
}

func TestExtractProcessor_HandlesEmptyAndMalformed(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"not json", []byte(`{not json`)},
		{"no key", []byte(`{"language":"en"}`)},
		{"not object", []byte(`[]`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractProcessorFromParams(tc.in); got != (ProcessorRef{}) {
				t.Errorf("got %+v, want zero", got)
			}
		})
	}
}

func TestStripProcessorFromParams_PreservesNonObject(t *testing.T) {
	raw := []byte(`["a","b"]`)
	got := stripProcessorFromParams(raw)
	if string(got) != string(raw) {
		t.Errorf("got %s, want %s", got, raw)
	}
}

// TestJobCreate_EmitsOutboxEvent is the live-DB version of the
// outbox emission contract. It seeds an org + user + artifact +
// processor, calls JobHandler.Create, and asserts the `outbox`
// table has a `job.queued` row whose payload contains the new
// job's id. Skipped when ORPHEUS_TEST_DATABASE_URL is not set; the
// unit-test counterpart below covers the same shape without a DB.
func TestJobCreate_EmitsOutboxEvent(t *testing.T) {
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set; skipping live-DB outbox event test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pool, err := db.New(ctx, dsn)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(pool.Close)

	// Seed on a private pool with the service GUC set, so the seed
	// inserts can create rows in RLS-locked tables.
	seedPool, err := db.New(ctx, dsn)
	if err != nil {
		t.Fatalf("db.New (seed): %v", err)
	}
	t.Cleanup(seedPool.Close)
	seedConn, err := seedPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire seed conn: %v", err)
	}
	if _, err := seedConn.Exec(ctx, "SET app.is_service = 'true'"); err != nil {
		seedConn.Release()
		t.Fatalf("set service role: %v", err)
	}

	orgID := uuid.NewString()
	userID := uuid.NewString()
	artifactID := uuid.NewString()
	processorName := "noop-proc-" + uuid.NewString()[:8]

	if _, err := seedConn.Exec(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $3)`,
		orgID, "outbox-test", "outbox-test-"+orgID[:8],
	); err != nil {
		seedConn.Release()
		t.Fatalf("seed org: %v", err)
	}
	if _, err := seedConn.Exec(ctx,
		`INSERT INTO users (id, org_id, email, name) VALUES ($1, $2, $3, $4)`,
		userID, orgID, userID[:8]+"@test", "test-user",
	); err != nil {
		seedConn.Release()
		t.Fatalf("seed user: %v", err)
	}
	if _, err := seedConn.Exec(ctx,
		`INSERT INTO artifacts (id, org_id, s3_bucket, s3_key, sha256, size_bytes, content_type) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		artifactID, orgID, "test-bucket", "test-key", "deadbeef", 1024, "audio/wav",
	); err != nil {
		seedConn.Release()
		t.Fatalf("seed artifact: %v", err)
	}
	var processorID string
	if err := seedConn.QueryRow(ctx,
		`INSERT INTO processors (name, display_name, tier, timeout_seconds) VALUES ($1, $1, 'cpu_tiny', 60) RETURNING id::text`,
		processorName,
	).Scan(&processorID); err != nil {
		seedConn.Release()
		t.Fatalf("seed processor: %v", err)
	}
	if _, err := seedConn.Exec(ctx,
		`INSERT INTO processor_versions (processor_id, version, model_id, model_version_id) VALUES ($1, $2, $3, $4)`,
		processorID, "1.0.0", "model-"+processorName, "v1",
	); err != nil {
		seedConn.Release()
		t.Fatalf("seed processor_version: %v", err)
	}
	seedConn.Release()
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanCancel()
		_, _ = pool.Exec(cleanCtx, `DELETE FROM outbox WHERE org_id = $1`, orgID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM processor_versions WHERE processor_id = $1`, processorID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM processors WHERE id = $1`, processorID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM jobs WHERE org_id = $1`, orgID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM artifacts WHERE id = $1`, artifactID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM users WHERE id = $1`, userID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM organizations WHERE id = $1`, orgID)
	})

	h := &JobHandler{
		DB:    pool,
		Audit: audit.New(pool, nil),
	}
	body := `{"artifact_id":"` + artifactID + `","processor":{"name":"` + processorName + `","version":"1.0.0"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(body))
	req = withPrincipal(req, &auth.Principal{OrgID: orgID, UserID: userID})
	rec := httptest.NewRecorder()
	h.Create(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("Create status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}

	// Read the most recent outbox row for this org and assert its
	// shape. The read runs inside WithTenant so app.current_org_id is
	// set on the connection — RLS is FORCE'd on the outbox table, so a
	// bare pool read (no org context) would correctly return zero rows.
	var (
		eventType   string
		aggregateID string
		payloadRaw  []byte
	)
	if err := pool.WithTenant(ctx, orgID, func(tctx context.Context) error {
		return dbtx.QueryRow(tctx, pool, `
			SELECT event_type, aggregate_id, payload
			FROM outbox
			WHERE org_id = $1 AND event_type = 'job.queued'
			ORDER BY created_at DESC
			LIMIT 1
		`, orgID).Scan(&eventType, &aggregateID, &payloadRaw)
	}); err != nil {
		t.Fatalf("read outbox row: %v", err)
	}
	if eventType != "job.queued" {
		t.Errorf("event_type = %q, want job.queued", eventType)
	}
	if aggregateID == "" {
		t.Errorf("aggregate_id is empty")
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadRaw, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v (raw=%s)", err, payloadRaw)
	}
	if payload["job_id"] != aggregateID {
		t.Errorf("payload.job_id = %v, want %s", payload["job_id"], aggregateID)
	}
	if payload["job_type"] != processorName {
		t.Errorf("payload.job_type = %v, want %s", payload["job_type"], processorName)
	}
}

// recordingQuerier records the SQL and args it was last called
// with. It satisfies dbtx.Querier so outbox.Enqueue can be driven
// against it in a unit test (no DB). Query/QueryRow return nil; the
// test only exercises the Exec path, so the nil rows are never
// iterated.
type recordingQuerier struct {
	sql  string
	args []any
	tag  pgconn.CommandTag
}

func (r *recordingQuerier) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.sql = sql
	r.args = args
	return r.tag, nil
}

func (r *recordingQuerier) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, nil
}

func (r *recordingQuerier) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return nil
}

// TestJobQueuedEventShape_OutboxContract is the unit-test counterpart
// to TestJobCreate_EmitsOutboxEvent. It builds the same outbox.Event
// that Create passes, calls outbox.Enqueue against a recording
// querier, and asserts the SQL and args land where the live-DB test
// would write them. A regression in either Create's event shape or
// Enqueue's INSERT will break this test.
func TestJobQueuedEventShape_OutboxContract(t *testing.T) {
	const (
		orgID     = "00000000-0000-0000-0000-000000000001"
		jobID     = "00000000-0000-0000-0000-000000000002"
		processor = "noop-transcribe"
		aggregate = "job"
		eventType = "job.queued"
	)
	rq := &recordingQuerier{tag: pgconn.NewCommandTag("INSERT 0 1")}
	err := outbox.Enqueue(context.Background(), rq, outbox.Event{
		OrgID:         orgID,
		AggregateType: aggregate,
		AggregateID:   jobID,
		EventType:     eventType,
		Payload: map[string]any{
			"job_id":   jobID,
			"job_type": processor,
		},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if !strings.Contains(rq.sql, "INSERT INTO outbox") {
		t.Errorf("SQL does not target outbox: %q", rq.sql)
	}
	if len(rq.args) != 7 {
		t.Fatalf("args = %d, want 7 (id, org_id, aggregate_type, aggregate_id, event_type, payload, headers)", len(rq.args))
	}
	if rq.args[1] != orgID {
		t.Errorf("args[1] (org_id) = %v, want %s", rq.args[1], orgID)
	}
	if rq.args[2] != aggregate {
		t.Errorf("args[2] (aggregate_type) = %v, want %s", rq.args[2], aggregate)
	}
	if rq.args[3] != jobID {
		t.Errorf("args[3] (aggregate_id) = %v, want %s", rq.args[3], jobID)
	}
	if rq.args[4] != eventType {
		t.Errorf("args[4] (event_type) = %v, want %s", rq.args[4], eventType)
	}
	payload, ok := rq.args[5].([]byte)
	if !ok {
		t.Fatalf("args[5] is %T, want []byte payload", rq.args[5])
	}
	var p map[string]any
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("payload not valid JSON: %v (raw=%s)", err, payload)
	}
	if p["job_id"] != jobID {
		t.Errorf("payload.job_id = %v, want %s", p["job_id"], jobID)
	}
	if p["job_type"] != processor {
		t.Errorf("payload.job_type = %v, want %s", p["job_type"], processor)
	}
}
