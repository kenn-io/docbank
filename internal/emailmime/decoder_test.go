package emailmime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestDecodeKeepsRawAndDisplayIdentities(t *testing.T) {
	headers := "Subject: First\r\nSubject: Second\r\nContent-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: base64\r\n\r\n"
	raw := []byte(headers + "Y2Fm6Q==")
	sum := sha256.Sum256(raw)
	result, err := Decode(t.Context(), hex.EncodeToString(sum[:]), int64(len(raw)), bytes.NewReader(raw), canonicalTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, result.Close()) })
	require.Equal(t, document.EmailOutcome("decoded"), result.Evidence.Outcome)
	require.Len(t, result.Evidence.Inventory.Parts, 1)
	require.Len(t, result.Evidence.Inventory.Messages[0].Fields.Subject, 2)
	for _, tc := range []struct {
		role string
		want []byte
	}{
		{"raw_headers", []byte(headers)}, {"decoded_payload", []byte{'c', 'a', 'f', 0xe9}}, {"body_utf8", []byte("café")},
	} {
		stream, err := result.OpenArtifact(t.Context(), "1", tc.role)
		require.NoError(t, err)
		got, readErr := io.ReadAll(stream)
		closeErr := stream.Close()
		require.NoError(t, errors.Join(readErr, closeErr))
		require.Equal(t, tc.want, got)
	}
	part := result.Evidence.Inventory.Parts[0]
	require.NotEqual(t, part.Payload.SHA256, part.BodyUTF8.SHA256)
}

func TestDecodeDoesNotPublishInvalidUTF8BodyPrefix(t *testing.T) {
	raw := []byte("Content-Type: text/plain; charset=utf-8\r\n\r\nvalid\xff")
	sum := sha256.Sum256(raw)
	result, err := Decode(t.Context(), hex.EncodeToString(sum[:]), int64(len(raw)), bytes.NewReader(raw), canonicalTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, result.Close()) })
	part := result.Evidence.Inventory.Parts[0]
	require.Nil(t, part.BodyUTF8)
	require.Equal(t, document.EmailDisplayFailed, result.Evidence.Inventory.Messages[0].Alternatives[0].DisplayState)
	for _, artifact := range result.Artifacts() {
		require.NotEqual(t, document.EmailArtifactBodyUTF8, artifact.Reference.Role)
	}
}
