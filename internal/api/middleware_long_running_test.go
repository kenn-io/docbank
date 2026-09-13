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

func TestTimeoutExemptOperationsClearBodyReadDeadline(t *testing.T) {
	doc := NewOfflineServer().API().OpenAPI()
	marked := 0
	for path, item := range doc.Paths {
		if !timeoutExempt(path) {
			continue
		}
		for _, operation := range []*huma.Operation{
			item.Get, item.Put, item.Post, item.Delete,
			item.Options, item.Head, item.Patch, item.Trace,
		} {
			if operation == nil || operation.RequestBody == nil {
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
