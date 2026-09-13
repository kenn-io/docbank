package client_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

func TestClientEmailRoundTrip(t *testing.T) {
	c, s := newClient(t, serverKey)
	raw := "Subject: Client\r\nBcc: hidden@example.test\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nclient-email-marker"
	sum := sha256.Sum256([]byte(raw))
	uploaded, err := c.Upload(t.Context(), s.RootID(), "client.eml", "message/rfc822",
		fmtHash(sum), int64(len(raw)), strings.NewReader(raw))
	require.NoError(t, err)
	pending, err := c.EmailMetadata(t.Context(), uploaded.Node.CurrentVersionID)
	require.ErrorIs(t, err, store.ErrEmailPending)
	require.Equal(t, uploaded.Node.CurrentVersionID, pending.Version.ID)

	metadata, err := c.EnsureEmailMetadata(t.Context(), uploaded.Node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, uploaded.Node.CurrentVersionID, metadata.Version.ID)
	require.Len(t, metadata.Evidence.Inventory.Messages[0].Fields.Bcc, 1)
	exact, err := c.EmailMetadataGeneration(t.Context(), metadata.Version.ID, metadata.GenerationID)
	require.NoError(t, err)
	require.Equal(t, metadata, exact)

	stream, receipt, err := c.OpenEmailPart(
		t.Context(), metadata.Version.ID, metadata.GenerationID, "1", "decoded_payload",
	)
	require.NoError(t, err)
	payload, readErr := io.ReadAll(stream)
	closeErr := stream.Close()
	require.NoError(t, errors.Join(readErr, closeErr))
	assert.Equal(t, []byte("client-email-marker"), payload)
	assert.Equal(t, metadata.Version.ID, receipt.Version.ID)
	assert.Equal(t, metadata.GenerationID, receipt.GenerationID)
}

func TestClientEmailPartRejectsUnprovenResponses(t *testing.T) {
	metadata, payload := realEmailMetadata(t)
	tests := []struct {
		name   string
		mutate func(http.Header, *[]byte, *string)
		early  bool
	}{
		{"version selector", func(h http.Header, _ *[]byte, _ *string) {
			h.Set(api.ContentVersionHeader, "22222222-2222-4222-8222-222222222222")
		}, false},
		{"generation selector", func(h http.Header, _ *[]byte, _ *string) { h.Set(api.EmailGenerationHeader, strings.Repeat("0", 64)) }, false},
		{"attachment selector", func(h http.Header, _ *[]byte, _ *string) { h.Set(api.EmailAttachmentHeader, strings.Repeat("0", 64)) }, false},
		{"part selector", func(h http.Header, _ *[]byte, _ *string) { h.Set(api.EmailPartPathHeader, "2") }, false},
		{"role selector", func(h http.Header, _ *[]byte, _ *string) { h.Set(api.EmailPartRoleHeader, "body_utf8") }, false},
		{"forged hash", func(h http.Header, _ *[]byte, _ *string) { h.Set(api.BlobHashHeader, strings.Repeat("0", 64)) }, false},
		{"forged size", func(h http.Header, _ *[]byte, _ *string) { h.Set(api.BlobSizeHeader, "999") }, false},
		{"wrong payload", func(_ http.Header, body *[]byte, _ *string) { *body = []byte(strings.Repeat("x", len(*body))) }, false},
		{"short payload", func(_ http.Header, body *[]byte, _ *string) { *body = (*body)[:len(*body)-1] }, false},
		{"overflow payload", func(_ http.Header, body *[]byte, _ *string) { *body = append(*body, 'x') }, false},
		{"wrong digest", func(_ http.Header, _ *[]byte, digest *string) {
			*digest = "sha-256=:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=:"
		}, false},
		{"premature close", func(_ http.Header, _ *[]byte, _ *string) {}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := adversarialEmailServer(t, metadata, payload, tc.mutate)
			stream, _, err := client.New(ts.URL, "").OpenEmailPart(
				t.Context(), metadata.Version.ID, metadata.GenerationID, "1", "decoded_payload",
			)
			if err != nil {
				require.ErrorIs(t, err, client.ErrIntegrity)
				return
			}
			if tc.early {
				err = stream.Close()
				require.ErrorIs(t, err, client.ErrIntegrity)
				return
			}
			_, readErr := io.ReadAll(stream)
			closeErr := stream.Close()
			require.ErrorIs(t, errors.Join(readErr, closeErr), client.ErrIntegrity)
		})
	}
}

func TestClientEmailPartCloseCancelsBlockedRead(t *testing.T) {
	metadata, _ := realEmailMetadata(t)
	part := emailPartByPath(t, metadata.Evidence, "1")
	require.NotNil(t, part.Payload)
	release := make(chan struct{})
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/parts/") {
			_ = json.MarshalWrite(w, metadata)
			return
		}
		headers := w.Header()
		headers.Set("Content-Type", "application/octet-stream")
		headers.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": emailTestPartFilename(t, part)}))
		headers.Set("X-Content-Type-Options", "nosniff")
		headers.Set(api.ContentVersionHeader, metadata.Version.ID)
		headers.Set(api.EmailGenerationHeader, metadata.GenerationID)
		headers.Set(api.EmailAttachmentHeader, metadata.AttachmentID)
		headers.Set(api.EmailPartPathHeader, "1")
		headers.Set(api.EmailPartRoleHeader, "decoded_payload")
		headers.Set(api.BlobHashHeader, part.Payload.SHA256)
		headers.Set(api.BlobSizeHeader, strconv.FormatInt(part.Payload.Size, 10))
		headers.Set("Trailer", "Content-Digest")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test response writer does not support flushing")
			return
		}
		flusher.Flush()
		<-release
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		server.Close()
	})
	stream, _, err := client.New(server.URL, "").OpenEmailPart(
		t.Context(), metadata.Version.ID, metadata.GenerationID, "1", "decoded_payload",
	)
	require.NoError(t, err)
	readStarted := make(chan struct{})
	readDone := make(chan error, 1)
	go func() {
		close(readStarted)
		_, readErr := stream.Read(make([]byte, 1))
		readDone <- readErr
	}()
	<-readStarted
	closeDone := make(chan error, 1)
	go func() { closeDone <- stream.Close() }()
	select {
	case err := <-closeDone:
		require.ErrorIs(t, err, client.ErrIntegrity)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "closing email part did not cancel its blocked read")
	}
	select {
	case err := <-readDone:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "blocked email part read did not return after close")
	}
	releaseOnce.Do(func() { close(release) })
}

func TestClientEmailMetadataRejectsForgedIdentity(t *testing.T) {
	metadata, _ := realEmailMetadata(t)
	tests := map[string]func(*api.EmailMetadata){
		"version":    func(m *api.EmailMetadata) { m.Version.ID = "22222222-2222-4222-8222-222222222222" },
		"generation": func(m *api.EmailMetadata) { m.GenerationID = strings.Repeat("0", 64) },
		"attachment": func(m *api.EmailMetadata) { m.AttachmentID = strings.Repeat("0", 64) },
		"recipe":     func(m *api.EmailMetadata) { m.RecipeFingerprint = strings.Repeat("0", 64) },
		"checksum":   func(m *api.EmailMetadata) { m.Checksum = strings.Repeat("0", 64) },
		"source":     func(m *api.EmailMetadata) { m.Evidence.Source.SHA256 = strings.Repeat("0", 64) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			forged := metadata
			mutate(&forged)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.MarshalWrite(w, forged)
			}))
			t.Cleanup(server.Close)
			_, err := client.New(server.URL, "").EmailMetadataGeneration(
				t.Context(), metadata.Version.ID, metadata.GenerationID,
			)
			require.ErrorIs(t, err, client.ErrIntegrity)
		})
	}
}

func realEmailMetadata(t *testing.T) (api.EmailMetadata, []byte) {
	t.Helper()
	c, s := newClient(t, serverKey)
	payload := []byte("adversarial-email-part")
	raw := "Content-Type: text/plain; charset=utf-8\r\n\r\n" + string(payload)
	sum := sha256.Sum256([]byte(raw))
	uploaded, err := c.Upload(t.Context(), s.RootID(), "adversarial.eml", "message/rfc822",
		fmtHash(sum), int64(len(raw)), strings.NewReader(raw))
	require.NoError(t, err)
	metadata, err := c.EnsureEmailMetadata(t.Context(), uploaded.Node.CurrentVersionID)
	require.NoError(t, err)
	return metadata, payload
}

func adversarialEmailServer(
	t *testing.T, metadata api.EmailMetadata, payload []byte,
	mutate func(http.Header, *[]byte, *string),
) *httptest.Server {
	t.Helper()
	part := emailPartByPath(t, metadata.Evidence, "1")
	require.NotNil(t, part.Payload)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/parts/") {
			w.Header().Set("Content-Type", "application/json")
			if err := json.MarshalWrite(w, metadata); err != nil {
				t.Errorf("write metadata: %v", err)
			}
			return
		}
		headers := w.Header()
		headers.Set("Content-Type", "application/octet-stream")
		headers.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": emailTestPartFilename(t, part)}))
		headers.Set("X-Content-Type-Options", "nosniff")
		headers.Set(api.ContentVersionHeader, metadata.Version.ID)
		headers.Set(api.EmailGenerationHeader, metadata.GenerationID)
		headers.Set(api.EmailAttachmentHeader, metadata.AttachmentID)
		headers.Set(api.EmailPartPathHeader, "1")
		headers.Set(api.EmailPartRoleHeader, "decoded_payload")
		headers.Set(api.BlobHashHeader, part.Payload.SHA256)
		headers.Set(api.BlobSizeHeader, strconv.FormatInt(part.Payload.Size, 10))
		headers.Set("Trailer", "Content-Digest")
		body := append([]byte(nil), payload...)
		sum := sha256.Sum256(body)
		digest := "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
		if mutate != nil {
			mutate(headers, &body, &digest)
		}
		_, _ = w.Write(body)
		headers.Set("Content-Digest", digest)
	}))
	t.Cleanup(server.Close)
	return server
}

func emailPartByPath(t *testing.T, evidence document.EmailV1, path string) document.EmailPartV1 {
	t.Helper()
	require.NotNil(t, evidence.Inventory)
	for _, part := range evidence.Inventory.Parts {
		if part.Path == path {
			return part
		}
	}
	require.FailNow(t, "email part missing", "path: %s", path)
	return document.EmailPartV1{}
}

func emailTestPartFilename(t *testing.T, part document.EmailPartV1) string {
	t.Helper()
	if part.Filename.SafeName != "" {
		return part.Filename.SafeName
	}
	name, err := document.SafeEmailFilename("", part.Path)
	require.NoError(t, err)
	return name
}

func fmtHash(sum [sha256.Size]byte) string {
	return hex.EncodeToString(sum[:])
}
