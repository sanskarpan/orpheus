package orpheus

import "encoding/json"

// ProcessorRef identifies a processor by name + version.
type ProcessorRef struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// CreateJobRequest is the body for Jobs.Create.
type CreateJobRequest struct {
	ArtifactID string          `json:"artifact_id"`
	Processor  ProcessorRef    `json:"processor"`
	Params     json.RawMessage `json:"params,omitempty"`
}

// Job is a processing job.
type Job struct {
	ID        string          `json:"id"`
	Status    string          `json:"status"`
	Processor ProcessorRef    `json:"processor"`
	Result    json.RawMessage `json:"result,omitempty"`
	CostUSD   float64         `json:"cost_usd,omitempty"`
	CreatedAt string          `json:"created_at,omitempty"`
}

// CreateUploadRequest is the body for Uploads.Create.
type CreateUploadRequest struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	SHA256      string `json:"sha256,omitempty"`
}

// UploadPart is one presigned multipart part.
type UploadPart struct {
	PartNumber int    `json:"part_number"`
	URL        string `json:"url"`
}

// UploadSession is a multipart upload session.
type UploadSession struct {
	ID        string       `json:"id"`
	Status    string       `json:"status"`
	PartSize  int          `json:"part_size"`
	Parts     []UploadPart `json:"parts,omitempty"`
	ExpiresAt string       `json:"expires_at,omitempty"`
	CreatedAt string       `json:"created_at,omitempty"`
}

// Artifact is a stored media/result object.
type Artifact struct {
	ID          string `json:"id"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	CreatedAt   string `json:"created_at,omitempty"`
}

// Page is a cursor-paginated list response.
type Page[T any] struct {
	Data       []T    `json:"data"`
	NextCursor string `json:"next_cursor,omitempty"`
}
