package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

type failingRoundTripper func(*http.Request) (*http.Response, error)

func (f failingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestMCPRequestPathsDoNotClassifyRedirectResponseAsTransport(t *testing.T) {
	const maxBytes = 16
	attachmentID := strings.Repeat("a", 64)
	tests := []struct {
		name string
		call func(context.Context, *Client) error
	}{
		{name: "JSON", call: func(ctx context.Context, c *Client) error { return c.Health(ctx) }},
		{name: "processing start", call: func(ctx context.Context, c *Client) error {
			_, err := c.StartProcessing(ctx, api.StartProcessingRequest{})
			return err
		}},
		{name: "rendition", call: func(ctx context.Context, c *Client) error {
			_, err := c.Rendition(ctx, attachmentID, maxBytes)
			return err
		}},
		{name: "rendition selector", call: func(ctx context.Context, c *Client) error {
			_, err := c.RenditionForSelector(ctx, api.ProcessingSelector{}, maxBytes)
			return err
		}},
		{name: "rendition range", call: func(ctx context.Context, c *Client) error {
			_, err := c.RenditionRange(ctx, attachmentID, 0, maxBytes)
			return err
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Location", "/moved")
				response.WriteHeader(http.StatusTemporaryRedirect)
			}))
			t.Cleanup(server.Close)
			c := New(server.URL, "synthetic-key")
			c.hc.CheckRedirect = func(*http.Request, []*http.Request) error {
				return errors.New("synthetic redirect rejection")
			}

			err := testCase.call(t.Context(), c)
			require.Error(t, err)
			assert.False(t, IsTransportError(err), "a received response must never be replayable")
			status, received := responseStatus(err)
			assert.True(t, received)
			assert.Equal(t, http.StatusTemporaryRedirect, status)
		})
	}
}

func TestStartProcessingClassifiesTruncatedStreamAsReceivedResponse(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "truncated stream", contentType: "application/x-ndjson", body: `{"sequence":1`},
		{name: "invalid content type", contentType: "application/json", body: `{}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", testCase.contentType)
				_, _ = response.Write([]byte(testCase.body))
			}))
			t.Cleanup(server.Close)

			_, err := New(server.URL, "synthetic-key").StartProcessing(t.Context(), api.StartProcessingRequest{})
			require.Error(t, err)
			assert.False(t, IsTransportError(err))
			assert.True(t, IsResponseDecodeError(err), "processing had a successful response")
		})
	}
}

func TestRequestOutcomeClassifiesOnlyFailureBeforeResponseAsTransport(t *testing.T) {
	t.Run("before response", func(t *testing.T) {
		c := New("http://daemon.invalid", "synthetic-key")
		c.hc.Transport = failingRoundTripper(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("synthetic dial failure")
		})

		err := c.Health(t.Context())
		require.Error(t, err)
		assert.True(t, IsTransportError(err))
		assert.False(t, IsResponseDecodeError(err))
	})

	t.Run("redirect response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			response.Header().Set("Location", "/moved")
			response.WriteHeader(http.StatusTemporaryRedirect)
		}))
		t.Cleanup(server.Close)
		c := New(server.URL, "synthetic-key")
		c.hc.CheckRedirect = func(*http.Request, []*http.Request) error {
			return errors.New("synthetic redirect rejection")
		}

		err := c.Health(t.Context())
		require.Error(t, err)
		assert.False(t, IsTransportError(err), "a received response must never be replayable")
		status, received := responseStatus(err)
		assert.True(t, received)
		assert.Equal(t, http.StatusTemporaryRedirect, status)
	})

	t.Run("partial successful response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"id":`))
		}))
		t.Cleanup(server.Close)

		_, err := New(server.URL, "synthetic-key").Node(t.Context(), 1)
		require.Error(t, err)
		assert.False(t, IsTransportError(err))
		assert.True(t, IsResponseDecodeError(err))
	})
}
