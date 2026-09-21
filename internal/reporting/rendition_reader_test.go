package reporting

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/report"
	"go.kenn.io/kit/packstore"
)

type verifiedTestStream struct {
	*bytes.Reader

	verified bool
}

func (r *verifiedTestStream) Close() error   { return nil }
func (r *verifiedTestStream) Verified() bool { return r.verified }
func (r *verifiedTestStream) Verify() error {
	r.verified = true
	return nil
}

func TestCapturedTextReaderUsesFrozenHashAndVerifiesBytes(t *testing.T) {
	content := []byte("Document dated 2024-05-06")
	digest := sha256.Sum256(content)
	hash := hex.EncodeToString(digest[:])
	binding := report.TextBinding{Kind: "rendition", Document: report.Identity{
		NodeID: 1, VersionID: "version-one", SHA256: hash}, NodeRevision: 1,
		ProfileFingerprint: "profile", GenerationID: "generation", AttachmentID: "attachment",
		BuildID: "build", ArtifactID: "artifact", ArtifactRole: "sanitized_markdown",
		ArtifactSHA256: hash, RenditionID: "artifact", Size: int64(len(content))}
	stream := &verifiedTestStream{Reader: bytes.NewReader(content)}
	var opened string
	reader := CapturedTextReader{Open: func(_ context.Context, wanted string) (packstore.VerifiedReadCloser, int64, error) {
		opened = wanted
		return stream, int64(len(content)), nil
	}}
	budget := report.NewBudget(1 << 20)
	defer func() { _ = budget.Close() }()
	got, err := reader.ReadCapturedRendition(t.Context(), binding, budget)
	require.NoError(t, err)
	require.Equal(t, content, got)
	require.Equal(t, hash, opened)
	require.True(t, stream.verified)

	reader.Open = func(context.Context, string) (packstore.VerifiedReadCloser, int64, error) {
		return &verifiedTestStream{Reader: bytes.NewReader([]byte("changed"))}, int64(len(content)), nil
	}
	_, err = reader.ReadCapturedRendition(t.Context(), binding, budget)
	require.ErrorIs(t, err, ErrStaleRenditionEvidence)
	reader.Open = func(context.Context, string) (packstore.VerifiedReadCloser, int64, error) {
		return nil, 0, io.ErrUnexpectedEOF
	}
	_, err = reader.ReadCapturedRendition(t.Context(), binding, budget)
	require.ErrorIs(t, err, ErrStaleRenditionEvidence)

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = reader.ReadCapturedRendition(canceled, binding, budget)
	require.ErrorIs(t, err, context.Canceled)
}
