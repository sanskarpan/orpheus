package avscan_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/orpheus/api/internal/avscan"
	"github.com/orpheus/api/internal/config"
	"github.com/orpheus/api/internal/storage/s3"
)

// TestSignatureScanner_RealMinIO puts a clean object and an EICAR-carrying
// object into a real MinIO bucket via the SDK (which auto-corrects request
// clock skew, unlike a pre-signed URL), then runs the built-in scanner over
// each and asserts the infected one is flagged.
func TestSignatureScanner_RealMinIO(t *testing.T) {
	if os.Getenv("ORPHEUS_TEST_S3") == "" {
		t.Skip("ORPHEUS_TEST_S3 not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	endpoint := envOr("ORPHEUS_S3_ENDPOINT", "http://127.0.0.1:9000")
	access := envOr("ORPHEUS_S3_ACCESS_KEY", "orpheus")
	secret := envOr("ORPHEUS_S3_SECRET_KEY", "orpheus-dev-secret")
	bucket := envOr("ORPHEUS_S3_BUCKET", "orpheus-uploads")

	s3c, err := s3.New(ctx, &config.Config{S3Endpoint: endpoint, S3AccessKey: access, S3SecretKey: secret, S3Bucket: bucket})
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}

	// Raw SDK client for PutObject (the s3.Client wrapper is read-only here).
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(access, secret, "")),
	)
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}
	raw := awss3.NewFromConfig(awsCfg, func(o *awss3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	put := func(key string, body []byte) {
		if _, err := raw.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(body)}); err != nil {
			t.Fatalf("PutObject(%s): %v", key, err)
		}
		t.Cleanup(func() {
			c, cc := context.WithTimeout(context.Background(), 10*time.Second)
			defer cc()
			_, _ = raw.DeleteObject(c, &awss3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		})
	}

	eicar := []byte(`X5O!P%@AP[4\PZX54(P^)7CC)7}` + `$EICAR-STANDARD-ANTIVIRUS-` + `TEST-FILE!$H+H*`)
	infectedKey := "avscan-it/infected.wav"
	cleanKey := "avscan-it/clean.wav"
	put(infectedKey, append([]byte("RIFF....WAVEdata"), eicar...))
	put(cleanKey, append([]byte("RIFF....WAVEdata"), make([]byte, 1024)...))

	sc := &avscan.SignatureScanner{Reader: s3c}
	if err := sc.Scan(ctx, infectedKey); !errors.Is(err, avscan.ErrInfected) {
		t.Fatalf("infected object: want ErrInfected, got %v", err)
	}
	if err := sc.Scan(ctx, cleanKey); err != nil {
		t.Fatalf("clean object: want nil, got %v", err)
	}
	t.Logf("[PASS] built-in scanner flagged EICAR and passed clean bytes from real MinIO")
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
