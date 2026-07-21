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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
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
