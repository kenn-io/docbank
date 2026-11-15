package workerprotocol

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"hegel.dev/go/hegel"
)

func TestSubmissionKeyV1Golden(t *testing.T) {
	metadata := document.AuthorizedUploadMetadata{Filename: "sample.pdf", MediaFamily: "pdf", MediaType: "application/pdf", ByteLength: 3, SHA256: "abc", CapabilityRecordChecksum: "cap", ProviderMetadataChecksum: "meta", InputKind: document.RenditionInputOriginalFile}
	authorization := document.RenditionAuthorization{ProviderID: ProviderID, SourceSHA256: "abc", SourceBytes: 3, CapabilityRecordChecksum: "cap", ProviderMetadataChecksum: "meta", MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile, DiscloseFilename: true}
	key, err := SubmissionKey(metadata, authorization, [][2]string{{"to_formats", "json"}})
	require.NoError(t, err)
	require.Equal(t, "75f0a709ed770a727db6b825466e446a8df15f5b2a2fba5ec8ba1a0e10eddb7c", key)
	authorization.AuthorizedAt = "2026-01-01T00:00:00.000000000Z"
	authorization.ExpiresAt = "2026-01-01T01:00:00.000000000Z"
	shifted, err := SubmissionKey(metadata, authorization, [][2]string{{"to_formats", "json"}})
	require.NoError(t, err)
	require.Equal(t, key, shifted)
	hegel.Test(t, func(ht *hegel.T) {
		shift := hegel.Draw(ht, hegel.Integers[int64](-1<<63, 1<<63-1))
		start, err := time.Parse(time.RFC3339Nano, authorization.AuthorizedAt)
		require.NoError(ht, err)
		end, err := time.Parse(time.RFC3339Nano, authorization.ExpiresAt)
		require.NoError(ht, err)
		shiftedAuth := authorization
		shiftedAuth.AuthorizedAt = start.Add(time.Duration(shift)).Format("2006-01-02T15:04:05.000000000Z")
		shiftedAuth.ExpiresAt = end.Add(time.Duration(shift)).Format("2006-01-02T15:04:05.000000000Z")
		got, err := SubmissionKey(metadata, shiftedAuth, [][2]string{{"to_formats", "json"}})
		require.NoError(ht, err)
		require.Equal(ht, key, got)
	})
	authorization.RenditionRequestFingerprint = strings.Repeat("5", 64)
	changed, err := SubmissionKey(metadata, authorization, [][2]string{{"to_formats", "json"}})
	require.NoError(t, err)
	require.NotEqual(t, key, changed)
}
