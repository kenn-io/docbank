package api

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExportTicketPreparationTimeoutBoundary(t *testing.T) {
	const download = "/api/v1/exports/jobs/a2b864dd-bcd9-4c63-a1bd-321293fbbd34/download"
	for _, test := range []struct {
		method, path string
		deadline     bool
	}{
		{http.MethodPost, download, false},
		{http.MethodGet, download, true},
		{http.MethodPost, "/api/v1/exports/jobs/a2b864dd-bcd9-4c63-a1bd-321293fbbd34/cancel", true},
		{http.MethodPost, "/api/v1/exports/jobs/a2b864dd-bcd9-4c63-a1bd-321293fbbd34/extra/download", true},
		{http.MethodPost, "/api/v1/exports/jobs/not-a-job/download", true},
		{http.MethodPost, download + "/", true},
	} {
		t.Run(test.method+test.path, func(t *testing.T) {
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			called := false
			handler := timeoutMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				called = true
				_, hasDeadline := r.Context().Deadline()
				assert.Equal(t, test.deadline, hasDeadline)
				cancel()
				assert.ErrorIs(t, r.Context().Err(), context.Canceled)
			}))
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(parent, test.method, test.path, nil))
			require.True(t, called)
		})
	}
	parent, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	want, _ := parent.Deadline()
	timeoutMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, ok := r.Context().Deadline()
		assert.True(t, ok)
		assert.Equal(t, want, got, "ticket verification must preserve the caller's own deadline")
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(parent, http.MethodPost, download, nil))
}

func TestMailboxWatchTimeoutBoundary(t *testing.T) {
	const events = "/api/v1/mailbox/jobs/synthetic-import/events"
	for _, test := range []struct {
		method, path string
		deadline     bool
	}{
		{http.MethodGet, events, false},
		{http.MethodPost, events, true},
		{http.MethodGet, "/api/v1/mailbox/jobs/synthetic-import", true},
		{http.MethodGet, "/api/v1/mailbox/jobs/synthetic-import/extra/events", true},
		{http.MethodGet, "/api/v1/mailbox/jobs//events", true},
		{http.MethodGet, events + "/", true},
	} {
		t.Run(test.method+test.path, func(t *testing.T) {
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			timeoutMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				_, deadline := r.Context().Deadline()
				assert.Equal(t, test.deadline, deadline)
				cancel()
				assert.ErrorIs(t, r.Context().Err(), context.Canceled)
			})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(parent, test.method, test.path, nil))
		})
	}
	parent, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	want, _ := parent.Deadline()
	timeoutMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, ok := r.Context().Deadline()
		assert.True(t, ok)
		assert.Equal(t, want, got, "watch must preserve the caller's own deadline")
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(parent, http.MethodGet, events, nil))
}

func TestExportTicketOperationClearsBodyDeadlineWithoutRelaxingBounds(t *testing.T) {
	doc := NewOfflineServer().API().OpenAPI()
	operation := doc.Paths["/api/v1/exports/jobs/{id}/download"].Post
	require.NotNil(t, operation.RequestBody)
	require.Negative(t, operation.BodyReadTimeout)
	require.EqualValues(t, 1024, operation.MaxBodyBytes)
	for _, path := range []string{"/api/v1/exports/sources", "/api/v1/exports/jobs/{id}/cancel"} {
		require.GreaterOrEqual(t, doc.Paths[path].Post.BodyReadTimeout, time.Duration(0), path)
	}
}

func TestEmailPreparationClearsBodyDeadlineWithoutRelaxingReadRoutes(t *testing.T) {
	doc := NewOfflineServer().API().OpenAPI()
	operation := doc.Paths["/api/v1/versions/{version_id}/email"].Post
	require.Negative(t, operation.BodyReadTimeout)
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		require.False(t, timeoutExempt(method, "/api/v1/versions/selected/email"))
	}
}

func TestTimeoutExemptOperationsClearBodyReadDeadline(t *testing.T) {
	doc := NewOfflineServer().API().OpenAPI()
	marked := 0
	for path, item := range doc.Paths {
		for _, operation := range []*huma.Operation{
			item.Get, item.Put, item.Post, item.Delete,
			item.Options, item.Head, item.Patch, item.Trace,
		} {
			if operation == nil || operation.RequestBody == nil || !timeoutExempt(operation.Method, path) {
				continue
			}
			marked++
			assert.Negative(t, operation.BodyReadTimeout, "%s", path)
		}
	}
	assert.GreaterOrEqual(t, marked, 6)
}

func TestLongRunningHumaOperationOutlivesDefaultBodyDeadline(t *testing.T) {
	mux := http.NewServeMux()
	humaAPI := humago.New(mux, huma.DefaultConfig("test", "test"))
	entered := make(chan struct{})
	release := make(chan struct{})
	type input struct {
		Body struct{} `json:"body"`
	}
	type output struct {
		Body struct {
			Completed bool `json:"completed"`
		}
	}
	huma.Register(humaAPI, huma.Operation{
		OperationID: "testLongRunningBackup", Method: http.MethodPost,
		Path: "/api/v1/backup/snapshots",
	}, func(ctx context.Context, _ *input) (*output, error) {
		close(entered)
		select {
		case <-release:
			out := &output{}
			out.Body.Completed = true
			return out, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	clearLongRunningBodyReadDeadlines(humaAPI)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	type result struct {
		status int
		body   []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := http.Post(server.URL+"/api/v1/backup/snapshots",
			"application/json", bytes.NewReader([]byte(`{}`)))
		if err != nil {
			done <- result{err: err}
			return
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		done <- result{status: resp.StatusCode, body: body, err: readErr}
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("long-running handler did not start")
	}
	select {
	case got := <-done:
		t.Fatalf("request ended before Huma's default deadline was exceeded: %+v", got)
	case <-time.After(5200 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })

	select {
	case got := <-done:
		require.NoError(t, got.err)
		assert.Equal(t, http.StatusOK, got.status, string(got.body))
		var response map[string]any
		require.NoError(t, json.Unmarshal(got.body, &response))
		assert.Equal(t, true, response["completed"])
	case <-time.After(2 * time.Second):
		t.Fatal("long-running request did not finish after release")
	}
}

func TestEmailPartStreamAndEnsureHaveNoRequestDeadline(t *testing.T) {
	deadline := make(chan bool, 1)
	handler := timeoutMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, present := r.Context().Deadline()
		deadline <- present
	}))

	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/versions/11111111-1111-4111-8111-111111111111/email/generations/"+
			strings.Repeat("a", 64)+"/parts/1.2/decoded_payload", nil)
	handler.ServeHTTP(httptest.NewRecorder(), request)
	assert.False(t, <-deadline)

	request = httptest.NewRequest(http.MethodPost,
		"/api/v1/versions/11111111-1111-4111-8111-111111111111/email", nil)
	handler.ServeHTTP(httptest.NewRecorder(), request)
	assert.False(t, <-deadline)

	request = httptest.NewRequest(http.MethodGet,
		"/api/v1/versions/11111111-1111-4111-8111-111111111111/email", nil)
	handler.ServeHTTP(httptest.NewRecorder(), request)
	assert.True(t, <-deadline)
}
