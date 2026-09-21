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
	cache  map[store.SearchMediaEvidence]map[string]mediaEvidenceInput
	inputs int
	bytes  int64
}

const (
	maxCachedMediaInputs = 100_000
	maxCachedMediaBytes  = 64 << 20
)

type mediaEvidenceInput struct {
	excerpt string
	span    *MediaTimeSpan
}

func NewMediaEvidenceResolver(blobs MediaEvidenceBlobReader) *MediaEvidenceResolver {
	return &MediaEvidenceResolver{blobs: blobs, cache: make(map[store.SearchMediaEvidence]map[string]mediaEvidenceInput)}
}

func (resolver *MediaEvidenceResolver) resolveInput(ctx context.Context, artifacts store.SearchMediaEvidence,
	inputID string,
) (mediaEvidenceInput, error) {
	if err := ctx.Err(); err != nil {
		return mediaEvidenceInput{}, err
	}
	resolver.mu.Lock()
	inputs := resolver.cache[artifacts]
	resolver.mu.Unlock()
	if inputs == nil {
		var err error
		inputs, err = resolver.load(ctx, artifacts)
		if err != nil {
			return mediaEvidenceInput{}, err
		}
		resolver.mu.Lock()
		inputBytes := mediaEvidenceInputBytes(inputs)
		if resolver.cache[artifacts] == nil && len(inputs) <= maxCachedMediaInputs && inputBytes <= maxCachedMediaBytes {
			// ponytail: clear at the input ceiling; use LRU if large working sets churn it.
			if resolver.inputs+len(inputs) > maxCachedMediaInputs || resolver.bytes+inputBytes > maxCachedMediaBytes {
				clear(resolver.cache)
				resolver.inputs, resolver.bytes = 0, 0
			}
			resolver.cache[artifacts] = inputs
			resolver.inputs += len(inputs)
			resolver.bytes += inputBytes
		}
		resolver.mu.Unlock()
	}
	input, ok := inputs[inputID]
	if !ok {
		return mediaEvidenceInput{}, errors.New("embedding input is absent from its exact retained generation")
	}
	if input.span != nil {
		input.span = new(*input.span)
	}
	return input, nil
}

func mediaEvidenceInputBytes(inputs map[string]mediaEvidenceInput) int64 {
	var total int64
	for _, input := range inputs {
		if int64(len(input.excerpt)) > int64(^uint64(0)>>1)-total {
			return int64(^uint64(0) >> 1)
		}
		total += int64(len(input.excerpt))
	}
	return total
}

func (resolver *MediaEvidenceResolver) load(ctx context.Context, artifacts store.SearchMediaEvidence) (map[string]mediaEvidenceInput, error) {
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
	inputs := make(map[string]mediaEvidenceInput, len(generation.Inputs))
	for _, input := range generation.Inputs {
		span, err := mediaTimeSpan(evidence.Units[input.SourceSpan.UnitIndex].Locator)
		if err != nil {
			return nil, err
		}
		inputs[input.Key] = mediaEvidenceInput{excerpt: input.Content, span: span}
	}
	return inputs, nil
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
