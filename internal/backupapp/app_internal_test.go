package backupapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
	docsqlite "go.kenn.io/docbank/sqlite"
)

func TestIndexRestoreAcceptsVaultWithoutEmbeddingAuthority(t *testing.T) {
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	physical, err := blob.New(store.NewPackCatalog(metadata), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, physical.Close()) })

	report, err := rebuildRestoredVectorIndexes(t.Context(), metadata, physical)
	require.NoError(t, err)
	require.Empty(t, report.Rebuilt)
	require.Empty(t, report.Unavailable)
}

func TestPreparedIndexRestoreRebuildsPublishedVectorAuthority(t *testing.T) {
	fixture := newPreparedVectorRestoreFixture(t)
	fixture.prepare(t)
	require.NoError(t, verifyRestoredRenditionHeads(t.Context(), fixture.root,
		fixture.databasePath, store.DefaultSQLiteDriver()))

	metadata, err := store.Open(fixture.databasePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	record, err := metadata.ActiveVectorIndexGeneration(t.Context(), fixture.vectorSpaceID)
	require.NoError(t, err)
	generation, err := vectorindex.OpenGeneration(bytes.NewReader(record.Bytes), int64(len(record.Bytes)))
	require.NoError(t, err)
	neighbors, err := generation.Search([]float32{1, 0}, 1)
	require.NoError(t, err)
	require.Len(t, neighbors, 1)
	require.Equal(t, fixture.vectorSetID, neighbors[0].SetID)
}

func TestPreparedIndexRestoreRecordsMissingCoverageWithoutPartialPublication(t *testing.T) {
	fixture := newPreparedVectorRestoreFixture(t)
	require.NoError(t, fixture.blobs.Remove(fixture.vectorPayloadHash))
	present, err := fixture.blobs.Exists(fixture.healthyVectorPayloadHash)
	require.NoError(t, err)
	require.True(t, present, "the partial-publication regression needs one retained healthy vector set")
	source, err := fixture.metadata.CaptureVectorIndexSource(t.Context(), fixture.vectorSpaceID)
	require.NoError(t, err)
	require.Len(t, source.Members, 2)
	fixture.prepare(t)
	require.NoError(t, verifyRestoredRenditionHeads(t.Context(), fixture.root,
		fixture.databasePath, store.DefaultSQLiteDriver()))

	metadata, err := store.Open(fixture.databasePath)
	require.NoError(t, err)
	_, err = metadata.ActiveVectorIndexGeneration(t.Context(), fixture.vectorSpaceID)
	require.ErrorIs(t, err, store.ErrNotFound)
	require.NoError(t, metadata.Close())

	db, err := store.DefaultSQLiteDriver().Open(fixture.databasePath, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	var coverage, heads int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT
		(SELECT COUNT(*) FROM vector_index_unavailable_coverage),
		(SELECT COUNT(*) FROM vector_index_heads)`).Scan(&coverage, &heads))
	require.Equal(t, 1, coverage)
	require.Zero(t, heads)
}

func TestPreparedIndexRestoreKeepsNonVectorAndCorruptVectorBytesStrict(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*testing.T, *preparedVectorRestoreFixture)
	}{
		{name: "missing original", mutate: func(t *testing.T, fixture *preparedVectorRestoreFixture) {
			t.Helper()
			require.NoError(t, fixture.blobs.Remove(fixture.sourceHash))
		}},
		{name: "corrupt vector", mutate: func(t *testing.T, fixture *preparedVectorRestoreFixture) {
			t.Helper()
			layout, err := packstore.NewLayout(filepath.Join(fixture.root, "blobs"), packstore.LayoutOptions{
				Staging: packstore.StagingStoreDirectory, StagingDir: "tmp",
			})
			require.NoError(t, err)
			hash, err := packstore.ParseHash(fixture.vectorPayloadHash)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(layout.LoosePath(hash),
				bytes.Repeat([]byte{'x'}, fixture.vectorPayloadSize), 0o600))
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newPreparedVectorRestoreFixture(t)
			testCase.mutate(t, fixture)
			fixture.prepare(t)
			err := verifyRestoredRenditionHeads(t.Context(), fixture.root,
				fixture.databasePath, store.DefaultSQLiteDriver())
			require.Error(t, err)
			metadata, openErr := store.Open(fixture.databasePath)
			require.NoError(t, openErr)
			defer func() { require.NoError(t, metadata.Close()) }()
			_, activeErr := metadata.ActiveVectorIndexGeneration(t.Context(), fixture.vectorSpaceID)
			require.ErrorIs(t, activeErr, store.ErrNotFound)
		})
	}
}

type preparedVectorRestoreFixture struct {
	root, databasePath            string
	blobs                         *blob.Store
	metadata                      *store.Store
	sourceHash, vectorPayloadHash string
	healthyVectorPayloadHash      string
	vectorSpaceID, vectorSetID    string
	vectorPayloadSize             int
	next                          packstore.Ownership
}

func newPreparedVectorRestoreFixture(t *testing.T) *preparedVectorRestoreFixture {
	t.Helper()
	root := t.TempDir()
	databasePath := filepath.Join(root, "docbank.db")
	metadata, err := store.Open(databasePath)
	require.NoError(t, err)
	blobs, err := blob.New(store.NewPackCatalog(metadata), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = blobs.Close()
		_ = metadata.Close()
	})
	fixture := &preparedVectorRestoreFixture{root: root, databasePath: databasePath,
		metadata: metadata, blobs: blobs}

	const source = "synthetic prepared vector restore source"
	receipt, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader([]byte(source)))
	require.NoError(t, err)
	fixture.sourceHash = receipt.Hash
	node, err := metadata.CreateFile(t.Context(), metadata.RootID(), "synthetic-source.txt",
		receipt.Hash, receipt.Size, "text/plain")
	require.NoError(t, err)
	profile, descriptor, binding, fingerprints := preparedVectorRestoreProfile(t)
	const capturedPolicy = `{"roles":[],"version":1}`
	build := store.RenditionBuildRecord{
		ID: preparedVectorHash("build"), VaultID: metadata.VaultID(), SourceSHA256: receipt.Hash,
		RenditionRequestFingerprint:       fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:        fingerprints.EvidenceLexical,
		CapturedArtifactPolicyFingerprint: preparedVectorHash(capturedPolicy),
		CapturedArtifactPolicy:            jsontext.Value(capturedPolicy),
		AuthorizationChecksum:             preparedVectorHash("authorization"),
		ProviderOperationID:               "synthetic-prepared-vector-restore",
		ProviderReceipt:                   jsontext.Value(`{"provider":"synthetic"}`),
		EvidenceChecksum:                  preparedVectorHash("evidence"), RenditionChecksum: preparedVectorHash("rendition"),
		MarkdownChecksum: preparedVectorHash("markdown"), Completeness: document.EvidenceComplete,
		Warnings: []string{}, CompletedAt: "2026-09-07T01:00:00.000000000Z",
	}
	require.NoError(t, metadata.StageRenditionBuild(t.Context(), build))
	lexical, err := metadata.StageLexicalGeneration(t.Context(), preparedVectorHash("lexical"))
	require.NoError(t, err)
	attachment := store.RenditionAttachmentRecord{
		ID: preparedVectorHash("attachment"), VaultID: metadata.VaultID(),
		ContentVersionID: node.CurrentVersionID, BuildID: build.ID, Profile: profile,
		AttachedAt: "2026-09-07T01:01:00.000000000Z",
	}
	require.NoError(t, metadata.PublishRenditionAndLexicalHeads(t.Context(), attachment,
		store.RenditionHeadRecord{ContentVersionID: node.CurrentVersionID,
			ProcessingProfileFingerprint: profile.Fingerprint, AttachmentID: attachment.ID,
			PublishedAt: "2026-09-07T01:02:00.000000000Z"}, lexical.ID))

	set, err := document.NewVectorSetV1(document.VectorSetV1Input{
		VectorSpaceFingerprint: fingerprints.VectorSpace[binding.Name],
		Metric:                 binding.Metric, Normalization: binding.Normalization, Dimension: binding.Dimensions,
		InputKeys: []string{node.CurrentVersionID}, InputChecksums: []string{receipt.Hash},
		Values: [][]float64{{1, 0}},
	})
	require.NoError(t, err)
	payload, setID, err := document.EncodeVectorSetV1(set)
	require.NoError(t, err)
	payloadReceipt, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(payload))
	require.NoError(t, err)
	encoding, err := payloadReceipt.EncodingName()
	require.NoError(t, err)
	require.NoError(t, metadata.RecordRenditionBlob(t.Context(), payloadReceipt.Hash,
		payloadReceipt.Size, store.BlobPhysical{Encoding: encoding, StoredBytes: payloadReceipt.StoredSize,
			PackEligible: payloadReceipt.PackEligible, Created: payloadReceipt.Created}))
	record := store.EmbeddingSetRecord{
		ID: preparedVectorHash("embedding-set"), VaultID: metadata.VaultID(), BindingID: binding.Name,
		InputKind: document.EmbeddingInputOriginalFile, ContentVersionID: node.CurrentVersionID,
		ProcessingProfileFingerprint: profile.Fingerprint,
		EmbeddingInputFingerprint:    fingerprints.EmbeddingInput[binding.Name],
		VectorSpace: store.EmbeddingVectorSpaceRecord{ID: fingerprints.VectorSpace[binding.Name],
			ContractVersion: store.EmbeddingVectorSpaceContractV1, Descriptor: descriptor},
		InputGeneration: store.EmbeddingInputGenerationRecord{
			ID: preparedVectorHash("input-generation"), SourceVersionID: node.CurrentVersionID,
			ProcessingProfileFingerprint: profile.Fingerprint,
			EvidenceFingerprint:          preparedVectorHash("input-evidence"),
			TokenizerFingerprint:         preparedVectorHash("tokenizer"),
			ChunkPolicyFingerprint:       preparedVectorHash("chunk-policy"),
			FormatterFingerprint:         preparedVectorHash("formatter"), GenerationChecksum: receipt.Hash,
			Inputs:    []store.EmbeddingInputReference{{ID: node.CurrentVersionID, RenderedChecksum: receipt.Hash}},
			CreatedAt: "2026-09-07T01:03:00.000000000Z",
		},
		VectorSet: store.EmbeddingVectorSetRecord{ID: setID,
			ContractVersion: store.EmbeddingVectorSetContractV1,
			VectorSpaceID:   fingerprints.VectorSpace[binding.Name], PayloadBlobHash: payloadReceipt.Hash,
			PayloadChecksum: setID, Payload: payload},
		CreatedAt: "2026-09-07T01:04:00.000000000Z",
	}
	require.NoError(t, metadata.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, metadata.PublishEmbeddingHead(t.Context(), store.EmbeddingHeadRecord{
		FencingToken: 1,
		Key: store.EmbeddingHeadKey{ContentVersionID: node.CurrentVersionID,
			BindingID: binding.Name, InputKind: document.EmbeddingInputOriginalFile},
		SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint,
		PublishedAt:                  "2026-09-07T01:05:00.000000000Z",
	}))

	secondSource, err := blobs.WriteDetailedContext(t.Context(),
		bytes.NewReader([]byte("synthetic retained vector restore source")))
	require.NoError(t, err)
	secondNode, err := metadata.CreateFile(t.Context(), metadata.RootID(), "synthetic-retained.txt",
		secondSource.Hash, secondSource.Size, "text/plain")
	require.NoError(t, err)
	secondSet, err := document.NewVectorSetV1(document.VectorSetV1Input{
		VectorSpaceFingerprint: fingerprints.VectorSpace[binding.Name],
		Metric:                 binding.Metric, Normalization: binding.Normalization, Dimension: binding.Dimensions,
		InputKeys: []string{secondNode.CurrentVersionID}, InputChecksums: []string{secondSource.Hash},
		Values: [][]float64{{0, 1}},
	})
	require.NoError(t, err)
	secondPayload, secondSetID, err := document.EncodeVectorSetV1(secondSet)
	require.NoError(t, err)
	secondPayloadReceipt, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(secondPayload))
	require.NoError(t, err)
	secondEncoding, err := secondPayloadReceipt.EncodingName()
	require.NoError(t, err)
	require.NoError(t, metadata.RecordRenditionBlob(t.Context(), secondPayloadReceipt.Hash,
		secondPayloadReceipt.Size, store.BlobPhysical{Encoding: secondEncoding,
			StoredBytes: secondPayloadReceipt.StoredSize, PackEligible: secondPayloadReceipt.PackEligible,
			Created: secondPayloadReceipt.Created}))
	secondRecord := record
	secondRecord.ID = preparedVectorHash("retained-embedding-set")
	secondRecord.ContentVersionID = secondNode.CurrentVersionID
	secondRecord.InputGeneration = store.EmbeddingInputGenerationRecord{
		ID: preparedVectorHash("retained-input-generation"), SourceVersionID: secondNode.CurrentVersionID,
		ProcessingProfileFingerprint: profile.Fingerprint,
		EvidenceFingerprint:          preparedVectorHash("retained-input-evidence"),
		TokenizerFingerprint:         preparedVectorHash("retained-tokenizer"),
		ChunkPolicyFingerprint:       preparedVectorHash("retained-chunk-policy"),
		FormatterFingerprint:         preparedVectorHash("retained-formatter"), GenerationChecksum: secondSource.Hash,
		Inputs: []store.EmbeddingInputReference{{ID: secondNode.CurrentVersionID,
			RenderedChecksum: secondSource.Hash}},
		CreatedAt: "2026-09-07T01:06:00.000000000Z",
	}
	secondRecord.VectorSet = store.EmbeddingVectorSetRecord{
		ID: secondSetID, ContractVersion: store.EmbeddingVectorSetContractV1,
		VectorSpaceID: fingerprints.VectorSpace[binding.Name], PayloadBlobHash: secondPayloadReceipt.Hash,
		PayloadChecksum: secondSetID, Payload: secondPayload,
	}
	secondRecord.CreatedAt = "2026-09-07T01:07:00.000000000Z"
	require.NoError(t, metadata.StageEmbeddingSet(t.Context(), secondRecord))
	require.NoError(t, metadata.PublishEmbeddingHead(t.Context(), store.EmbeddingHeadRecord{
		FencingToken: 1,
		Key: store.EmbeddingHeadKey{ContentVersionID: secondNode.CurrentVersionID,
			BindingID: binding.Name, InputKind: document.EmbeddingInputOriginalFile},
		SetID: secondRecord.ID, VectorSpaceID: secondRecord.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint,
		PublishedAt:                  "2026-09-07T01:08:00.000000000Z",
	}))
	fixture.vectorPayloadHash = payloadReceipt.Hash
	fixture.healthyVectorPayloadHash = secondPayloadReceipt.Hash
	fixture.vectorPayloadSize = len(payload)
	fixture.vectorSpaceID = record.VectorSpace.ID
	fixture.vectorSetID = setID
	fixture.next = store.NewPackCatalog(metadata).PrimaryOwnership()
	return fixture
}

func (fixture *preparedVectorRestoreFixture) prepare(t *testing.T) {
	t.Helper()
	require.NoError(t, fixture.blobs.Close())
	require.NoError(t, fixture.metadata.Close())
	backend, err := blob.NewFilesystemBackend(filepath.Join(fixture.root, "blobs"), nil)
	require.NoError(t, err)
	current, err := backend.Ownership(t.Context())
	require.NoError(t, err)
	prior := current
	prior.Epoch = "20000000-0000-4000-8000-000000000008"
	require.NotEqual(t, fixture.next, prior)
	require.NoError(t, backend.ReplaceOwnership(t.Context(), prior, &current))
	require.NoError(t, backend.Close())
	priorDatabaseDigest := ""
	handoff, err := blob.NewPrimaryRestoreHandoff(filepath.Join(fixture.root, "blobs"),
		fixture.next, &priorDatabaseDigest)
	require.NoError(t, err)
	require.NoError(t, handoff.Prepare(t.Context()))
	pending, err := blob.PrimaryRestoreHandoffPending(filepath.Join(fixture.root, "blobs"))
	require.NoError(t, err)
	require.True(t, pending)
	t.Cleanup(func() { require.NoError(t, handoff.Rollback(context.WithoutCancel(t.Context()))) })
}

func preparedVectorRestoreProfile(t *testing.T) (
	store.ProcessingProfileRecord, document.EmbeddingDescriptor, document.EmbeddingBindingV1,
	document.FingerprintSet,
) {
	t.Helper()
	modelInput, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: "synthetic-prepared-space",
		Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "document: {{content}}"},
		Query:    document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "query: {{content}}"},
	})
	require.NoError(t, err)
	descriptor, err := document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{
		ID: "synthetic-prepared-embedder", ContractVersion: document.EmbeddingProviderContractVersion,
		PolicyFingerprint: preparedVectorHash("embedding-policy"), TrustBoundary: document.EmbeddingTrustLocalProcess,
		Model: "synthetic-v1", ModelRevision: "2026-09-07", Dimension: 2,
		Metric: document.VectorMetricDotProduct, Normalization: document.VectorNormalizationNone,
		ScalarEncoding: "float32", DocumentFormatter: "document/v1", QueryFormatter: "query/v1",
		InputKinds:      []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile},
		CompatibilityID: modelInput.CompatibilityID, SupportsTextQuery: true, ModelInput: modelInput,
		SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText},
	})
	require.NoError(t, err)
	binding := document.EmbeddingBindingV1{
		Name: "vectors", Activation: document.EmbeddingOptional,
		AuthorizationFingerprint: preparedVectorHash("embedding-authorization"),
		CompatibilityID:          descriptor.CompatibilityID, CredentialBinding: "credential:synthetic-prepared",
		Descriptor: document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint},
		Dimensions: descriptor.Dimension, DisclosureFingerprint: preparedVectorHash("embedding-disclosure"),
		DocumentFormatter: descriptor.DocumentFormatter, InputKind: document.EmbeddingInputOriginalFile,
		MaxBatchItems: 8, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20,
		Metric: descriptor.Metric, Model: descriptor.Model, ModelInput: descriptor.ModelInput,
		Normalization: descriptor.Normalization, QueryFormatter: descriptor.QueryFormatter,
		ScalarEncoding: descriptor.ScalarEncoding, TrustBoundary: string(descriptor.TrustBoundary),
	}
	profile := document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1,
		Rendition: &document.RenditionBindingV1{
			Name: "primary", AdapterContract: "rendition-adapter/v1",
			AuthorizationFingerprint: preparedVectorHash("rendition-authorization"),
			CredentialBinding:        "credential:synthetic-prepared",
			DeploymentFingerprint:    preparedVectorHash("rendition-deployment"),
			Descriptor: document.ProviderDescriptorV1{ID: "synthetic-rendition",
				Fingerprint: preparedVectorHash("rendition-provider")},
			DisclosureFingerprint: preparedVectorHash("rendition-disclosure"),
			MaxDocumentBytes:      1 << 20, MaxResponseBytes: 1 << 20, MaxUnits: 10,
			RequestedArtifacts: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
			TrustBoundary:      "synthetic-vault", UploadOptionsFingerprint: preparedVectorHash("upload-options"),
		},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			CompletenessFingerprint:     preparedVectorHash("completeness"),
			LexicalSegmenterFingerprint: preparedVectorHash("segmenter"), MaxDocumentChars: 10_000,
			MaxSegmentRunes: 100, MaxUnitRunes: 1_000,
			NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
			NormalizerFingerprint:      preparedVectorHash("normalizer"), RenditionContract: document.RenditionContractV1,
			SanitizerFingerprint: preparedVectorHash("sanitizer"), SourceEvidenceContract: document.SourceEvidenceContractV1,
		},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{
			AttachmentPolicyFingerprint: preparedVectorHash("attachment-policy"),
			ConsentFingerprint:          preparedVectorHash("consent"), RetainSanitizedMarkdown: true,
			RetainTypedArtifacts: true, TrustBoundary: "synthetic-vault",
		},
		Retrieval:  document.RetrievalPolicyV1{LexicalLimit: 10, VectorLimit: 10},
		Embeddings: []document.EmbeddingBindingV1{binding},
	}
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	record := store.ProcessingProfileRecord{
		Fingerprint: fingerprints.Profile, CanonicalProfile: jsontext.Value(canonical),
		RenditionRequestFingerprint:    fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:     fingerprints.EvidenceLexical,
		RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
		AttachmentPolicyFingerprint:    profile.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             profile.RetentionDisclosure.ConsentFingerprint,
		RenditionDisclosureFingerprint: profile.Rendition.DisclosureFingerprint,
		TrustBoundary:                  profile.RetentionDisclosure.TrustBoundary,
	}
	return record, descriptor, binding, fingerprints
}

func preparedVectorHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
