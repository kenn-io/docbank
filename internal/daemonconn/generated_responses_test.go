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
		{"not modified on another route", true, 304, true},
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

func TestPhotoPreviewConditionalResponses(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNotModified, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Api-Key") != "synthetic-key" ||
					r.Header.Get("If-None-Match") != `"synthetic-generation"` {
					t.Error("preview credentials or validator missing")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("ETag", `"synthetic-generation"`)
				w.WriteHeader(status)
				if status == http.StatusOK {
					_, _ = io.WriteString(w, "synthetic JPEG")
				}
				if status == http.StatusNotFound {
					_, _ = io.WriteString(w, `{"status":404,"code":"not_found","detail":"Photo is no longer included."}`)
				}
			}))
			t.Cleanup(server.Close)
			for _, mode := range []string{"buffered", "streamed"} {
				t.Run(mode, func(t *testing.T) {
					connection := New(server.URL, "synthetic-key")
					client := connection.API()
					ctx := t.Context()
					var response *http.Response
					if mode == "streamed" {
						client = connection.apiWithResponse(&response)
						ctx = runtime.WithStreamingResponse(ctx)
					}
					result, err := client.ReadPhotoPreview(ctx, &apiclient.ReadPhotoPreviewRequestOptions{
						PathParams: &apiclient.ReadPhotoPreviewPath{AssetID: "00000000-0000-4000-8000-000000000001", GenerationID: "synthetic-generation"},
						Header:     &apiclient.ReadPhotoPreviewHeaders{IfNoneMatch: new(`"synthetic-generation"`)},
					})
					if status == http.StatusNotFound {
						require.ErrorContains(t, err, "Photo is no longer included.")
						require.Nil(t, result)
						return
					}
					require.NoError(t, err)
					if mode == "streamed" {
						require.Equal(t, status, response.StatusCode)
						require.Equal(t, `"synthetic-generation"`, response.Header.Get("ETag"))
						body, readErr := io.ReadAll(response.Body)
						require.NoError(t, readErr)
						require.NoError(t, response.Body.Close())
						if status == http.StatusOK {
							require.Equal(t, "synthetic JPEG", string(body))
						} else {
							require.Empty(t, body)
						}
					} else if status == http.StatusNotModified {
						require.NotNil(t, result.Status304)
						require.Nil(t, result.Status200)
					} else {
						require.Nil(t, result.Status304)
						require.NotNil(t, result.Status200)
						require.Equal(t, "synthetic JPEG", string(*result.Status200))
					}
				})
			}
		})
	}
}
