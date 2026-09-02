package backupapp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"go.kenn.io/kit/pack"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/sqlite"
)

type archiveFixture struct {
	root     string
	metadata *store.Store
	blobs    *blob.Store
	content  map[string]string
}

type zeroReader struct{}

type freezeCoordinator struct {
	end func(context.Context) error
}

func (freezeCoordinator) Begin(context.Context) error { return nil }

func (f freezeCoordinator) End(ctx context.Context) error { return f.end(ctx) }

type rawMetadataSource struct{ metadata []byte }

func (rawMetadataSource) Format() string { return backupapp.MetadataFormat }

func (source rawMetadataSource) OpenSnapshot(context.Context) (backup.MetadataSnapshot, error) {
	return rawMetadataSnapshot(source), nil
}

type rawMetadataSnapshot struct{ metadata []byte }

type overriddenMetadataSource struct {
	source   backup.MetadataSource
	metadata []byte
}

func (s overriddenMetadataSource) Format() string { return s.source.Format() }

func (s overriddenMetadataSource) OpenSnapshot(ctx context.Context) (backup.MetadataSnapshot, error) {
	snapshot, err := s.source.OpenSnapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("opening overridden metadata snapshot: %w", err)
	}
	return overriddenMetadataSnapshot{MetadataSnapshot: snapshot, metadata: s.metadata}, nil
}

type overriddenMetadataSnapshot struct {
	backup.MetadataSnapshot

	metadata []byte
}

func (s overriddenMetadataSnapshot) OpenMetadata(context.Context) (io.ReadCloser, int64, error) {
	return io.NopCloser(bytes.NewReader(s.metadata)), int64(len(s.metadata)), nil
}

func (s overriddenMetadataSnapshot) AuxiliaryArtifacts(
	ctx context.Context,
) ([]backup.AuxiliaryArtifact, error) {
	source, ok := s.MetadataSnapshot.(backup.AuxiliarySource)
	if !ok {
		return nil, nil
	}
	artifacts, err := source.AuxiliaryArtifacts(ctx)
	if err != nil {
		return nil, fmt.Errorf("opening overridden auxiliary artifacts: %w", err)
	}
	return artifacts, nil
}

type auxiliaryTargetFunc func(context.Context, []backup.RestoredAuxiliary) error

func (f auxiliaryTargetFunc) StageAuxiliary(
	ctx context.Context, artifacts []backup.RestoredAuxiliary,
) (backup.AuxiliaryRestore, error) {
	if err := f(ctx, artifacts); err != nil {
		return nil, err
	}
	return testAuxiliaryRestore{}, nil
}

type testAuxiliaryRestore struct{}

func (testAuxiliaryRestore) Commit(context.Context) error   { return nil }
func (testAuxiliaryRestore) Rollback(context.Context) error { return nil }

func (snapshot rawMetadataSnapshot) OpenMetadata(context.Context) (io.ReadCloser, int64, error) {
	return io.NopCloser(bytes.NewReader(snapshot.metadata)), int64(len(snapshot.metadata)), nil
}

func (rawMetadataSnapshot) ContentInfo(context.Context) (*backup.ContentInfo, error) {
	return &backup.ContentInfo{}, nil
}

func (rawMetadataSnapshot) Stats(context.Context) (jsontext.Value, error) {
	return jsontext.Value(`{}`), nil
}

func (rawMetadataSnapshot) Close() error { return nil }

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func newArchiveFixture(t *testing.T) *archiveFixture {
	t.Helper()
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	blobsDir := filepath.Join(root, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	blobs, err := blob.New(store.NewPackCatalog(metadata), blobsDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, blobs.Close())
		require.NoError(t, metadata.Close())
	})
	fixture := &archiveFixture{
		root: root, metadata: metadata, blobs: blobs,
		content: map[string]string{"alpha.txt": "alpha backup", "bravo.txt": "bravo backup"},
	}
	require.NoError(t, blobs.WithMutation(t.Context(), func() error {
		for name, content := range fixture.content {
			hash, size, writeErr := blobs.WriteContext(t.Context(), strings.NewReader(content))
			if writeErr != nil {
				return writeErr
			}
			if _, createErr := metadata.CreateFile(t.Context(), metadata.RootID(), name,
				hash, size, "text/plain"); createErr != nil {
				return createErr
			}
		}
		return nil
	}))
	return fixture
}

func seedOwnedVault(t *testing.T, target string) packstore.Ownership {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(target, "blobs", "tmp"), 0o700))
	metadata, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	blobs, err := blob.New(
		store.NewPackCatalog(metadata), filepath.Join(target, "blobs"),
	)
	require.NoError(t, err)
	ownership := store.NewPackCatalog(metadata).PrimaryOwnership()
	require.NoError(t, blobs.Close())
	require.NoError(t, metadata.Close())
	return ownership
}

func exportMetadata(t *testing.T, metadata *store.Store) []byte {
	t.Helper()
	var dst bytes.Buffer
	require.NoError(t, metadata.ExportMetadata(t.Context(), &dst))
	return dst.Bytes()
}

func exportBackupMetadata(t *testing.T, metadata *store.Store) []byte {
	t.Helper()
	snapshot, err := backupapp.NewMetadataSource(metadata).OpenSnapshot(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, snapshot.Close()) })
	stream, _, err := snapshot.OpenMetadata(t.Context())
	require.NoError(t, err)
	data, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	return data
}

func markRestoreTarget(t *testing.T, target string) string {
	t.Helper()
	seedOwnedVault(t, target)
	metadata, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	_, err = metadata.Mkdir(t.Context(), metadata.RootID(), "preexisting-restore-marker")
	require.NoError(t, err)
	require.NoError(t, metadata.Close())
	data, err := os.ReadFile(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func assertRestoreTargetUnchanged(t *testing.T, target, wantDigest string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	digest := sha256.Sum256(data)
	assert.Equal(t, wantDigest, hex.EncodeToString(digest[:]))
	metadata, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, metadata.Close()) }()
	_, err = metadata.NodeByPath(t.Context(), "/preexisting-restore-marker")
	assert.NoError(t, err)
}

func TestJSONLLooseSnapshotVerifyAndRestore(t *testing.T) {
	fixture := newArchiveFixture(t)
	wantMetadata := exportMetadata(t, fixture.metadata)
	sourceStore, err := fixture.metadata.PrimaryBlobStore(t.Context())
	require.NoError(t, err)
	archiveStore, err := fixture.metadata.PrepareSecondaryBlobStore(
		"archive", "filesystem", "archive_nas",
	)
	require.NoError(t, err)
	require.NoError(t, fixture.metadata.RegisterBlobStore(t.Context(), archiveStore))
	app := backupapp.New("test-version")
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)

	manifest, err := backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs, backup.CreateOptions{
			Jobs: 2,
		})
	require.NoError(t, err)
	require.NotNil(t, manifest.Metadata)
	require.Len(t, manifest.Auxiliary, 1)
	assert.Equal(t, "placement", manifest.Auxiliary[0].Name)
	assert.Equal(t, backupapp.PlacementFormat, manifest.Auxiliary[0].Format)
	assert.Equal(t, backupapp.MetadataFormat, manifest.Metadata.Format)
	assert.Empty(t, manifest.DB.Engine)
	assert.Equal(t, int64(2), manifest.Attachments.Rows)
	assert.Equal(t, int64(2), manifest.Attachments.Blobs)
	assert.Equal(t, int64(len("alpha backup")+len("bravo backup")), manifest.Attachments.BlobBytes)
	stats, err := backupapp.ParseStats(manifest.Stats)
	require.NoError(t, err)
	assert.Equal(t, int64(3), stats.Nodes, "root plus two files")
	assert.Equal(t, int64(2), stats.Files)
	assert.Equal(t, int64(2), stats.ContentVersions)
	assert.Equal(t, manifest.Attachments.BlobBytes, stats.BlobBytes)

	verified, err := backup.Verify(t.Context(), repo, app, backup.VerifyOptions{Jobs: 2})
	require.NoError(t, err)
	assert.Empty(t, verified.Problems)
	assert.Equal(t, []string{manifest.SnapshotID}, verified.Snapshots)

	target := filepath.Join(t.TempDir(), "restored")
	_, err = backup.Restore(t.Context(), repo, app, backup.RestoreOptions{
		TargetDir: filepath.Join(t.TempDir(), "missing-metadata-restorer"), Jobs: 2,
		AuxiliaryTarget: auxiliaryTargetFunc(func(
			context.Context, []backup.RestoredAuxiliary,
		) error {
			return nil
		}),
	})
	require.ErrorContains(t, err, "requires a MetadataRestorer")

	restored, err := backupapp.Restore(t.Context(), repo, "test-version", backup.RestoreOptions{
		TargetDir: target, Jobs: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), restored.AttachmentBlobs)
	assert.Equal(t, int64(2), restored.PackedAttachmentBlobs)
	assert.Zero(t, restored.LooseAttachmentBlobs)

	restoredStore, err := store.OpenForRestore(
		filepath.Join(target, "docbank.db"), store.DefaultSQLiteDriver(),
	)
	require.NoError(t, err)
	assert.Equal(t, string(wantMetadata), string(exportMetadata(t, restoredStore)))
	restoredStores, err := restoredStore.BlobStores(t.Context())
	require.NoError(t, err)
	require.Len(t, restoredStores, 1)
	assert.Equal(t, "primary", restoredStores[0].Role)
	assert.NotEqual(t, sourceStore.ID, restoredStores[0].ID)
	assert.NotEqual(t, sourceStore.OwnershipEpoch, restoredStores[0].OwnershipEpoch)
	restoredBlobs, err := blob.New(store.NewPackCatalog(restoredStore), filepath.Join(target, "blobs"))
	require.NoError(t, err)
	for name, want := range fixture.content {
		node, nodeErr := restoredStore.NodeByPath(t.Context(), "/"+name)
		require.NoError(t, nodeErr)
		reader, openErr := restoredBlobs.OpenContext(t.Context(), node.BlobHash)
		require.NoError(t, openErr)
		got, readErr := io.ReadAll(reader)
		require.NoError(t, readErr)
		require.NoError(t, reader.Close())
		assert.Equal(t, want, string(got))
		version, versionErr := restoredStore.ContentVersionByID(t.Context(), node.CurrentVersionID)
		require.NoError(t, versionErr)
		assert.Equal(t, node.ID, version.NodeID)
		assert.Equal(t, node.BlobHash, version.BlobHash)
	}
	require.NoError(t, restoredBlobs.Close())
	require.NoError(t, restoredStore.Close())
}

func TestJSONLSnapshotRestoresRenditionBytesBeforeVerifyingHeads(t *testing.T) {
	fixture := newArchiveFixture(t)
	source, err := fixture.metadata.NodeByPath(t.Context(), "/alpha.txt")
	require.NoError(t, err)

	artifactContents := []struct {
		name    string
		content string
	}{
		{name: "evidence.bin", content: "synthetic restored evidence"},
		{name: "rendition.md", content: "synthetic restored markdown"},
	}
	artifactHashes := make([]string, 0, len(artifactContents))
	require.NoError(t, fixture.blobs.WithMutation(t.Context(), func() error {
		for _, artifact := range artifactContents {
			hash, size, writeErr := fixture.blobs.WriteContext(
				t.Context(), strings.NewReader(artifact.content),
			)
			if writeErr != nil {
				return writeErr
			}
			if _, createErr := fixture.metadata.CreateFile(
				t.Context(), fixture.metadata.RootID(), artifact.name,
				hash, size, "application/octet-stream",
			); createErr != nil {
				return createErr
			}
			artifactHashes = append(artifactHashes, hash)
		}
		return nil
	}))

	profile := document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1,
		Rendition: &document.RenditionBindingV1{
			AdapterContract: "rendition-adapter/v1", AuthorizationFingerprint: strings.Repeat("a", 64),
			CredentialBinding: "credential:restore", DeploymentFingerprint: strings.Repeat("b", 64),
			Descriptor:            document.ProviderDescriptorV1{ID: "synthetic-restore", Fingerprint: strings.Repeat("c", 64)},
			DisclosureFingerprint: strings.Repeat("d", 64), MaxDocumentBytes: 1 << 20,
			MaxResponseBytes: 1 << 20, MaxUnits: 100, Name: "restore",
			RequestedArtifacts: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
			TrustBoundary:      "synthetic-restore", UploadOptionsFingerprint: strings.Repeat("e", 64),
		},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			CompletenessFingerprint: strings.Repeat("1", 64), LexicalSegmenterFingerprint: strings.Repeat("2", 64),
			MaxDocumentChars: 100_000, MaxSegmentRunes: 100, MaxUnitRunes: 1000,
			NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
			NormalizerFingerprint:      strings.Repeat("3", 64), RenditionContract: document.RenditionContractV1,
			SanitizerFingerprint: strings.Repeat("4", 64), SourceEvidenceContract: document.SourceEvidenceContractV1,
		},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{
			AttachmentPolicyFingerprint: strings.Repeat("5", 64), ConsentFingerprint: strings.Repeat("6", 64),
			RetainSanitizedMarkdown: true, RetainTypedArtifacts: true, TrustBoundary: "synthetic-restore",
		},
		Retrieval: document.RetrievalPolicyV1{LexicalLimit: 100, VectorLimit: 100},
	}
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	profileRecord := store.ProcessingProfileRecord{
		Fingerprint: fingerprints.Profile, CanonicalProfile: jsontext.Value(canonical),
		RenditionRequestFingerprint:    fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:     fingerprints.EvidenceLexical,
		RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
		AttachmentPolicyFingerprint:    profile.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             profile.RetentionDisclosure.ConsentFingerprint,
		RenditionDisclosureFingerprint: profile.Rendition.DisclosureFingerprint,
		TrustBoundary:                  profile.RetentionDisclosure.TrustBoundary,
	}
	capturedPolicy := jsontext.Value(
		`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`,
	)
	build := store.RenditionBuildRecord{
		ID: strings.Repeat("7", 64), VaultID: fixture.metadata.VaultID(), SourceSHA256: source.BlobHash,
		RenditionRequestFingerprint:       fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:        fingerprints.EvidenceLexical,
		CapturedArtifactPolicyFingerprint: fmt.Sprintf("%x", sha256.Sum256(capturedPolicy)),
		CapturedArtifactPolicy:            capturedPolicy, AuthorizationChecksum: strings.Repeat("8", 64),
		ProviderOperationID: "synthetic-restore", ProviderReceipt: jsontext.Value(`{"provider":"synthetic"}`),
		EvidenceChecksum: artifactHashes[0], RenditionChecksum: artifactHashes[1],
		MarkdownChecksum: artifactHashes[1], Completeness: document.EvidenceComplete,
		Warnings: []string{}, CompletedAt: "2026-08-25T12:00:00.000000000Z",
		DeclaredArtifactCount: 2,
		Artifacts: []store.RenditionArtifactRecord{
			{ID: "evidence", Role: "normalized_evidence", BlobHash: artifactHashes[0],
				Size: int64(len(artifactContents[0].content)), Checksum: artifactHashes[0], State: store.RenditionArtifactVerified},
			{ID: "markdown", Role: "sanitized_markdown", BlobHash: artifactHashes[1],
				Size: int64(len(artifactContents[1].content)), Checksum: artifactHashes[1], State: store.RenditionArtifactVerified},
		},
	}
	require.NoError(t, fixture.metadata.StageRenditionBuild(t.Context(), build))
	generation, err := fixture.metadata.StageLexicalGeneration(t.Context(), strings.Repeat("f", 64))
	require.NoError(t, err)
	attachment := store.RenditionAttachmentRecord{
		ID: strings.Repeat("9", 64), VaultID: fixture.metadata.VaultID(),
		ContentVersionID: source.CurrentVersionID, BuildID: build.ID,
		Profile: profileRecord, AttachedAt: "2026-08-25T12:01:00.000000000Z",
	}
	require.NoError(t, fixture.metadata.PublishRenditionAndLexicalHeads(t.Context(), attachment, store.RenditionHeadRecord{
		ContentVersionID:             source.CurrentVersionID,
		ProcessingProfileFingerprint: profileRecord.Fingerprint,
		AttachmentID:                 attachment.ID, PublishedAt: "2026-08-25T12:02:00.000000000Z",
	}, generation.ID))

	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	_, err = backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs, backup.CreateOptions{},
	)
	require.NoError(t, err)
	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(t.Context(), repo, "test-version", backup.RestoreOptions{TargetDir: target})
	require.NoError(t, err)

	restoredMetadata, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restoredMetadata.Close()) })
	restoredBlobs, err := blob.New(store.NewPackCatalog(restoredMetadata), filepath.Join(target, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restoredBlobs.Close()) })
	view, err := restoredMetadata.ActiveRendition(
		t.Context(), source.CurrentVersionID, profileRecord.Fingerprint,
	)
	require.NoError(t, err)
	assert.Equal(t, build.ID, view.Build.ID)
	for index, hash := range artifactHashes {
		reader, openErr := restoredBlobs.OpenContext(t.Context(), hash)
		require.NoError(t, openErr)
		contents, readErr := io.ReadAll(reader)
		require.NoError(t, readErr)
		require.NoError(t, reader.Close())
		assert.Equal(t, artifactContents[index].content, string(contents))
	}
}

func TestOverwriteRestorePublishesMatchingPrimaryOwnership(t *testing.T) {
	fixture := newArchiveFixture(t)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	_, err = backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs,
		backup.CreateOptions{Jobs: 1},
	)
	require.NoError(t, err)

	target := filepath.Join(t.TempDir(), "existing-vault")
	priorOwnership := seedOwnedVault(t, target)

	_, err = backupapp.Restore(
		t.Context(), repo, "test-version",
		backup.RestoreOptions{TargetDir: target, Overwrite: true, Jobs: 1},
	)
	require.NoError(t, err)

	restoredMetadata, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, restoredMetadata.Close()) }()
	restoredOwnership := store.NewPackCatalog(restoredMetadata).PrimaryOwnership()
	assert.NotEqual(t, priorOwnership, restoredOwnership)
	restoredBlobs, err := blob.New(
		store.NewPackCatalog(restoredMetadata), filepath.Join(target, "blobs"),
	)
	require.NoError(t, err)
	require.NoError(t, restoredBlobs.Close())
}

func TestFailedMappedOverwriteRestoresPrimaryOwnershipAndCanRetry(t *testing.T) {
	fixture := newArchiveFixture(t)
	sourcePrimary, err := fixture.metadata.PrimaryBlobStore(t.Context())
	require.NoError(t, err)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	_, err = backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs,
		backup.CreateOptions{Jobs: 1},
	)
	require.NoError(t, err)

	target := filepath.Join(t.TempDir(), "existing-vault")
	priorOwnership := seedOwnedVault(t, target)
	claimedPath := filepath.Join(t.TempDir(), "claimed-store")
	claimedBinding := config.StoreBindingConfig{Kind: "filesystem", Path: claimedPath}
	claimed, err := blob.NewConfiguredBackend(t.Context(), claimedBinding, nil)
	require.NoError(t, err)
	require.NoError(t, claimed.ReplaceOwnership(t.Context(), packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "20000000-0000-4000-8000-000000000001",
		Store:  "20000000-0000-4000-8000-000000000002",
		Epoch:  "20000000-0000-4000-8000-000000000003",
	}, nil))
	if closer, ok := claimed.(io.Closer); ok {
		require.NoError(t, closer.Close())
	}
	mapping := backupapp.RestoreStoreMap{
		Version: backupapp.RestoreStoreMapVersion,
		Stores: []backupapp.RestoreStoreMapping{{
			SourceID: sourcePrimary.ID, Name: "claimed",
			Binding: "claimed",
		}},
	}
	_, err = backupapp.RestoreWithPlacement(
		t.Context(), repo, "test-version", store.DefaultSQLiteDriver(),
		backup.RestoreOptions{TargetDir: target, Overwrite: true, Jobs: 1},
		backupapp.RestorePlacementOptions{
			Map: &mapping,
			Bindings: map[string]config.StoreBindingConfig{
				"claimed": claimedBinding,
			},
		},
	)
	require.ErrorContains(t, err, "explicit takeover is required")

	priorMetadata, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	assert.Equal(t, priorOwnership, store.NewPackCatalog(priorMetadata).PrimaryOwnership())
	priorBlobs, err := blob.New(
		store.NewPackCatalog(priorMetadata), filepath.Join(target, "blobs"),
	)
	require.NoError(t, err)
	require.NoError(t, priorBlobs.Close())
	require.NoError(t, priorMetadata.Close())

	_, err = backupapp.Restore(
		t.Context(), repo, "test-version",
		backup.RestoreOptions{TargetDir: target, Overwrite: true, Jobs: 1},
	)
	require.NoError(t, err)
	restoredMetadata, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	restoredBlobs, err := blob.New(
		store.NewPackCatalog(restoredMetadata), filepath.Join(target, "blobs"),
	)
	require.NoError(t, err)
	require.NoError(t, restoredBlobs.Close())
	require.NoError(t, restoredMetadata.Close())
}

func TestOverwriteRestoreRecoversInterruptedHandoffOverOpaqueDatabase(t *testing.T) {
	fixture := newArchiveFixture(t)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	_, err = backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs,
		backup.CreateOptions{Jobs: 1},
	)
	require.NoError(t, err)

	target := filepath.Join(t.TempDir(), "opaque-target")
	require.NoError(t, os.MkdirAll(target, 0o700))
	opaqueDatabase := []byte("not a docbank database")
	databasePath := filepath.Join(target, "docbank.db")
	require.NoError(t, os.WriteFile(databasePath, opaqueDatabase, 0o600))
	digest := fmt.Sprintf("%x", sha256.Sum256(opaqueDatabase))
	handoff, err := blob.NewPrimaryRestoreHandoff(
		filepath.Join(target, "blobs"),
		packstore.Ownership{
			Format: packstore.OwnershipFormatV1,
			Vault:  "30000000-0000-4000-8000-000000000001",
			Store:  "30000000-0000-4000-8000-000000000002",
			Epoch:  "30000000-0000-4000-8000-000000000003",
		},
		&digest,
	)
	require.NoError(t, err)
	require.NoError(t, handoff.Prepare(t.Context()))

	_, err = backupapp.Restore(
		t.Context(), repo, "test-version",
		backup.RestoreOptions{TargetDir: target, Overwrite: true, Jobs: 1},
	)
	require.NoError(t, err)
	restoredMetadata, err := store.Open(databasePath)
	require.NoError(t, err)
	restoredBlobs, err := blob.New(
		store.NewPackCatalog(restoredMetadata), filepath.Join(target, "blobs"),
	)
	require.NoError(t, err)
	require.NoError(t, restoredBlobs.Close())
	require.NoError(t, restoredMetadata.Close())
}

func TestPlacementArtifactUsesPinnedCatalogAuthority(t *testing.T) {
	fixture := newArchiveFixture(t)
	archiveStore, err := fixture.metadata.PrepareSecondaryBlobStore(
		"archive", "filesystem", "archive_nas",
	)
	require.NoError(t, err)
	require.NoError(t, fixture.metadata.RegisterBlobStore(t.Context(), archiveStore))

	snapshot, err := backupapp.NewMetadataSource(fixture.metadata).OpenSnapshot(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, snapshot.Close()) }()
	auxiliary, ok := snapshot.(backup.AuxiliarySource)
	require.True(t, ok)
	artifacts, err := auxiliary.AuxiliaryArtifacts(t.Context())
	require.NoError(t, err)
	require.Len(t, artifacts, 1)
	reader, size, err := artifacts[0].Open(t.Context())
	require.NoError(t, err)
	raw, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	assert.Equal(t, int64(len(raw)), size)

	var placement struct {
		Format string `json:"format"`
		Stores []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Kind    string `json:"kind"`
			Role    string `json:"role"`
			Objects int64  `json:"objects"`
			Bytes   int64  `json:"bytes"`
		} `json:"stores"`
		Locations []struct {
			Hash     string   `json:"hash"`
			StoreIDs []string `json:"store_ids"`
		} `json:"locations"`
	}
	require.NoError(t, json.Unmarshal(raw, &placement))
	assert.Equal(t, backupapp.PlacementFormat, placement.Format)
	require.Len(t, placement.Stores, 2)
	assert.Equal(t, "primary", placement.Stores[0].Name)
	assert.Equal(t, int64(2), placement.Stores[0].Objects)
	assert.Equal(t, int64(len("alpha backup")+len("bravo backup")), placement.Stores[0].Bytes)
	assert.Equal(t, "archive", placement.Stores[1].Name)
	assert.Zero(t, placement.Stores[1].Objects)
	require.Len(t, placement.Locations, 2)
	assert.Less(t, placement.Locations[0].Hash, placement.Locations[1].Hash)
	assert.Equal(t, []string{placement.Stores[0].ID}, placement.Locations[0].StoreIDs)
	assert.NotContains(t, string(raw), "archive_nas")
	assert.NotContains(t, string(raw), archiveStore.OwnershipEpoch)
}

func TestRestoreStoreMapRebuildsRemoteOnlyPlacementWithFreshIdentity(t *testing.T) {
	fixture := newArchiveFixture(t)
	sourcePrimary, err := fixture.metadata.PrimaryBlobStore(t.Context())
	require.NoError(t, err)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	_, err = backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs,
		backup.CreateOptions{Jobs: 1},
	)
	require.NoError(t, err)

	archivePath := filepath.Join(t.TempDir(), "archive-store")
	binding := config.StoreBindingConfig{Kind: "filesystem", Path: archivePath}
	existing, err := blob.NewConfiguredBackend(t.Context(), binding, nil)
	require.NoError(t, err)
	priorOwnership := packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "20000000-0000-4000-8000-000000000001",
		Store:  "20000000-0000-4000-8000-000000000002",
		Epoch:  "20000000-0000-4000-8000-000000000003",
	}
	require.NoError(t, existing.ReplaceOwnership(t.Context(), priorOwnership, nil))
	for _, content := range fixture.content {
		hashText := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
		hash, parseErr := packstore.ParseHash(hashText)
		require.NoError(t, parseErr)
		receipt, publishErr := existing.PublishLoose(
			t.Context(), hash, strings.NewReader(content), packstore.PublishOptions{
				ExpectedSize: int64(len(content)), SizeKnown: true,
				MaxBytes: int64(len(content)),
			},
		)
		require.NoError(t, publishErr)
		assert.True(t, receipt.Created)
	}
	corruptContent := "alpha backup"
	corruptHash, err := packstore.ParseHash(
		fmt.Sprintf("%x", sha256.Sum256([]byte(corruptContent))),
	)
	require.NoError(t, err)
	archiveLayout, err := packstore.NewLayout(archivePath, packstore.LayoutOptions{
		Staging: packstore.StagingStoreDirectory, StagingDir: "tmp",
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		archiveLayout.LoosePath(corruptHash),
		[]byte(strings.Repeat("x", len(corruptContent))),
		0o600,
	))
	if closer, ok := existing.(io.Closer); ok {
		require.NoError(t, closer.Close())
	}
	target := filepath.Join(t.TempDir(), "restored")
	mapping := backupapp.RestoreStoreMap{
		Version: backupapp.RestoreStoreMapVersion,
		Stores: []backupapp.RestoreStoreMapping{{
			SourceID: sourcePrimary.ID, Name: "restored archive",
			Binding: "archive", Takeover: true, RemoteOnly: true,
		}},
	}
	result, err := backupapp.RestoreWithPlacement(
		t.Context(), repo, "test-version", store.DefaultSQLiteDriver(),
		backup.RestoreOptions{TargetDir: target, Jobs: 1},
		backupapp.RestorePlacementOptions{
			Map: &mapping,
			Bindings: map[string]config.StoreBindingConfig{
				"archive": binding,
			},
		},
	)
	require.NoError(t, err)
	assert.Equal(t, int64(2), result.LooseAttachmentBlobs)
	assert.Zero(t, result.PackedAttachmentBlobs)

	restored, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, restored.Close()) }()
	stores, err := restored.BlobStores(t.Context())
	require.NoError(t, err)
	require.Len(t, stores, 2)
	assert.NotEqual(t, sourcePrimary.ID, stores[0].ID)
	assert.Equal(t, "restored archive", stores[1].Name)
	assert.NotEqual(t, sourcePrimary.ID, stores[1].ID)
	assert.NotEqual(t, sourcePrimary.OwnershipEpoch, stores[1].OwnershipEpoch)
	restoredConfig, err := config.Load(target)
	require.NoError(t, err)
	require.Equal(t, binding, restoredConfig.StoreBindings["archive"])
	reopenedRegistry := blob.NewRegistry(
		t.Context(), restored.VaultID(), restoredConfig.StoreBindings,
		[]blob.StoreSpec{{
			ID: stores[1].ID, Kind: stores[1].Kind, Role: stores[1].Role,
			Lifecycle: stores[1].Lifecycle, Binding: stores[1].Binding,
			OwnershipEpoch: stores[1].OwnershipEpoch,
		}},
	)
	t.Cleanup(func() { require.NoError(t, reopenedRegistry.Close()) })
	assert.Equal(t, blob.StoreOnline, reopenedRegistry.Observation(stores[1].ID).State)
	for _, content := range fixture.content {
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
		parsed, parseErr := packstore.ParseHash(hash)
		require.NoError(t, parseErr)
		resolution, resolveErr := restored.ResolveBlobLocations(t.Context(), parsed)
		require.NoError(t, resolveErr)
		require.Len(t, resolution.Candidates, 1)
		assert.Equal(t, packstore.StoreID(stores[1].ID), resolution.Candidates[0].StoreID)
		layout, layoutErr := packstore.NewLayout(
			filepath.Join(target, "blobs"), packstore.LayoutOptions{
				Staging: packstore.StagingStoreDirectory, StagingDir: "tmp",
			},
		)
		require.NoError(t, layoutErr)
		assert.FileExists(t, layout.LoosePath(parsed),
			"remote-only restore leaves untracked primary bytes for post-publication GC")
	}
	repaired, err := os.ReadFile(archiveLayout.LoosePath(corruptHash))
	require.NoError(t, err)
	assert.Equal(t, corruptContent, string(repaired))
}

func TestRestoreStoreMapFailureLeavesTargetDatabaseUnpublished(t *testing.T) {
	fixture := newArchiveFixture(t)
	sourcePrimary, err := fixture.metadata.PrimaryBlobStore(t.Context())
	require.NoError(t, err)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	_, err = backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs,
		backup.CreateOptions{Jobs: 1},
	)
	require.NoError(t, err)

	target := filepath.Join(t.TempDir(), "restored")
	mapping := backupapp.RestoreStoreMap{
		Version: backupapp.RestoreStoreMapVersion,
		Stores: []backupapp.RestoreStoreMapping{{
			SourceID: sourcePrimary.ID, Name: "missing", Binding: "missing",
		}},
	}
	_, err = backupapp.RestoreWithPlacement(
		t.Context(), repo, "test-version", store.DefaultSQLiteDriver(),
		backup.RestoreOptions{TargetDir: target, Jobs: 1},
		backupapp.RestorePlacementOptions{Map: &mapping},
	)
	require.ErrorContains(t, err, "is not loaded")
	assert.NoFileExists(t, filepath.Join(target, "docbank.db"))
}

func TestLoadRestoreStoreMapReadsOwnerPrivateBindingNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store-map.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
version = 1

[[stores]]
source_id = "10000000-0000-4000-8000-000000000001"
name = "cold archive"
binding = "archive_s3"
takeover = true
remote_only = true
allow_audited_remote_only = true
`), 0o600))
	mapping, err := backupapp.LoadRestoreStoreMap(path)
	require.NoError(t, err)
	require.Len(t, mapping.Stores, 1)
	assert.Equal(t, "archive_s3", mapping.Stores[0].Binding)
	assert.True(t, mapping.Stores[0].Takeover)
	assert.True(t, mapping.Stores[0].RemoteOnly)
	assert.True(t, mapping.Stores[0].AllowAuditedRemoteOnly)
}

func TestRestoreStoreMapKeepsAuditedBytesOnPrimaryWithoutAcknowledgment(t *testing.T) {
	fixture := newArchiveFixture(t)
	plan, err := fixture.metadata.PreviewInitialAudit(
		t.Context(), fixture.metadata.RootID(), "api", nil,
	)
	require.NoError(t, err)
	_, err = fixture.metadata.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	sourcePrimary, err := fixture.metadata.PrimaryBlobStore(t.Context())
	require.NoError(t, err)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	_, err = backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs,
		backup.CreateOptions{Jobs: 1},
	)
	require.NoError(t, err)

	target := filepath.Join(t.TempDir(), "restored")
	mapping := backupapp.RestoreStoreMap{
		Version: backupapp.RestoreStoreMapVersion,
		Stores: []backupapp.RestoreStoreMapping{{
			SourceID: sourcePrimary.ID, Name: "cold",
			Binding: "cold", RemoteOnly: true,
		}},
	}
	_, err = backupapp.RestoreWithPlacement(
		t.Context(), repo, "test-version", store.DefaultSQLiteDriver(),
		backup.RestoreOptions{TargetDir: target, Jobs: 1},
		backupapp.RestorePlacementOptions{
			Map: &mapping,
			Bindings: map[string]config.StoreBindingConfig{
				"cold": {Kind: "filesystem", Path: filepath.Join(t.TempDir(), "cold")},
			},
		},
	)
	require.NoError(t, err)
	restored, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, restored.Close()) }()
	for _, content := range fixture.content {
		hash, parseErr := packstore.ParseHash(
			fmt.Sprintf("%x", sha256.Sum256([]byte(content))),
		)
		require.NoError(t, parseErr)
		resolution, resolveErr := restored.ResolveBlobLocations(t.Context(), hash)
		require.NoError(t, resolveErr)
		assert.Len(t, resolution.Candidates, 2)
	}

	acknowledgedTarget := filepath.Join(t.TempDir(), "acknowledged")
	acknowledged := backupapp.RestoreStoreMap{
		Version: backupapp.RestoreStoreMapVersion,
		Stores: []backupapp.RestoreStoreMapping{{
			SourceID: sourcePrimary.ID, Name: "acknowledged cold",
			Binding: "acknowledged", RemoteOnly: true,
			AllowAuditedRemoteOnly: true,
		}},
	}
	_, err = backupapp.RestoreWithPlacement(
		t.Context(), repo, "test-version", store.DefaultSQLiteDriver(),
		backup.RestoreOptions{TargetDir: acknowledgedTarget, Jobs: 1},
		backupapp.RestorePlacementOptions{
			Map: &acknowledged,
			Bindings: map[string]config.StoreBindingConfig{
				"acknowledged": {
					Kind: "filesystem", Path: filepath.Join(t.TempDir(), "acknowledged-cold"),
				},
			},
		},
	)
	require.NoError(t, err)
	acknowledgedStore, err := store.Open(filepath.Join(acknowledgedTarget, "docbank.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, acknowledgedStore.Close()) }()
	for _, content := range fixture.content {
		hash, parseErr := packstore.ParseHash(
			fmt.Sprintf("%x", sha256.Sum256([]byte(content))),
		)
		require.NoError(t, parseErr)
		resolution, resolveErr := acknowledgedStore.ResolveBlobLocations(t.Context(), hash)
		require.NoError(t, resolveErr)
		require.Len(t, resolution.Candidates, 1)
	}
}

func TestCompressedLooseSnapshotRestoresIndexedAuthority(t *testing.T) {
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	blobsDir := filepath.Join(root, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	blobs, err := blob.NewWithOptions(store.NewPackCatalog(metadata), blobsDir, blob.Options{
		LooseCompression: blob.LooseCompressionOptions{
			Enabled: true, MinBytes: 1024, MinSavingsPercent: 10,
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, blobs.Close())
		require.NoError(t, metadata.Close())
	})

	content := strings.Repeat("compressible snapshot content\n", 1024)
	var written blob.WriteReceipt
	require.NoError(t, blobs.WithMutation(t.Context(), func() error {
		var writeErr error
		written, writeErr = blobs.WriteDetailedContext(t.Context(), strings.NewReader(content))
		if writeErr != nil {
			return writeErr
		}
		_, writeErr = metadata.CreateFile(t.Context(), metadata.RootID(), "compressed.txt",
			written.Hash, written.Size, "text/plain", store.BlobPhysical{
				Encoding: "zstd", StoredBytes: written.StoredSize, PackEligible: written.PackEligible,
			})
		return writeErr
	}))
	require.Equal(t, packstore.LooseEncodingZstd, written.Encoding)

	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	_, err = backupapp.Create(t.Context(), repo, "test-version", metadata, blobs, backup.CreateOptions{})
	require.NoError(t, err)
	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(t.Context(), repo, "test-version", backup.RestoreOptions{TargetDir: target})
	require.NoError(t, err)

	restoredStore, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restoredStore.Close()) })
	physical, err := restoredStore.PhysicalContent(t.Context(), written.Hash)
	require.NoError(t, err)
	assert.Equal(t, "packed", physical.Kind)
	assert.Equal(t, "zstd", physical.Encoding)
	assert.Equal(t, written.Size, physical.LogicalBytes)
	assert.Less(t, physical.StoredBytes, physical.LogicalBytes)
	assert.True(t, physical.PackEligible)
	backlog, err := restoredStore.LooseBacklog(t.Context())
	require.NoError(t, err)
	assert.Equal(t, store.LooseBacklog{}, backlog)

	restoredBlobs, err := blob.New(store.NewPackCatalog(restoredStore), filepath.Join(target, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restoredBlobs.Close()) })
	reader, size, err := restoredBlobs.OpenStreamContext(t.Context(), written.Hash)
	require.NoError(t, err)
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, written.Size, size)
	assert.Equal(t, content, string(got))
	assert.True(t, reader.Verified())
	require.NoError(t, reader.Close())
}

func TestStagedRenditionVerificationReadsLooseZstdAuthority(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "docbank.db")
	metadata, err := store.Open(databasePath)
	require.NoError(t, err)
	blobsDir := filepath.Join(root, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	blobs, err := blob.NewWithOptions(store.NewPackCatalog(metadata), blobsDir, blob.Options{
		LooseCompression: blob.LooseCompressionOptions{
			Enabled: true, MinBytes: 1, MinSavingsPercent: 0,
		},
	})
	require.NoError(t, err)

	content := strings.Repeat("synthetic staged rendition source\n", 64)
	var sourceHash string
	require.NoError(t, blobs.WithMutation(t.Context(), func() error {
		receipt, writeErr := blobs.WriteDetailedContext(t.Context(), strings.NewReader(content))
		if writeErr != nil {
			return writeErr
		}
		if receipt.Encoding != packstore.LooseEncodingZstd {
			return fmt.Errorf("staged rendition encoding = %v, want zstd", receipt.Encoding)
		}
		sourceHash = receipt.Hash
		node, createErr := metadata.CreateFile(
			t.Context(), metadata.RootID(), "staged.txt",
			receipt.Hash, receipt.Size, "text/plain",
			store.BlobPhysical{
				Encoding: "zstd", StoredBytes: receipt.StoredSize,
				PackEligible: receipt.PackEligible, Created: receipt.Created,
			},
		)
		if createErr != nil {
			return createErr
		}
		return metadata.RecordExtraction(t.Context(), store.ExtractionResult{
			BlobHash: node.BlobHash, Extractor: "plain-text", ExtractorVersion: 1,
			Status: store.ExtractionOK, Text: "synthetic extracted text",
		})
	}))
	report, err := metadata.MigrateLegacyPlainText(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, report.MigratedBuilds)
	require.NoError(t, blobs.Close())
	require.NoError(t, metadata.Close())

	driver := store.DefaultSQLiteDriver()
	raw, err := driver.Open(databasePath, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
	})
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `DELETE FROM rendition_heads`)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `DELETE FROM rendition_attachments`)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `UPDATE blobs SET size=size-1 WHERE hash=?`, sourceHash)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	restored, err := store.OpenForRestore(databasePath, driver)
	require.NoError(t, err)
	defer func() { require.NoError(t, restored.Close()) }()
	restoredBlobs, err := blob.New(store.NewPackCatalog(restored), blobsDir)
	require.NoError(t, err)
	defer func() { require.NoError(t, restoredBlobs.Close()) }()
	require.NoError(t, restored.VerifyRenditionBlobAuthority(t.Context()),
		"the catalog-only check cannot derive decoded zstd length")
	err = restored.VerifyRenditionBlobBytes(t.Context(), restoredBlobs)
	require.ErrorContains(t, err, "opening restored rendition blob "+sourceHash)
}

func TestAuditedIncrementalSnapshotRestoresCompleteProtectedHistory(t *testing.T) {
	fixture := newArchiveFixture(t)
	writeFile := func(parentID int64, name, content string) store.Node {
		t.Helper()
		var node store.Node
		require.NoError(t, fixture.blobs.WithMutation(t.Context(), func() error {
			hash, size, err := fixture.blobs.WriteContext(
				t.Context(), strings.NewReader(content),
			)
			if err != nil {
				return err
			}
			node, err = fixture.metadata.CreateFile(
				t.Context(), parentID, name, hash, size, "text/plain",
			)
			return err
		}))
		return node
	}

	records, err := fixture.metadata.Mkdir(t.Context(), fixture.metadata.RootID(), "Records")
	require.NoError(t, err)
	contracts, err := fixture.metadata.Mkdir(t.Context(), fixture.metadata.RootID(), "Contracts")
	require.NoError(t, err)
	document := writeFile(records.ID, "return.txt", "original tax return")
	initialVersionID := document.CurrentVersionID
	plan, err := fixture.metadata.PreviewInitialAudit(
		t.Context(), records.ID, "cli", nil,
	)
	require.NoError(t, err)
	initialStatus, err := fixture.metadata.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	require.Len(t, initialStatus.Scopes, 1)

	packed, err := fixture.blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 3, packed.BlobsPacked)
	require.Equal(t, 1, packed.PacksSealed)

	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	baseline, err := backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs,
		backup.CreateOptions{Jobs: 2},
	)
	require.NoError(t, err)
	assert.Empty(t, baseline.ParentID)
	secondPlan, err := fixture.metadata.PreviewInitialAudit(
		t.Context(), contracts.ID, "cli", nil,
	)
	require.NoError(t, err)
	secondStatus, err := fixture.metadata.EnableInitialAudit(t.Context(), secondPlan)
	require.NoError(t, err)
	require.Len(t, secondStatus.Scopes, 2)
	contract := writeFile(contracts.ID, "agreement.txt", "signed agreement")
	contractVersionID := contract.CurrentVersionID

	const replacementContent = "corrected tax return"
	var replacement store.ContentVersion
	require.NoError(t, fixture.blobs.WithMutation(t.Context(), func() error {
		hash, size, writeErr := fixture.blobs.WriteContext(
			t.Context(), strings.NewReader(replacementContent),
		)
		if writeErr != nil {
			return writeErr
		}
		document, replacement, writeErr = fixture.metadata.ReplaceContent(
			t.Context(), document.ID, document.Revision, hash, size, "text/plain",
		)
		return writeErr
	}))
	archive, err := fixture.metadata.Mkdir(t.Context(), records.ID, "Archive")
	require.NoError(t, err)
	receipt := writeFile(records.ID, "receipt.txt", "supporting receipt")
	receiptVersionID := receipt.CurrentVersionID
	receipt, _, err = fixture.metadata.Move(
		t.Context(), receipt.ID, archive.ID, receipt.Name, receipt.Revision,
	)
	require.NoError(t, err)
	trashedReceipt, _, err := fixture.metadata.Trash(
		t.Context(), receipt.ID, receipt.Revision,
	)
	require.NoError(t, err)
	tag, err := fixture.metadata.CreateTag(t.Context(), "filed")
	require.NoError(t, err)
	assignment, err := fixture.metadata.AssignTag(
		t.Context(), tag.ID, document.ID, document.Revision,
	)
	require.NoError(t, err)
	document = assignment.Node

	sourceStorage, err := fixture.blobs.Stats(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 3, int(sourceStorage.PackedBlobs))
	assert.Equal(t, 3, sourceStorage.LooseBlobs)
	wantMetadata := exportMetadata(t, fixture.metadata)
	sourceAudit, err := fixture.metadata.VerifyAudit(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, sourceAudit.Evidence.Enabled)
	require.Len(t, sourceAudit.Evidence.Scopes, 2)
	assert.ElementsMatch(t,
		[]string{initialStatus.Scopes[0].ID, secondPlan.Preview().ScopeID},
		[]string{sourceAudit.Evidence.Scopes[0].ID, sourceAudit.Evidence.Scopes[1].ID},
	)
	require.Len(t, sourceAudit.ProtectedBlobs, 4)

	manifest, err := backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs,
		backup.CreateOptions{Jobs: 2},
	)
	require.NoError(t, err)
	assert.Equal(t, baseline.SnapshotID, manifest.ParentID)
	assert.Equal(t, int64(6), manifest.Attachments.Blobs)
	verified, err := backup.Verify(
		t.Context(), repo, backupapp.New("test-version"),
		backup.VerifyOptions{All: true, Jobs: 2},
	)
	require.NoError(t, err)
	assert.Empty(t, verified.Problems)
	assert.ElementsMatch(t, []string{baseline.SnapshotID, manifest.SnapshotID}, verified.Snapshots)

	target := filepath.Join(t.TempDir(), "restored")
	result, err := backupapp.Restore(
		t.Context(), repo, "test-version",
		backup.RestoreOptions{TargetDir: target, Jobs: 2},
	)
	require.NoError(t, err)
	assert.Equal(t, manifest.Attachments.Blobs, result.AttachmentBlobs)
	restoredStore, err := store.OpenForRestore(
		filepath.Join(target, "docbank.db"), store.DefaultSQLiteDriver(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restoredStore.Close()) })
	assert.Equal(t, wantMetadata, exportMetadata(t, restoredStore))

	restoredAudit, err := restoredStore.VerifyAudit(t.Context(), &sourceAudit.Evidence)
	require.NoError(t, err)
	assert.Equal(t, sourceAudit.Evidence, restoredAudit.Evidence)
	require.NotNil(t, restoredAudit.EvidenceCheck)
	assert.True(t, restoredAudit.EvidenceCheck.Extends)
	assert.Empty(t, restoredAudit.EvidenceCheck.Problems)
	assert.Equal(t, sourceAudit.ProtectedBlobs, restoredAudit.ProtectedBlobs)
	assert.Equal(t, sourceAudit.ProtectedBytes, restoredAudit.ProtectedBytes)

	restoredBlobs, err := blob.New(
		store.NewPackCatalog(restoredStore), filepath.Join(target, "blobs"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restoredBlobs.Close()) })
	for versionID, want := range map[string]string{
		initialVersionID:  "original tax return",
		replacement.ID:    replacementContent,
		receiptVersionID:  "supporting receipt",
		contractVersionID: "signed agreement",
	} {
		version, versionErr := restoredStore.ContentVersionByID(t.Context(), versionID)
		require.NoError(t, versionErr)
		stream, _, openErr := restoredBlobs.OpenStreamContext(t.Context(), version.BlobHash)
		require.NoError(t, openErr)
		got, readErr := io.ReadAll(stream)
		require.NoError(t, readErr)
		assert.True(t, stream.Verified())
		require.NoError(t, stream.Close())
		assert.Equal(t, want, string(got))
	}

	restoredReceipt, err := restoredStore.NodeByID(t.Context(), trashedReceipt.ID)
	require.NoError(t, err)
	assert.NotNil(t, restoredReceipt.TrashedAt)
	restoredStatus, err := restoredStore.AuditStatus(t.Context(), &restoredReceipt.ID)
	require.NoError(t, err)
	require.NotNil(t, restoredStatus.Membership)
	assert.True(t, restoredStatus.Membership.Protected)
	assert.Equal(t, []string{initialStatus.Scopes[0].ID}, restoredStatus.Membership.ScopeIDs)
	restoredContractStatus, err := restoredStore.AuditStatus(t.Context(), &contract.ID)
	require.NoError(t, err)
	require.NotNil(t, restoredContractStatus.Membership)
	assert.True(t, restoredContractStatus.Membership.Protected)
	assert.Equal(t, []string{secondPlan.Preview().ScopeID},
		restoredContractStatus.Membership.ScopeIDs)
	history, err := restoredStore.AuditHistory(t.Context(), restoredReceipt.ID, 100, "")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, history.Total, 4)

	_, err = restoredStore.PruneContentVersions(
		t.Context(), document.ID, document.Revision,
		store.VersionPruneSelector{AllPrior: true}, true,
	)
	require.ErrorIs(t, err, store.ErrAuditMutationUnsupported)
	_, err = restoredStore.TrashEmpty(t.Context(), 0, true)
	require.ErrorIs(t, err, store.ErrAuditMutationUnsupported)
}

func TestTruncatedAuditJSONLRestoreLeavesNoPublishedDatabase(t *testing.T) {
	fixture := newArchiveFixture(t)
	records, err := fixture.metadata.Mkdir(t.Context(), fixture.metadata.RootID(), "Records")
	require.NoError(t, err)
	plan, err := fixture.metadata.PreviewInitialAudit(
		t.Context(), records.ID, "cli", nil,
	)
	require.NoError(t, err)
	_, err = fixture.metadata.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)

	metadata := bytes.TrimSuffix(exportMetadata(t, fixture.metadata), []byte("\n"))
	lastBreak := bytes.LastIndexByte(metadata, '\n')
	require.Positive(t, lastBreak)
	require.Contains(t, string(metadata[lastBreak+1:]), `"type":"audit_record"`)
	truncated := bytes.Clone(metadata[:lastBreak+1])

	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	_, err = backup.Create(t.Context(), repo, backupapp.New("test-version"), backup.CreateOptions{
		MetadataSource: rawMetadataSource{metadata: truncated},
	})
	require.NoError(t, err)
	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(
		t.Context(), repo, "test-version", backup.RestoreOptions{TargetDir: target},
	)
	require.ErrorContains(t, err, "audit")
	_, statErr := os.Stat(filepath.Join(target, "docbank.db"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestRestoreSupportsLegacySQLitePageSnapshots(t *testing.T) {
	fixture := newArchiveFixture(t)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	app := backupapp.New("legacy-version")
	manifest, err := backup.Create(t.Context(), repo, app, backup.CreateOptions{
		DBPath:        filepath.Join(fixture.root, "docbank.db"),
		ContentSource: backupapp.NewContentSource(fixture.blobs),
		SQLiteOpener:  backupapp.SQLiteOpener(fixture.metadata.SQLiteDriver()),
		Jobs:          2,
	})
	require.NoError(t, err)
	assert.Nil(t, manifest.Metadata)
	assert.NotEmpty(t, manifest.DB.Engine)

	target := filepath.Join(t.TempDir(), "restored")
	result, err := backupapp.Restore(t.Context(), repo, "current-version", backup.RestoreOptions{
		TargetDir: target,
		Jobs:      2,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), result.AttachmentBlobs)

	restoredStore, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	restoredBlobs, err := blob.New(store.NewPackCatalog(restoredStore), filepath.Join(target, "blobs"))
	require.NoError(t, err)
	for name, want := range fixture.content {
		node, nodeErr := restoredStore.NodeByPath(t.Context(), "/"+name)
		require.NoError(t, nodeErr)
		reader, openErr := restoredBlobs.OpenContext(t.Context(), node.BlobHash)
		require.NoError(t, openErr)
		got, readErr := io.ReadAll(reader)
		require.NoError(t, readErr)
		require.NoError(t, reader.Close())
		assert.Equal(t, want, string(got))
		_, versionErr := restoredStore.ContentVersionByID(t.Context(), node.CurrentVersionID)
		require.NoError(t, versionErr)
	}
	require.NoError(t, restoredBlobs.Close())
	require.NoError(t, restoredStore.Close())
}

func TestJSONLSnapshotRemainsStableAfterFreezeEnds(t *testing.T) {
	fixture := newArchiveFixture(t)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	freezer := freezeCoordinator{end: func(ctx context.Context) error {
		_, mkdirErr := fixture.metadata.Mkdir(ctx, fixture.metadata.RootID(), "created-after-snapshot")
		return mkdirErr
	}}

	manifest, err := backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs,
		backup.CreateOptions{Freezer: freezer, Jobs: 2})
	require.NoError(t, err)
	stats, err := backupapp.ParseStats(manifest.Stats)
	require.NoError(t, err)
	assert.Equal(t, int64(3), stats.Nodes)
	_, err = fixture.metadata.NodeByPath(t.Context(), "/created-after-snapshot")
	require.NoError(t, err)

	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(
		t.Context(), repo, "test-version", backup.RestoreOptions{TargetDir: target, Jobs: 2})
	require.NoError(t, err)
	restoredStore, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	_, err = restoredStore.NodeByPath(t.Context(), "/created-after-snapshot")
	require.Error(t, err)
	require.NoError(t, restoredStore.Close())
}

func TestJSONLSnapshotRejectsMalformedLiveMetadata(t *testing.T) {
	tests := []struct {
		name      string
		statement string
		want      string
	}{
		{
			name:      "invalid operation ID",
			statement: `UPDATE content_versions SET introduced_operation_id='not-a-uuid'`,
			want:      "invalid content version operation ID",
		},
		{
			name: "dangling blob reference",
			statement: `UPDATE content_versions SET blob_hash='` + strings.Repeat("d", 64) + `'
				WHERE rowid=(SELECT rowid FROM content_versions LIMIT 1)`,
			want: "metadata violates foreign key",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newArchiveFixture(t)
			rawDB, err := fixture.metadata.SQLiteDriver().Open(
				filepath.Join(fixture.root, "docbank.db"), docsqlite.OpenOptions{
					Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
				})
			require.NoError(t, err)
			rawDB.SetMaxOpenConns(1)
			_, err = rawDB.Exec(`PRAGMA foreign_keys=OFF`)
			require.NoError(t, err)
			_, err = rawDB.Exec(tt.statement)
			require.NoError(t, err)
			require.NoError(t, rawDB.Close())

			repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
			require.NoError(t, err)
			manifest, err := backupapp.Create(
				t.Context(), repo, "test-version", fixture.metadata, fixture.blobs, backup.CreateOptions{})
			require.ErrorContains(t, err, tt.want)
			assert.Nil(t, manifest)
			snapshots, err := repo.ListSnapshots()
			require.NoError(t, err)
			assert.Empty(t, snapshots)
		})
	}
}

func TestMalformedJSONLRestoreLeavesNoPublishedDatabase(t *testing.T) {
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	_, err = backup.Create(t.Context(), repo, backupapp.New("test-version"), backup.CreateOptions{
		MetadataSource: rawMetadataSource{metadata: []byte("{malformed\n")},
	})
	require.NoError(t, err)

	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(
		t.Context(), repo, "test-version", backup.RestoreOptions{TargetDir: target})
	require.ErrorContains(t, err, "importing metadata JSONL")
	_, statErr := os.Stat(filepath.Join(target, "docbank.db"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestLooseAbovePackingLimitSnapshotVerifyAndRestore(t *testing.T) {
	fixture := newArchiveFixture(t)
	size := blob.MaxPackedBlobBytes + 1
	var hash string
	require.NoError(t, fixture.blobs.WithMutation(t.Context(), func() error {
		var err error
		hash, _, err = fixture.blobs.WriteContext(t.Context(), io.LimitReader(zeroReader{}, size))
		if err != nil {
			return err
		}
		_, err = fixture.metadata.CreateFile(t.Context(), fixture.metadata.RootID(), "large-loose.bin",
			hash, size, "application/octet-stream")
		return err
	}))

	app := backupapp.New("test-version")
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	manifest, err := backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs, backup.CreateOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(3), manifest.Attachments.Blobs)
	verified, err := backup.Verify(t.Context(), repo, app, backup.VerifyOptions{})
	require.NoError(t, err)
	assert.Empty(t, verified.Problems)

	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(
		t.Context(), repo, "test-version", backup.RestoreOptions{TargetDir: target})
	require.NoError(t, err)
	restoredStore, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restoredStore.Close()) })
	restoredBlobs, err := blob.New(store.NewPackCatalog(restoredStore), filepath.Join(target, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restoredBlobs.Close()) })
	stream, restoredSize, err := restoredBlobs.OpenStreamContext(t.Context(), hash)
	require.NoError(t, err)
	assert.Equal(t, size, restoredSize)
	written, err := io.Copy(io.Discard, stream)
	require.NoError(t, err)
	assert.Equal(t, size, written)
	assert.True(t, stream.Verified())
	require.NoError(t, stream.Close())
	physical, err := restoredStore.PhysicalContent(t.Context(), hash)
	require.NoError(t, err)
	assert.Equal(t, store.PhysicalContent{
		Kind: "loose", Encoding: "raw", LogicalBytes: size,
		StoredBytes: size, PackEligible: false,
	}, physical)
	backlog, err := restoredStore.LooseBacklog(t.Context())
	require.NoError(t, err)
	assert.Equal(t, store.LooseBacklog{}, backlog)
}

func TestPackedSnapshotRequiresAndUsesPackedRestoreTarget(t *testing.T) {
	fixture := newArchiveFixture(t)
	packed, err := fixture.blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 2, packed.BlobsPacked)
	require.Equal(t, 1, packed.PacksSealed)

	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	app := backupapp.New("test-version")
	manifest, err := backupapp.Create(
		context.Background(), repo, "test-version", fixture.metadata, fixture.blobs, backup.CreateOptions{
			Jobs: 2,
		})
	require.NoError(t, err)
	assert.Equal(t, int64(2), manifest.Attachments.Blobs)

	verified, err := backup.Verify(t.Context(), repo, app, backup.VerifyOptions{Jobs: 2})
	require.NoError(t, err)
	assert.Empty(t, verified.Problems)

	unsafeTarget := filepath.Join(t.TempDir(), "unsafe-restored")
	_, err = backup.Restore(t.Context(), repo, app, backup.RestoreOptions{
		TargetDir: unsafeTarget, Jobs: 2,
		AuxiliaryTarget: auxiliaryTargetFunc(func(
			context.Context, []backup.RestoredAuxiliary,
		) error {
			return nil
		}),
	})
	require.ErrorContains(t, err, "requires a MetadataRestorer")

	target := filepath.Join(t.TempDir(), "restored")
	restored, err := backupapp.Restore(t.Context(), repo, "test-version", backup.RestoreOptions{
		TargetDir: target, Jobs: 2,
	})
	require.NoError(t, err)
	assert.Zero(t, restored.LooseAttachmentBlobs)
	assert.Equal(t, int64(2), restored.PackedAttachmentBlobs)
	assert.Positive(t, restored.AttachmentPacks)

	restoredStore, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	restoredCatalog := store.NewPackCatalog(restoredStore)
	records, err := restoredCatalog.ListPackRecords(t.Context())
	require.NoError(t, err)
	assert.NotEmpty(t, records)
	entries, err := restoredCatalog.ListIndexed(t.Context())
	require.NoError(t, err)
	assert.Len(t, entries, 2)
	restoredBlobs, err := blob.New(restoredCatalog, filepath.Join(target, "blobs"))
	require.NoError(t, err)
	for name, want := range fixture.content {
		node, nodeErr := restoredStore.NodeByPath(t.Context(), "/"+name)
		require.NoError(t, nodeErr)
		reader, openErr := restoredBlobs.OpenContext(t.Context(), node.BlobHash)
		require.NoError(t, openErr)
		got, readErr := io.ReadAll(reader)
		require.NoError(t, readErr)
		require.NoError(t, reader.Close())
		assert.Equal(t, want, string(got))
		version, versionErr := restoredStore.ContentVersionByID(t.Context(), node.CurrentVersionID)
		require.NoError(t, versionErr)
		assert.Equal(t, node.BlobHash, version.BlobHash)
	}
	require.NoError(t, restoredBlobs.Close())
	require.NoError(t, restoredStore.Close())
}

func TestVersionedEditingRoundTripsPackedRevertSource(t *testing.T) {
	fixture := newArchiveFixture(t)
	alpha, err := fixture.metadata.NodeByPath(t.Context(), "/alpha.txt")
	require.NoError(t, err)
	priorVersionID := alpha.CurrentVersionID
	packed, err := fixture.blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 2, packed.BlobsPacked)

	const replacement = "alpha replacement"
	var replaced store.Node
	require.NoError(t, fixture.blobs.WithMutation(t.Context(), func() error {
		hash, size, writeErr := fixture.blobs.WriteContext(t.Context(), strings.NewReader(replacement))
		if writeErr != nil {
			return writeErr
		}
		replaced, _, writeErr = fixture.metadata.ReplaceContent(
			t.Context(), alpha.ID, alpha.Revision, hash, size, "text/plain",
		)
		return writeErr
	}))
	reverted, revertVersion, _, err := fixture.metadata.RevertContent(
		t.Context(), alpha.ID, replaced.Revision, priorVersionID,
	)
	require.NoError(t, err)
	wantMetadata := exportMetadata(t, fixture.metadata)

	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	manifest, err := backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs,
		backup.CreateOptions{Jobs: 2})
	require.NoError(t, err)
	assert.Equal(t, int64(3), manifest.Attachments.Blobs,
		"backup must include both heads plus the other file")
	stats, err := backupapp.ParseStats(manifest.Stats)
	require.NoError(t, err)
	assert.Equal(t, int64(4), stats.ContentVersions)

	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(t.Context(), repo, "test-version", backup.RestoreOptions{
		TargetDir: target, Jobs: 2,
	})
	require.NoError(t, err)
	restoredStore, err := store.OpenForRestore(
		filepath.Join(target, "docbank.db"), store.DefaultSQLiteDriver(),
	)
	require.NoError(t, err)
	assert.Equal(t, string(wantMetadata), string(exportMetadata(t, restoredStore)),
		"JSONL restore must preserve the complete replacement history byte-for-byte")
	restoredBlobs, err := blob.New(store.NewPackCatalog(restoredStore), filepath.Join(target, "blobs"))
	require.NoError(t, err)

	restoredNode, err := restoredStore.NodeByID(t.Context(), alpha.ID)
	require.NoError(t, err)
	assert.Equal(t, reverted.CurrentVersionID, restoredNode.CurrentVersionID)
	assert.Equal(t, int64(3), restoredNode.Revision)
	for versionID, want := range map[string]string{
		priorVersionID:                "alpha backup",
		replaced.CurrentVersionID:     replacement,
		restoredNode.CurrentVersionID: "alpha backup",
	} {
		version, versionErr := restoredStore.ContentVersionByID(t.Context(), versionID)
		require.NoError(t, versionErr)
		stream, _, openErr := restoredBlobs.OpenStreamContext(t.Context(), version.BlobHash)
		require.NoError(t, openErr)
		got, readErr := io.ReadAll(stream)
		require.NoError(t, readErr)
		assert.True(t, stream.Verified())
		require.NoError(t, stream.Close())
		assert.Equal(t, want, string(got))
	}
	restoredRevert, err := restoredStore.ContentVersionByID(t.Context(), revertVersion.ID)
	require.NoError(t, err)
	require.NotNil(t, restoredRevert.SourceVersionID)
	assert.Equal(t, priorVersionID, *restoredRevert.SourceVersionID)
	require.NoError(t, restoredBlobs.Close())
	require.NoError(t, restoredStore.Close())
}

func TestPrunedVersionHistoryRoundTripsWithoutResurrection(t *testing.T) {
	fixture := newArchiveFixture(t)
	alpha, err := fixture.metadata.NodeByPath(t.Context(), "/alpha.txt")
	require.NoError(t, err)
	prunedVersionID := alpha.CurrentVersionID
	prunedBlobHash := alpha.BlobHash
	const replacement = "retained replacement"
	var replaced store.Node
	require.NoError(t, fixture.blobs.WithMutation(t.Context(), func() error {
		hash, size, writeErr := fixture.blobs.WriteContext(t.Context(), strings.NewReader(replacement))
		if writeErr != nil {
			return writeErr
		}
		replaced, _, writeErr = fixture.metadata.ReplaceContent(
			t.Context(), alpha.ID, alpha.Revision, hash, size, "text/plain",
		)
		return writeErr
	}))
	currentVersion, err := fixture.metadata.ContentVersionByID(t.Context(), replaced.CurrentVersionID)
	require.NoError(t, err)
	require.NoError(t, fixture.metadata.RecordExtraction(t.Context(), store.ExtractionResult{
		BlobHash: prunedBlobHash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: store.ExtractionOK, Text: "stale pruned extraction",
	}))
	require.NoError(t, fixture.metadata.RecordExtraction(t.Context(), store.ExtractionResult{
		BlobHash: currentVersion.BlobHash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: store.ExtractionOK, Text: "live retained extraction",
	}))
	pruned, err := fixture.metadata.PruneContentVersions(t.Context(), alpha.ID, replaced.Revision,
		store.VersionPruneSelector{AllPrior: true}, true)
	require.NoError(t, err)
	require.Equal(t, 1, pruned.DeletedVersions)
	ordinaryMetadata := exportMetadata(t, fixture.metadata)
	assert.Contains(t, string(ordinaryMetadata), prunedBlobHash,
		"ordinary metadata export retains unreachable rows for direct import and upgrades")
	assert.Contains(t, string(ordinaryMetadata), "stale pruned extraction")
	wantMetadata := exportBackupMetadata(t, fixture.metadata)
	assert.NotContains(t, string(wantMetadata), prunedBlobHash,
		"backup metadata excludes a blob with no retained logical authority")
	assert.NotContains(t, string(wantMetadata), "stale pruned extraction",
		"backup metadata excludes cache rows whose blobs have no retained authority")
	assert.Contains(t, string(wantMetadata), "live retained extraction",
		"backup metadata retains cache rows for current blob authority")

	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	manifest, err := backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs,
		backup.CreateOptions{Jobs: 2})
	require.NoError(t, err)
	stats, err := backupapp.ParseStats(manifest.Stats)
	require.NoError(t, err)
	assert.Equal(t, int64(2), stats.ContentVersions,
		"backup metadata must contain only the retained heads")
	assert.Equal(t, int64(1), stats.ExtractedText,
		"backup stats count only cache rows carried by the scoped metadata stream")

	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(t.Context(), repo, "test-version", backup.RestoreOptions{
		TargetDir: target, Jobs: 2,
	})
	require.NoError(t, err)
	restoredStore, err := store.OpenForRestore(
		filepath.Join(target, "docbank.db"), store.DefaultSQLiteDriver(),
	)
	require.NoError(t, err)
	defer func() { require.NoError(t, restoredStore.Close()) }()
	restoredMetadata := exportMetadata(t, restoredStore)
	assert.Equal(t, string(wantMetadata), string(restoredMetadata))
	assert.NotContains(t, string(restoredMetadata), prunedBlobHash)
	assert.NotContains(t, string(restoredMetadata), "stale pruned extraction")
	assert.Contains(t, string(restoredMetadata), "live retained extraction")
	_, err = restoredStore.ContentVersionByID(t.Context(), prunedVersionID)
	require.ErrorIs(t, err, store.ErrNotFound,
		"backup and restore must not resurrect released history")
	restoredAlpha, err := restoredStore.NodeByPath(t.Context(), "/alpha.txt")
	require.NoError(t, err)
	versions, total, err := restoredStore.ContentVersions(t.Context(), restoredAlpha.ID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, versions, 1)
	assert.Equal(t, replaced.CurrentVersionID, versions[0].ID)
}

func TestWatchCursorSurvivesBackupWithoutRetainingPrunedSourceBytes(t *testing.T) {
	fixture := newArchiveFixture(t)
	const (
		watchName  = "synthetic-watch"
		sourceRef  = "inbox/watched.txt"
		sourceData = "watched source before a manual edit"
		manualData = "manual replacement retained as authority"
	)
	var watched store.Node
	var sourceHash string
	require.NoError(t, fixture.blobs.WithMutation(t.Context(), func() error {
		var sourceSize int64
		var err error
		sourceHash, sourceSize, err = fixture.blobs.WriteContext(
			t.Context(), strings.NewReader(sourceData),
		)
		if err != nil {
			return err
		}
		run, err := fixture.metadata.BeginIngest(t.Context(), "watch", watchName)
		if err != nil {
			return err
		}
		watched, err = fixture.metadata.IngestFileExact(
			t.Context(), run, fixture.metadata.RootID(), "watched.txt",
			sourceHash, sourceSize, "text/plain", sourceRef, "",
		)
		return err
	}))
	prunedVersionID := watched.CurrentVersionID
	var replaced store.Node
	require.NoError(t, fixture.blobs.WithMutation(t.Context(), func() error {
		manualHash, manualSize, err := fixture.blobs.WriteContext(
			t.Context(), strings.NewReader(manualData),
		)
		if err != nil {
			return err
		}
		replaced, _, err = fixture.metadata.ReplaceContent(
			t.Context(), watched.ID, watched.Revision,
			manualHash, manualSize, "text/plain",
		)
		return err
	}))
	pruned, err := fixture.metadata.PruneContentVersions(
		t.Context(), watched.ID, replaced.Revision,
		store.VersionPruneSelector{AllPrior: true}, true,
	)
	require.NoError(t, err)
	require.Equal(t, 1, pruned.DeletedVersions)
	unreachable, err := fixture.metadata.UnreachableBlobs(t.Context())
	require.NoError(t, err)
	assert.Contains(t, unreachable, store.BlobInfo{Hash: sourceHash, Size: int64(len(sourceData))},
		"the operational cursor must not defeat explicit version pruning")

	ordinaryMetadata := string(exportMetadata(t, fixture.metadata))
	assert.Contains(t, ordinaryMetadata, `{"type":"blob","hash":"`+sourceHash+`"`,
		"ordinary metadata export preserves catalog rows for direct import")
	wantMetadata := string(exportBackupMetadata(t, fixture.metadata))
	assert.NotContains(t, wantMetadata, `{"type":"blob","hash":"`+sourceHash+`"`,
		"backup must not retain bytes released from document authority")
	assert.Contains(t, wantMetadata,
		`{"type":"watch_source","watch_name":"`+watchName+`","source_ref":"`+sourceRef+`"`,
		"backup must preserve the operational cursor across daemon restarts")
	assert.Contains(t, wantMetadata, `"blob_hash":"`+sourceHash+`"`)

	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	manifest, err := backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs,
		backup.CreateOptions{Jobs: 2},
	)
	require.NoError(t, err)
	assert.Equal(t, int64(3), manifest.Attachments.Blobs,
		"backup carries the two fixture heads and the manual replacement, not pruned source bytes")

	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(t.Context(), repo, "test-version", backup.RestoreOptions{
		TargetDir: target, Jobs: 2,
	})
	require.NoError(t, err)
	restoredStore, err := store.OpenForRestore(
		filepath.Join(target, "docbank.db"), store.DefaultSQLiteDriver(),
	)
	require.NoError(t, err)
	defer func() { require.NoError(t, restoredStore.Close()) }()
	restoredMetadata := string(exportMetadata(t, restoredStore))
	assert.Contains(t, restoredMetadata,
		`{"type":"watch_source","watch_name":"`+watchName+`","source_ref":"`+sourceRef+`"`)
	assert.NotContains(t, restoredMetadata, `{"type":"blob","hash":"`+sourceHash+`"`)
	_, err = restoredStore.ContentVersionByID(t.Context(), prunedVersionID)
	require.ErrorIs(t, err, store.ErrNotFound)
	restoredNode, err := restoredStore.NodeByID(t.Context(), watched.ID)
	require.NoError(t, err)
	assert.Equal(t, replaced.CurrentVersionID, restoredNode.CurrentVersionID)
	restoredBlobs, err := blob.New(
		store.NewPackCatalog(restoredStore), filepath.Join(target, "blobs"),
	)
	require.NoError(t, err)
	defer func() { require.NoError(t, restoredBlobs.Close()) }()
	var unchanged store.Node
	var noVersion store.ContentVersion
	var changed bool
	require.NoError(t, restoredBlobs.WithMutation(t.Context(), func() error {
		incomingHash, incomingSize, writeErr := restoredBlobs.WriteContext(
			t.Context(), strings.NewReader(sourceData),
		)
		if writeErr != nil {
			return writeErr
		}
		if incomingHash != sourceHash {
			return fmt.Errorf("restored watch input hash = %s, want %s", incomingHash, sourceHash)
		}
		unchanged, noVersion, changed, writeErr = restoredStore.SyncWatchedContent(
			t.Context(), watchName, sourceRef,
			incomingHash, incomingSize, "text/plain",
		)
		return writeErr
	}))
	assert.False(t, changed,
		"the restored cursor must recognize unchanged source bytes")
	assert.Empty(t, noVersion.ID)
	assert.Equal(t, replaced.CurrentVersionID, unchanged.CurrentVersionID,
		"an unchanged watched source must not overwrite the retained manual edit")
}

func TestDerivativeAuthoritySnapshotRestoresCatalogBlobsAndRebuildsLexicalProjection(t *testing.T) {
	fixture := newArchiveFixture(t)
	source, err := fixture.metadata.NodeByPath(t.Context(), "/alpha.txt")
	require.NoError(t, err)
	var providerCalls atomic.Int64
	providerSentinel := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		providerCalls.Add(1)
	}))
	t.Cleanup(providerSentinel.Close)

	orphanProviderOutput := "synthetic provider result abandoned before staging"
	evidence := "synthetic normalized evidence"
	markdown := "# Synthetic backup rendition\n"
	stagedSource := "synthetic source retained by an unattached staged build"
	var orphanHash, evidenceHash, markdownHash, stagedSourceHash string
	require.NoError(t, fixture.blobs.WithMutation(t.Context(), func() error {
		orphanReceipt, writeErr := fixture.blobs.WriteDetailedContext(
			t.Context(), strings.NewReader(orphanProviderOutput),
		)
		if writeErr != nil {
			return writeErr
		}
		orphanEncoding, err := orphanReceipt.EncodingName()
		if err != nil {
			return err
		}
		orphanHash = orphanReceipt.Hash
		if err := fixture.metadata.RecordRenditionBlob(t.Context(), orphanReceipt.Hash, orphanReceipt.Size,
			store.BlobPhysical{Encoding: orphanEncoding, StoredBytes: orphanReceipt.StoredSize,
				PackEligible: orphanReceipt.PackEligible, Created: orphanReceipt.Created}); err != nil {
			return err
		}

		stagedSourceReceipt, writeErr := fixture.blobs.WriteDetailedContext(
			t.Context(), strings.NewReader(stagedSource),
		)
		if writeErr != nil {
			return writeErr
		}
		stagedSourceEncoding, err := stagedSourceReceipt.EncodingName()
		if err != nil {
			return err
		}
		stagedSourceHash = stagedSourceReceipt.Hash
		if err := fixture.metadata.RecordRenditionBlob(
			t.Context(), stagedSourceReceipt.Hash, stagedSourceReceipt.Size,
			store.BlobPhysical{Encoding: stagedSourceEncoding, StoredBytes: stagedSourceReceipt.StoredSize,
				PackEligible: stagedSourceReceipt.PackEligible, Created: stagedSourceReceipt.Created},
		); err != nil {
			return err
		}

		evidenceReceipt, writeErr := fixture.blobs.WriteDetailedContext(t.Context(), strings.NewReader(evidence))
		if writeErr != nil {
			return writeErr
		}
		evidenceEncoding, err := evidenceReceipt.EncodingName()
		if err != nil {
			return err
		}
		evidenceHash = evidenceReceipt.Hash
		if err := fixture.metadata.RecordRenditionBlob(t.Context(), evidenceReceipt.Hash, evidenceReceipt.Size,
			store.BlobPhysical{Encoding: evidenceEncoding, StoredBytes: evidenceReceipt.StoredSize,
				PackEligible: evidenceReceipt.PackEligible, Created: evidenceReceipt.Created}); err != nil {
			return err
		}
		markdownReceipt, writeErr := fixture.blobs.WriteDetailedContext(t.Context(), strings.NewReader(markdown))
		if writeErr != nil {
			return writeErr
		}
		markdownEncoding, err := markdownReceipt.EncodingName()
		if err != nil {
			return err
		}
		markdownHash = markdownReceipt.Hash
		return fixture.metadata.RecordRenditionBlob(t.Context(), markdownReceipt.Hash, markdownReceipt.Size,
			store.BlobPhysical{Encoding: markdownEncoding, StoredBytes: markdownReceipt.StoredSize,
				PackEligible: markdownReceipt.PackEligible, Created: markdownReceipt.Created})
	}))

	profile := backupProcessingProfile(t)
	build := store.RenditionBuildRecord{
		ID:                                backupHash("build"),
		VaultID:                           fixture.metadata.VaultID(),
		SourceSHA256:                      source.BlobHash,
		RenditionRequestFingerprint:       profile.RenditionRequestFingerprint,
		EvidenceLexicalFingerprint:        profile.EvidenceLexicalFingerprint,
		CapturedArtifactPolicyFingerprint: backupHash(backupCapturedArtifactPolicy),
		CapturedArtifactPolicy:            jsontext.Value(backupCapturedArtifactPolicy),
		AuthorizationChecksum:             backupHash("authorization"),
		ProviderOperationID:               "synthetic-provider-operation",
		ProviderReceipt: jsontext.Value(fmt.Sprintf(
			`{"endpoint":%q,"provider":"synthetic"}`, providerSentinel.URL,
		)),
		EvidenceChecksum:      evidenceHash,
		RenditionChecksum:     backupHash("rendition"),
		MarkdownChecksum:      markdownHash,
		Completeness:          document.EvidenceComplete,
		Warnings:              []string{},
		CompletedAt:           "2026-08-23T00:00:00.000000000Z",
		DeclaredArtifactCount: 2,
		Artifacts: []store.RenditionArtifactRecord{
			{ID: "evidence", Role: "normalized_evidence", BlobHash: evidenceHash,
				Size: int64(len(evidence)), Checksum: evidenceHash, State: store.RenditionArtifactVerified},
			{ID: "markdown", Role: "sanitized_markdown", BlobHash: markdownHash,
				Size: int64(len(markdown)), Checksum: markdownHash, State: store.RenditionArtifactVerified},
		},
		Units: []store.RenditionUnitRecord{{
			ID: "unit", EvidenceUnitID: "evidence-unit", Order: 0, Checksum: backupHash("unit"),
			HeadingPath: []string{"Synthetic backup rendition"},
			Locator: document.EvidenceLocatorV1{Kind: document.EvidenceLocatorPage,
				IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1},
		}},
		LexicalSegments: []store.RenditionLexicalSegmentRecord{{
			ID: "segment", UnitID: "unit", Order: 0, CharStart: 0, CharEnd: len("Synthetic backup rendition"),
			Checksum: backupHash("segment"), Text: "Synthetic backup rendition",
		}},
	}
	require.NoError(t, fixture.metadata.StageRenditionBuild(t.Context(), build))
	stagedBuild := build
	stagedBuild.ID = backupHash("unattached-staged-build")
	stagedBuild.SourceSHA256 = stagedSourceHash
	stagedBuild.ProviderOperationID = "synthetic-unattached-staged-build"
	require.NoError(t, fixture.metadata.StageRenditionBuild(t.Context(), stagedBuild))
	generation, err := fixture.metadata.StageLexicalGeneration(
		t.Context(), backupHash("restored-lexical-generation"),
	)
	require.NoError(t, err)
	attachment := store.RenditionAttachmentRecord{
		ID: backupHash("attachment"), VaultID: fixture.metadata.VaultID(),
		ContentVersionID: source.CurrentVersionID, BuildID: build.ID, Profile: profile,
		AttachedAt: "2026-08-23T00:01:00.000000000Z",
	}
	require.NoError(t, fixture.metadata.PublishRenditionAndLexicalHeads(
		t.Context(), attachment, store.RenditionHeadRecord{
			ContentVersionID: source.CurrentVersionID, ProcessingProfileFingerprint: profile.Fingerprint,
			AttachmentID: attachment.ID, PublishedAt: "2026-08-23T00:02:00.000000000Z",
		}, generation.ID))
	assert.Contains(t, string(exportMetadata(t, fixture.metadata)), orphanHash,
		"ordinary metadata export preserves upgrade and direct-import blob semantics")
	backupMetadata := exportBackupMetadata(t, fixture.metadata)
	assert.NotContains(t, string(backupMetadata), orphanHash,
		"backup metadata must omit provider output with no catalog authority")
	assert.Contains(t, string(backupMetadata), stagedSourceHash,
		"backup metadata exports every retained staged rendition build source")

	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	manifest, err := backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs, backup.CreateOptions{},
	)
	require.NoError(t, err)
	assert.Equal(t, int64(5), manifest.Attachments.Rows,
		"two original versions, two derivative artifacts, and one staged build source are the complete authority")
	assert.Equal(t, int64(5), manifest.Attachments.Blobs)
	assert.Equal(t, int64(len("alpha backup")+len("bravo backup")+len(evidence)+len(markdown)+len(stagedSource)),
		manifest.Attachments.BlobBytes)
	known, err := repo.LoadBlobIndex()
	require.NoError(t, err)
	refs, _, err := backup.LoadListRefs(repo, known, manifest.Attachments.Lists, nil, packstore.PackExt)
	require.NoError(t, err)
	assert.NotContains(t, refs, backup.ContentRef{Hash: orphanHash, Size: int64(len(orphanProviderOutput))})
	assert.Contains(t, refs, backup.ContentRef{Hash: evidenceHash, Size: int64(len(evidence))})
	assert.Contains(t, refs, backup.ContentRef{Hash: markdownHash, Size: int64(len(markdown))})
	assert.Contains(t, refs, backup.ContentRef{Hash: stagedSourceHash, Size: int64(len(stagedSource))})
	var snapshotStats struct {
		DerivativeAuthority *struct {
			Version           int      `json:"version"`
			ProviderDependent []string `json:"provider_dependent"`
			Classes           []struct {
				Class          string `json:"class"`
				Classification string `json:"classification"`
				Count          int64  `json:"count"`
				LogicalBytes   int64  `json:"logical_bytes"`
				BlobCount      int64  `json:"blob_count"`
				Checksum       string `json:"checksum"`
			} `json:"classes"`
		} `json:"derivative_authority"`
	}
	require.NoError(t, json.Unmarshal(manifest.Stats, &snapshotStats))
	require.NotNil(t, snapshotStats.DerivativeAuthority)
	authority := snapshotStats.DerivativeAuthority
	assert.Equal(t, 1, authority.Version)
	assert.Empty(t, authority.ProviderDependent, "no vector authority exists before the embedding foundation")
	require.Len(t, authority.Classes, 3)
	assert.Equal(t, "normalized_evidence", authority.Classes[0].Class)
	assert.Equal(t, "included", authority.Classes[0].Classification)
	assert.Equal(t, int64(2), authority.Classes[0].Count)
	assert.Equal(t, int64(2*len(evidence)), authority.Classes[0].LogicalBytes)
	assert.Equal(t, int64(1), authority.Classes[0].BlobCount)
	assert.NotEmpty(t, authority.Classes[0].Checksum)
	assert.Equal(t, "sanitized_markdown", authority.Classes[1].Class)
	assert.Equal(t, "included", authority.Classes[1].Classification)
	assert.Equal(t, int64(2), authority.Classes[1].Count)
	assert.Equal(t, int64(2*len(markdown)), authority.Classes[1].LogicalBytes)
	assert.Equal(t, int64(1), authority.Classes[1].BlobCount)
	assert.NotEmpty(t, authority.Classes[1].Checksum)
	assert.Equal(t, "lexical_projection", authority.Classes[2].Class)
	assert.Equal(t, "reconstructible", authority.Classes[2].Classification)
	assert.Equal(t, int64(2), authority.Classes[2].Count)
	assert.Equal(t, int64(2*len("Synthetic backup rendition")), authority.Classes[2].LogicalBytes)
	assert.Zero(t, authority.Classes[2].BlobCount)
	assert.NotEmpty(t, authority.Classes[2].Checksum)

	target := filepath.Join(t.TempDir(), "restored")
	result, err := backupapp.Restore(t.Context(), repo, "test-version", backup.RestoreOptions{TargetDir: target})
	require.NoError(t, err)
	assert.Equal(t, int64(5), result.AttachmentBlobs)
	assert.Equal(t, int64(5), result.PackedAttachmentBlobs)
	assert.Zero(t, result.LooseAttachmentBlobs)
	restored, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	_, err = restored.BlobInfo(t.Context(), orphanHash)
	require.ErrorIs(t, err, store.ErrNotFound,
		"restore must not publish an unreferenced provider-output blob row")
	orphanID, err := packstore.ParseHash(orphanHash)
	require.NoError(t, err)
	orphanResolution, err := restored.ResolveBlobLocations(t.Context(), orphanID)
	require.NoError(t, err)
	assert.False(t, orphanResolution.Member,
		"restore must not publish physical authority for unreferenced provider output")
	for _, hash := range []string{source.BlobHash, evidenceHash, markdownHash, stagedSourceHash} {
		want, sourceErr := fixture.metadata.BlobInfo(t.Context(), hash)
		require.NoError(t, sourceErr)
		got, restoreErr := restored.BlobInfo(t.Context(), hash)
		require.NoError(t, restoreErr)
		assert.Equal(t, want, got, "authorized blob record %s must remain byte-stable", hash)
		id, parseErr := packstore.ParseHash(hash)
		require.NoError(t, parseErr)
		resolution, resolveErr := restored.ResolveBlobLocations(t.Context(), id)
		require.NoError(t, resolveErr)
		assert.True(t, resolution.Member)
		assert.NotEmpty(t, resolution.Candidates)
	}
	_, err = restored.ActiveLexicalGeneration(t.Context())
	require.NoError(t, err, "restore must rebuild the excluded lexical projection locally")
	hits, truncated, err := restored.SearchPage(t.Context(), "Synthetic backup", 10)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, hits, 1)
	assert.Equal(t, source.ID, hits[0].Node.ID)
	assert.Zero(t, providerCalls.Load(), "restore must not contact the configured provider sentinel")

	looseRepo, err := backup.Init(filepath.Join(t.TempDir(), "loose-repo"))
	require.NoError(t, err)
	_, err = backupapp.Create(
		t.Context(), looseRepo, "test-version", fixture.metadata, fixture.blobs, backup.CreateOptions{},
	)
	require.NoError(t, err)
	looseTarget := filepath.Join(t.TempDir(), "loose-restored")
	looseResult, err := backupapp.RestoreWithPlacement(
		t.Context(), looseRepo, "test-version", store.DefaultSQLiteDriver(),
		backup.RestoreOptions{TargetDir: looseTarget},
		backupapp.RestorePlacementOptions{Map: &backupapp.RestoreStoreMap{
			Version: backupapp.RestoreStoreMapVersion,
		}},
	)
	require.NoError(t, err)
	assert.Equal(t, int64(5), looseResult.AttachmentBlobs)
	assert.Zero(t, looseResult.PackedAttachmentBlobs)
	assert.Equal(t, int64(5), looseResult.LooseAttachmentBlobs)
	looseStore, err := store.Open(filepath.Join(looseTarget, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, looseStore.Close()) })
	_, err = looseStore.ActiveRendition(t.Context(), source.CurrentVersionID, profile.Fingerprint)
	require.NoError(t, err)
	assert.Zero(t, providerCalls.Load(), "loose restore must not contact the configured provider sentinel")

	t.Run("rejects a staged rendition source whose restored packed authority disagrees", func(t *testing.T) {
		originalMetadata := exportBackupMetadata(t, fixture.metadata)
		mismatch := bytes.Replace(
			originalMetadata,
			[]byte(fmt.Sprintf(`"hash":"%s","size":%d,`, stagedSourceHash, len(stagedSource))),
			[]byte(fmt.Sprintf(`"hash":"%s","size":%d,`, stagedSourceHash, len(stagedSource)-1)),
			1,
		)
		require.NotEqual(t, originalMetadata, mismatch)
		mismatchRepo, createErr := backup.Init(filepath.Join(t.TempDir(), "mismatch-repo"))
		require.NoError(t, createErr)
		_, createErr = backup.Create(t.Context(), mismatchRepo, backupapp.New("test-version"), backup.CreateOptions{
			MetadataSource: overriddenMetadataSource{
				source: backupapp.NewMetadataSource(fixture.metadata), metadata: mismatch,
			},
			ContentSource: backupapp.NewContentSource(fixture.blobs),
		})
		require.NoError(t, createErr)

		failureTarget := filepath.Join(t.TempDir(), "preexisting-target")
		beforeDigest := markRestoreTarget(t, failureTarget)
		_, restoreErr := backupapp.Restore(t.Context(), mismatchRepo, "test-version", backup.RestoreOptions{
			TargetDir: failureTarget, Overwrite: true,
		})
		require.ErrorContains(t, restoreErr, "restored rendition blob "+stagedSourceHash+" size")
		assertRestoreTargetUnchanged(t, failureTarget, beforeDigest)
		assert.Zero(t, providerCalls.Load(), "authority rejection must not contact a provider")
	})

	for _, tt := range []struct {
		name   string
		mutate func(t *testing.T, repo *backup.Repo, preseedIndex string, hash string)
		want   string
	}{
		{
			name: "missing authorized derivative",
			mutate: func(t *testing.T, _ *backup.Repo, preseedIndex, _ string) {
				t.Helper()
				require.NoError(t, os.Remove(preseedIndex))
			},
			want: "not present in any index",
		},
		{
			name: "checksum mismatched authorized derivative",
			mutate: func(t *testing.T, repo *backup.Repo, _ string, hash string) {
				t.Helper()
				corruptArchiveBlob(t, repo, hash)
			},
			want: evidenceHash,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			corruptRepo, createErr := backup.Init(filepath.Join(t.TempDir(), "corrupt-repo"))
			require.NoError(t, createErr)
			preseedIndex := preseedArchiveBlob(t, corruptRepo, []byte(evidence))
			_, createErr = backupapp.Create(
				t.Context(), corruptRepo, "test-version", fixture.metadata, fixture.blobs, backup.CreateOptions{},
			)
			require.NoError(t, createErr)

			failureTarget := filepath.Join(t.TempDir(), "existing-target")
			_, restoreErr := backupapp.Restore(
				t.Context(), corruptRepo, "test-version", backup.RestoreOptions{TargetDir: failureTarget},
			)
			require.NoError(t, restoreErr)
			marker, markerErr := store.Open(filepath.Join(failureTarget, "docbank.db"))
			require.NoError(t, markerErr)
			_, markerErr = marker.Mkdir(t.Context(), marker.RootID(), "preexisting-restore-marker")
			require.NoError(t, markerErr)
			require.NoError(t, marker.Close())
			beforeDigestData, readErr := os.ReadFile(filepath.Join(failureTarget, "docbank.db"))
			require.NoError(t, readErr)
			beforeDigestSum := sha256.Sum256(beforeDigestData)
			beforeDigest := hex.EncodeToString(beforeDigestSum[:])
			before := activeDerivative(t, failureTarget, source.CurrentVersionID, profile.Fingerprint)

			tt.mutate(t, corruptRepo, preseedIndex, evidenceHash)
			_, restoreErr = backupapp.Restore(
				t.Context(), corruptRepo, "test-version",
				backup.RestoreOptions{TargetDir: failureTarget, Overwrite: true},
			)
			require.ErrorContains(t, restoreErr, tt.want)
			after := activeDerivative(t, failureTarget, source.CurrentVersionID, profile.Fingerprint)
			assert.Equal(t, before.Head, after.Head, "failed restore must not publish a replacement head")
			assert.Equal(t, before.Attachment.ID, after.Attachment.ID)
			assert.Equal(t, before.Build.ID, after.Build.ID)
			assertRestoreTargetUnchanged(t, failureTarget, beforeDigest)
			assert.Zero(t, providerCalls.Load(), "failed restore must not contact the configured provider sentinel")
		})
	}
}

func preseedArchiveBlob(t *testing.T, repo *backup.Repo, raw []byte) string {
	t.Helper()
	known, err := repo.LoadBlobIndex()
	require.NoError(t, err)
	appender := backup.NewPackAppender(repo, known, pack.DefaultZstdLevel, nil, packstore.PackExt)
	id, added, err := appender.Add(raw)
	require.NoError(t, err)
	assert.True(t, added)
	assert.Equal(t, backupHash(string(raw)), id.String())
	_, entries, err := appender.Finish()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	indexID, err := repo.WriteIndex(entries)
	require.NoError(t, err)
	return repo.Path("indexes", indexID+".mvidx")
}

func corruptArchiveBlob(t *testing.T, repo *backup.Repo, hash string) {
	t.Helper()
	id, err := pack.ParseBlobID(hash)
	require.NoError(t, err)
	known, err := repo.LoadBlobIndex()
	require.NoError(t, err)
	entry, ok := known[id]
	require.True(t, ok)
	path := repo.Path("packs", entry.PackID[:2], entry.PackID+packstore.PackExt)
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	var byteAtEntry [1]byte
	_, err = file.ReadAt(byteAtEntry[:], int64(entry.Offset))
	require.NoError(t, err)
	byteAtEntry[0] ^= 0xff
	_, err = file.WriteAt(byteAtEntry[:], int64(entry.Offset))
	require.NoError(t, err)
	require.NoError(t, file.Sync())
	require.NoError(t, file.Close())
}

func activeDerivative(t *testing.T, target, versionID, profileID string) store.RenditionView {
	t.Helper()
	metadata, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, metadata.Close()) }()
	view, err := metadata.ActiveRendition(t.Context(), versionID, profileID)
	require.NoError(t, err)
	return view
}

const backupCapturedArtifactPolicy = `{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`

func backupHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func backupProcessingProfile(t *testing.T) store.ProcessingProfileRecord {
	t.Helper()
	profile := document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1,
		Rendition: &document.RenditionBindingV1{
			AdapterContract: "rendition-adapter/v1", AuthorizationFingerprint: backupHash("authorization-binding"),
			CredentialBinding: "credential:synthetic", DeploymentFingerprint: backupHash("deployment"),
			Descriptor:            document.ProviderDescriptorV1{ID: "synthetic-rendition", Fingerprint: backupHash("provider")},
			DisclosureFingerprint: backupHash("disclosure"), MaxDocumentBytes: 1 << 20,
			MaxResponseBytes: 1 << 20, MaxUnits: 100, Name: "primary",
			RequestedArtifacts: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
			TrustBoundary:      "synthetic-vault", UploadOptionsFingerprint: backupHash("upload-options"),
		},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			CompletenessFingerprint: backupHash("completeness"), LexicalSegmenterFingerprint: backupHash("segmenter"),
			MaxDocumentChars: 100_000, MaxSegmentRunes: 100, MaxUnitRunes: 1_000,
			NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
			NormalizerFingerprint:      backupHash("normalizer"), RenditionContract: document.RenditionContractV1,
			SanitizerFingerprint: backupHash("sanitizer"), SourceEvidenceContract: document.SourceEvidenceContractV1,
		},
		Retrieval: document.RetrievalPolicyV1{LexicalLimit: 100, VectorLimit: 100},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{
			AttachmentPolicyFingerprint: backupHash("attachment-policy"), ConsentFingerprint: backupHash("consent"),
			RetainSanitizedMarkdown: true, RetainTypedArtifacts: true, TrustBoundary: "synthetic-vault",
		},
	}
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	return store.ProcessingProfileRecord{
		Fingerprint: fingerprints.Profile, CanonicalProfile: jsontext.Value(canonical),
		RenditionRequestFingerprint:    fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:     fingerprints.EvidenceLexical,
		RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
		AttachmentPolicyFingerprint:    profile.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             profile.RetentionDisclosure.ConsentFingerprint,
		RenditionDisclosureFingerprint: profile.Rendition.DisclosureFingerprint,
		TrustBoundary:                  profile.RetentionDisclosure.TrustBoundary,
	}
}

// legacyMetadataSnapshot reproduces the pre-scope v1 capture surface: every
// blobs row in the attachment lists and metadata stream, and fidelity stats
// counted over the complete tables rather than catalog authority.
type legacyMetadataSource struct{ metadata *store.Store }

func (legacyMetadataSource) Format() string { return backupapp.MetadataFormat }

func (s legacyMetadataSource) OpenSnapshot(
	ctx context.Context,
) (backup.MetadataSnapshot, error) {
	embedded, err := backupapp.NewMetadataSource(s.metadata).OpenSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	pinned, err := s.metadata.BeginMetadataSnapshot(ctx)
	if err != nil {
		_ = embedded.Close()
		return nil, err
	}
	return legacyMetadataSnapshot{MetadataSnapshot: embedded, pinned: pinned}, nil
}

type legacyMetadataSnapshot struct {
	backup.MetadataSnapshot

	pinned *store.MetadataSnapshot
}

func (s legacyMetadataSnapshot) AuxiliaryArtifacts(
	ctx context.Context,
) ([]backup.AuxiliaryArtifact, error) {
	source, ok := s.MetadataSnapshot.(backup.AuxiliarySource)
	if !ok {
		return nil, nil
	}
	artifacts, err := source.AuxiliaryArtifacts(ctx)
	if err != nil {
		return nil, fmt.Errorf("opening legacy metadata auxiliary artifacts: %w", err)
	}
	return artifacts, nil
}

func (s legacyMetadataSnapshot) OpenMetadata(
	ctx context.Context,
) (io.ReadCloser, int64, error) {
	var buffer bytes.Buffer
	if err := s.pinned.Export(ctx, &buffer); err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(buffer.Bytes())), int64(buffer.Len()), nil
}

func (s legacyMetadataSnapshot) ContentInfo(
	ctx context.Context,
) (*backup.ContentInfo, error) {
	rows, err := s.pinned.QueryContext(ctx, `SELECT hash, size FROM blobs ORDER BY hash`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var refs []backup.ContentRef
	for rows.Next() {
		var ref backup.ContentRef
		if err := rows.Scan(&ref.Hash, &ref.Size); err != nil {
			return nil, err
		}
		parsed, err := packstore.ParseHash(ref.Hash)
		if err != nil {
			return nil, fmt.Errorf("legacy blob hash %q: %w", ref.Hash, err)
		}
		_ = parsed
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &backup.ContentInfo{Refs: refs, Rows: int64(len(refs))}, nil
}

func (s legacyMetadataSnapshot) Stats(ctx context.Context) (jsontext.Value, error) {
	stats := backupapp.Stats{}
	counts := []struct {
		dst   *int64
		query string
	}{
		{&stats.Nodes, `SELECT COUNT(*) FROM nodes`},
		{&stats.Files, `SELECT COUNT(*) FROM nodes WHERE kind = 'file'`},
		{&stats.Directories, `SELECT COUNT(*) FROM nodes WHERE kind = 'dir'`},
		{&stats.TrashedNodes, `SELECT COUNT(*) FROM nodes WHERE trashed_at IS NOT NULL`},
		{&stats.ContentVersions, `SELECT COUNT(*) FROM content_versions`},
		{&stats.Ingests, `SELECT COUNT(*) FROM ingests`},
		{&stats.Provenance, `SELECT COUNT(*) FROM provenance`},
		{&stats.Tags, `SELECT COUNT(*) FROM tags`},
		{&stats.NodeTags, `SELECT COUNT(*) FROM node_tags`},
	}
	for _, count := range counts {
		if err := s.pinned.QueryRowContext(ctx, count.query).Scan(count.dst); err != nil {
			return nil, err
		}
	}
	if err := s.pinned.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM blobs`,
	).Scan(&stats.Blobs, &stats.BlobBytes); err != nil {
		return nil, err
	}
	if err := s.pinned.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM extracted_text`).Scan(&stats.ExtractedText); err != nil {
		return nil, err
	}
	return json.Marshal(stats)
}

func (s legacyMetadataSnapshot) Close() error {
	return errors.Join(s.pinned.Close(), s.MetadataSnapshot.Close())
}

func TestPlacementRestoreDoesNotMigrateLegacySnapshotBeforePublication(t *testing.T) {
	// v0.14 snapshots used metadata JSONL v1 with full-table fidelity stats.
	// That released schema had extracted_text but no rendition catalog, so its
	// manifest cannot contain derivative_authority.
	fixture := newArchiveFixture(t)
	alpha, err := fixture.metadata.NodeByPath(t.Context(), "/alpha.txt")
	require.NoError(t, err)
	require.NoError(t, fixture.metadata.RecordExtraction(t.Context(), store.ExtractionResult{
		BlobHash: alpha.BlobHash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: store.ExtractionOK, Text: "synthetic legacy searchable text",
	}))
	wantMetadata := exportMetadata(t, fixture.metadata)

	repo, err := backup.Init(filepath.Join(t.TempDir(), "legacy-placement-repo"))
	require.NoError(t, err)
	manifest, err := backup.Create(t.Context(), repo, backupapp.New("legacy-version"),
		backup.CreateOptions{
			MetadataSource: legacyMetadataSource{metadata: fixture.metadata},
			ContentSource:  backupapp.NewContentSource(fixture.blobs),
		})
	require.NoError(t, err)
	require.NotNil(t, manifest.Metadata)
	assert.Equal(t, backupapp.MetadataFormat, manifest.Metadata.Format)
	assert.NotContains(t, string(manifest.Stats), `"derivative_authority"`)

	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.RestoreWithPlacement(
		t.Context(), repo, "legacy-version", store.DefaultSQLiteDriver(),
		backup.RestoreOptions{TargetDir: target},
		backupapp.RestorePlacementOptions{Map: &backupapp.RestoreStoreMap{
			Version: backupapp.RestoreStoreMapVersion,
		}},
	)
	require.NoError(t, err)
	restored, err := store.OpenForRestore(
		filepath.Join(target, "docbank.db"), store.DefaultSQLiteDriver(),
	)
	require.NoError(t, err)
	defer func() { require.NoError(t, restored.Close()) }()
	assert.Equal(t, string(wantMetadata), string(exportMetadata(t, restored)),
		"placement reconstruction must not add rendition authority absent from the snapshot")
}

func TestLegacyUnscopedSnapshotWithUnreachableBlobRestoresFaithfully(t *testing.T) {
	fixture := newArchiveFixture(t)
	orphanPayload := "synthetic pre-scope unreferenced blob"
	var orphanHash string
	require.NoError(t, fixture.blobs.WithMutation(t.Context(), func() error {
		receipt, writeErr := fixture.blobs.WriteDetailedContext(
			t.Context(), strings.NewReader(orphanPayload),
		)
		if writeErr != nil {
			return writeErr
		}
		encoding, err := receipt.EncodingName()
		if err != nil {
			return err
		}
		orphanHash = receipt.Hash
		if err := fixture.metadata.RecordRenditionBlob(t.Context(), receipt.Hash, receipt.Size,
			store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize,
				PackEligible: receipt.PackEligible, Created: receipt.Created}); err != nil {
			return err
		}
		return fixture.metadata.RecordExtraction(t.Context(), store.ExtractionResult{
			BlobHash: orphanHash, Extractor: "synthetic-legacy-extractor", ExtractorVersion: 1,
			Status: store.ExtractionOK, Text: "synthetic legacy extraction",
		})
	}))

	repo, err := backup.Init(filepath.Join(t.TempDir(), "legacy-repo"))
	require.NoError(t, err)
	manifest, err := backup.Create(t.Context(), repo, backupapp.New("legacy-version"),
		backup.CreateOptions{
			MetadataSource: legacyMetadataSource{metadata: fixture.metadata},
			ContentSource:  backupapp.NewContentSource(fixture.blobs),
		})
	require.NoError(t, err)
	assert.Equal(t, int64(3), manifest.Attachments.Blobs,
		"a legacy snapshot captured every blobs row including the unreachable one")
	stats, err := backupapp.ParseStats(manifest.Stats)
	require.NoError(t, err)
	assert.Equal(t, int64(3), stats.Blobs)
	assert.Equal(t, int64(1), stats.ExtractedText)

	target := filepath.Join(t.TempDir(), "restored")
	result, err := backupapp.Restore(t.Context(), repo, "legacy-version",
		backup.RestoreOptions{TargetDir: target, Jobs: 2},
	)
	require.NoError(t, err,
		"a pre-scope v1 snapshot containing an unreachable blob must restore faithfully")
	assert.Equal(t, int64(3), result.AttachmentBlobs)

	restored, err := store.Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	info, err := restored.BlobInfo(t.Context(), orphanHash)
	require.NoError(t, err, "the legacy snapshot's unreachable blob row must be restored")
	assert.Equal(t, int64(len(orphanPayload)), info.Size)
	restoredBlobs, err := blob.New(
		store.NewPackCatalog(restored), filepath.Join(target, "blobs"),
	)
	require.NoError(t, err)
	reader, err := restoredBlobs.OpenContext(t.Context(), orphanHash)
	require.NoError(t, err)
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	assert.Equal(t, orphanPayload, string(got),
		"the legacy snapshot's unreachable blob bytes must be materialized")
	require.NoError(t, restoredBlobs.Close())
}

func TestDerivativeAuthorityCoversEveryProviderArtifactRole(t *testing.T) {
	fixture := newArchiveFixture(t)
	evidence := "synthetic normalized evidence"
	markdown := "# Synthetic provider-role rendition\n"
	rolePayloads := []struct {
		role    document.EvidenceArtifactRole
		payload string
	}{
		{document.EvidenceArtifactImage, "synthetic provider image payload"},
		{document.EvidenceArtifactMarkdown, "synthetic provider markdown payload"},
		{document.EvidenceArtifactStructured, "synthetic structured evidence payload"},
		{document.EvidenceArtifactTranscript, "synthetic provider transcript payload"},
	}
	hashes := map[string]string{}
	require.NoError(t, fixture.blobs.WithMutation(t.Context(), func() error {
		write := func(key, payload string) error {
			receipt, writeErr := fixture.blobs.WriteDetailedContext(
				t.Context(), strings.NewReader(payload),
			)
			if writeErr != nil {
				return writeErr
			}
			encoding, err := receipt.EncodingName()
			if err != nil {
				return err
			}
			hashes[key] = receipt.Hash
			return fixture.metadata.RecordRenditionBlob(t.Context(), receipt.Hash, receipt.Size,
				store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize,
					PackEligible: receipt.PackEligible, Created: receipt.Created})
		}
		if err := write("normalized_evidence", evidence); err != nil {
			return err
		}
		if err := write("sanitized_markdown", markdown); err != nil {
			return err
		}
		for _, entry := range rolePayloads {
			if err := write(string(entry.role), entry.payload); err != nil {
				return err
			}
		}
		return nil
	}))

	source, err := fixture.metadata.NodeByPath(t.Context(), "/alpha.txt")
	require.NoError(t, err)
	policy := `{"roles":[` +
		`{"max_count":1,"min_count":1,"role":"normalized_evidence"},` +
		`{"max_count":1,"min_count":1,"role":"provider_image"},` +
		`{"max_count":1,"min_count":1,"role":"provider_markdown"},` +
		`{"max_count":1,"min_count":1,"role":"provider_transcript"},` +
		`{"max_count":1,"min_count":1,"role":"sanitized_markdown"},` +
		`{"max_count":1,"min_count":1,"role":"structured_evidence"}` +
		`],"version":1}`
	artifacts := []store.RenditionArtifactRecord{
		{ID: "artifact_evidence", Role: "normalized_evidence", BlobHash: hashes["normalized_evidence"],
			Size: int64(len(evidence)), Checksum: hashes["normalized_evidence"],
			State: store.RenditionArtifactVerified},
		{ID: "artifact_markdown", Role: "sanitized_markdown", BlobHash: hashes["sanitized_markdown"],
			Size: int64(len(markdown)), Checksum: hashes["sanitized_markdown"],
			State: store.RenditionArtifactVerified},
	}
	for _, entry := range rolePayloads {
		artifacts = append(artifacts, store.RenditionArtifactRecord{
			ID: "artifact_" + strings.ToLower(string(entry.role)), Role: string(entry.role),
			BlobHash: hashes[string(entry.role)], Size: int64(len(entry.payload)),
			Checksum: hashes[string(entry.role)], State: store.RenditionArtifactVerified,
		})
	}
	profile := backupProcessingProfile(t)
	build := store.RenditionBuildRecord{
		ID:                                backupHash("provider-role-build"),
		VaultID:                           fixture.metadata.VaultID(),
		SourceSHA256:                      source.BlobHash,
		RenditionRequestFingerprint:       profile.RenditionRequestFingerprint,
		EvidenceLexicalFingerprint:        profile.EvidenceLexicalFingerprint,
		CapturedArtifactPolicyFingerprint: backupHash(policy),
		CapturedArtifactPolicy:            jsontext.Value(policy),
		AuthorizationChecksum:             backupHash("authorization"),
		ProviderOperationID:               "synthetic-provider-operation",
		ProviderReceipt:                   jsontext.Value(`{"provider":"synthetic"}`),
		EvidenceChecksum:                  hashes["normalized_evidence"],
		RenditionChecksum:                 backupHash("rendition"),
		MarkdownChecksum:                  hashes["sanitized_markdown"],
		Completeness:                      document.EvidenceComplete,
		Warnings:                          []string{},
		CompletedAt:                       "2026-08-23T00:00:00.000000000Z",
		DeclaredArtifactCount:             len(artifacts),
		Artifacts:                         artifacts,
		Units: []store.RenditionUnitRecord{{
			ID: "unit", EvidenceUnitID: "evidence-unit", Order: 0, Checksum: backupHash("unit"),
			HeadingPath: []string{"Synthetic provider-role rendition"},
			Locator: document.EvidenceLocatorV1{Kind: document.EvidenceLocatorPage,
				IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1},
		}},
		LexicalSegments: []store.RenditionLexicalSegmentRecord{{
			ID: "segment", UnitID: "unit", Order: 0, CharStart: 0,
			CharEnd: len("Synthetic provider-role rendition"), Checksum: backupHash("segment"),
			Text: "Synthetic provider-role rendition",
		}},
	}
	require.NoError(t, fixture.metadata.StageRenditionBuild(t.Context(), build))

	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	manifest, err := backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs, backup.CreateOptions{},
	)
	require.NoError(t, err,
		"every catalog-authorized provider artifact role must be capturable")
	stats, err := backupapp.ParseStats(manifest.Stats)
	require.NoError(t, err)
	require.NotNil(t, stats.DerivativeAuthority)
	var classes []string
	for _, class := range stats.DerivativeAuthority.Classes {
		classes = append(classes, class.Class)
		want := "included"
		if class.Class == "lexical_projection" {
			want = "reconstructible"
		}
		assert.Equal(t, want, class.Classification)
		assert.Equal(t, int64(1), class.Count)
	}
	assert.Equal(t, []string{
		"normalized_evidence", "sanitized_markdown",
		"provider_image", "provider_markdown", "structured_evidence",
		"provider_transcript", "lexical_projection",
	}, classes)
}
