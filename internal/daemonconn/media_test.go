package daemonconn

import (
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestMediaClientEscapesMultipartFilenames(t *testing.T) {
	for _, test := range []struct{ filename, want string }{
		{`call"quoted.txt`, `call"quoted.txt`},
		{`call\"quoted.txt`, `call\"quoted.txt`},
		{"call\r\nline.txt", "call%0D%0Aline.txt"},
	} {
		t.Run(test.filename, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reader, err := r.MultipartReader()
				if err != nil {
					http.Error(w, err.Error(), http.StatusUnprocessableEntity)
					return
				}
				if _, err := reader.NextPart(); err != nil { // Metadata precedes the file.
					http.Error(w, err.Error(), http.StatusUnprocessableEntity)
					return
				}
				file, err := reader.NextPart()
				if err != nil {
					http.Error(w, err.Error(), http.StatusUnprocessableEntity)
					return
				}
				_, params, err := mime.ParseMediaType(file.Header.Get("Content-Disposition"))
				if err != nil || params["name"] != "file" || params["filename"] != test.want {
					t.Errorf("file disposition = %v, %v; want filename %q", params, err, test.want)
				}
				_, _ = w.Write([]byte(`{"vault_uid":"vault","source_id":"source","operation_id":"operation","operation_state":"succeeded"}`))
			}))
			t.Cleanup(server.Close)
			c := New(server.URL, "key")
			_, err := c.SubmitSuppliedMedia(t.Context(), api.MediaSuppliedMetadata{
				OperationID: "operation", Filename: test.filename, MediaType: "text/plain",
			}, strings.NewReader("synthetic"))
			require.NoError(t, err)
			_, err = c.ImportMediaArtifact(t.Context(), "source", api.MediaArtifactMetadata{
				OperationID: "operation", Filename: test.filename, MediaType: "text/plain",
			}, strings.NewReader("synthetic"))
			require.NoError(t, err)
		})
	}
}

func TestMediaClientRejectsDuplicateOrMismatchedIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/media/sources":
			_, _ = w.Write([]byte(`{"items":[{"source_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},{"source_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],"total":2}`))
		default:
			_, _ = w.Write([]byte(`{"vault_uid":"00000000-0000-4000-8000-000000000001","source_id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","operation_id":"00000000-0000-4000-8000-000000000001","operation_state":"succeeded","coverage_state":"unprocessed"}`))
		}
	}))
	t.Cleanup(server.Close)
	c := New(server.URL, "key")
	_, err := c.MediaSources(t.Context(), "", 10)
	require.ErrorContains(t, err, "duplicate")
	_, err = c.MediaStatus(t.Context(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.ErrorContains(t, err, "different source")
}

func TestMediaClientAcceptsEarlyReplayResponse(t *testing.T) {
	var result api.MediaReceipt
	err := readMediaMultipartReceipt(api.MediaArtifactMetadata{
		OperationID: "operation", Filename: "file.txt", MediaType: "text/plain",
	}, "file.txt", "text/plain", strings.NewReader("synthetic"), &result,
		func(edit runtime.RequestEditorFn) (*http.Response, error) {
			req := &http.Request{Header: make(http.Header)}
			require.NoError(t, edit(t.Context(), req))
			require.NoError(t, req.Body.Close())
			return &http.Response{StatusCode: http.StatusOK,
				Body: io.NopCloser(strings.NewReader(
					`{"vault_uid":"vault","source_id":"source","operation_id":"operation","operation_state":"succeeded"}`,
				))}, nil
		})
	require.NoError(t, err)
	require.Equal(t, "source", result.SourceID)
}
