package s3store

import (
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
)

func TestNewNormalizesDefaultLimits(t *testing.T) {
	backend, err := New(t.Context(), testConfig())

	require.NoError(t, err)
	assert.Equal(t, packstore.DefaultLimits(), backend.limits)
}

func TestNewRejectsPartialInvalidLimits(t *testing.T) {
	config := testConfig()
	config.Limits = packstore.Limits{
		BlobBytes: 1, PackBytes: 1, FooterBytes: 1,
	}

	_, err := New(t.Context(), config)

	require.ErrorContains(t, err, "invalid")
}

func TestNewRejectsPartBytesAboveS3Maximum(t *testing.T) {
	config := testConfig()
	config.PartBytes = (5 << 30) + 1

	_, err := New(t.Context(), config)

	require.ErrorContains(t, err, "at most 5 GiB")
}

func TestValidatePartBytesRejectsPlatformIntOverflow(t *testing.T) {
	err := validatePartBytes(3<<30, (1<<31)-1)

	require.ErrorContains(t, err, "platform int maximum")
}

func TestNewValidatesEndpointTransport(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		allow    bool
		wantErr  string
	}{
		{name: "default AWS endpoint"},
		{name: "HTTPS", endpoint: "https://objects.example.test"},
		{
			name: "HTTP denied", endpoint: "http://objects.example.test",
			wantErr: "insecure HTTP endpoint",
		},
		{
			name: "HTTP opted in", endpoint: "http://objects.example.test",
			allow: true,
		},
		{
			name: "scheme less", endpoint: "localhost:9000", allow: true,
			wantErr: "absolute HTTP or HTTPS",
		},
		{
			name: "FTP", endpoint: "ftp://objects.example.test", allow: true,
			wantErr: "absolute HTTP or HTTPS",
		},
		{
			name: "missing host", endpoint: "https:///bucket", allow: true,
			wantErr: "absolute HTTP or HTTPS",
		},
		{
			name: "malformed", endpoint: "://bad", allow: true,
			wantErr: "absolute HTTP or HTTPS",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := testConfig()
			config.Endpoint = tt.endpoint
			config.AllowInsecureTransport = tt.allow

			_, err := New(t.Context(), config)

			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantErr)
			}
		})
	}
}

func TestNewDoesNotInheritSDKEndpointOverrides(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{name: "global", key: "AWS_ENDPOINT_URL"},
		{name: "S3", key: "AWS_ENDPOINT_URL_S3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AWS_ENDPOINT_URL", "")
			t.Setenv("AWS_ENDPOINT_URL_S3", "")
			t.Setenv(tt.key, "http://objects.example.test")

			backend, err := New(t.Context(), testConfig())

			require.NoError(t, err)
			assert.Nil(t, backend.client.Options().BaseEndpoint)
		})
	}
}

func testConfig() Config {
	return Config{
		Region: "us-east-1",
		Bucket: "test-bucket",
		Credentials: credentials.NewStaticCredentialsProvider(
			"test-access-key",
			"test-secret-key",
			"",
		),
	}
}

func newHTTPBackend(
	limits packstore.Limits,
	roundTrip func(*http.Request) (*http.Response, error),
) *Backend {
	client := s3.NewFromConfig(aws.Config{
		Region: "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider(
			"test-access-key",
			"test-secret-key",
			"",
		),
		HTTPClient: &http.Client{Transport: roundTripFunc(roundTrip)},
	}, func(options *s3.Options) {
		options.BaseEndpoint = aws.String("https://example.test")
		options.UsePathStyle = true
	})
	return &Backend{
		client: client,
		bucket: "test-bucket",
		part:   5,
		page:   defaultInventorySize,
		limits: limits,
	}
}

func attachTestBackend(backend *Backend) {
	backend.setOwnership(packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "test-vault",
		Store:  "archive",
		Epoch:  "epoch-1",
	}, `"owner-etag"`)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
