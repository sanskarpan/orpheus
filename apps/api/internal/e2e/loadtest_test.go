package e2e

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexedwards/argon2id"
	"github.com/google/uuid"

	"github.com/orpheus/api/internal/db"

	"github.com/orpheus/api/internal/e2e/setup"
)

// TestLoad_APIThroughput is an in-depth load test of the public API under
// sustained concurrency. It boots the real server against the live stack
// (Postgres/NATS/MinIO), authenticates with a real API key, and drives a
// realistic read/write mix (job list, artifact list, job create, job
// get) while collecting latency percentiles, throughput, and error rate.
//
// It is gated behind ORPHEUS_LOADTEST=1 (in addition to the e2e gates) so
// it never runs in the normal PR pipeline. Tunables (env):
//
//	ORPHEUS_LOAD_DURATION    (default 15s)
//	ORPHEUS_LOAD_CONCURRENCY (default 50)
//	ORPHEUS_LOAD_ARTIFACTS   (default 200)
//	ORPHEUS_LOAD_MAX_ERROR_RATE (default 0.01 → 1%)
//	ORPHEUS_LOAD_MAX_P99_MS  (default 1500)
func TestLoad_APIThroughput(t *testing.T) {
	if os.Getenv("ORPHEUS_LOADTEST") != "1" {
		t.Skip("set ORPHEUS_LOADTEST=1 to run the load test")
	}
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	natsURL := os.Getenv("ORPHEUS_TEST_NATS_URL")
	if dsn == "" || natsURL == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL / ORPHEUS_TEST_NATS_URL not set")
	}

	duration := envDuration("ORPHEUS_LOAD_DURATION", 15*time.Second)
	concurrency := envInt("ORPHEUS_LOAD_CONCURRENCY", 50)
	nArtifacts := envInt("ORPHEUS_LOAD_ARTIFACTS", 200)
	maxErrRate := envFloat("ORPHEUS_LOAD_MAX_ERROR_RATE", 0.01)
	maxP99 := time.Duration(envInt("ORPHEUS_LOAD_MAX_P99_MS", 1500)) * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), duration+120*time.Second)
	defer cancel()

	setup.RunMigrations(t, ctx, dsn)
	pool := setup.ServicePool(t, ctx, dsn)

	orgID := uuid.NewString()
	setup.SeedOrg(t, ctx, pool, orgID)
	// Full-access ("*") key so the load hits every endpoint (the shared
	// SeedAPIKey is intentionally scoped to jobs:* only).
	secret := seedFullAccessKey(t, ctx, pool, orgID)
	procName := "loadproc-" + orgID[:8]
	setup.SeedProcessor(t, ctx, pool, procName, "1.0.0")

	artifactIDs := make([]string, 0, nArtifacts)
	for i := 0; i < nArtifacts; i++ {
		id := setup.SeedArtifact(t, ctx, pool, orgID, "load-bucket", fmt.Sprintf("load/%s/%d", orgID, i), 1024)
		artifactIDs = append(artifactIDs, id)
	}
	t.Cleanup(func() { setup.CleanupOrgData(t, pool, orgID) })

	apiURL, shutdown := setup.StartAPI(t, ctx, pool, natsURL, 18090)
	defer shutdown()

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        concurrency * 2,
			MaxIdleConnsPerHost: concurrency * 2,
			MaxConnsPerHost:     concurrency * 2,
			IdleConnTimeout:     30 * time.Second,
		},
	}

	// Shared pool of created job ids so GET /v1/jobs/{id} hits real rows.
	var jobMu sync.Mutex
	var jobIDs []string
	addJob := func(id string) {
		jobMu.Lock()
		if len(jobIDs) < 5000 {
			jobIDs = append(jobIDs, id)
		}
		jobMu.Unlock()
	}
	randJob := func(seed int) string {
		jobMu.Lock()
		defer jobMu.Unlock()
		if len(jobIDs) == 0 {
			return ""
		}
		return jobIDs[seed%len(jobIDs)]
	}

	type sample struct {
		op     string
		d      time.Duration
		status int
		err    bool
	}
	perWorker := make([][]sample, concurrency)

	deadline := time.Now().Add(duration)
	var wg sync.WaitGroup
	var totalReqs atomic.Int64
	start := make(chan struct{})

	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			samples := make([]sample, 0, 4096)
			<-start
			n := wid
			for time.Now().Before(deadline) {
				n++
				op, req := buildOp(ctx, n, apiURL, secret, procName, artifactIDs, randJob(n))
				t0 := time.Now()
				resp, err := client.Do(req)
				d := time.Since(t0)
				s := sample{op: op, d: d}
				if err != nil {
					s.err = true
				} else {
					s.status = resp.StatusCode
					body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
					_ = resp.Body.Close()
					if op == "job.create" && resp.StatusCode == http.StatusAccepted {
						if id := extractJobID(body); id != "" {
							addJob(id)
						}
					}
					if resp.StatusCode >= 500 {
						s.err = true
					}
				}
				samples = append(samples, s)
				totalReqs.Add(1)
			}
			perWorker[wid] = samples
		}(w)
	}

	wallStart := time.Now()
	close(start)
	wg.Wait()
	wall := time.Since(wallStart)

	// Aggregate.
	all := make([]sample, 0, totalReqs.Load())
	byOp := map[string][]time.Duration{}
	var errCount int
	statusDist := map[int]int{}
	for _, ws := range perWorker {
		for _, s := range ws {
			all = append(all, s)
			byOp[s.op] = append(byOp[s.op], s.d)
			if s.err {
				errCount++
			}
			statusDist[s.status]++
		}
	}
	total := len(all)
	if total == 0 {
		t.Fatal("no requests issued")
	}
	overall := make([]time.Duration, 0, total)
	for _, s := range all {
		overall = append(overall, s.d)
	}

	rps := float64(total) / wall.Seconds()
	errRate := float64(errCount) / float64(total)

	t.Logf("═══ LOAD TEST REPORT ═══")
	t.Logf("duration=%s concurrency=%d artifacts=%d", duration, concurrency, nArtifacts)
	t.Logf("total requests=%d  wall=%s  throughput=%.0f req/s", total, wall.Round(time.Millisecond), rps)
	t.Logf("errors=%d (%.3f%%)", errCount, errRate*100)
	t.Logf("status distribution: %v", statusDist)
	logLatency(t, "OVERALL", overall)
	for _, op := range []string{"job.list", "artifact.list", "job.create", "job.get"} {
		if ds := byOp[op]; len(ds) > 0 {
			logLatency(t, op, ds)
		}
	}

	// Thresholds.
	if errRate > maxErrRate {
		t.Errorf("error rate %.3f%% exceeds max %.3f%% (status dist %v)", errRate*100, maxErrRate*100, statusDist)
	}
	if p := percentile(overall, 99); p > maxP99 {
		t.Errorf("p99 latency %s exceeds max %s", p.Round(time.Microsecond), maxP99)
	}
}

// seedFullAccessKey inserts an api_keys row with the "*" scope and
// returns the cleartext ak_live_ secret.
func seedFullAccessKey(t *testing.T, ctx context.Context, pool *db.DB, orgID string) string {
	t.Helper()
	buf := make([]byte, 32)
	if _, err := crand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	secret := "ak_live_" + base64.RawURLEncoding.EncodeToString(buf)
	hashed, err := argon2id.CreateHash(secret, argon2id.DefaultParams)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO api_keys (id, org_id, name, hashed_secret, prefix, scopes) VALUES ($1,$2,'load',$3,$4,$5)`,
		uuid.NewString(), orgID, hashed, secret[:9], []string{"*"},
	); err != nil {
		t.Fatalf("insert key: %v", err)
	}
	return secret
}

func buildOp(ctx context.Context, n int, apiURL, secret, procName string, artifacts []string, jobID string) (string, *http.Request) {
	mod := n % 20
	var op, method, url string
	var body []byte
	switch {
	case mod < 9: // 45% job list
		op, method, url = "job.list", http.MethodGet, apiURL+"/v1/jobs?limit=20"
	case mod < 14: // 25% artifact list
		op, method, url = "artifact.list", http.MethodGet, apiURL+"/v1/artifacts?limit=20"
	case mod < 18: // 20% job create (write path)
		op, method, url = "job.create", http.MethodPost, apiURL+"/v1/jobs"
		art := artifacts[n%len(artifacts)]
		body = []byte(fmt.Sprintf(`{"artifact_id":%q,"processor":{"name":%q,"version":"1.0.0"}}`, art, procName))
	default: // 10% job get (falls back to list if no jobs yet)
		if jobID != "" {
			op, method, url = "job.get", http.MethodGet, apiURL+"/v1/jobs/"+jobID
		} else {
			op, method, url = "job.list", http.MethodGet, apiURL+"/v1/jobs?limit=20"
		}
	}
	var r *http.Request
	if body != nil {
		r, _ = http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r, _ = http.NewRequestWithContext(ctx, method, url, nil)
	}
	r.Header.Set("X-API-Key", secret)
	return op, r
}

func extractJobID(body []byte) string {
	// naive: find "id":"..."
	const k = `"id":"`
	i := bytes.Index(body, []byte(k))
	if i < 0 {
		return ""
	}
	rest := body[i+len(k):]
	j := bytes.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return string(rest[:j])
}

func logLatency(t *testing.T, label string, ds []time.Duration) {
	t.Helper()
	t.Logf("  %-14s n=%-7d p50=%-9s p90=%-9s p99=%-9s max=%-9s",
		label, len(ds),
		percentile(ds, 50).Round(time.Microsecond),
		percentile(ds, 90).Round(time.Microsecond),
		percentile(ds, 99).Round(time.Microsecond),
		maxDur(ds).Round(time.Microsecond),
	)
}

func percentile(ds []time.Duration, p int) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	cp := make([]time.Duration, len(ds))
	copy(cp, ds)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := (p * len(cp)) / 100
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

func maxDur(ds []time.Duration) time.Duration {
	var m time.Duration
	for _, d := range ds {
		if d > m {
			m = d
		}
	}
	return m
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
