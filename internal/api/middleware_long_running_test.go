package api

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestLongRunningRequestTimeoutBoundary(t *testing.T) {
	t.Parallel()
	const download = "/api/v1/exports/jobs/a2b864dd-bcd9-4c63-a1bd-321293fbbd34/download"
	for _, test := range []struct {
		method, path string
		deadline     bool
	}{
		{http.MethodPost, download, false},
		{http.MethodPost, "/api/v1/photos/rejects/trash", false},
		{http.MethodPost, "/api/v1/photos/rejects/preflight", false},
		{http.MethodGet, download, true},
		{http.MethodPost, "/api/v1/exports/jobs/a2b864dd-bcd9-4c63-a1bd-321293fbbd34/cancel", true},
		{http.MethodPost, "/api/v1/exports/jobs/a2b864dd-bcd9-4c63-a1bd-321293fbbd34/extra/download", true},
		{http.MethodPost, "/api/v1/exports/jobs/not-a-job/download", true},
		{http.MethodPost, download + "/", true},
		{http.MethodPost, "/api/v1/bates/exports/a2b864dd-bcd9-4c63-a1bd-321293fbbd34/download", false},
		{http.MethodGet, "/api/v1/bates/exports/a2b864dd-bcd9-4c63-a1bd-321293fbbd34/content", false},
		{http.MethodGet, "/api/v1/bates/exports/a2b864dd-bcd9-4c63-a1bd-321293fbbd34/download", true},
		{http.MethodGet, "/api/v1/bates/exports/a2b864dd-bcd9-4c63-a1bd-321293fbbd34", true},
		{http.MethodGet, "/api/v1/bates/exports/candidates", true},
		{http.MethodGet, "/api/v1/bates/exports/not-an-export/content", true},
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
	t.Parallel()
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
	t.Parallel()
	doc := NewOfflineServer().API().OpenAPI()
	operation := doc.Paths["/api/v1/exports/jobs/{id}/download"].Post
	require.NotNil(t, operation.RequestBody)
	require.Negative(t, operation.BodyReadTimeout)
	require.EqualValues(t, 1024, operation.MaxBodyBytes)
	require.GreaterOrEqual(t, doc.Paths["/api/v1/exports/jobs/{id}/cancel"].Post.BodyReadTimeout, time.Duration(0))
}

func TestTimeoutExemptOperationsClearBodyReadDeadline(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	case <-time.After(10 * time.Second):
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
	case <-time.After(10 * time.Second):
		t.Fatal("long-running request did not finish after release")
	}
}

func TestEmailPartStreamAndEnsureHaveNoRequestDeadline(t *testing.T) {
	t.Parallel()
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

func TestMediaRetryOutlivesRequestTimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const route = "/api/v1/media/sources/{source_id}/retry"
		operation := NewOfflineServer().API().OpenAPI().Paths[route].Post
		mux := http.NewServeMux()
		humaAPI := humago.New(mux, huma.DefaultConfig("test", "test"))
		huma.Register(humaAPI, huma.Operation{
			OperationID: "testMediaRetry", Method: http.MethodPost, Path: route,
			BodyReadTimeout: operation.BodyReadTimeout,
		}, func(ctx context.Context, _ *struct{ Body struct{} }) (*struct{ Body string }, error) {
			// Source inspection can outlast the ordinary request deadline.
			time.Sleep(61 * time.Second)
			return &struct{ Body string }{Body: "queued"}, ctx.Err()
		})
		server := httptest.NewTestServer(t, timeoutMiddleware(mux))
		reader, writer := io.Pipe()
		written := make(chan struct{})
		defer func() {
			_ = reader.Close()
			<-written
		}()
		go func() {
			defer close(written)
			time.Sleep(6 * time.Second)
			_, err := writer.Write([]byte(`{}`))
			_ = writer.CloseWithError(err)
		}()
		response, err := server.Client().Post(server.URL+"/api/v1/media/sources/source-1/retry",
			"application/json", reader)
		require.NoError(t, err)
		defer func() { require.NoError(t, response.Body.Close()) }()
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode, string(body))
		require.JSONEq(t, `"queued"`, string(body))
	})
}

func TestPackagePreflightHasNoRequestDeadline(t *testing.T) {
	t.Parallel()
	handler := timeoutMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, present := r.Context().Deadline()
		assert.False(t, present)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/packages/preflights", nil))
}

func TestExportPreparationOutlivesRequestTimeout(t *testing.T) {
	t.Parallel()
	for _, route := range []string{
		"/api/v1/exports/sources", "/api/v1/exports/sources/{id}/seal",
		"/api/v1/exports/plans", "/api/v1/exports/jobs/{id}/download",
	} {
		t.Run(route, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				operation := NewOfflineServer().API().OpenAPI().Paths[route].Post
				mux := http.NewServeMux()
				humaAPI := humago.New(mux, huma.DefaultConfig("test", "test"))
				huma.Register(humaAPI, huma.Operation{
					OperationID: "testExportPreparation", Method: http.MethodPost, Path: route,
					BodyReadTimeout: operation.BodyReadTimeout,
				}, func(ctx context.Context, _ *struct{ Body struct{} }) (*struct{ Body string }, error) {
					time.Sleep(61 * time.Second)
					return &struct{ Body string }{Body: "ready"}, ctx.Err()
				})
				server := httptest.NewTestServer(t, timeoutMiddleware(mux))
				reader, writer := io.Pipe()
				written := make(chan struct{})
				defer func() {
					_ = reader.Close()
					<-written
				}()
				go func() {
					defer close(written)
					time.Sleep(6 * time.Second)
					_, err := writer.Write([]byte(`{}`))
					_ = writer.CloseWithError(err)
				}()
				response, err := server.Client().Post(server.URL+strings.ReplaceAll(route, "{id}", "a2b864dd-bcd9-4c63-a1bd-321293fbbd34"), "application/json", reader)
				require.NoError(t, err)
				defer func() { require.NoError(t, response.Body.Close()) }()
				body, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, response.StatusCode, string(body))
				require.JSONEq(t, `"ready"`, string(body))
			})
		})
	}
}

func TestExportContextErrorsRemainDistinct(t *testing.T) {
	t.Parallel()
	require.Equal(t, http.StatusGatewayTimeout, exportProblem(context.DeadlineExceeded).Status)
	require.Equal(t, http.StatusRequestTimeout, exportProblem(context.Canceled).Status)
}

func TestMailboxTimeoutBoundary(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		method, path string
		exempt       bool
	}{
		{http.MethodPut, "/api/v1/mailbox/containers/source/chunks/0", true},
		{http.MethodPost, "/api/v1/mailbox/containers/source/seal", true},
		{http.MethodPost, "/api/v1/mailbox/transfers", true},
		{http.MethodGet, "/api/v1/mailbox/jobs/job/events", true},
		{http.MethodPost, "/api/v1/mailbox/containers/source%2Fgroup/seal", true},
		{http.MethodGet, "/api/v1/mailbox/jobs/job%2Fgroup/events", true},
		{http.MethodGet, "/api/v1/mailbox/jobs/job", false},
		{http.MethodPost, "/api/v1/mailbox/containers/source/preview", false},
		{http.MethodPost, "/api/v1/mailbox/jobs/job/cancel", false},
		{http.MethodGet, "/api/v1/mailbox/containers/source/seal", false},
		{http.MethodPost, "/api/v1/mailbox/jobs/job/events", false},
		{http.MethodPut, "/api/v1/mailbox/containers/source/chunks/0/extra", false},
	} {
		t.Run(test.method+test.path, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
				defer cancel()
				handler := timeoutMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					time.Sleep(61 * time.Second)
					if !test.exempt {
						assert.ErrorIs(t, r.Context().Err(), context.DeadlineExceeded)
						return
					}
					assert.NoError(t, r.Context().Err())
					want, _ := parent.Deadline()
					got, ok := r.Context().Deadline()
					assert.True(t, ok)
					assert.Equal(t, want, got)
					cancel()
					assert.ErrorIs(t, r.Context().Err(), context.Canceled)
				}))
				handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(parent, test.method, test.path, nil))
			})
		})
	}
}

func TestLargeAudioPlanContextOutlivesOnlyServerDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		wantDeadline, _ := parent.Deadline()
		handler := timeoutMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			type identityKey struct{}
			bounded := context.WithValue(r.Context(), identityKey{}, "synthetic:operator")
			audio, cleanup, err := withoutRequestTimeout(bounded)
			require.NoError(t, err)
			defer cleanup()
			deadline, ok := audio.Deadline()
			require.True(t, ok)
			require.Equal(t, wantDeadline, deadline)
			require.Equal(t, "synthetic:operator", audio.Value(identityKey{}))
			time.Sleep(61 * time.Second)
			require.ErrorIs(t, bounded.Err(), context.DeadlineExceeded)
			require.NoError(t, audio.Err())
			require.NoError(t, context.Cause(audio))
			derived, stop := context.WithCancel(audio)
			defer stop()
			cancel()
			synctest.Wait()
			require.ErrorIs(t, audio.Err(), context.Canceled)
			require.ErrorIs(t, derived.Err(), context.Canceled)
		}))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(parent, http.MethodPost, "/api/v1/processing/plans", nil))
	})
}

func TestLargeAudioPlanClearsHandlerReadDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		mux := http.NewServeMux()
		humaAPI := humago.New(mux, huma.DefaultConfig("test", "test"))
		huma.Register(humaAPI, huma.Operation{OperationID: "testLargeAudioPlan", Method: http.MethodPost, Path: "/api/v1/processing/plans"}, func(ctx context.Context, _ *struct{ Body struct{} }) (*struct{ Body string }, error) {
			long, cleanup, err := withoutRequestTimeout(ctx)
			if err != nil {
				return nil, err
			}
			defer cleanup()
			time.Sleep(61 * time.Second)
			return &struct{ Body string }{Body: "planned"}, long.Err()
		})
		server := httptest.NewTestServer(t, timeoutMiddleware(mux))
		response, err := server.Client().Post(server.URL+"/api/v1/processing/plans", "application/json", strings.NewReader("{}"))
		require.NoError(t, err)
		defer func() { require.NoError(t, response.Body.Close()) }()
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode, string(body))
		require.JSONEq(t, `"planned"`, string(body))
	})
}

func TestLargeAudioPlanKeepsUnrelatedLimits(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		mediaType     string
		size, maximum int64
		long          bool
	}{
		{"audio/wav", 1 << 30, 4 << 30, false},
		{"audio/wav", (1 << 30) + 1, 4 << 30, true},
		{"audio/wav", 4 << 30, 4 << 30, true},
		{"audio/wav", (4 << 30) + 1, 4 << 30, false},
		{"audio/wav", 4 << 30, 3 << 30, false},
		{"text/plain", 4 << 30, 4 << 30, false},
		{"video/mp4", 4 << 30, 4 << 30, false},
		{"audio/mp4", 4 << 30, 4 << 30, true},
		{"audio/x-m4a", 4 << 30, 4 << 30, true},
	} {
		require.Equal(t, test.long, largeAudioProcessingPlan(store.ContentVersion{Size: test.size, MimeType: test.mediaType}, test.maximum))
	}
}

func TestLargeAudioPlanPreservesBrowserSessionRevocation(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		sessions := newWebSessionRegistry()
		token, _, err := sessions.issue()
		require.NoError(t, err)
		request := httptest.NewRequest(http.MethodPost, "/api/v1/processing/plans", nil)
		request.Header.Set(WebSessionHeader, token)
		handler := timeoutMiddleware(authMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			audio, cleanup, err := withoutRequestTimeout(r.Context())
			require.NoError(t, err)
			defer cleanup()
			require.True(t, browserSessionRequest(audio))
			time.Sleep(61 * time.Second)
			require.NoError(t, audio.Err())
			sessions.revoke(token)
			synctest.Wait()
			require.ErrorIs(t, r.Context().Err(), context.DeadlineExceeded)
			require.ErrorIs(t, audio.Err(), context.Canceled)
		}), "synthetic-master-key", sessions, "master"))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	})
}

func TestLargeAudioPlanPreservesCallerCancellationCause(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cause := errors.New("synthetic caller cancellation")
		parent, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		handler := timeoutMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			audio, cleanup, err := withoutRequestTimeout(r.Context())
			require.NoError(t, err)
			defer cleanup()
			time.Sleep(61 * time.Second)
			cancel(cause)
			require.ErrorIs(t, audio.Err(), context.Canceled)
			require.ErrorIs(t, context.Cause(audio), cause)
		}))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(parent, http.MethodPost, "/api/v1/processing/plans", nil))
	})
}
