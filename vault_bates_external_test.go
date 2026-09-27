package docbank_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/sqlite"
)

func TestEmbeddedBatesPlanningAndReservationAPI(t *testing.T) {
	vaultRoot := t.TempDir()
	vault, err := docbank.New(t.Context(), docbank.Config{Root: vaultRoot})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "VOL001", "DATA"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "VOL001", "NATIVES"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "DATA", "load.dat"),
		[]byte("þDOCIDþ\x14þNATIVEþ\r\nþDOC-Aþ\x14þNATIVES/DOC-A.pdfþ\r\n"), 0o600))
	pdf := fpdf.NewCustom(&fpdf.InitType{UnitStr: "pt", Size: fpdf.SizeType{Wd: 612, Ht: 792}})
	pdf.AddPage()
	var native bytes.Buffer
	require.NoError(t, pdf.Output(&native))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "NATIVES", "DOC-A.pdf"), native.Bytes(), 0o600))

	mapping := []byte(`{"contract":"loadfile-mapping/v1","columns":[{"source":"DOCID","source_ordinal":0,"canonical":"loadfile.document.id"},{"source":"NATIVE","source_ordinal":1,"canonical":"loadfile.file.native"}]}`)
	preflight, err := vault.PreflightPackage(t.Context(), docbank.PackagePreflightRequest{
		SourceKind: "root", SourceRef: root, Profile: "dat-concordance-v1", Encoding: "utf-8", Mapping: mapping,
	})
	require.NoError(t, err)
	require.False(t, preflight.Blocking)
	operationID := uuid.NewString()
	_, err = vault.ImportPackage(t.Context(), docbank.PackageImportRequest{
		PreflightID: preflight.PreflightID, Into: "/", Name: "bates-source", OperationID: operationID,
	})
	require.NoError(t, err)
	var status docbank.PackageImportJob
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		status, err = vault.PackageImportStatus(t.Context(), operationID)
		require.NoError(t, err)
		if status.State == "complete" || status.State == "partial" || status.State == "failed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.Equal(t, "complete", status.State)
	packages, err := vault.Packages(t.Context(), docbank.PackageListRequest{Direction: "received", Limit: 10})
	require.NoError(t, err)
	require.Len(t, packages.Items, 1)
	members, err := vault.PackageMembers(t.Context(), docbank.PackageMemberRequest{
		PackageID: packages.Items[0].PackageID, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, members.Items, 1)
	representation := members.Items[0].Representations[0]
	for _, candidate := range members.Items[0].Representations {
		if candidate.Role == "native" {
			representation = candidate
			break
		}
	}
	require.Equal(t, "native", representation.Role)
	source := document.PageSource{VersionID: representation.ContentVersionID,
		SHA256: representation.BlobSHA256, Size: representation.Size}
	frame, err := document.NewPDFPageFrame(source, 1, [4]float64{0, 0, 612, 792}, [4]float64{0, 0, 612, 792}, 0)
	require.NoError(t, err)
	pageDocument, pageChecksum, err := document.MarshalPageDocumentV1(document.PageDocumentV1{
		Contract: document.PageFrameContractV1, Source: source, PageCount: 1, Frames: []document.PageFrameV1{frame},
	})
	require.NoError(t, err)
	frameJSON, frameChecksum, err := document.MarshalPageFrameV1(frame)
	require.NoError(t, err)
	db, err := store.DefaultSQLiteDriver().Open(filepath.Join(vaultRoot, "docbank.db"),
		sqlite.OpenOptions{Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Deferred})
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO page_documents(version_id,canonical_json,checksum) VALUES(?,?,?)`,
		source.VersionID, pageDocument, pageChecksum)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO page_frames(version_id,page,canonical_json,checksum) VALUES(?,?,?,?)`,
		source.VersionID, 1, frameJSON, frameChecksum)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `DROP TRIGGER collection_snapshot_members_immutable_update`)
	require.NoError(t, err)
	var frozenJSON []byte
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT canonical_json FROM collection_snapshot_members
		WHERE snapshot_id=? AND occurrence_id=?`, packages.Items[0].SnapshotID, members.Items[0].OccurrenceID).Scan(&frozenJSON))
	frozen, err := canonical.Decode[store.CollectionSnapshotMember](frozenJSON)
	require.NoError(t, err)
	frozen.SelectedPDFSHA256, frozen.SourcePageCount, frozen.SelectedSourcePages = source.SHA256, 1, []int{1}
	frozenJSON, err = canonical.Marshal(frozen)
	require.NoError(t, err)
	digest := sha256.Sum256(frozenJSON)
	_, err = db.ExecContext(t.Context(), `UPDATE collection_snapshot_members
		SET selected_source_pages_json='[1]',selected_pdf_sha256=?,source_page_count=1,canonical_json=?,checksum=?
		WHERE snapshot_id=? AND occurrence_id=?`, source.SHA256, frozenJSON, hex.EncodeToString(digest[:]),
		packages.Items[0].SnapshotID, members.Items[0].OccurrenceID)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	namespace, err := vault.EnsureBatesNamespace(t.Context(), docbank.BatesNamespaceRequest{Prefix: "API", Padding: 6})
	require.NoError(t, err)
	namespaces, err := vault.BatesNamespaces(t.Context(), docbank.BatesNamespaceListRequest{Limit: 10})
	require.NoError(t, err)
	require.Len(t, namespaces.Items, 1)
	assert.Equal(t, namespace, namespaces.Items[0])

	recipe := docbank.BatesRecipe{Contract: docbank.BatesRecipeContractV1, NamespaceID: namespace.NamespaceID,
		Prefix: "API", Padding: 6, StartAt: 41, Position: "bottom-right", MarginPoints: 24, FontName: "Helvetica",
		FontSizePoints: 9, Color: "#000000", Opacity: 1, Units: "point", RotationPolicy: "follow_page",
		EngineIdentity: docbank.BatesStampEngine{Name: "pdfcpu", Version: "v0.15.0", API: "AddWatermarksMap",
			Options: []string{"onTop=true", "update=restamp"}}}
	recipeSHA, err := recipe.SHA256()
	require.NoError(t, err)
	pages := []docbank.BatesPageInput{{OccurrenceID: members.Items[0].OccurrenceID,
		UnstampedSHA256: source.SHA256, SourcePage: 1, VerifiedPageCount: 1}}
	plan, err := vault.PlanBatesStamp(t.Context(), docbank.BatesPlanRequest{NamespaceID: namespace.NamespaceID,
		SnapshotID: packages.Items[0].SnapshotID, StartAt: 41, Pages: pages})
	require.NoError(t, err)
	assert.True(t, plan.StampedNothing)
	require.Len(t, plan.Labels, 1)
	assert.Equal(t, "API000041", plan.Labels[0].Label)

	mismatched := recipe
	mismatched.Padding = 7
	_, err = vault.ReserveBatesRange(t.Context(), docbank.BatesReserveRequest{OperationID: uuid.NewString(),
		SnapshotID: packages.Items[0].SnapshotID, Recipe: mismatched, Pages: pages})
	require.ErrorIs(t, err, docbank.ErrInvalidBatesRequest, "a recipe must match its namespace's label format")
	allocation, err := vault.ReserveBatesRange(t.Context(), docbank.BatesReserveRequest{OperationID: uuid.NewString(),
		SnapshotID: packages.Items[0].SnapshotID, Recipe: recipe, Pages: pages})
	require.NoError(t, err)
	assert.Equal(t, int64(41), allocation.StartSequence)
	assert.Equal(t, recipeSHA, allocation.RecipeSHA256, "the daemon derives the digest from the recipe")
	retained, err := vault.BatesAllocation(t.Context(), allocation.AllocationID)
	require.NoError(t, err)
	assert.Equal(t, allocation, retained)

	published, err := vault.PublishBatesExport(t.Context(), allocation.AllocationID, recipe)
	require.NoError(t, err)
	assert.Equal(t, allocation.AllocationID, published.AllocationID)
	require.Len(t, published.Pages, 1)
	assert.Equal(t, "API000041", published.Pages[0].Label)
	history, err := vault.BatesExports(t.Context(), "", 10)
	require.NoError(t, err)
	require.Equal(t, 1, history.Total)
	assert.Equal(t, published, history.Items[0])
	data, read, err := vault.ReadBatesExport(t.Context(), allocation.AllocationID)
	require.NoError(t, err)
	assert.Equal(t, published, read)
	assert.Equal(t, published.Size, int64(len(data)))
	_, err = vault.BatesExports(t.Context(), "not-a-cursor", 10)
	require.ErrorIs(t, err, docbank.ErrInvalidBatesCursor)
}
