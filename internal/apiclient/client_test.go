package apiclient

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/require"
)

type responseTransport struct {
	runtime.APIClient

	response *runtime.Response
}

func (t responseTransport) ExecuteRequest(context.Context, *http.Request, string) (*runtime.Response, error) {
	return t.response, nil
}

func TestGeneratedErrorDecoding(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		malformed  bool
	}{
		{"problem", `{"status":503,"code":"unavailable","detail":"synthetic failure"}`, false},
		{"empty", "", false},
		{"malformed", `{`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			builder, err := runtime.NewAPIClient("http://example.invalid")
			require.NoError(t, err)
			client := NewClient(responseTransport{APIClient: builder, response: &runtime.Response{
				StatusCode: http.StatusServiceUnavailable,
				Headers:    http.Header{"Content-Type": {"application/problem+json"}},
				Content:    []byte(tc.body),
			}})
			result, err := client.VaultInfo(t.Context())
			require.Nil(t, result)
			require.Error(t, err)
			if tc.malformed {
				decode, ok := errors.AsType[*runtime.ResponseDecodeError](err)
				require.True(t, ok)
				require.Equal(t, http.StatusServiceUnavailable, decode.StatusCode)
				require.Equal(t, []byte(tc.body), decode.Body)
			} else {
				problem, ok := errors.AsType[*runtime.ClientAPIError](err)
				require.True(t, ok)
				require.Equal(t, http.StatusServiceUnavailable, problem.StatusCode())
				if tc.body != "" {
					require.Contains(t, err.Error(), "synthetic failure")
				}
			}
		})
	}
}

func TestReadPhotoPreviewConditionalRequestPreservesErrors(t *testing.T) {
	builder, err := runtime.NewAPIClient("http://example.invalid")
	require.NoError(t, err)
	client := NewClient(responseTransport{APIClient: builder, response: &runtime.Response{
		StatusCode: http.StatusNotFound,
		Headers:    http.Header{"Content-Type": {"application/problem+json"}},
		Content:    []byte(`{"status":404,"code":"not_found","detail":"Photo is no longer included."}`),
	}})
	requested := false
	result, err := client.ReadPhotoPreview(t.Context(), &ReadPhotoPreviewRequestOptions{
		PathParams: &ReadPhotoPreviewPath{AssetID: "00000000-0000-4000-8000-000000000001", GenerationID: "synthetic-generation"},
		Header:     &ReadPhotoPreviewHeaders{IfNoneMatch: new(`"cached-preview"`)},
	}, func(_ context.Context, request *http.Request) error {
		requested = true
		require.Equal(t, `"cached-preview"`, request.Header.Get("If-None-Match"))
		return nil
	})
	require.True(t, requested, "conditional request could not be built: %v", err)
	require.Nil(t, result)
	problem, ok := errors.AsType[*runtime.ClientAPIError](err)
	require.True(t, ok)
	require.Equal(t, http.StatusNotFound, problem.StatusCode())
	require.Contains(t, err.Error(), "not_found")
	require.Contains(t, err.Error(), "Photo is no longer included.")
}
