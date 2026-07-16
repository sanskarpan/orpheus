package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// TTLs for presigned URLs.
//
// presignPutTTL is the lifetime of a single part-upload URL. The
// browser has 15 minutes to start the PUT; that is enough to cover
// slow uplinks and transient network blips without leaving long-lived
// credentials exposed if a URL is leaked.
//
// presignGetTTLMax caps download URLs. 1h matches the longest
// playback we expect for a finalized artifact; if a user wants longer
// we re-sign on demand. We never want a "permanent" download URL.
const (
	presignPutTTL    = 15 * time.Minute
	presignGetTTLMax = 1 * time.Hour
)

// Presigner issues time-limited URLs the browser can hit directly
// without going through the API. It wraps an *s3.PresignClient bound
// to the same underlying *s3.Client the API uses for server-side
// operations; the two share credentials and endpoint config.
type Presigner struct {
	client *s3.PresignClient
}

// NewPresigner wraps an existing *s3.Client for presigned URL
// generation. Exposed so callers can build a Presigner from a client
// created without going through [New] (for example, in tests with a
// custom HTTP client).
func NewPresigner(c *s3.Client) *Presigner {
	return &Presigner{client: s3.NewPresignClient(c)}
}

// CompletedPart is a single uploaded part the client hands back to
// CompleteMultipartUpload. It carries the ETag returned by S3 in
// response to the part PUT and the 1-based part number the client
// assigned when requesting the presigned URL.
type CompletedPart struct {
	ETag       string
	PartNumber int
}

// CreateMultipartUpload starts a multipart upload and returns the
// upload ID. The client uses this ID to request per-part presigned
// URLs and to finalise or abort the upload.
func (c *Client) CreateMultipartUpload(ctx context.Context, key, contentType string) (string, error) {
	if key == "" {
		return "", errors.New("s3.create_multipart: key is required")
	}
	out, err := c.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("s3.create_multipart: %w", err)
	}
	if out.UploadId == nil {
		return "", errors.New("s3.create_multipart: nil upload id")
	}
	return *out.UploadId, nil
}

// PresignUploadPart returns a presigned PUT URL for a single part of
// an in-progress multipart upload. The browser PUTs the part bytes
// directly to this URL; S3 returns an ETag header that the client
// later sends to CompleteMultipartUpload.
func (p *Presigner) PresignUploadPart(
	ctx context.Context,
	bucket, key, uploadId string,
	partNumber int32,
) (string, error) {
	if partNumber < 1 || partNumber > 10000 {
		return "", fmt.Errorf("s3.presign_upload_part: part number %d out of range [1, 10000]", partNumber)
	}
	req, err := p.client.PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(bucket),
		Key:        aws.String(key),
		UploadId:   aws.String(uploadId),
		PartNumber: aws.Int32(partNumber),
	}, s3.WithPresignExpires(presignPutTTL))
	if err != nil {
		return "", fmt.Errorf("s3.presign_upload_part: %w", err)
	}
	return req.URL, nil
}

// CompleteMultipartUpload finalises a multipart upload. parts must be
// sorted by PartNumber ascending; S3 requires this. The caller is the
// upload-session owner and is responsible for ordering.
func (c *Client) CompleteMultipartUpload(
	ctx context.Context,
	key, uploadId string,
	parts []CompletedPart,
) error {
	if uploadId == "" {
		return errors.New("s3.complete_multipart: upload id is required")
	}
	if len(parts) == 0 {
		return errors.New("s3.complete_multipart: at least one part is required")
	}
	s3parts := make([]types.CompletedPart, len(parts))
	for i, p := range parts {
		if p.ETag == "" {
			return fmt.Errorf("s3.complete_multipart: part %d has empty ETag", p.PartNumber)
		}
		s3parts[i] = types.CompletedPart{
			ETag:       aws.String(p.ETag),
			PartNumber: aws.Int32(int32(p.PartNumber)),
		}
	}
	_, err := c.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadId),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: s3parts,
		},
	})
	if err != nil {
		return fmt.Errorf("s3.complete_multipart: %w", err)
	}
	return nil
}

// AbortMultipartUpload cancels an in-progress multipart upload and
// frees the storage S3 is holding for the partial parts. The handler
// calls this when a user explicitly cancels an upload, when a session
// expires, or when the upload is abandoned partway.
func (c *Client) AbortMultipartUpload(ctx context.Context, key, uploadId string) error {
	if uploadId == "" {
		return errors.New("s3.abort_multipart: upload id is required")
	}
	_, err := c.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadId),
	})
	if err != nil {
		return fmt.Errorf("s3.abort_multipart: %w", err)
	}
	return nil
}

// PresignGetObject returns a presigned GET URL for downloading an
// object. ttl is clamped to [presignGetTTLMax] so callers can pass any
// value without accidentally minting a long-lived URL.
func (p *Presigner) PresignGetObject(
	ctx context.Context,
	bucket, key string,
	ttl time.Duration,
) (string, error) {
	if ttl <= 0 {
		ttl = presignGetTTLMax
	}
	if ttl > presignGetTTLMax {
		ttl = presignGetTTLMax
	}
	req, err := p.client.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("s3.presign_get: %w", err)
	}
	return req.URL, nil
}

// HeadObject returns the size and content type of an object. The
// handler uses this after a finalised upload to record the artifact
// size in Postgres and to validate the content type the client
// declared at upload start.
func (c *Client) HeadObject(ctx context.Context, key string) (int64, string, error) {
	out, err := c.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return 0, "", fmt.Errorf("s3.head: %w", err)
	}
	return aws.ToInt64(out.ContentLength), aws.ToString(out.ContentType), nil
}

// GetObjectRange reads up to n leading bytes of an object using a ranged
// GET, so callers can sniff the file header (magic bytes) without pulling
// the whole object. Returns fewer than n bytes if the object is smaller.
func (c *Client) GetObjectRange(ctx context.Context, key string, n int64) ([]byte, error) {
	if key == "" {
		return nil, errors.New("s3.get_range: key is required")
	}
	if n <= 0 {
		n = 512
	}
	out, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
		Range:  aws.String(fmt.Sprintf("bytes=0-%d", n-1)),
	})
	if err != nil {
		return nil, fmt.Errorf("s3.get_range: %w", err)
	}
	defer func() { _ = out.Body.Close() }()
	buf, err := io.ReadAll(io.LimitReader(out.Body, n))
	if err != nil {
		return nil, fmt.Errorf("s3.get_range.read: %w", err)
	}
	return buf, nil
}

// DeleteObject removes an object. Used when an upload is aborted
// after finalisation (e.g. the user uploaded a file but rejected it
// before processing) and as a cleanup path in tests.
func (c *Client) DeleteObject(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("s3.delete: key is required")
	}
	_, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3.delete: %w", err)
	}
	return nil
}
