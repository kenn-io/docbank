package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type archiveWriterFunc func([]byte) (int, error)

func (f archiveWriterFunc) Write(p []byte) (int, error) { return f(p) }

func TestNativeExportArchiveReadHasOnlyExactLongStreamTimeoutExemption(t *testing.T) {
	const id = "a2b864dd-bcd9-4c63-a1bd-321293fbbd34"
	const archive = "/api/v1/exports/jobs/" + id + "/archive"
	for _, tc := range []struct {
		method, path string
		wantDeadline bool
	}{
		{http.MethodGet, archive, false},
		{http.MethodGet, "/api/v1/exports/jobs/{id}/archive", true},
		{http.MethodPost, archive, true},
		{http.MethodGet, "/api/v1/exports/jobs/" + id + "/download", true},
		{http.MethodGet, "/api/v1/exports/jobs/" + id + "/cancel", true},
		{http.MethodGet, "/api/v1/exports/jobs/" + id + "/archive/extra", true},
		{http.MethodGet, "/api/v1/exports/jobs/not-a-job/archive", true},
		{http.MethodGet, archive + "/", true},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			called := false
			timeoutMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				called = true
				_, hasDeadline := r.Context().Deadline()
				assert.Equal(t, tc.wantDeadline, hasDeadline)
			})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, tc.path, nil))
			require.True(t, called)
		})
	}
	parent, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	want, _ := parent.Deadline()
	timeoutMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, ok := r.Context().Deadline()
		assert.True(t, ok)
		assert.Equal(t, want, got)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(parent, http.MethodGet, archive, nil))
}

func TestNativeExportArchiveCopyIsBoundedAndStopsOnCancellation(t *testing.T) {
	const chunk = 256 << 10
	content := bytes.Repeat([]byte("synthetic archive bytes"), 3*chunk/23+1)
	file, err := os.CreateTemp(t.TempDir(), "archive-*.zip")
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()
	_, err = file.Write(content)
	require.NoError(t, err)
	_, err = file.Seek(0, io.SeekStart)
	require.NoError(t, err)

	var bounded bytes.Buffer
	require.NoError(t, copyExportArchive(t.Context(), &bounded, file, int64(len(content)-1)))
	require.Equal(t, content[:len(content)-1], bounded.Bytes())
	_, err = file.Seek(0, io.SeekStart)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	var interrupted bytes.Buffer
	writer := archiveWriterFunc(func(p []byte) (int, error) {
		_, _ = interrupted.Write(p)
		cancel()
		return len(p), nil
	})
	err = copyExportArchive(ctx, writer, file, int64(len(content)))
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, interrupted.Len(), len(content))
}
