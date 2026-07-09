// Package s3 is the storage layer for the Orpheus API. It wraps the AWS
// SDK v2 S3 client and exposes the small set of operations the rest of
// the codebase needs: multipart upload management, presigned URL
// generation, and basic object inspection. The same code path serves
// both MinIO (dev) and real AWS S3 (prod); the only difference is
// whether a custom endpoint is configured.
package s3

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/orpheus/api/internal/config"
)

// Client is the Orpheus S3 wrapper. It holds the underlying *awss3.Client
// (used for server-side operations like CreateMultipartUpload, Abort,
// Head, Delete) and the bucket the API owns, plus a Presigner for
// issuing presigned URLs that the browser uses to PUT parts and GET
// finished objects directly.
type Client struct {
	client *awss3.Client
	bucket string
	pub    *Presigner
}

// New builds an S3 client from runtime config.
//
// The same construction path works for MinIO and real S3. The
// distinguishing signal is whether cfg.S3Endpoint is set: a non-empty
// value means "use this as the base endpoint, and address buckets
// path-style" (MinIO and any other S3-compatible store). An empty
// value means "use the default AWS regional endpoint with virtual-host
// bucket addressing" (real S3 in prod).
//
// Static credentials are used because Phase 1 runs the API on a single
// tenant of its own bucket and does not need the full IMDS / SSO /
// web-identity chain. Phase 2+ can revisit this if we run on EKS with
// IRSA.
func New(ctx context.Context, cfg *config.Config) (*Client, error) {
	if cfg.S3Bucket == "" {
		return nil, errors.New("s3.new: S3Bucket is required")
	}

	creds := credentials.NewStaticCredentialsProvider(
		cfg.S3AccessKey,
		cfg.S3SecretKey,
		"",
	)

	// The SDK requires a region. For MinIO the region is meaningless,
	// but for real S3 it must match the bucket's region. We default to
	// us-east-1 in both cases; a Phase 2+ change will surface this as
	// ORPHEUS_S3_REGION when we run multi-region.
	region := "us-east-1"

	awsCfg, err := awsconfig.LoadDefaultConfig(
		ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(creds),
	)
	if err != nil {
		return nil, fmt.Errorf("s3.new.load_aws_config: %w", err)
	}

	opts := []func(*awss3.Options){}

	if cfg.S3Endpoint != "" {
		endpoint := cfg.S3Endpoint
		opts = append(opts, func(o *awss3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		})
	}

	s3client := awss3.NewFromConfig(awsCfg, opts...)

	return &Client{
		client: s3client,
		bucket: cfg.S3Bucket,
		pub:    NewPresigner(s3client),
	}, nil
}

// Bucket returns the bucket this client targets. It is the single
// bucket for the deployment; per-tenant isolation is achieved by
// key prefix (e.g. uploads/{org_id}/{upload_id}/...).
func (c *Client) Bucket() string { return c.bucket }

// Raw exposes the underlying AWS SDK client. Handlers that need an
// operation not wrapped by this package can call it directly. New
// helpers should be added to this package instead so the dependency on
// the SDK type stays in one place.
func (c *Client) Raw() *awss3.Client { return c.client }

// Presigner returns the Presigner for issuing time-limited URLs that
// the browser uses to PUT parts and GET finished objects.
func (c *Client) Presigner() *Presigner { return c.pub }
