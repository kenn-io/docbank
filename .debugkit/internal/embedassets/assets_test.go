package embedassets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCacheVerifiedAndOffline(t *testing.T) {
	data := "public model fixture"
	digest := sha256.Sum256([]byte(data))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(data)) }))
	a := Asset{"model", server.URL, hex.EncodeToString(digest[:]), int64(len(data))}
	dir := t.TempDir()
	require.NoError(t, CacheAsset(t.Context(), server.Client(), dir, a))
	server.Close()
	require.NoError(t, CacheAsset(t.Context(), server.Client(), dir, a))
	got, err := os.ReadFile(filepath.Join(dir, "model"))
	require.NoError(t, err)
	assert.Equal(t, data, string(got))
}

func TestCacheRejectsBadDownloads(t *testing.T) {
	digest := sha256.Sum256([]byte("valid"))
	for _, tc := range []struct {
		name, body string
		size       int64
	}{
		{"digest mismatch", "wrong", 5},
		{"too short", "valid", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "model")
			require.NoError(t, os.WriteFile(path, []byte("old corrupt cache"), 0o600))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.body)) }))
			defer server.Close()
			a := Asset{"model", server.URL, hex.EncodeToString(digest[:]), tc.size}
			require.Error(t, CacheAsset(t.Context(), server.Client(), dir, a))
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, "old corrupt cache", string(got))
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			assert.Len(t, entries, 1)
		})
	}
}

func TestVerifyStopsAfterSizeLimit(t *testing.T) {
	reader := strings.NewReader("valid trailing")
	digest := sha256.Sum256([]byte("vali"))
	require.Error(t, verify(reader, io.Discard, Asset{Size: 4, Digest: hex.EncodeToString(digest[:])}))
	assert.Equal(t, 9, reader.Len())
}

func TestCacheInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer func() { cancel(); server.CloseClientConnections(); server.Close() }()
	dir := t.TempDir()
	a := Asset{"model", server.URL, "unused", 100}
	done := make(chan error, 1)
	go func() { done <- CacheAsset(ctx, server.Client(), dir, a) }()
	require.Eventually(t, func() bool {
		entries, err := os.ReadDir(dir)
		return err == nil && len(entries) == 1 && entries[0].Name() != "model"
	}, 5*time.Second, time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "cancellation did not interrupt the download")
	}
}

func TestCacheModelUsesPinnedURLAndCacheLayout(t *testing.T) {
	previousTransport, previousAssets := http.DefaultTransport, modelAssets
	var called atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		called.Store(true)
		assert.Equal(t, "https://huggingface.co/nomic-ai/nomic-embed-text-v1.5/resolve/"+revision+"/unused", "https://"+req.Host+req.URL.RequestURI())
		_, _ = w.Write([]byte("valid"))
	}))
	defer server.Close()
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = "example.com"
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	defer transport.CloseIdleConnections()
	http.DefaultTransport = transport
	digest := sha256.Sum256([]byte("valid"))
	modelAssets = []Asset{{"model.onnx", "unused", hex.EncodeToString(digest[:]), 5}}
	defer func() { http.DefaultTransport, modelAssets = previousTransport, previousAssets }()
	dir := t.TempDir()
	cached := filepath.Join(dir, "nomic-embed-text-v1.5", revision)
	got, err := CacheModel(t.Context(), dir)
	require.NoError(t, err)
	assert.True(t, called.Load())
	assert.Equal(t, cached, got)
	data, err := os.ReadFile(filepath.Join(cached, "model.onnx"))
	require.NoError(t, err)
	assert.Equal(t, "valid", string(data))
}

func TestMissingModel(t *testing.T) {
	dir := t.TempDir()
	digest := sha256.Sum256([]byte("valid"))
	assets := []Asset{{Name: "model.onnx", Digest: hex.EncodeToString(digest[:]), Size: 5}}
	path := filepath.Join(dir, "model.onnx")
	require.NoError(t, os.WriteFile(path, []byte("wrong"), 0o600))
	missing, err := MissingModel(dir, assets)
	require.NoError(t, err)
	assert.Equal(t, assets, missing)
	assert.FileExists(t, path)
}
