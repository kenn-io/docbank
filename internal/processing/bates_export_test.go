package processing

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-pdf/fpdf"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/packagetest"
	"go.kenn.io/docbank/internal/pdfstamp"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/sqlite"
)

func TestBatesExportPublishesSelectedPagesBeforeCommittingAllocation(t *testing.T) {
	ctx := t.Context()
	vault := t.TempDir()
	dbPath := filepath.Join(vault, "docbank.db")
	catalog, err := store.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(vault, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })

	source := batesEightPagePDF(t)
	written, err := blobs.WriteDetailedContext(ctx, bytes.NewReader(source))
	require.NoError(t, err)
	physical := processingBlobPhysical(t, written)
	node, err := catalog.CreateFile(ctx, catalog.RootID(), "eight-pages.pdf", written.Hash, written.Size, "application/pdf", physical)
	require.NoError(t, err)
	batesPutPageDocument(t, dbPath, node.CurrentVersionID, written.Hash, written.Size, 8)
	childNode, err := catalog.CreateFile(ctx, catalog.RootID(), "child-eight-pages.pdf", written.Hash, written.Size, "application/pdf", physical)
	require.NoError(t, err)
	batesPutPageDocument(t, dbPath, childNode.CurrentVersionID, written.Hash, written.Size, 8)
	occurrence := strings.Repeat("a", 32)
	childOccurrence := strings.Repeat("b", 32)
	snapshot, err := catalog.SealCollectionSnapshot(ctx, store.SnapshotSealRequest{SnapshotID: uuid.NewString(), Members: []store.CollectionSnapshotMember{
		{Ordinal: 1, OccurrenceID: occurrence, NodeID: node.ID, ContentVersionID: node.CurrentVersionID,
			BlobSHA256: written.Hash, Size: written.Size, FamilyID: occurrence, FamilyOrder: 1,
			DisplayName: node.Name, FrozenFieldsJSON: "{}", DocumentKind: "other",
			SelectedSourcePages: []int{3, 6}, SelectedPDFSHA256: written.Hash, SourcePageCount: 8},
		{Ordinal: 2, OccurrenceID: childOccurrence, NodeID: childNode.ID, ContentVersionID: childNode.CurrentVersionID,
			BlobSHA256: written.Hash, Size: written.Size, FamilyID: occurrence, ParentOccurrenceID: occurrence, FamilyOrder: 2,
			DisplayName: childNode.Name, FrozenFieldsJSON: "{}", DocumentKind: "other",
			SelectedSourcePages: []int{8}, SelectedPDFSHA256: written.Hash, SourcePageCount: 8},
	}})
	require.NoError(t, err)
	namespace, err := catalog.EnsureBatesNamespace(ctx, "OUR", "", 6)
	require.NoError(t, err)
	recipe := batesExportRecipe(namespace, 41)
	recipeSHA, err := recipe.SHA256()
	require.NoError(t, err)
	allocation, err := catalog.ReserveBatesRange(ctx, store.BatesPlanRequest{
		OperationID: uuid.NewString(), NamespaceID: namespace.NamespaceID, SnapshotID: snapshot.SnapshotID,
		RecipeSHA256: recipeSHA, StartAt: 41, Pages: []store.BatesPageInput{
			{OccurrenceID: occurrence, UnstampedSHA256: written.Hash, SourcePage: 3, VerifiedPageCount: 8},
			{OccurrenceID: occurrence, UnstampedSHA256: written.Hash, SourcePage: 6, VerifiedPageCount: 8},
			{OccurrenceID: childOccurrence, UnstampedSHA256: written.Hash, SourcePage: 8, VerifiedPageCount: 8},
		},
	})
	require.NoError(t, err)

	artifact, err := PublishBatesExport(ctx, catalog, blobs, allocation.AllocationID, recipe)
	require.NoError(t, err)
	require.Equal(t, "verified", artifact.State)
	require.Equal(t, 3, artifact.PageCount)
	require.Equal(t, []store.BatesArtifactPage{
		{Ordinal: 1, OccurrenceID: occurrence, SourceBlobSHA256: written.Hash, SourcePage: 3, OutputPage: 1, Label: "OUR000041"},
		{Ordinal: 2, OccurrenceID: occurrence, SourceBlobSHA256: written.Hash, SourcePage: 6, OutputPage: 2, Label: "OUR000042"},
		{Ordinal: 3, OccurrenceID: childOccurrence, SourceBlobSHA256: written.Hash, SourcePage: 8, OutputPage: 3, Label: "OUR000043"},
	}, artifact.Pages)
	require.NotEqual(t, written.Hash, artifact.BlobSHA256)
	committed, err := catalog.BatesAllocation(ctx, allocation.AllocationID)
	require.NoError(t, err)
	require.Equal(t, "committed", committed.State)

	read, size, err := blobs.OpenStreamContext(ctx, artifact.BlobSHA256)
	require.NoError(t, err)
	output, err := io.ReadAll(read)
	require.NoError(t, err)
	require.Equal(t, size, int64(len(output)))
	require.NoError(t, read.Close())
	require.True(t, read.Verified())
	visible, err := packagetest.PDFText(ctx, output)
	require.NoError(t, err)
	require.Contains(t, visible, "SOURCE PAGE 03")
	require.Contains(t, visible, "OUR000041")
	require.Contains(t, visible, "SOURCE PAGE 06")
	require.Contains(t, visible, "OUR000042")
	require.Contains(t, visible, "SOURCE PAGE 08")
	require.Contains(t, visible, "OUR000043")

	retry, err := PublishBatesExport(ctx, catalog, blobs, allocation.AllocationID, recipe)
	require.NoError(t, err)
	require.Equal(t, artifact, retry)
	secondRecipe := batesExportRecipe(namespace, 44)
	secondRecipeSHA, err := secondRecipe.SHA256()
	require.NoError(t, err)
	secondAllocation, err := catalog.ReserveBatesRange(ctx, store.BatesPlanRequest{
		OperationID: uuid.NewString(), NamespaceID: namespace.NamespaceID, SnapshotID: snapshot.SnapshotID,
		RecipeSHA256: secondRecipeSHA, StartAt: 44, Pages: []store.BatesPageInput{
			{OccurrenceID: occurrence, UnstampedSHA256: written.Hash, SourcePage: 3, VerifiedPageCount: 8},
			{OccurrenceID: occurrence, UnstampedSHA256: written.Hash, SourcePage: 6, VerifiedPageCount: 8},
			{OccurrenceID: childOccurrence, UnstampedSHA256: written.Hash, SourcePage: 8, VerifiedPageCount: 8},
		},
	})
	require.NoError(t, err)
	secondArtifact, err := PublishBatesExport(ctx, catalog, blobs, secondAllocation.AllocationID, secondRecipe)
	require.NoError(t, err)

	db, err := store.DefaultSQLiteDriver().Open(dbPath, sqlite.OpenOptions{Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Deferred})
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DROP TRIGGER bates_artifacts_immutable_update`)
	require.NoError(t, err)
	artifact.CreatedAt = "2026-09-21T12:02:00.000000000Z"
	secondArtifact.CreatedAt = "2026-09-21T12:01:00.000000000Z"
	_, err = db.ExecContext(ctx, `UPDATE bates_artifacts SET created_at=? WHERE artifact_id=?`, artifact.CreatedAt, artifact.ArtifactID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE bates_artifacts SET created_at=? WHERE artifact_id=?`, secondArtifact.CreatedAt, secondArtifact.ArtifactID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `CREATE TRIGGER bates_artifacts_immutable_update
		BEFORE UPDATE ON bates_artifacts BEGIN
			SELECT RAISE(ABORT, 'Bates artifacts are immutable');
		END`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	history, err := catalog.BatesArtifacts(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, history, 2)
	require.Equal(t, []string{secondArtifact.ArtifactID, artifact.ArtifactID},
		[]string{history[0].ArtifactID, history[1].ArtifactID})
	firstPage, err := catalog.BatesArtifacts(ctx, "", 1)
	require.NoError(t, err)
	require.Len(t, firstPage, 1)
	require.Equal(t, secondArtifact.ArtifactID, firstPage[0].ArtifactID)
	secondPage, err := catalog.BatesArtifacts(ctx, firstPage[0].ArtifactID, 1)
	require.NoError(t, err)
	require.Len(t, secondPage, 1)
	require.Equal(t, artifact.ArtifactID, secondPage[0].ArtifactID)

	var metadata bytes.Buffer
	require.NoError(t, catalog.ExportMetadata(ctx, &metadata))
	restored, err := store.Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(metadata.Bytes())))
	restoredArtifact, err := restored.BatesArtifact(ctx, allocation.AllocationID)
	require.NoError(t, err)
	require.Equal(t, artifact, restoredArtifact)
}

func TestConcurrentBatesExportRetriesConvergeOnOneArtifact(t *testing.T) {
	env := newBatesExportFixture(t)
	var artifacts [2]store.BatesArtifact
	var errs [2]error
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := range 2 {
		wait.Go(func() {
			<-start
			artifacts[index], errs[index] = PublishBatesExport(t.Context(), env.catalog, env.blobs, env.allocationID, env.recipe)
		})
	}
	close(start)
	wait.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	require.Equal(t, artifacts[0], artifacts[1])
}

type batesExportFixtureValue struct {
	catalog      *store.Store
	blobs        *blob.Store
	allocationID string
	recipe       pdfstamp.Recipe
}

func newBatesExportFixture(t *testing.T) batesExportFixtureValue {
	t.Helper()
	ctx := t.Context()
	vault := t.TempDir()
	dbPath := filepath.Join(vault, "docbank.db")
	catalog, err := store.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(vault, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	source := batesEightPagePDF(t)
	written, err := blobs.WriteDetailedContext(ctx, bytes.NewReader(source))
	require.NoError(t, err)
	node, err := catalog.CreateFile(ctx, catalog.RootID(), "eight-pages.pdf", written.Hash, written.Size,
		"application/pdf", processingBlobPhysical(t, written))
	require.NoError(t, err)
	batesPutPageDocument(t, dbPath, node.CurrentVersionID, written.Hash, written.Size, 8)
	occurrence := strings.Repeat("e", 32)
	snapshot, err := catalog.SealCollectionSnapshot(ctx, store.SnapshotSealRequest{SnapshotID: uuid.NewString(), Members: []store.CollectionSnapshotMember{{
		Ordinal: 1, OccurrenceID: occurrence, NodeID: node.ID, ContentVersionID: node.CurrentVersionID,
		BlobSHA256: written.Hash, Size: written.Size, FamilyID: occurrence, FamilyOrder: 1,
		DisplayName: node.Name, FrozenFieldsJSON: "{}", DocumentKind: "other",
		SelectedSourcePages: []int{3, 6, 8}, SelectedPDFSHA256: written.Hash, SourcePageCount: 8,
	}}})
	require.NoError(t, err)
	namespace, err := catalog.EnsureBatesNamespace(ctx, "CON", "", 6)
	require.NoError(t, err)
	recipe := batesExportRecipe(namespace, 1)
	recipeSHA, err := recipe.SHA256()
	require.NoError(t, err)
	allocation, err := catalog.ReserveBatesRange(ctx, store.BatesPlanRequest{OperationID: uuid.NewString(),
		NamespaceID: namespace.NamespaceID, SnapshotID: snapshot.SnapshotID, RecipeSHA256: recipeSHA,
		Pages: []store.BatesPageInput{
			{OccurrenceID: occurrence, UnstampedSHA256: written.Hash, SourcePage: 3, VerifiedPageCount: 8},
			{OccurrenceID: occurrence, UnstampedSHA256: written.Hash, SourcePage: 6, VerifiedPageCount: 8},
			{OccurrenceID: occurrence, UnstampedSHA256: written.Hash, SourcePage: 8, VerifiedPageCount: 8},
		}})
	require.NoError(t, err)
	return batesExportFixtureValue{catalog: catalog, blobs: blobs, allocationID: allocation.AllocationID, recipe: recipe}
}

func batesEightPagePDF(t *testing.T) []byte {
	t.Helper()
	pdf := fpdf.NewCustom(&fpdf.InitType{UnitStr: "pt", Size: fpdf.SizeType{Wd: 612, Ht: 792}})
	pdf.SetFont("Helvetica", "", 16)
	for page := 1; page <= 8; page++ {
		pdf.AddPage()
		pdf.SetXY(72, 72)
		pdf.Cell(250, 25, fmt.Sprintf("SOURCE PAGE %02d", page))
	}
	var output bytes.Buffer
	require.NoError(t, pdf.Output(&output))
	return output.Bytes()
}

func batesPutPageDocument(t *testing.T, dbPath, versionID, hash string, size int64, pages int) {
	t.Helper()
	db, err := store.DefaultSQLiteDriver().Open(dbPath, sqlite.OpenOptions{Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Deferred})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	source := document.PageSource{VersionID: versionID, SHA256: hash, Size: size}
	frames := make([]document.PageFrameV1, pages)
	for index := range frames {
		frames[index], err = document.NewPDFPageFrame(source, index+1, [4]float64{0, 0, 612, 792}, [4]float64{0, 0, 612, 792}, 0)
		require.NoError(t, err)
	}
	encoded, checksum, err := document.MarshalPageDocumentV1(document.PageDocumentV1{Contract: document.PageFrameContractV1, Source: source, PageCount: pages, Frames: frames})
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO page_documents(version_id,canonical_json,checksum) VALUES(?,?,?)`, versionID, encoded, checksum)
	require.NoError(t, err)
	for _, frame := range frames {
		frameJSON, frameChecksum, frameErr := document.MarshalPageFrameV1(frame)
		require.NoError(t, frameErr)
		_, frameErr = db.ExecContext(t.Context(), `INSERT INTO page_frames(version_id,page,canonical_json,checksum) VALUES(?,?,?,?)`, versionID, frame.Page, frameJSON, frameChecksum)
		require.NoError(t, frameErr)
	}
}

func batesExportRecipe(namespace store.BatesNamespace, start int) pdfstamp.Recipe {
	return pdfstamp.Recipe{Contract: pdfstamp.RecipeContractV1, NamespaceID: namespace.NamespaceID,
		Prefix: namespace.Prefix, Suffix: namespace.Suffix, Padding: namespace.Padding, StartAt: start,
		Position: "bottom-right", MarginPoints: 24, FontName: "Helvetica", FontSizePoints: 9,
		Color: "#000000", Opacity: 1, Units: "point", RotationPolicy: "follow_page",
		EngineIdentity: pdfstamp.EngineIdentity{Name: "pdfcpu", Version: "v0.15.0", API: "AddWatermarksMap", Options: []string{"onTop=true", "update=restamp"}}}
}
