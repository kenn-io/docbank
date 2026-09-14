package retrieval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"sync"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
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

// MediaEvidenceResolver projects verified immutable artifacts into search locators.
// One resolver belongs to one vault's processing service.
type MediaEvidenceResolver struct {
	blobs  MediaEvidenceBlobReader
	mu     sync.Mutex
	cache  map[store.SearchMediaEvidence]map[string]*MediaTimeSpan
	inputs int
}

const maxCachedMediaInputs = 100_000

func NewMediaEvidenceResolver(blobs MediaEvidenceBlobReader) *MediaEvidenceResolver {
	return &MediaEvidenceResolver{blobs: blobs, cache: make(map[store.SearchMediaEvidence]map[string]*MediaTimeSpan)}
}

func (resolver *MediaEvidenceResolver) resolve(ctx context.Context, artifacts store.SearchMediaEvidence,
	inputID string,
) (*MediaTimeSpan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resolver.mu.Lock()
	locators := resolver.cache[artifacts]
	resolver.mu.Unlock()
	if locators == nil {
		var err error
		locators, err = resolver.load(ctx, artifacts)
		if err != nil {
			return nil, err
		}
		resolver.mu.Lock()
		if resolver.cache[artifacts] == nil && len(locators) <= maxCachedMediaInputs {
			// ponytail: clear at the input ceiling; use LRU if large working sets churn it.
			if resolver.inputs+len(locators) > maxCachedMediaInputs {
				clear(resolver.cache)
				resolver.inputs = 0
			}
			resolver.cache[artifacts] = locators
			resolver.inputs += len(locators)
		}
		resolver.mu.Unlock()
	}
	span, ok := locators[inputID]
	if !ok {
		return nil, errors.New("embedding input is absent from its exact retained generation")
	}
	if span == nil {
		return nil, nil //nolint:nilnil // Untimed inputs have no media interval.
	}
	return new(*span), nil
}

func (resolver *MediaEvidenceResolver) load(ctx context.Context, artifacts store.SearchMediaEvidence) (map[string]*MediaTimeSpan, error) {
	if artifacts.BuildID == "" || artifacts.GenerationBlobHash == "" || artifacts.GenerationEncodedSize <= 0 ||
		artifacts.GenerationChecksum == "" || artifacts.EvidenceFingerprint == "" ||
		artifacts.EvidenceEncodedSize <= 0 || artifacts.InputCount <= 0 {
		return nil, errors.New("rendition-chunk evidence artifact authority is incomplete")
	}
	generationBytes, err := readExactSearchBlob(ctx, resolver.blobs,
		artifacts.GenerationBlobHash, artifacts.GenerationEncodedSize)
	if err != nil {
		return nil, fmt.Errorf("reading embedding generation: %w", err)
	}
	evidenceBytes, err := readExactSearchBlob(ctx, resolver.blobs,
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
	if err := generation.ValidateEvidence(evidence); err != nil {
		return nil, err
	}
	locators := make(map[string]*MediaTimeSpan, len(generation.Inputs))
	for _, input := range generation.Inputs {
		span, err := mediaTimeSpan(evidence.Units[input.SourceSpan.UnitIndex].Locator)
		if err != nil {
			return nil, err
		}
		locators[input.Key] = span
	}
	return locators, nil
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
