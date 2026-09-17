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
