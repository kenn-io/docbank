package s3

import (
	"bytes"
	"crypto/sha256"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/internal/s3test"
	"go.kenn.io/kit/objectstore"
)

func TestS3ObjectConformance(t *testing.T) {
	fixture := s3test.Load(t)
	store, err := New(t.Context(), Config{Bucket: fixture.Bucket, Prefix: fixture.Prefix, Endpoint: fixture.Endpoint, Region: "us-east-1", Credentials: fixture.Credentials, ForcePathStyle: true, AllowInsecureTransport: true, PartBytes: 5 << 20, PageSize: 1})
	require.NoError(t, err)
	s3test.Prepare(t, store.client, fixture)
	checkObjectConformance(t, store)
}

// TestS3LiveConformance runs the same contract against a real S3 bucket. It
// is the evidence of provider support that loopback services cannot give.
func TestS3LiveConformance(t *testing.T) {
	fixture := s3test.LoadLive(t)
	store, err := New(t.Context(), Config{Bucket: fixture.Bucket, Prefix: fixture.Prefix, Region: fixture.Region, Credentials: fixture.Credentials, PartBytes: 5 << 20, PageSize: 1})
	require.NoError(t, err)
	s3test.Prepare(t, store.client, fixture)
	checkObjectConformance(t, store)
}

func checkObjectConformance(t *testing.T, store *Store) {
	t.Helper()
	// Readback stays off, so each upload must verify from provider checksums.
	verification := []string{"provider-sha256", "provider-sha256", "provider-crc32c"}
	for i, size := range []int{0, 17, (5 << 20) + 123} {
		data := bytes.Repeat([]byte("x"), size)
		sum := sha256.Sum256(data)
		key := []string{"empty", "small", "large"}[i]
		receipt, err := store.Publish(t.Context(), key, bytes.NewReader(data), int64(size), sum)
		require.NoError(t, err)
		assert.Equal(t, sum, receipt.SHA256)
		assert.Equal(t, verification[i], receipt.Verification, key)
		again, err := store.Publish(t.Context(), key, bytes.NewReader(data), int64(size), sum)
		require.NoError(t, err)
		assert.Equal(t, sum, again.SHA256)
		other := []byte("different")
		zero, err := store.Publish(t.Context(), key, bytes.NewReader(other), int64(len(other)), sha256.Sum256(other))
		require.ErrorIs(t, err, objectstore.ErrExists)
		assert.Zero(t, zero)
		if size > 0 {
			body, err := store.OpenRange(t.Context(), key, receipt.Object.Revision, 0, int64(min(10, size)))
			require.NoError(t, err)
			got, err := io.ReadAll(body)
			require.NoError(t, err)
			require.NoError(t, body.Close())
			assert.Equal(t, data[:min(10, size)], got)
		}
		require.ErrorIs(t, store.Delete(t.Context(), key, "\"stale\""), objectstore.ErrRevisionChanged)
		meta, err := store.Inspect(t.Context(), key)
		require.NoError(t, err)
		assert.Equal(t, int64(size), meta.Size)
	}
	foreign := []string{"foreign-object", "foreign/\x01+entry", "foreign/literal%2F+name/nested"}
	for _, key := range foreign {
		_, err := store.client.PutObject(t.Context(), &awss3.PutObjectInput{Bucket: aws.String(store.bucket), Key: aws.String(store.prefix + key), Body: bytes.NewReader([]byte("foreign"))})
		require.NoError(t, err)
	}
	keys := []string{}
	token := ""
	for {
		page, err := store.ListPage(t.Context(), "", token, 1)
		require.NoError(t, err)
		for _, m := range page.Objects {
			keys = append(keys, m.Key)
		}
		if page.NextToken == "" {
			break
		}
		token = page.NextToken
	}
	assert.ElementsMatch(t, append([]string{"empty", "small", "large"}, foreign...), keys)
	require.NoError(t, store.Delete(t.Context(), "small", ""))
	require.NoError(t, store.Delete(t.Context(), "small", ""))
}
