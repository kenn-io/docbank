package gcs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/auth/credentials"
	"cloud.google.com/go/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/iterator"

	"go.kenn.io/kit/objectstore"
)

// TestGCSLiveConformance runs the store's contract against a real bucket
// named by KIT_GCS_LIVE_URL, as gs://bucket/prefix. Setting it is the consent
// to run against GCS with Application Default Credentials. fake-gcs-server
// cannot show GCS's own checksums, generation preconditions, or listing.
func TestGCSLiveConformance(t *testing.T) {
	raw := os.Getenv("KIT_GCS_LIVE_URL")
	if raw == "" {
		t.Skip("KIT_GCS_LIVE_URL is not configured")
	}
	u, err := url.Parse(raw)
	require.NoError(t, err)
	require.Equal(t, "gs", u.Scheme, "KIT_GCS_LIVE_URL must be gs://bucket/prefix")
	require.NotEmpty(t, u.Host, "KIT_GCS_LIVE_URL must name a bucket")
	creds, err := credentials.DetectDefault(&credentials.DetectOptions{Scopes: []string{storage.ScopeReadWrite}})
	require.NoError(t, err)
	var id [16]byte
	_, err = rand.Read(id[:])
	require.NoError(t, err)
	prefix := strings.Trim(u.Path, "/")
	if prefix != "" {
		prefix += "/"
	}
	s, err := New(t.Context(), Config{Bucket: u.Host, Prefix: prefix + hex.EncodeToString(id[:]), Credentials: creds})
	require.NoError(t, err)
	t.Cleanup(func() { removeLivePrefix(t, s) })

	// Just over the client's 16 MiB chunk, so the upload takes several requests.
	keys := []string{"empty", "small", "large"}
	for i, size := range []int{0, 17, (16 << 20) + 123} {
		key := keys[i]
		data := bytes.Repeat([]byte("x"), size)
		sum := sha256.Sum256(data)
		receipt, err := s.Publish(t.Context(), key, bytes.NewReader(data), int64(size), sum)
		require.NoError(t, err, key)
		assert.Equal(t, sum, receipt.SHA256, key)
		assert.Equal(t, "provider-crc32c", receipt.Verification, key)
		assert.Equal(t, int64(size), receipt.Object.Size, key)

		again, err := s.Publish(t.Context(), key, bytes.NewReader(data), int64(size), sum)
		require.NoError(t, err, key)
		assert.Equal(t, "readback-sha256", again.Verification, key)
		assert.Equal(t, receipt.Object.Revision, again.Object.Revision, key)

		other := []byte("different")
		zero, err := s.Publish(t.Context(), key, bytes.NewReader(other), int64(len(other)), sha256.Sum256(other))
		require.ErrorIs(t, err, objectstore.ErrExists, key)
		assert.Zero(t, zero, key)

		if size > 0 {
			body, err := s.OpenRange(t.Context(), key, receipt.Object.Revision, 0, int64(min(10, size)))
			require.NoError(t, err, key)
			got, err := io.ReadAll(body)
			require.NoError(t, err, key)
			require.NoError(t, body.Close(), key)
			assert.Equal(t, data[:min(10, size)], got, key)
		}

		generation, err := strconv.ParseInt(receipt.Object.Revision, 10, 64)
		require.NoError(t, err, key)
		stale := strconv.FormatInt(generation+1, 10)
		require.ErrorIs(t, s.Delete(t.Context(), key, stale), objectstore.ErrRevisionChanged, key)
		meta, err := s.Inspect(t.Context(), key)
		require.NoError(t, err, key)
		assert.Equal(t, int64(size), meta.Size, key)
	}

	foreign := []string{"foreign-object", "foreign/\x01+entry", "foreign/literal%2F+name/nested"}
	for _, key := range foreign {
		w := s.bucket.Object(s.prefix + key).NewWriter(t.Context())
		_, err := w.Write([]byte("foreign"))
		require.NoError(t, err)
		require.NoError(t, w.Close())
	}
	listed := []string{}
	token := ""
	for {
		page, err := s.ListPage(t.Context(), "", token, 1)
		require.NoError(t, err)
		for _, m := range page.Objects {
			listed = append(listed, m.Key)
		}
		if page.NextToken == "" {
			break
		}
		token = page.NextToken
	}
	assert.ElementsMatch(t, append(keys, foreign...), listed)

	small, err := s.Inspect(t.Context(), "small")
	require.NoError(t, err)
	require.NoError(t, s.Delete(t.Context(), "small", small.Revision))
	_, err = s.Inspect(t.Context(), "small")
	require.ErrorIs(t, err, objectstore.ErrNotFound)
	require.NoError(t, s.Delete(t.Context(), "small", ""), "deleting a missing object succeeds")
}

// removeLivePrefix deletes every object under the test's own prefix, and
// nothing else, then closes the store.
func removeLivePrefix(t *testing.T, s *Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
	defer cancel()
	it := s.bucket.Objects(ctx, &storage.Query{Prefix: s.prefix})
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(attrs.Name, s.prefix))
		require.NoError(t, s.bucket.Object(attrs.Name).Delete(ctx))
	}
	require.NoError(t, s.Close())
}
