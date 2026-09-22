package reporting

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"go.kenn.io/docbank/report"
	"go.kenn.io/kit/packstore"
)

var ErrStaleRenditionEvidence = errors.New("captured rendition evidence is unavailable or changed")

// CapturedTextReader opens only the immutable artifact named by a frozen
// binding. Its Open function must use the vault's verified blob reader while
// the caller holds the preservation/lifecycle boundary.
type CapturedTextReader struct {
	Open func(context.Context, string) (packstore.VerifiedReadCloser, int64, error)
}

func (r CapturedTextReader) ReadCapturedRendition(ctx context.Context, binding report.TextBinding, budget report.Budget) (_ []byte, retErr error) {
	if r.Open == nil || budget == nil || binding.Kind != "rendition" || binding.Native != nil ||
		binding.Document.NodeID < 1 || binding.Document.VersionID == "" || binding.Document.SHA256 == "" ||
		binding.NodeRevision < 1 || binding.ProfileFingerprint == "" || binding.GenerationID == "" ||
		binding.AttachmentID == "" || binding.BuildID == "" || binding.ArtifactID == "" ||
		binding.ArtifactRole != "sanitized_markdown" || binding.RenditionID != binding.ArtifactID ||
		binding.Size < 0 || binding.Size > 16<<20 || len(binding.ArtifactSHA256) != 64 {
		return nil, ErrStaleRenditionEvidence
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reader, size, err := r.Open(ctx, binding.ArtifactSHA256)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrStaleRenditionEvidence, err)
	}
	defer func() { retErr = errors.Join(retErr, reader.Close()) }()
	if size != binding.Size {
		return nil, ErrStaleRenditionEvidence
	}
	if _, err := budget.Reserve(ctx, size); err != nil {
		return nil, err
	}
	bytes := make([]byte, size)
	if _, err := io.ReadFull(reader, bytes); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrStaleRenditionEvidence, err)
	}
	var extra [1]byte
	n, err := reader.Read(extra[:])
	if n != 0 || !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("artifact is longer than its frozen size")
		}
		return nil, fmt.Errorf("%w: %w", ErrStaleRenditionEvidence, err)
	}
	if err := reader.Verify(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrStaleRenditionEvidence, err)
	}
	digest := sha256.Sum256(bytes)
	if hex.EncodeToString(digest[:]) != binding.ArtifactSHA256 {
		return nil, ErrStaleRenditionEvidence
	}
	return bytes, nil
}
