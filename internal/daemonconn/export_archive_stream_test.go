package daemonconn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"uuid"
)

func TestExportArchiveStreamChecksExactTransportBytes(t *testing.T) {
	id := uuid.New().String()
	body := []byte("synthetic archive bytes")
	digest := sha256.Sum256(body)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("archive method = %q, want GET", r.Method)
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		if want := "/api/v1/exports/jobs/" + id + "/archive"; r.URL.Path != want {
			t.Errorf("archive path = %q, want %q", r.URL.Path, want)
			http.Error(w, "wrong path", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("X-Api-Key"); got != "synthetic-key" {
			t.Errorf("archive API key = %q, want synthetic-key", got)
			http.Error(w, "wrong API key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Docbank-Archive-Sha256", hex.EncodeToString(digest[:]))
		w.Header().Set("Docbank-Plan-Fingerprint", strings.Repeat("a", 64))
		_, _ = w.Write(body)
	}))
	defer server.Close()
	connection := New(server.URL, "synthetic-key")
	stream, err := connection.OpenExportArchive(context.Background(), id)
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()
	got, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.Equal(t, body, got)

	stream, err = connection.OpenExportArchive(context.Background(), id)
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()
	var output strings.Builder
	_, err = stream.CopyVerified(&output)
	require.NoError(t, err)
	require.Equal(t, string(body), output.String())

	_, err = connection.OpenExportArchive(context.Background(), "not-a-job")
	require.Error(t, err)
}

func TestExportArchiveStreamRejectsDigestMismatch(t *testing.T) {
	id := uuid.New().String()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Length", "5")
		w.Header().Set("Docbank-Archive-Sha256", strings.Repeat("0", 64))
		w.Header().Set("Docbank-Plan-Fingerprint", strings.Repeat("a", 64))
		_, _ = io.WriteString(w, "other")
	}))
	defer server.Close()
	stream, err := New(server.URL, "synthetic-key").OpenExportArchive(t.Context(), id)
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()
	_, err = stream.CopyVerified(io.Discard)
	require.ErrorContains(t, err, "digest")
}
