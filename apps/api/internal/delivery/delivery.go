// Package delivery pushes job results to a tenant's own S3 destination (PRD 06).
//
// Two destination types share one interface. `s3_static` uses the platform's
// own credentials against a (possibly custom, e.g. MinIO) endpoint — the
// testable path and a fit for same-account/dev delivery. `s3_sts` assumes a
// cross-account role the tenant grants us (with a per-destination external_id
// as a confused-deputy defense) via STS and writes with the temporary
// credentials — so no tenant secret keys are ever stored.
package delivery

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// Destination is a resolved delivery target row.
type Destination struct {
	Type       string // s3_static | s3_sts
	Bucket     string
	Prefix     string
	Region     string
	RoleARN    string
	ExternalID string
	Endpoint   string // s3_static only (custom endpoint / MinIO)
}

// Deliverer builds an S3 API client per destination and writes objects.
// StaticAccessKey/StaticSecretKey/StaticEndpoint are the platform credentials
// used for s3_static destinations.
type Deliverer struct {
	StaticEndpoint  string
	StaticAccessKey string
	StaticSecretKey string
}

func (d *Deliverer) client(ctx context.Context, dest Destination) (*s3.Client, error) {
	region := dest.Region
	if region == "" {
		region = "us-east-1"
	}
	switch dest.Type {
	case "s3_static":
		endpoint := dest.Endpoint
		if endpoint == "" {
			endpoint = d.StaticEndpoint
		}
		cfg, err := awsconfig.LoadDefaultConfig(ctx,
			awsconfig.WithRegion(region),
			awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(d.StaticAccessKey, d.StaticSecretKey, "")),
		)
		if err != nil {
			return nil, err
		}
		return s3.NewFromConfig(cfg, func(o *s3.Options) {
			if endpoint != "" {
				o.BaseEndpoint = &endpoint
				o.UsePathStyle = true // MinIO / non-AWS
			}
		}), nil
	case "s3_sts":
		base, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
		if err != nil {
			return nil, err
		}
		stsClient := sts.NewFromConfig(base)
		provider := stscreds.NewAssumeRoleProvider(stsClient, dest.RoleARN, func(o *stscreds.AssumeRoleOptions) {
			if dest.ExternalID != "" {
				o.ExternalID = &dest.ExternalID
			}
			o.RoleSessionName = "orpheus-delivery"
		})
		base.Credentials = provider
		return s3.NewFromConfig(base), nil
	default:
		return nil, fmt.Errorf("delivery: unknown destination type %q", dest.Type)
	}
}

// Verify proves we can write+delete under the destination prefix. It writes a
// small probe object and deletes it, returning an error the caller records as
// last_error.
func (d *Deliverer) Verify(ctx context.Context, dest Destination) error {
	c, err := d.client(ctx, dest)
	if err != nil {
		return err
	}
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	key := dest.Prefix + ".orpheus-verify-" + hex.EncodeToString(buf)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &dest.Bucket, Key: &key, Body: bytes.NewReader([]byte("orpheus-verify")),
	}); err != nil {
		return fmt.Errorf("probe write: %w", err)
	}
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &dest.Bucket, Key: &key}); err != nil {
		return fmt.Errorf("probe delete: %w", err)
	}
	return nil
}

// Push writes body to {prefix}{key} in the destination bucket.
func (d *Deliverer) Push(ctx context.Context, dest Destination, key string, body []byte, contentType string) error {
	c, err := d.client(ctx, dest)
	if err != nil {
		return err
	}
	full := dest.Prefix + key
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &dest.Bucket, Key: &full, Body: bytes.NewReader(body), ContentType: &contentType,
	})
	return err
}
