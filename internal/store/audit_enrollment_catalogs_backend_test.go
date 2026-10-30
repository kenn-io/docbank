package store

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

// Enrollment must validate retained catalogs even when their documents are
// outside the new scope. Use native publications so nested readers see real rows.
func TestAuditEnrollmentCatalogsPostgres(t *testing.T) {
	t.Parallel()
	for name, seed := range map[string]func(*testing.T) *Store{
		"processing": func(t *testing.T) *Store {
			t.Helper()
			s := newTestStore(t)
			seedProcessingMetadataCatalog(t, s)
			return s
		},
		"embeddings": func(t *testing.T) *Store {
			t.Helper()
			s, version, profile, attachment := newEmbeddingCatalogFixture(t)
			for _, input := range []struct {
				kind                EmbeddingInputKind
				binding, attachment string
			}{
				{document.EmbeddingInputOriginalFile, "optional", ""},
				{document.EmbeddingInputRenditionChunk, "chunk", attachment},
			} {
				record := embeddingSetFixture(s, version, profile.Fingerprint, input.kind, input.binding, input.attachment)
				require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
				require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
					FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: version, BindingID: input.binding, InputKind: input.kind},
					SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
				}))
			}
			return s
		},
		"pages and numbering": func(t *testing.T) *Store {
			t.Helper()
			s := newTestStore(t)
			snapshot, inputs := batesFixture(t, s)
			namespace, err := s.EnsureBatesNamespace(t.Context(), "DOC", "", 6)
			require.NoError(t, err)
			_, err = s.ReserveBatesRange(t.Context(), batesRequest(t, namespace, snapshot, inputs))
			require.NoError(t, err)
			return s
		},
		"received package": func(t *testing.T) *Store {
			t.Helper()
			s := newTestStore(t)
			seedReceivedPackage(t, s, "collection")
			return s
		},
		"email and media": func(t *testing.T) *Store {
			t.Helper()
			s := newTestStore(t)
			email := newEmailFixture(t, s, "message.eml")
			_, err := s.PublishEmailGeneration(t.Context(), email.publication)
			require.NoError(t, err)
			_, err = s.RetainSuppliedMedia(t.Context(), suppliedMediaPublicationFixture(t, s))
			require.NoError(t, err)
			return s
		},
		"mailbox": func(t *testing.T) *Store {
			t.Helper()
			s := newTestStore(t)
			request := MailboxContainerRequest{ID: "synthetic-mailbox", Owner: "operator", SHA256: fakeHash("ac"), Size: 3, Format: "mbox"}
			_, err := s.BeginMailboxContainer(t.Context(), request)
			require.NoError(t, err)
			require.NoError(t, s.RecordRenditionBlob(t.Context(), request.SHA256, 3, BlobPhysical{Encoding: "raw", StoredBytes: 3, PackEligible: true, Created: true}))
			require.NoError(t, s.PutMailboxChunk(t.Context(), request.Owner, request.ID, MailboxChunk{Index: 0, SHA256: request.SHA256, Size: 3}))
			_, err = s.SealMailboxContainer(t.Context(), request.Owner, request.ID, request.SHA256, 3)
			require.NoError(t, err)
			return s
		},
		"photo checkout": func(t *testing.T) *Store {
			t.Helper()
			s := newTestStore(t)
			asset := albumAsset(t, s, "photo.jpg")
			_, _, _, err := s.CreatePhotoCheckout(t.Context(), PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, "synthetic-checkout", "", 100)
			require.NoError(t, err)
			return s
		},
		"people": func(t *testing.T) *Store {
			t.Helper()
			s := newTestStore(t)
			file, err := s.CreateFile(t.Context(), s.RootID(), "report.txt", fakeHash("a1"), 9, "text/plain")
			require.NoError(t, err)
			_, err = s.CreatePerson(t.Context(), "Example Person", "operator")
			require.NoError(t, err)
			_, err = s.SetCustodian(t.Context(), CustodianRequest{
				Scope:    CustodianScope{Kind: "document", NodeID: file.ID, ContentVersionID: file.CurrentVersionID},
				RawLabel: "Example Person", Rank: "primary", Basis: "operator_assigned", IfMatchRevision: 1,
			})
			require.NoError(t, err)
			return s
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			setup, owner := pgtest.NewDatabase(t)
			source := seed(t)
			target, err := source.Mkdir(t.Context(), source.RootID(), "Audited")
			require.NoError(t, err)
			destination := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
			destination.vaultID = source.vaultID
			_, err = setup.ExecContext(t.Context(), `UPDATE documents.vault_metadata SET vault_uid=$1`, source.vaultID)
			require.NoError(t, err)
			copyStoreCatalogFixture(t, source, setup, postgresEnrollmentCatalogs)
			plan, err := destination.PreviewInitialAudit(t.Context(), target.ID, "cli", nil)
			require.NoError(t, err)
			want, err := source.EnableInitialAudit(t.Context(), plan)
			require.NoError(t, err)
			got, err := destination.EnableInitialAudit(t.Context(), plan)
			require.NoError(t, err)
			require.Equal(t, want, got)
			wantRecords, err := loadInitialAuditRecords(t.Context(), source.db)
			require.NoError(t, err)
			records, err := loadInitialAuditRecords(t.Context(), destination.db)
			require.NoError(t, err)
			require.Len(t, records, len(wantRecords))
			for kind, group := range wantRecords {
				require.ElementsMatch(t, group, records[kind], kind)
			}
		})
	}
}

func TestAuditEnrollmentFreshPostgres(t *testing.T) {
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	plan, err := s.PreviewInitialAudit(t.Context(), s.RootID(), "cli", nil)
	require.NoError(t, err)
	status, err := s.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	require.Len(t, status.Scopes, 1)
	_, err = s.EnableInitialAudit(t.Context(), plan)
	require.ErrorIs(t, err, ErrAuditAlreadyEnabled)
}

func TestAuditEnrollmentChangedPreviewPostgres(t *testing.T) {
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	destination := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	source := newTestStore(t)
	source.vaultID = destination.vaultID
	_, err := source.writeDB.ExecContext(t.Context(), `UPDATE vault_metadata SET vault_uid=$1`, source.vaultID)
	require.NoError(t, err)
	first, err := source.Mkdir(t.Context(), source.RootID(), "First")
	require.NoError(t, err)
	second, err := source.Mkdir(t.Context(), source.RootID(), "Second")
	require.NoError(t, err)
	copyStoreCatalogFixture(t, source, setup, postgresEnrollmentCatalogs)
	for _, target := range []Node{first, second} {
		plan, err := destination.PreviewInitialAudit(t.Context(), target.ID, "cli", nil)
		require.NoError(t, err)
		// A real directory write changes the frozen topology and allocation head.
		_, err = source.Mkdir(t.Context(), first.ID, "Created after "+target.Name)
		require.NoError(t, err)
		copyStoreCatalogFixture(t, source, setup, postgresEnrollmentCatalogs)
		before, err := destination.AuditStatus(t.Context(), nil)
		require.NoError(t, err)
		_, err = destination.EnableInitialAudit(t.Context(), plan)
		require.ErrorIs(t, err, ErrAuditPreviewStale)
		status, err := destination.AuditStatus(t.Context(), nil)
		require.NoError(t, err)
		require.Equal(t, before, status)
		plan, err = destination.PreviewInitialAudit(t.Context(), target.ID, "cli", nil)
		require.NoError(t, err)
		want, err := source.EnableInitialAudit(t.Context(), plan)
		require.NoError(t, err)
		got, err := destination.EnableInitialAudit(t.Context(), plan)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
}

func TestAuditEnrollmentPhotoReceiptPostgres(t *testing.T) {
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	destination := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	source := newTestStore(t)
	source.vaultID = destination.vaultID
	_, err := source.writeDB.ExecContext(t.Context(), `UPDATE vault_metadata SET vault_uid=$1`, source.vaultID)
	require.NoError(t, err)
	asset := albumAsset(t, source, "photo.jpg")
	receipt, err := editPhotoAuthored(t.Context(), source, []PhotoAuthoredTarget{{FileID: asset.Files[0].ID, Revision: 1, Patch: PhotoAuthoredPatch{Rating: new(3)}}})
	require.NoError(t, err)
	target, err := source.Mkdir(t.Context(), source.RootID(), "Audited")
	require.NoError(t, err)
	copyStoreCatalogFixture(t, source, setup, postgresEnrollmentCatalogs)
	plan, err := destination.PreviewInitialAudit(t.Context(), target.ID, "cli", nil)
	require.NoError(t, err)
	// Keep canonical receipt fields consistent, but refer to a different node.
	receipt.Before[0].NodeID = target.ID
	receipt.After[0].NodeID = target.ID
	beforeJSON, err := json.Marshal(receipt.Before, json.Deterministic(true))
	require.NoError(t, err)
	afterJSON, err := json.Marshal(receipt.afterState(), json.Deterministic(true))
	require.NoError(t, err)
	_, err = setup.ExecContext(t.Context(), `UPDATE documents.photo_change_receipts
 SET before_json=$1,after_json=$2 WHERE receipt_id=$3`, string(beforeJSON), string(afterJSON), receipt.ReceiptID)
	require.NoError(t, err)

	_, err = destination.EnableInitialAudit(t.Context(), plan)
	require.ErrorContains(t, err, "authored receipts reference missing or different file nodes")
	counts, err := auditAuthorityCounts(t.Context(), destination.db)
	require.NoError(t, err)
	require.Equal(t, [5]int64{}, counts)
}
