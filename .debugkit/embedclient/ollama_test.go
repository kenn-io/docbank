package embedclient_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
)

func TestOllamaMetalRecoveryKeepsTheGoodVector(t *testing.T) {
	var nativeBodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/embeddings":
			_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[3,4]},{"index":1,"embedding":[null,1]}]}`)
		case "/api/embed":
			body, _ := io.ReadAll(r.Body)
			nativeBodies = append(nativeBodies, string(body))
			_, _ = io.WriteString(w, `{"embeddings":[[3,4]]}`)
		case "/api/ps":
			_, _ = io.WriteString(w, `{"models":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := embedclient.New(embedclient.Options{
		Model:               unitModel(),
		Deployment:          embedconfig.Deployment{BaseURL: srv.URL + "/v1"},
		Batch:               embedconfig.Batch{Items: 4},
		OllamaMetalRecovery: true,
	})
	require.NoError(t, err)
	got, err := client.Embed(t.Context(), []embedmodel.Content{
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "alpha"},
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "beta"},
	})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.InDelta(t, 0.6, got[0][0], 0.0001)
	assert.InDelta(t, 0.8, got[0][1], 0.0001)
	assert.InDelta(t, 0.6, got[1][0], 0.0001)
	require.Len(t, nativeBodies, 1)
	assert.Contains(t, nativeBodies[0], `"keep_alive":"0s"`)
	assert.NotContains(t, nativeBodies[0], "num_gpu")
	assert.Contains(t, nativeBodies[0], `"input":["beta"]`)
}

func TestOllamaRecoverySendsDimensionsOnlyWhenRequested(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request bool
	}{
		{name: "provider default", request: false},
		{name: "requested", request: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var nativeBodies []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/embeddings":
					_, _ = io.WriteString(w, `{"data":[{"embedding":[null,1]}]}`)
				case "/api/embed":
					body, _ := io.ReadAll(r.Body)
					nativeBodies = append(nativeBodies, string(body))
					_, _ = io.WriteString(w, `{"embeddings":[[3,4]]}`)
				case "/api/ps":
					_, _ = io.WriteString(w, `{"models":[]}`)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			model := unitModel()
			model.RequestDimensions = tc.request
			client, err := embedclient.New(embedclient.Options{
				Model:               model,
				Deployment:          embedconfig.Deployment{BaseURL: srv.URL + "/v1"},
				Batch:               embedconfig.Batch{Items: 4},
				OllamaMetalRecovery: true,
			})
			require.NoError(t, err)
			_, err = client.Embed(t.Context(), oneText())
			require.NoError(t, err)
			require.NotEmpty(t, nativeBodies)
			for _, body := range nativeBodies {
				if tc.request {
					assert.Contains(t, body, `"dimensions":2`)
				} else {
					assert.NotContains(t, body, `"dimensions"`)
				}
			}
		})
	}
}

func TestOllamaNativeRejectsANullComponent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/embeddings":
			_, _ = io.WriteString(w, `{"data":[{"embedding":[null,1]}]}`)
		case "/api/ps":
			_, _ = io.WriteString(w, `{"models":[]}`)
		case "/api/embed":
			_, _ = io.WriteString(w, `{"embeddings":[[null,1]]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := embedclient.New(embedclient.Options{
		Model:               unitModel(),
		Deployment:          embedconfig.Deployment{BaseURL: srv.URL + "/v1"},
		Batch:               embedconfig.Batch{Items: 4},
		OllamaMetalRecovery: true,
	})
	require.NoError(t, err)
	_, err = client.Embed(t.Context(), oneText())
	require.Error(t, err)
	assert.ErrorContains(t, err, "null")
}

func TestOllamaMetalRecoveryFallsBackToCPU(t *testing.T) {
	var nativeBodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/embeddings":
			_, _ = io.WriteString(w, `{"data":[{"embedding":[null,1]}]}`)
		case "/api/ps":
			_, _ = io.WriteString(w, `{"models":[]}`)
		case "/api/embed":
			body, _ := io.ReadAll(r.Body)
			nativeBodies = append(nativeBodies, string(body))
			if len(nativeBodies) < 3 {
				http.Error(w, "runner failed", http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, `{"embeddings":[[3,4]]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := embedclient.New(embedclient.Options{
		Model:               unitModel(),
		Deployment:          embedconfig.Deployment{BaseURL: srv.URL + "/v1"},
		Batch:               embedconfig.Batch{Items: 4},
		OllamaMetalRecovery: true,
	})
	require.NoError(t, err)
	got, err := client.Embed(t.Context(), oneText())
	require.NoError(t, err)
	require.Len(t, nativeBodies, 3)
	assert.NotContains(t, nativeBodies[1], "num_gpu")
	assert.Contains(t, nativeBodies[2], `"num_gpu":0`)
	assert.InDelta(t, 0.6, got[0][0], 0.0001)
}

func TestNewRejectsOllamaRecoveryWithoutAV1Path(t *testing.T) {
	_, err := embedclient.New(embedclient.Options{
		Model:               unitModel(),
		Deployment:          embedconfig.Deployment{BaseURL: "https://example.test/embeddings"},
		OllamaMetalRecovery: true,
	})
	require.Error(t, err)
}

func TestOllamaRecoveryWaitHonorsTheCallerContext(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/embeddings":
			_, _ = io.WriteString(w, `{"data":[{"embedding":[null,1]}]}`)
		case "/api/embed":
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			_, _ = io.WriteString(w, `{"embeddings":[[3,4]]}`)
		case "/api/ps":
			_, _ = io.WriteString(w, `{"models":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := embedclient.New(embedclient.Options{
		Model:               unitModel(),
		Deployment:          embedconfig.Deployment{BaseURL: srv.URL + "/v1"},
		OllamaMetalRecovery: true,
	})
	require.NoError(t, err)

	first := make(chan error, 1)
	go func() {
		_, err := client.Embed(t.Context(), oneText())
		first <- err
	}()
	<-entered // the first recovery holds the gate

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond) //nolint:kennlint // deadlinetest: the deadline is the expected result; the waiter queues behind a recovery gate the test holds
	defer cancel()
	start := time.Now()
	_, err = client.Embed(ctx, oneText())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 5*time.Second, "the waiter returns when its context ends")

	close(release)
	require.NoError(t, <-first)
}

func TestOllamaRecoveryIsNotBoundByTheRequestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/embeddings":
			_, _ = io.WriteString(w, `{"data":[{"embedding":[null,1]}]}`)
		case "/api/embed":
			// A slow reload or CPU pass: answer well after the 50ms
			// request timeout, unless the client gives up first.
			select {
			case <-time.After(300 * time.Millisecond): //nolint:kennlint // deadlinetest: simulated slow reload on a real HTTP server that must outlast the client's 50ms request timeout
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, `{"embeddings":[[3,4]]}`)
		case "/api/ps":
			_, _ = io.WriteString(w, `{"models":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := embedclient.New(embedclient.Options{
		Model:               unitModel(),
		Deployment:          embedconfig.Deployment{BaseURL: srv.URL + "/v1"},
		Transport:           embedconfig.Transport{Timeout: 50 * time.Millisecond}, //nolint:kennlint // deadlinetest: the deliberately slow recovery must outlast the normal HTTP request timeout
		OllamaMetalRecovery: true,
	})
	require.NoError(t, err)
	got, err := client.Embed(t.Context(), oneText())
	require.NoError(t, err)
	assert.InDelta(t, 0.6, got[0][0], 0.0001)
}
