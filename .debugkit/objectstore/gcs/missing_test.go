package gcs

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/objectstore"
	"google.golang.org/api/googleapi"
)

func TestMissingObjectRequiresSuccessfulBucketListing(t *testing.T) {
	for _, operation := range []string{"inspect", "read", "delete", "publish"} {
		for _, failure := range []string{"denied", "canceled"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				listings := 0
				f, store := newFixture(t, func(next http.RoundTripper) http.RoundTripper {
					return roundTripFunc(func(r *http.Request) (*http.Response, error) {
						if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/o") {
							return next.RoundTrip(r)
						}
						listings++
						assert.Equal(t, "namespace/missing", r.URL.Query().Get("prefix"))
						if failure == "canceled" {
							cancel()
							return nil, r.Context().Err()
						}
						return &http.Response{
							StatusCode: http.StatusForbidden,
							Header:     http.Header{"Content-Type": {"application/json"}},
							Body: io.NopCloser(strings.NewReader(
								`{"error":{"code":403,"message":"listing denied"}}`)),
							Request: r,
						}, nil
					})
				})
				var err error
				switch operation {
				case "inspect":
					_, err = store.Inspect(ctx, "missing")
				case "read":
					var body io.ReadCloser
					body, err = store.OpenRange(ctx, "missing", "", 0, 1)
					if body != nil {
						require.NoError(t, body.Close())
					}
				case "delete":
					err = store.Delete(ctx, "missing", "")
				case "publish":
					// A rejected upload still needs reconciliation: an SDK retry
					// can be rejected after an earlier request stored the object.
					f.uploadStatus = http.StatusForbidden
					var receipt objectstore.Receipt
					receipt, err = store.Publish(ctx, "missing", strings.NewReader("bytes"), 5,
						sha256.Sum256([]byte("bytes")))
					assert.Zero(t, receipt)
					require.ErrorIs(t, err, objectstore.ErrOutcomeUnknown)
				}
				require.Positive(t, listings, "the failure must occur during confirmation")
				require.Error(t, err)
				require.NotErrorIs(t, err, objectstore.ErrNotFound)
				if failure == "canceled" {
					require.ErrorIs(t, err, context.Canceled)
				} else {
					apiErr, ok := errors.AsType[*googleapi.Error](err)
					require.True(t, ok)
					assert.Equal(t, http.StatusForbidden, apiErr.Code)
					assert.Equal(t, "listing denied", apiErr.Message)
				}
			})
		}
	}
}
