// Package s3test owns the opt-in, disposable loopback S3 conformance fixture.
package s3test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/internal/s3io"
)

// Namespace is unique to a test; the bucket and service are caller-owned.
type Namespace struct {
	Endpoint, Bucket, Prefix, Region string
	Credentials                      aws.CredentialsProvider
	// Live marks a real provider bucket, which tests must never create.
	Live bool
}

// Load skips unless the existing explicit test endpoint is configured. Only
// loopback services are admitted; live provider contracts need separate consent.
func Load(t *testing.T) Namespace {
	t.Helper()
	endpoint := os.Getenv("KIT_S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("KIT_S3_TEST_ENDPOINT is not configured")
	}
	u, err := url.Parse(endpoint)
	require.NoError(t, err)
	ip := net.ParseIP(u.Hostname())
	require.True(t, u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()), "S3 conformance requires a disposable loopback endpoint")
	var id [16]byte
	_, err = rand.Read(id[:])
	require.NoError(t, err)
	return Namespace{Endpoint: endpoint, Bucket: envOr("KIT_S3_TEST_BUCKET", "kit-conformance"), Prefix: "kit-conformance/" + hex.EncodeToString(id[:]), Credentials: credentials.NewStaticCredentialsProvider(envOr("KIT_S3_TEST_ACCESS_KEY", "kit-test"), envOr("KIT_S3_TEST_SECRET_KEY", "kit-test-secret"), "")}
}

// LoadLive skips unless KIT_S3_LIVE_URL names a real bucket and prefix, as
// s3://bucket/prefix. Setting it is the consent to run against a provider:
// credentials come from the AWS SDK's default chain, and the region from
// KIT_S3_LIVE_REGION or AWS_REGION. Each test writes under its own random
// prefix below the configured one.
func LoadLive(t *testing.T) Namespace {
	t.Helper()
	raw := os.Getenv("KIT_S3_LIVE_URL")
	if raw == "" {
		t.Skip("KIT_S3_LIVE_URL is not configured")
	}
	u, err := url.Parse(raw)
	require.NoError(t, err)
	require.Equal(t, "s3", u.Scheme, "KIT_S3_LIVE_URL must be s3://bucket/prefix")
	require.NotEmpty(t, u.Host, "KIT_S3_LIVE_URL must name a bucket")
	region := envOr("KIT_S3_LIVE_REGION", os.Getenv("AWS_REGION"))
	require.NotEmpty(t, region, "set KIT_S3_LIVE_REGION or AWS_REGION for the live bucket")
	cfg, err := config.LoadDefaultConfig(t.Context(), config.WithRegion(region))
	require.NoError(t, err)
	var id [16]byte
	_, err = rand.Read(id[:])
	require.NoError(t, err)
	prefix := strings.Trim(u.Path, "/")
	if prefix != "" {
		prefix += "/"
	}
	return Namespace{Bucket: u.Host, Prefix: prefix + hex.EncodeToString(id[:]), Region: region, Credentials: cfg.Credentials, Live: true}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// Prepare reuses an existing loopback bucket, creating it if needed, and
// registers exact-prefix cleanup. It never creates a live bucket, deletes a
// bucket, or touches other keys.
func Prepare(t *testing.T, client *s3.Client, n Namespace) {
	t.Helper()
	if !n.Live {
		_, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String(n.Bucket)})
		if err != nil {
			_, headErr := client.HeadBucket(t.Context(), &s3.HeadBucketInput{Bucket: aws.String(n.Bucket)})
			require.NoError(t, headErr, "create bucket: %v", err)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()
		token := ""
		for {
			out, err := s3io.List(ctx, client, n.Bucket, n.Prefix+"/", token, 1000)
			require.NoError(t, err)
			for _, o := range out.Contents {
				require.True(t, strings.HasPrefix(aws.ToString(o.Key), n.Prefix+"/"))
				_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(n.Bucket), Key: o.Key})
				require.NoError(t, err)
			}
			if !aws.ToBool(out.IsTruncated) {
				return
			}
			require.NotEmpty(t, aws.ToString(out.NextContinuationToken))
			require.NotEqual(t, token, aws.ToString(out.NextContinuationToken))
			token = aws.ToString(out.NextContinuationToken)
		}
	})
}
