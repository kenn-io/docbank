package daemonconn

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/apiclient"
)

func TestGeneratedSuccessVariants(t *testing.T) {
	for _, status := range []int{200, 201, 202, 206} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				switch {
				case strings.HasSuffix(r.URL.Path, "/uploads"):
					_, _ = io.WriteString(w, `{"status":"skipped"}`)
				case strings.HasSuffix(r.URL.Path, "/email"):
					_, _ = io.WriteString(w, `{"generation_id":"synthetic","state":"pending"}`)
				default:
					_, _ = io.WriteString(w, "# synthetic")
				}
			}))
			t.Cleanup(server.Close)
			client := New(server.URL, "").API()
			if status == 200 || status == 201 {
				result, err := client.UploadFile(t.Context(), &apiclient.UploadFileRequestOptions{})
				require.NoError(t, err)
				if status == 200 {
					require.Equal(t, "skipped", result.Status200.Status)
				} else {
					require.Equal(t, "skipped", result.Status201.Status)
				}
			}
			if status == 200 || status == 202 {
				result, err := client.GetEmailMetadata(t.Context(), &apiclient.GetEmailMetadataRequestOptions{PathParams: &apiclient.GetEmailMetadataPath{VersionID: uuid.New()}})
				require.NoError(t, err)
				if status == 200 {
					require.Equal(t, "synthetic", result.Status200.GenerationID)
				} else {
					require.Equal(t, "pending", result.Status202.State)
				}
			}
			if status == 200 || status == 206 {
				result, err := client.GetDocumentRendition(t.Context(), &apiclient.GetDocumentRenditionRequestOptions{PathParams: &apiclient.GetDocumentRenditionPath{AttachmentID: "synthetic"}})
				require.NoError(t, err)
				if status == 200 {
					require.Equal(t, "# synthetic", string(*result.Status200))
				} else {
					require.Equal(t, "# synthetic", string(*result.Status206))
				}
			}
		})
	}
}

type observedResponseBody struct {
	io.Reader

	closed bool
}

func (b *observedResponseBody) Close() error { b.closed = true; return nil }

func TestGeneratedStreamOwnershipAndStatus(t *testing.T) {
	for _, tc := range []struct {
		name      string
		capture   bool
		status    int
		wantError bool
	}{
		{"owned", true, 200, false},
		{"unexpected status", true, 202, true},
		{"unowned", false, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &observedResponseBody{Reader: strings.NewReader("synthetic stream")}
			c := New("http://example.invalid", "")
			c.hc.Transport = failingRoundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: body}, nil
			})
			client := c.API()
			var response *http.Response
			if tc.capture {
				client = c.apiWithResponse(&response)
			}
			_, err := client.GetNodeContent(runtime.WithStreamingResponse(t.Context()), &apiclient.GetNodeContentRequestOptions{PathParams: &apiclient.GetNodeContentPath{ID: 1}})
			if tc.wantError {
				require.Error(t, err)
				require.True(t, body.closed)
			} else {
				require.NoError(t, err)
				require.False(t, body.closed)
				data, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				require.Equal(t, "synthetic stream", string(data))
				require.NoError(t, response.Body.Close())
				require.True(t, body.closed)
			}
		})
	}
}

func TestEmptyAcceptedResponseRequiresBodyExceptShutdown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) }))
	t.Cleanup(server.Close)
	c := New(server.URL, "")
	_, err := c.API().CreateTimelineRebuild(t.Context(), &apiclient.CreateTimelineRebuildRequestOptions{})
	require.Error(t, err)
	require.True(t, IsResponseDecodeError(err))
	_, err = c.API().ShutdownDaemon(t.Context(), &apiclient.ShutdownDaemonRequestOptions{})
	require.NoError(t, err)
}
