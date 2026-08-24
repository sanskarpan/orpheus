// Package orpheus is the Go client SDK for the Orpheus API.
//
// It is a thin, dependency-free (stdlib-only) client over the /v1 REST surface,
// mirroring the Python and TypeScript SDKs: API-key auth, RFC 7807 error
// mapping, and typed resources for jobs, uploads, and artifacts.
//
//	client := orpheus.New("https://api.orpheus.dev", orpheus.WithAPIKey("ak_live_..."))
//	job, err := client.Jobs.Create(ctx, orpheus.CreateJobRequest{
//	    ArtifactID: artifactID,
//	    Processor:  orpheus.ProcessorRef{Name: "transcribe", Version: "1.0.0"},
//	})
package orpheus

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Client is an Orpheus API client. Construct it with New. It is safe for
// concurrent use.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	userAgent  string

	Jobs      *JobsService
	Uploads   *UploadsService
	Artifacts *ArtifactsService
}

// Option configures a Client.
type Option func(*Client)

// WithAPIKey sets the X-API-Key credential.
func WithAPIKey(key string) Option { return func(c *Client) { c.apiKey = key } }

// WithHTTPClient overrides the underlying *http.Client (timeouts, transport).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.httpClient = h } }

// WithUserAgent overrides the User-Agent header.
func WithUserAgent(ua string) Option { return func(c *Client) { c.userAgent = ua } }

// New constructs a Client for the given base URL (e.g. "https://api.orpheus.dev").
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL:    trimSlash(baseURL),
		httpClient: &http.Client{Timeout: 30 * time.Second},
		userAgent:  "orpheus-sdk-go/0.1.0",
	}
	for _, o := range opts {
		o(c)
	}
	c.Jobs = &JobsService{c: c}
	c.Uploads = &UploadsService{c: c}
	c.Artifacts = &ArtifactsService{c: c}
	return c
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// do performs a request against path (e.g. "/v1/jobs"), JSON-encoding body when
// non-nil, and decodes a 2xx response into out (when non-nil). Non-2xx responses
// are mapped to *APIError.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("orpheus: marshal request: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("orpheus: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("orpheus: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseAPIError(resp.StatusCode, respBody)
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("orpheus: decode response: %w", err)
		}
	}
	return nil
}

// ── Jobs ─────────────────────────────────────────────────────────────

type JobsService struct{ c *Client }

// Create submits a job (POST /v1/jobs → 202).
func (s *JobsService) Create(ctx context.Context, req CreateJobRequest) (*Job, error) {
	var job Job
	if err := s.c.do(ctx, http.MethodPost, "/v1/jobs", req, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

// Get fetches a job by id.
func (s *JobsService) Get(ctx context.Context, id string) (*Job, error) {
	var job Job
	if err := s.c.do(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(id), nil, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

// List returns a page of jobs.
func (s *JobsService) List(ctx context.Context, opts *ListOptions) (*Page[Job], error) {
	var page Page[Job]
	if err := s.c.do(ctx, http.MethodGet, "/v1/jobs"+opts.query(), nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// WaitForCompletion polls Get until the job reaches a terminal status
// (completed/failed/dead_letter/canceled) or ctx is done.
func (s *JobsService) WaitForCompletion(ctx context.Context, id string, poll time.Duration) (*Job, error) {
	if poll <= 0 {
		poll = time.Second
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		job, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		switch job.Status {
		case "completed", "failed", "dead_letter", "canceled":
			return job, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// ── Uploads ──────────────────────────────────────────────────────────

type UploadsService struct{ c *Client }

func (s *UploadsService) Create(ctx context.Context, req CreateUploadRequest) (*UploadSession, error) {
	var u UploadSession
	if err := s.c.do(ctx, http.MethodPost, "/v1/uploads", req, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *UploadsService) Get(ctx context.Context, id string) (*UploadSession, error) {
	var u UploadSession
	if err := s.c.do(ctx, http.MethodGet, "/v1/uploads/"+url.PathEscape(id), nil, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// Complete finalizes a multipart upload with the collected part ETags and
// returns the created Artifact.
func (s *UploadsService) Complete(ctx context.Context, id string, parts []CompletedPart) (*Artifact, error) {
	var a Artifact
	path := "/v1/uploads/" + url.PathEscape(id) + "/complete"
	if err := s.c.do(ctx, http.MethodPost, path, CompleteUploadRequest{Parts: parts}, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// putBytes PUTs raw bytes to a presigned URL (S3/R2) and returns the ETag.
func (c *Client) putBytes(ctx context.Context, rawURL string, data []byte, contentType string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, rawURL, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("orpheus: build upload PUT: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("orpheus: upload PUT failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("orpheus: upload PUT %d: %s", resp.StatusCode, string(b))
	}
	return strings.Trim(resp.Header.Get("ETag"), `"`), nil
}

// UploadFile uploads a local file end-to-end (create session, PUT each presigned
// part, complete) and returns the finalized Artifact.
func (c *Client) UploadFile(ctx context.Context, path string) (*Artifact, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("orpheus: read file: %w", err)
	}
	ct := mime.TypeByExtension(filepath.Ext(path))
	if ct == "" {
		ct = "application/octet-stream"
	}
	sum := sha256.Sum256(data)
	sess, err := c.Uploads.Create(ctx, CreateUploadRequest{
		Filename:    filepath.Base(path),
		ContentType: ct,
		SizeBytes:   int64(len(data)),
		SHA256:      hex.EncodeToString(sum[:]),
	})
	if err != nil {
		return nil, err
	}
	parts := make([]CompletedPart, 0, len(sess.Parts))
	for _, p := range sess.Parts {
		start := (p.PartNumber - 1) * sess.PartSize
		end := start + sess.PartSize
		if end > len(data) {
			end = len(data)
		}
		etag, err := c.putBytes(ctx, p.URL, data[start:end], ct)
		if err != nil {
			return nil, err
		}
		parts = append(parts, CompletedPart{PartNumber: p.PartNumber, ETag: etag})
	}
	return c.Uploads.Complete(ctx, sess.ID, parts)
}

// TranscribeOptions tunes a Transcribe call.
type TranscribeOptions struct {
	// Params are extra transcribe params (e.g. {"language":"en","tier":"accurate"}).
	Params map[string]any
	// Version is the transcribe processor version (default "1.0.0").
	Version string
	// Poll is the completion poll interval (default 2s). The overall timeout is
	// governed by the ctx deadline.
	Poll time.Duration
}

// Transcribe uploads an audio file and transcribes it, waiting for completion.
// Returns the completed Job (Job.Result holds the transcript); a non-completed
// terminal state is returned as an error along with the Job.
func (c *Client) Transcribe(ctx context.Context, path string, opts *TranscribeOptions) (*Job, error) {
	if opts == nil {
		opts = &TranscribeOptions{}
	}
	version := opts.Version
	if version == "" {
		version = "1.0.0"
	}
	artifact, err := c.UploadFile(ctx, path)
	if err != nil {
		return nil, err
	}
	var params json.RawMessage
	if len(opts.Params) > 0 {
		if params, err = json.Marshal(opts.Params); err != nil {
			return nil, fmt.Errorf("orpheus: marshal params: %w", err)
		}
	}
	job, err := c.Jobs.Create(ctx, CreateJobRequest{
		ArtifactID: artifact.ID,
		Processor:  ProcessorRef{Name: "transcribe", Version: version},
		Params:     params,
	})
	if err != nil {
		return nil, err
	}
	poll := opts.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	job, err = c.Jobs.WaitForCompletion(ctx, job.ID, poll)
	if err != nil {
		return nil, err
	}
	if job.Status != "completed" {
		return job, fmt.Errorf("orpheus: transcribe job %s ended %s", job.ID, job.Status)
	}
	return job, nil
}

// ── Artifacts ────────────────────────────────────────────────────────

type ArtifactsService struct{ c *Client }

func (s *ArtifactsService) Get(ctx context.Context, id string) (*Artifact, error) {
	var a Artifact
	if err := s.c.do(ctx, http.MethodGet, "/v1/artifacts/"+url.PathEscape(id), nil, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *ArtifactsService) List(ctx context.Context, opts *ListOptions) (*Page[Artifact], error) {
	var page Page[Artifact]
	if err := s.c.do(ctx, http.MethodGet, "/v1/artifacts"+opts.query(), nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// ListOptions are common list/pagination query params.
type ListOptions struct {
	Limit  int
	Cursor string
}

func (o *ListOptions) query() string {
	if o == nil {
		return ""
	}
	v := url.Values{}
	if o.Limit > 0 {
		v.Set("limit", strconv.Itoa(o.Limit))
	}
	if o.Cursor != "" {
		v.Set("cursor", o.Cursor)
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}
