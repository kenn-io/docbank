package processing

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/upload"
	"go.kenn.io/docbank/internal/store"
)

type embeddingRuntimeBlobs interface {
	OpenContext(ctx context.Context, hash string) (io.ReadSeekCloser, error)
}

// ProviderEmbeddingRuntime reconstructs exact transient inputs from cataloged
// blob authority for one already-validated provider descriptor.
type ProviderEmbeddingRuntime struct {
	provider       document.EmbeddingProvider
	blobs          embeddingRuntimeBlobs
	spoolDirectory string
	classify       func(error) (EmbeddingProviderFailure, time.Duration)
}

// NewProviderEmbeddingRuntime binds one provider to exact blob reopening. A
// non-empty absolute spool directory is required for direct-file authority.
func NewProviderEmbeddingRuntime(provider document.EmbeddingProvider, blobs embeddingRuntimeBlobs,
	spoolDirectory string, classify func(error) (EmbeddingProviderFailure, time.Duration),
) (*ProviderEmbeddingRuntime, error) {
	if embeddingInterfaceNil(provider) || embeddingInterfaceNil(blobs) || classify == nil {
		return nil, errors.New("embedding provider runtime dependencies are invalid")
	}
	if !filepath.IsAbs(spoolDirectory) {
		return nil, errors.New("embedding provider runtime spool directory must be absolute")
	}
	return &ProviderEmbeddingRuntime{provider: provider, blobs: blobs, spoolDirectory: spoolDirectory, classify: classify}, nil
}

func (runtime *ProviderEmbeddingRuntime) Ready() bool {
	return runtime != nil && !embeddingInterfaceNil(runtime.provider) && !embeddingInterfaceNil(runtime.blobs) && runtime.classify != nil
}

// QueryProvider returns the exact provider only when it reproduces the
// requested persisted descriptor. A merely compatible model is not authority
// for an existing vector space.
func (runtime *ProviderEmbeddingRuntime) QueryProvider(
	descriptor document.EmbeddingDescriptor,
) (document.EmbeddingProvider, error) {
	if !runtime.Ready() || !reflect.DeepEqual(runtime.provider.Descriptor(), descriptor) ||
		!descriptor.SupportsTextQuery {
		return nil, ErrEmbeddingRuntimeUnavailable
	}
	return runtime.provider, nil
}

func (runtime *ProviderEmbeddingRuntime) Classify(err error) (EmbeddingProviderFailure, time.Duration) {
	if runtime == nil || runtime.classify == nil {
		return EmbeddingProviderPermanent, 0
	}
	return runtime.classify(err)
}

func (runtime *ProviderEmbeddingRuntime) Prepare(ctx context.Context, work EmbeddingWork) (EmbeddingExecution, error) {
	if !runtime.Ready() || !reflect.DeepEqual(runtime.provider.Descriptor(), work.Descriptor) {
		return EmbeddingExecution{}, ErrEmbeddingRuntimeUnavailable
	}
	switch work.Binding.InputKind {
	case document.EmbeddingInputRenditionChunk:
		hydrated, err := hydrateEmbeddingGeneration(ctx, runtime.blobs, work.InputGeneration)
		if err != nil {
			return EmbeddingExecution{}, err
		}
		data := hydrated.GenerationJSON
		generation, err := document.DecodeEmbeddingInputGeneration(data, document.EmbeddingInputGenerationDecodeBounds{
			MaxEncodedBytes: hydrated.GenerationEncodedSize, MaxInputs: len(hydrated.Inputs), MaxObjectFields: 32,
			MaxStringBytes: min(hydrated.GenerationEncodedSize, 16<<20), MaxTotalStringBytes: hydrated.GenerationEncodedSize,
		})
		if err != nil {
			return EmbeddingExecution{}, err
		}
		inputs, err := generation.ToEmbeddingInputs(work.Descriptor.ModelInput)
		if err != nil {
			return EmbeddingExecution{}, err
		}
		return EmbeddingExecution{Provider: runtime.provider, Inputs: inputs,
			InputGenerationJSON: data, Classify: runtime.classify}, nil
	case document.EmbeddingInputOriginalFile:
		if work.SourceBytes < 1 || work.Binding.MaxInputBytes < 1 || work.SourceBytes > work.Binding.MaxInputBytes {
			return EmbeddingExecution{}, errors.New("embedding original-file byte authority is invalid")
		}
		reader, err := runtime.blobs.OpenContext(ctx, work.SourceBlobHash)
		if err != nil {
			return EmbeddingExecution{}, err
		}
		work.SourceFilename = syntheticFilename(work.SourceFilename, work.SourceMediaType, false)
		policy := directEmbeddingInspectionPolicy(work)
		capability, err := media.InspectCapability(reader, policy)
		if err != nil {
			_ = reader.Close()
			return EmbeddingExecution{}, err
		}
		if _, err := reader.Seek(0, io.SeekStart); err != nil {
			_ = reader.Close()
			return EmbeddingExecution{}, err
		}
		authorized, err := upload.Authorize(ctx, upload.Source{Reader: reader, Directory: runtime.spoolDirectory},
			capability, upload.UploadMetadata{Filename: work.SourceFilename})
		if err != nil {
			return EmbeddingExecution{}, err
		}
		return EmbeddingExecution{Provider: runtime.provider, Inputs: []document.EmbeddingInput{{
			Key: work.ContentVersionID, Role: document.EmbeddingRoleDocument,
			Kind: document.EmbeddingInputOriginalFile, Source: authorized,
		}}, Classify: runtime.classify}, nil
	default:
		return EmbeddingExecution{}, ErrEmbeddingRuntimeUnavailable
	}
}

func hydrateEmbeddingGeneration(ctx context.Context, blobs embeddingRuntimeBlobs,
	record store.EmbeddingInputGenerationRecord,
) (store.EmbeddingInputGenerationRecord, error) {
	data, err := readExactEmbeddingBlob(ctx, blobs, record.GenerationBlobHash, record.GenerationEncodedSize)
	if err != nil {
		return store.EmbeddingInputGenerationRecord{}, err
	}
	return store.HydrateEmbeddingInputGeneration(record, data)
}

func readExactEmbeddingBlob(ctx context.Context, blobs embeddingRuntimeBlobs, hash string, size int64) ([]byte, error) {
	if hash == "" || size < 1 || size > 64<<20 {
		return nil, errors.New("embedding generation blob authority is invalid")
	}
	reader, err := blobs.OpenContext(ctx, hash)
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(io.LimitReader(reader, size+1))
	if err != nil || int64(len(data)) != size {
		return nil, errors.New("embedding generation blob could not be read exactly")
	}
	return data, nil
}

func directEmbeddingInspectionPolicy(work EmbeddingWork) media.InspectionPolicy {
	maximum := work.Binding.MaxInputBytes
	return media.InspectionPolicy{
		Filename: work.SourceFilename, DeclaredMediaType: work.SourceMediaType,
		ExpectedBytes: work.SourceBytes, ExpectedSHA256: work.SourceBlobHash,
		DescriptorFingerprint: work.Descriptor.Fingerprint,
		ProfileFingerprint:    work.ProcessingProfile.Fingerprint,
		DisclosureFingerprint: work.Binding.DisclosureFingerprint,
		InputKind:             document.RenditionInputOriginalFile, MaxSourceBytes: maximum,
		MaxExpandedBytes: maximum, MaxEntryBytes: maximum, MaxEntries: 100_000, MaxNestingDepth: 1,
		MaxTextLines: 10_000_000, MaxCharacters: 1_000_000_000, MaxRecords: 10_000_000,
		MaxPages: 1_000_000, MaxSlides: 1_000_000, MaxSheets: 1_000_000, MaxCells: 100_000_000,
		MaxSpineItems: 1_000_000, MaxResources: 1_000_000, MaxPixels: 1_000_000_000,
		MaxFrames: 1_000_000, MaxDurationMS: 24 * 60 * 60 * 1000,
	}
}
