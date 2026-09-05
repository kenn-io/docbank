package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

// Mutation caught: accepting a store-local input-kind vocabulary lets a caller
// stage authority that does not name the exact E1 processing-profile binding.
func TestEmbeddingCatalogUsesCanonicalInputKindsAndProfileAuthority(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	record.BindingID = "not-in-profile"
	require.ErrorContains(t, s.StageEmbeddingSet(t.Context(), record), "processing profile binding")
}

func TestEmbeddingCatalogDoesNotPersistGenerationTextInSQLiteOrMetadata(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	require.Contains(t, string(record.InputGeneration.GenerationJSON), "Synthetic evidence")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))

	var columns string
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info('embedding_input_generations') ORDER BY cid`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var columnsSb34 strings.Builder
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		columnsSb34.WriteString(name + "\n")
	}
	require.NoError(t, rows.Err())
	columns += columnsSb34.String()
	require.NotContains(t, columns, "generation_json")

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	require.NotContains(t, exported.String(), "Synthetic evidence")
	require.Contains(t, exported.String(), record.InputGeneration.GenerationBlobHash)
}

func TestEmbeddingCatalogRejectsOriginalFileAttachmentBypassAndVectorHeaderMismatch(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	original := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	original.InputGeneration.AttachmentID = attachmentID
	require.ErrorContains(t, s.StageEmbeddingSet(t.Context(), original), "cannot claim rendition attachment")

	chunk := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	decoded, _, err := document.DecodeVectorSetV1(chunk.VectorSet.Payload, document.VectorBounds{MaxRows: 128, MaxDimension: 8, MaxBytes: len(chunk.VectorSet.Payload)})
	require.NoError(t, err)
	values := make([][]float64, len(decoded.Vectors))
	for index, vector := range decoded.Vectors {
		values[index] = make([]float64, len(vector))
		for column, scalar := range vector {
			values[index][column] = float64(scalar)
		}
	}
	wrong, err := document.NewVectorSetV1(document.VectorSetV1Input{
		VectorSpaceFingerprint: decoded.VectorSpaceFingerprint, Metric: document.VectorMetricDotProduct,
		Normalization: decoded.Normalization, Dimension: decoded.Dimension,
		InputKeys: decoded.InputKeys, InputChecksums: decoded.InputChecksums, Values: values,
	})
	require.NoError(t, err)
	payload, checksum, err := document.EncodeVectorSetV1(wrong)
	require.NoError(t, err)
	chunk.VectorSet.Payload = payload
	chunk.VectorSet.ID, chunk.VectorSet.PayloadChecksum, chunk.VectorSet.PayloadBlobHash = checksum, checksum, testSHA256(payload)
	require.NoError(t, s.withStorageTx(context.Background(), func(tx *sql.Tx) error {
		return s.EnsureBlobTx(tx, chunk.VectorSet.PayloadBlobHash, int64(len(payload)))
	}))
	require.ErrorContains(t, s.StageEmbeddingSet(t.Context(), chunk), "metric or normalization")
}

func TestEmbeddingCatalogRejectsGenerationBlobIdentityMismatch(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	record.InputGeneration.GenerationBlobHash = fakeHash("wrong generation blob")
	require.ErrorContains(t, s.StageEmbeddingSet(t.Context(), record), "blob hash")
}

func TestEmbeddingCatalogRejectsGenerationFromDifferentChunkPolicy(t *testing.T) {
	s, versionID, profileRecord, attachmentID := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profileRecord.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	var profile document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(profileRecord.CanonicalProfile, &profile))
	_, fingerprints, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	var binding document.EmbeddingBindingV1
	for _, candidate := range profile.Embeddings {
		if candidate.Name == "chunk" {
			binding = candidate
		}
	}
	changedBinding := binding
	changedChunk := *binding.Chunk
	changedChunk.OverlapTokens = 1
	changedBinding.Chunk = &changedChunk
	contextSnapshot, err := document.NewAttachmentContextSnapshot("Synthetic title", "Synthetic context")
	require.NoError(t, err)
	policy, err := document.NewInputPolicy(changedBinding, embeddingCatalogTokenizer{}, fingerprints.EvidenceLexical, &contextSnapshot)
	require.NoError(t, err)
	generation, err := document.BuildEmbeddingInputs(embeddingCatalogEvidence(t), policy, document.GenerationLimits{
		MaxInputs: 128, MaxTotalContentTokens: 4096, MaxTotalRenderedTokens: 8192,
		MaxTotalContentBytes: 1 << 20, MaxTotalRenderedBytes: 2 << 20,
		MaxFittingWorkTokens: 1 << 20, MaxFittingWorkBytes: 8 << 20,
	})
	require.NoError(t, err)
	encoded, err := document.MarshalEmbeddingInputGeneration(generation)
	require.NoError(t, err)
	record.InputGeneration.GenerationJSON = encoded
	record.InputGeneration.GenerationBlobHash = testSHA256(encoded)
	record.InputGeneration.GenerationEncodedSize = int64(len(encoded))
	record.InputGeneration.GenerationChecksum = generation.Checksum
	record.InputGeneration.ID = testSHA256([]byte("embedding-generation-attachment/v1\x00" + generation.Checksum + "\x00" + attachmentID))
	require.NoError(t, s.withStorageTx(context.Background(), func(tx *sql.Tx) error {
		return s.EnsureBlobTx(tx, record.InputGeneration.GenerationBlobHash, int64(len(encoded)))
	}))
	require.ErrorContains(t, s.StageEmbeddingSet(t.Context(), record), "exact processing profile")
}

func TestEmbeddingCatalogRejectsGenerationOutsideInputAuthority(t *testing.T) {
	for _, mutation := range []string{"rendered", "content"} {
		t.Run(mutation, func(t *testing.T) {
			s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
			record := embeddingSetFixture(s, versionID, profile.Fingerprint,
				document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
			generation, err := document.DecodeEmbeddingInputGeneration(
				record.InputGeneration.GenerationJSON,
				document.EmbeddingInputGenerationDecodeBounds{
					MaxEncodedBytes: int64(len(record.InputGeneration.GenerationJSON)), MaxInputs: 128,
				},
			)
			require.NoError(t, err)

			input := &generation.Inputs[0]
			mutatedRendered := strings.Repeat("x", len(input.Rendered))
			require.NotEqual(t, input.Rendered, mutatedRendered)
			if mutation == "content" {
				content := strings.Repeat("x", len(input.Content))
				input.Rendered = strings.Replace(input.Rendered, input.Content, content, 1)
				input.Content = content
			} else {
				input.Rendered = mutatedRendered
			}
			input.Checksum = testSHA256([]byte(input.Rendered))
			input.Key = fmt.Sprintf("chunk-%06d-%s", 0, input.Checksum[:12])
			generation.Checksum = ""
			checksumInput, err := canonical.Marshal(generation)
			require.NoError(t, err)
			generation.Checksum = testSHA256(checksumInput)
			encodedGeneration, err := document.MarshalEmbeddingInputGeneration(generation)
			require.NoError(t, err)
			record.InputGeneration.GenerationJSON = encodedGeneration
			record.InputGeneration.GenerationBlobHash = testSHA256(encodedGeneration)
			record.InputGeneration.GenerationEncodedSize = int64(len(encodedGeneration))
			record.InputGeneration.GenerationChecksum = generation.Checksum
			record.InputGeneration.ID = testSHA256([]byte(
				"embedding-generation-attachment/v1\x00" + generation.Checksum + "\x00" + attachmentID,
			))

			decodedVectors, _, err := document.DecodeVectorSetV1(record.VectorSet.Payload, document.VectorBounds{
				MaxRows: 128, MaxDimension: record.VectorSpace.Descriptor.Dimension, MaxBytes: len(record.VectorSet.Payload),
			})
			require.NoError(t, err)
			decodedVectors.InputKeys[0] = input.Key
			decodedVectors.InputChecksums[0] = input.Checksum
			values := make([][]float64, len(decodedVectors.Vectors))
			for row := range decodedVectors.Vectors {
				values[row] = make([]float64, len(decodedVectors.Vectors[row]))
				for column, value := range decodedVectors.Vectors[row] {
					values[row][column] = float64(value)
				}
			}
			mutatedVectors, err := document.NewVectorSetV1(document.VectorSetV1Input{
				VectorSpaceFingerprint: decodedVectors.VectorSpaceFingerprint,
				Metric:                 decodedVectors.Metric,
				Normalization:          decodedVectors.Normalization,
				Dimension:              decodedVectors.Dimension,
				InputKeys:              decodedVectors.InputKeys,
				InputChecksums:         decodedVectors.InputChecksums,
				Values:                 values,
			})
			require.NoError(t, err)
			encodedVectors, vectorChecksum, err := document.EncodeVectorSetV1(mutatedVectors)
			require.NoError(t, err)
			record.VectorSet.Payload = encodedVectors
			record.VectorSet.ID = vectorChecksum
			record.VectorSet.PayloadChecksum = vectorChecksum
			record.VectorSet.PayloadBlobHash = testSHA256(encodedVectors)
			require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
				if err := s.EnsureBlobTx(tx, record.InputGeneration.GenerationBlobHash, int64(len(encodedGeneration))); err != nil {
					return err
				}
				return s.EnsureBlobTx(tx, record.VectorSet.PayloadBlobHash, int64(len(encodedVectors)))
			}))

			want := "model-input contract"
			if mutation == "content" {
				want = "source span"
			}
			require.ErrorContains(t, s.StageEmbeddingSet(t.Context(), record), want)
			if mutation == "content" {
				// Simulate imported orphan authority: no set remains to validate the
				// generation on its behalf. Restore must still reject the forged text.
				normalized, err := normalizeEmbeddingSetRecord(record)
				require.NoError(t, err)
				require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
					return insertInputGenerationTx(t.Context(), tx, normalized.InputGeneration)
				}))
				var metadata bytes.Buffer
				require.NoError(t, s.ExportMetadata(t.Context(), &metadata))
				target := newTestStore(t)
				layout := materializeEmbeddingRestoreArtifacts(t, target, map[string][]byte{
					catalogSourceHash:                               catalogBlobContents[catalogSourceHash],
					catalogEvidenceBlobHash:                         catalogBlobContents[catalogEvidenceBlobHash],
					catalogMarkdownBlobHash:                         catalogBlobContents[catalogMarkdownBlobHash],
					record.InputGeneration.GenerationBlobHash:       record.InputGeneration.GenerationJSON,
					testSHA256(record.InputGeneration.EvidenceJSON): record.InputGeneration.EvidenceJSON,
				})
				require.NoError(t, target.ImportMetadata(t.Context(), &metadata))
				backend, err := packstore.NewFilesystemBackend(*layout, packstore.FilesystemBackendOptions{})
				require.NoError(t, err)
				defer func() { require.NoError(t, backend.Close()) }()
				require.ErrorContains(t, target.VerifyRenditionBlobBytes(t.Context(), &embeddingRestoreReader{
					store: target, backend: backend,
				}), "source span")
			}
		})
	}
}

func TestEmbeddingCatalogRevalidatesExactArtifactsProviderFree(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	generationBytes := append([]byte(nil), record.InputGeneration.GenerationJSON...)
	vectorBytes := append([]byte(nil), record.VectorSet.Payload...)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		stored, err := loadEmbeddingSetTx(t.Context(), tx, record.ID)
		require.NoError(t, err)
		require.NoError(t, validateExactEmbeddingGenerationArtifact(stored.InputGeneration, generationBytes))
		require.NoError(t, validateExactEmbeddingVectorArtifact(stored.VectorSet, stored.VectorSpace, vectorBytes))
		corruptGeneration := append([]byte(nil), generationBytes...)
		corruptGeneration[len(corruptGeneration)/2] ^= 1
		require.Error(t, validateExactEmbeddingGenerationArtifact(stored.InputGeneration, corruptGeneration))
		corruptVector := append([]byte(nil), vectorBytes...)
		corruptVector[len(corruptVector)-1] ^= 1
		require.Error(t, validateExactEmbeddingVectorArtifact(stored.VectorSet, stored.VectorSpace, corruptVector))
		return nil
	}))
}

func TestEmbeddingCatalogMetadataRejectsDivergentE1Projection(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	mutated := strings.Replace(exported.String(),
		`"provider_revision":"2026-08-25"`, `"provider_revision":"forged-revision"`, 1)
	require.NotEqual(t, exported.String(), mutated)
	target := newTestStore(t)
	require.ErrorContains(t, target.ImportMetadata(t.Context(), strings.NewReader(mutated)), "projection")
}

func TestEmbeddingCatalogReadsPackedArtifactAuthority(t *testing.T) {
	s := newTestStore(t)
	data := []byte("synthetic packed embedding authority")
	hash, err := packstore.ParseHash(testSHA256(data))
	require.NoError(t, err)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return s.EnsureBlobTx(tx, hash.String(), int64(len(data)))
	}))
	layout, err := packstore.NewLayout(filepath.Join(filepath.Dir(s.path), "blobs"),
		packstore.LayoutOptions{Staging: packstore.StagingStoreDirectory, StagingDir: "tmp"})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.PacksDir(), 0o700))
	writer, err := pack.NewWriter(layout.PacksDir(), pack.WriterOptions{})
	require.NoError(t, err)
	entry, err := writer.Append(data)
	require.NoError(t, err)
	packID := writer.ID()
	require.NoError(t, os.MkdirAll(filepath.Dir(layout.PackPath(packID)), 0o700))
	entries, err := writer.Seal(layout.PackPath(packID))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	index := packstore.IndexEntry{
		Hash: hash, PackID: packID, Offset: int64(entry.Offset), StoredLen: int64(entry.StoredLen),
		RawLen: int64(entry.RawLen), Flags: uint8(entry.Flags), CRC32C: entry.CRC32C,
	}
	require.NoError(t, NewPackCatalog(s).RecordPack(t.Context(), packstore.PackRecord{
		PackID: packID, EntryCount: 1, StoredBytes: int64(entry.StoredLen), CreatedAt: time.Now().UTC(),
	}, []packstore.Adoption{{Entry: index, OriginalHashes: []string{hash.String()}}}))
	backend, err := packstore.NewFilesystemBackend(layout, packstore.FilesystemBackendOptions{})
	require.NoError(t, err)
	defer func() { require.NoError(t, backend.Close()) }()
	resolution, err := s.ResolveBlobLocations(t.Context(), hash)
	require.NoError(t, err)
	require.Len(t, resolution.Candidates, 1)
	require.NotNil(t, resolution.Candidates[0].Pack)
	stream, logicalSize, err := backend.OpenPack(t.Context(), hash, *resolution.Candidates[0].Pack)
	require.NoError(t, err)
	defer func() { require.NoError(t, stream.Close()) }()
	require.Equal(t, int64(len(data)), logicalSize)
	read, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Verify())
	require.Equal(t, data, read)
}

func TestEmbeddingCatalogProviderFreeRestoreVerifiesLooseAndPackedArtifacts(t *testing.T) {
	for _, packed := range []bool{false, true} {
		t.Run(map[bool]string{false: "loose", true: "packed"}[packed], func(t *testing.T) {
			source, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
			record := embeddingSetFixture(source, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
			require.NoError(t, source.StageEmbeddingSet(t.Context(), record))
			var metadata bytes.Buffer
			require.NoError(t, source.ExportMetadata(t.Context(), &metadata))

			target := newTestStore(t)
			artifacts := map[string][]byte{
				catalogSourceHash:                               catalogBlobContents[catalogSourceHash],
				catalogEvidenceBlobHash:                         catalogBlobContents[catalogEvidenceBlobHash],
				catalogMarkdownBlobHash:                         catalogBlobContents[catalogMarkdownBlobHash],
				record.InputGeneration.GenerationBlobHash:       record.InputGeneration.GenerationJSON,
				testSHA256(record.InputGeneration.EvidenceJSON): record.InputGeneration.EvidenceJSON,
				record.VectorSet.PayloadBlobHash:                record.VectorSet.Payload,
			}
			layout := materializeEmbeddingRestoreArtifacts(t, target, artifacts)
			require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(metadata.Bytes())))
			if packed {
				packEmbeddingRestoreArtifacts(t, target, layout, map[string][]byte{
					record.InputGeneration.GenerationBlobHash:       record.InputGeneration.GenerationJSON,
					testSHA256(record.InputGeneration.EvidenceJSON): record.InputGeneration.EvidenceJSON,
					record.VectorSet.PayloadBlobHash:                record.VectorSet.Payload,
				})
			}
			require.NoError(t, target.VerifyRenditionBlobAuthority(t.Context()))
			backend, err := packstore.NewFilesystemBackend(*layout, packstore.FilesystemBackendOptions{})
			require.NoError(t, err)
			defer func() { require.NoError(t, backend.Close()) }()
			require.NoError(t, target.VerifyRenditionBlobBytes(t.Context(), &embeddingRestoreReader{
				store: target, backend: backend,
			}))
		})
	}
}

func TestEmbeddingCatalogRestoreVerifiesArtifactsRetainedAfterVersionPrune(t *testing.T) {
	source, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(source, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, source.StageEmbeddingSet(t.Context(), record))
	chunk := embeddingSetFixture(source, versionID, profile.Fingerprint,
		document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	require.NoError(t, source.StageEmbeddingSet(t.Context(), chunk))

	var nodeID, revision int64
	require.NoError(t, source.db.QueryRow(`SELECT id,revision FROM nodes WHERE current_version_id=?`,
		versionID).Scan(&nodeID, &revision))
	replacementHash := testSHA256([]byte("orphan-vector-restore-replacement"))
	require.NoError(t, source.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return source.EnsureBlobTx(tx, replacementHash, 24)
	}))
	updated, _, err := source.ReplaceContent(
		t.Context(), nodeID, revision, replacementHash, 24, "application/pdf",
	)
	require.NoError(t, err)
	_, err = source.PruneContentVersions(t.Context(), nodeID, updated.Revision,
		VersionPruneSelector{VersionIDs: []string{versionID}}, true)
	require.NoError(t, err)

	var metadata bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &metadata))
	target := newTestStore(t)
	layout := materializeEmbeddingRestoreArtifacts(t, target, map[string][]byte{
		catalogSourceHash:                              catalogBlobContents[catalogSourceHash],
		catalogEvidenceBlobHash:                        catalogBlobContents[catalogEvidenceBlobHash],
		catalogMarkdownBlobHash:                        catalogBlobContents[catalogMarkdownBlobHash],
		record.VectorSet.PayloadBlobHash:               record.VectorSet.Payload,
		chunk.VectorSet.PayloadBlobHash:                chunk.VectorSet.Payload,
		chunk.InputGeneration.GenerationBlobHash:       chunk.InputGeneration.GenerationJSON,
		testSHA256(chunk.InputGeneration.EvidenceJSON): chunk.InputGeneration.EvidenceJSON,
	})
	require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(metadata.Bytes())))
	require.NoError(t, target.VerifyRenditionBlobAuthority(t.Context()))
	backend, err := packstore.NewFilesystemBackend(*layout, packstore.FilesystemBackendOptions{})
	require.NoError(t, err)
	defer func() { require.NoError(t, backend.Close()) }()
	require.NoError(t, target.VerifyRenditionBlobBytes(t.Context(), &embeddingRestoreReader{
		store: target, backend: backend,
	}))
}

type embeddingRestoreReader struct {
	store   *Store
	backend *packstore.FilesystemBackend
}

func (r *embeddingRestoreReader) OpenStreamContext(
	ctx context.Context, rawHash string,
) (packstore.VerifiedReadCloser, int64, error) {
	hash, err := packstore.ParseHash(rawHash)
	if err != nil {
		return nil, 0, fmt.Errorf("parse embedding artifact hash: %w", err)
	}
	resolution, err := r.store.ResolveBlobLocations(ctx, hash)
	if err != nil {
		return nil, 0, err
	}
	for _, location := range resolution.Candidates {
		if location.Loose != nil {
			stream, size, err := r.backend.OpenLoose(ctx, hash, *location.Loose)
			if err != nil {
				return nil, 0, fmt.Errorf("open loose embedding artifact: %w", err)
			}
			return stream, size, nil
		}
		if location.Pack != nil {
			stream, size, err := r.backend.OpenPack(ctx, hash, *location.Pack)
			if err != nil {
				return nil, 0, fmt.Errorf("open packed embedding artifact: %w", err)
			}
			return stream, size, nil
		}
	}
	return nil, 0, errors.New("no catalog-authorized embedding artifact location")
}

func materializeEmbeddingRestoreArtifacts(t *testing.T, s *Store, artifacts map[string][]byte) *packstore.Layout {
	t.Helper()
	layout, err := packstore.NewLayout(filepath.Join(filepath.Dir(s.path), "blobs"),
		packstore.LayoutOptions{Staging: packstore.StagingStoreDirectory, StagingDir: "tmp"})
	require.NoError(t, err)
	for rawHash, data := range artifacts {
		hash, err := packstore.ParseHash(rawHash)
		require.NoError(t, err)
		path := layout.LoosePath(hash)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, data, 0o600))
	}
	return &layout
}

func packEmbeddingRestoreArtifacts(t *testing.T, s *Store, layout *packstore.Layout, artifacts map[string][]byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(layout.PacksDir(), 0o700))
	writer, err := pack.NewWriter(layout.PacksDir(), pack.WriterOptions{})
	require.NoError(t, err)
	adoptions := make([]packstore.Adoption, 0, len(artifacts))
	for rawHash, data := range artifacts {
		hash, err := packstore.ParseHash(rawHash)
		require.NoError(t, err)
		entry, err := writer.Append(data)
		require.NoError(t, err)
		adoptions = append(adoptions, packstore.Adoption{Entry: packstore.IndexEntry{
			Hash: hash, PackID: writer.ID(), Offset: int64(entry.Offset), StoredLen: int64(entry.StoredLen),
			RawLen: int64(entry.RawLen), Flags: uint8(entry.Flags), CRC32C: entry.CRC32C,
		}, OriginalHashes: []string{rawHash}})
	}
	packID := writer.ID()
	require.NoError(t, os.MkdirAll(filepath.Dir(layout.PackPath(packID)), 0o700))
	entries, err := writer.Seal(layout.PackPath(packID))
	require.NoError(t, err)
	require.Len(t, entries, len(artifacts))
	var storedBytes int64
	for _, adoption := range adoptions {
		storedBytes += adoption.Entry.StoredLen
	}
	require.NoError(t, NewPackCatalog(s).RecordPack(t.Context(), packstore.PackRecord{
		PackID: packID, EntryCount: int64(len(adoptions)), StoredBytes: storedBytes, CreatedAt: time.Now().UTC(),
	}, adoptions))
	for rawHash := range artifacts {
		hash, err := packstore.ParseHash(rawHash)
		require.NoError(t, err)
		require.NoError(t, os.Remove(layout.LoosePath(hash)))
		resolution, err := s.ResolveBlobLocations(t.Context(), hash)
		require.NoError(t, err)
		require.NotEmpty(t, resolution.Candidates)
		require.NotNil(t, resolution.Candidates[0].Pack)
	}
}

// Mutation caught: trusting caller-authored input rows instead of decoding the
// exact E2 artifact loses headings, spans, truncation and attachment context.
func TestEmbeddingCatalogRequiresCanonicalGenerationAndVectorPayloads(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	record.InputGeneration.GenerationJSON = nil
	require.ErrorContains(t, s.StageEmbeddingSet(t.Context(), record), "generation JSON")

	record = embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	evidenceJSON := record.InputGeneration.EvidenceJSON
	record.InputGeneration.EvidenceJSON = nil
	require.ErrorContains(t, s.StageEmbeddingSet(t.Context(), record), "canonical evidence bytes")
	record.InputGeneration.EvidenceJSON = bytes.Replace(evidenceJSON, []byte("Synthetic evidence"), []byte("Invented evidence!"), 1)
	require.NotEqual(t, evidenceJSON, record.InputGeneration.EvidenceJSON)
	require.ErrorContains(t, s.StageEmbeddingSet(t.Context(), record), "evidence hash mismatch")

	record = embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	record.VectorSet.Payload = nil
	require.ErrorContains(t, s.StageEmbeddingSet(t.Context(), record), "vector payload")
}

func TestEmbeddingCatalogRejectsRotatedE1AndReorderedVectorAuthority(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)

	rotated := cloneEmbeddingSetRecord(record)
	rotated.VectorSpace.Descriptor.ModelRevision = "rotated"
	rotated.VectorSpace.Descriptor.Fingerprint = ""
	descriptor, err := document.NewEmbeddingDescriptor(rotated.VectorSpace.Descriptor)
	require.NoError(t, err)
	rotated.VectorSpace.Descriptor = descriptor
	require.ErrorContains(t, s.StageEmbeddingSet(t.Context(), rotated), "processing profile embedding binding")

	reordered := cloneEmbeddingSetRecord(record)
	decoded, _, err := document.DecodeVectorSetV1(reordered.VectorSet.Payload, document.VectorBounds{MaxRows: 128, MaxDimension: 8, MaxBytes: len(reordered.VectorSet.Payload)})
	require.NoError(t, err)
	require.Len(t, decoded.InputKeys, 2)
	decoded.InputKeys[0], decoded.InputKeys[1] = decoded.InputKeys[1], decoded.InputKeys[0]
	decoded.InputChecksums[0], decoded.InputChecksums[1] = decoded.InputChecksums[1], decoded.InputChecksums[0]
	decoded.Vectors[0], decoded.Vectors[1] = decoded.Vectors[1], decoded.Vectors[0]
	payload, checksum, err := document.EncodeVectorSetV1(decoded)
	require.NoError(t, err)
	reordered.VectorSet.Payload = payload
	reordered.VectorSet.PayloadBlobHash = testSHA256(payload)
	reordered.VectorSet.PayloadChecksum = checksum
	reordered.VectorSet.ID = checksum
	require.NoError(t, s.withStorageTx(context.Background(), func(tx *sql.Tx) error {
		return s.EnsureBlobTx(tx, reordered.VectorSet.PayloadBlobHash, int64(len(payload)))
	}))
	require.ErrorContains(t, s.StageEmbeddingSet(t.Context(), reordered), "ordered generation inputs")
}

func TestEmbeddingCatalogFailureCodesAreClosedProviderNeutralTokens(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	for _, code := range []EmbeddingFailureCode{"provider said secret=token", "provider\nmessage", "", "timeout: https://provider.invalid/request/1"} {
		err := s.RecordEmbeddingFailure(t.Context(), EmbeddingFailureRecord{
			ContentVersionID: versionID, ProcessingProfileFingerprint: profile.Fingerprint,
			BindingID: "required", InputKind: document.EmbeddingInputOriginalFile,
			FailureCode: code, FailedAt: embeddingCatalogTime,
		})
		require.ErrorContains(t, err, "provider-neutral vocabulary")
	}
}
