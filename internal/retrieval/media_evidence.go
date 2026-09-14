package retrieval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"

	"go.kenn.io/docbank/document"
	"go.kenn.io/kit/packstore"
)

// MediaEvidenceBlobReader opens exact retained evidence artifacts. Readers
// must authenticate the bytes before they can be projected into search output.
type MediaEvidenceBlobReader interface {
	OpenStreamContext(ctx context.Context, hash string) (packstore.VerifiedReadCloser, int64, error)
}

func mediaTimeSpan(locator document.EvidenceLocatorV1) (*MediaTimeSpan, error) {
	if locator.Kind != document.EvidenceLocatorSegment {
		return nil, nil //nolint:nilnil // Absence is the truthful result for an untimed locator.
	}
	if locator.IndexOrigin != document.EvidenceIndexOriginZero || locator.Start < 0 || locator.End <= locator.Start {
		return nil, errors.New("invalid retained media locator")
	}
	return &MediaTimeSpan{StartMS: locator.Start, EndMS: locator.End}, nil
}

func (searcher *Searcher) attachMediaEvidence(ctx context.Context, report Report) (Report, error) {
	for resultIndex := range report.Results {
		references := report.Results[resultIndex].Evidence
		projected := make([]EvidenceReference, 0, len(references))
		for _, reference := range references {
			resolved, err := searcher.resolveMediaEvidence(ctx, reference)
			if err != nil {
				return Report{}, fmt.Errorf("resolving %s search evidence: %w", reference.Kind, err)
			}
			projected = append(projected, resolved...)
		}
		report.Results[resultIndex].Evidence = projected
	}
	return report, nil
}

func (searcher *Searcher) resolveMediaEvidence(
	ctx context.Context, reference EvidenceReference,
) ([]EvidenceReference, error) {
	if reference.mediaLocator != nil {
		span, err := mediaTimeSpan(*reference.mediaLocator)
		if err != nil {
			return nil, err
		}
		reference.TimeSpan = span
		reference.mediaLocator = nil
		return []EvidenceReference{reference}, nil
	}
	if reference.InputKind != document.EmbeddingInputRenditionChunk {
		return []EvidenceReference{reference}, nil
	}
	if reference.mediaArtifacts == nil {
		return nil, errors.New("rendition-chunk evidence lacks exact retained artifact authority")
	}
	artifacts := *reference.mediaArtifacts
	if artifacts.BuildID == "" || artifacts.GenerationBlobHash == "" || artifacts.GenerationEncodedSize <= 0 ||
		artifacts.GenerationChecksum == "" || artifacts.EvidenceFingerprint == "" ||
		artifacts.EvidenceEncodedSize <= 0 || artifacts.InputCount <= 0 {
		return nil, errors.New("rendition-chunk evidence artifact authority is incomplete")
	}
	generationBytes, err := readExactSearchBlob(ctx, searcher.mediaEvidence,
		artifacts.GenerationBlobHash, artifacts.GenerationEncodedSize)
	if err != nil {
		return nil, fmt.Errorf("reading embedding generation: %w", err)
	}
	evidenceBytes, err := readExactSearchBlob(ctx, searcher.mediaEvidence,
		artifacts.EvidenceFingerprint, artifacts.EvidenceEncodedSize)
	if err != nil {
		return nil, fmt.Errorf("reading normalized evidence: %w", err)
	}
	generation, err := document.DecodeEmbeddingInputGeneration(generationBytes,
		document.EmbeddingInputGenerationDecodeBounds{
			MaxEncodedBytes: artifacts.GenerationEncodedSize,
			MaxInputs:       artifacts.InputCount,
		})
	if err != nil {
		return nil, err
	}
	if generation.Checksum != artifacts.GenerationChecksum ||
		generation.EvidenceChecksum != artifacts.EvidenceFingerprint {
		return nil, errors.New("embedding generation disagrees with catalog authority")
	}
	var evidence document.NormalizedEvidenceV1
	if err := json.Unmarshal(evidenceBytes, &evidence, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("decoding normalized evidence: %w", err)
	}
	canonicalEvidence, checksum, err := document.MarshalNormalizedEvidenceV1(evidence)
	if err != nil {
		return nil, err
	}
	if checksum != artifacts.EvidenceFingerprint || !bytes.Equal(canonicalEvidence, evidenceBytes) {
		return nil, errors.New("normalized evidence is not the exact canonical retained artifact")
	}
	if err := generation.ValidateEvidence(evidence); err != nil {
		return nil, err
	}
	for _, input := range generation.Inputs {
		if input.Key != reference.InputID {
			continue
		}
		if input.SourceSpan.UnitIndex < 0 || input.SourceSpan.UnitIndex >= len(evidence.Units) {
			return nil, errors.New("embedding input names a missing evidence unit")
		}
		span, err := mediaTimeSpan(evidence.Units[input.SourceSpan.UnitIndex].Locator)
		if err != nil {
			return nil, err
		}
		reference.BuildID = artifacts.BuildID
		reference.TimeSpan = span
		reference.mediaArtifacts = nil
		return []EvidenceReference{reference}, nil
	}
	return nil, errors.New("embedding input is absent from its exact retained generation")
}

func readExactSearchBlob(
	ctx context.Context, blobs MediaEvidenceBlobReader, hash string, expectedSize int64,
) (_ []byte, retErr error) {
	if blobs == nil {
		return nil, errors.New("media evidence blob reader is unavailable")
	}
	stream, size, err := blobs.OpenStreamContext(ctx, hash)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, stream.Close()) }()
	if size != expectedSize {
		return nil, errors.New("retained artifact size disagrees with catalog authority")
	}
	data, err := io.ReadAll(io.LimitReader(stream, expectedSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != expectedSize {
		return nil, errors.New("retained artifact bytes disagree with catalog authority")
	}
	if err := stream.Verify(); err != nil {
		return nil, fmt.Errorf("verifying retained artifact: %w", err)
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != hash {
		return nil, errors.New("retained artifact checksum disagrees with catalog authority")
	}
	return data, nil
}
