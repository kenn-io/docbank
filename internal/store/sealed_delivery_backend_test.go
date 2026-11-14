package store

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkSealedDeliveryReservationRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	ctx := t.Context()
	snapshot, request := numberedSnapshotFixture(t, s)
	var role string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
	role = pgx.Identifier{role}.Sanitize()
	// Fail after the snapshot and allocation have been inserted.
	_, err := setup.ExecContext(ctx, "REVOKE INSERT ON documents.bates_page_labels FROM "+role)
	require.NoError(t, err)
	_, err = s.ReserveSnapshotBatesRange(ctx, snapshot, request)
	require.ErrorContains(t, err, "permission denied")
	var snapshots, allocations, next int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM collection_snapshots`).Scan(&snapshots))
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM bates_allocations`).Scan(&allocations))
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT next_sequence FROM bates_namespace_cursors`).Scan(&next))
	require.Zero(t, snapshots)
	require.Zero(t, allocations)
	require.Equal(t, 1, next)
	_, err = setup.ExecContext(ctx, "GRANT INSERT ON documents.bates_page_labels TO "+role)
	require.NoError(t, err)
	first, err := s.ReserveSnapshotBatesRange(ctx, snapshot, request)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, snapshot.Members[0].NodeID, UnconditionalRev)
	require.NoError(t, err)
	request.OperationID, request.SnapshotID = uuid.New().String(), uuid.New().String()
	snapshot.SnapshotID = request.SnapshotID
	replay, err := s.ReserveSnapshotBatesRange(ctx, snapshot, request)
	require.NoError(t, err)
	require.Equal(t, first, replay)
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM collection_snapshots`).Scan(&snapshots))
	require.Equal(t, 1, snapshots)
	checkRetentionMetadata(t, s)
}

func checkSealedDeliveryArtifactRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	ctx := t.Context()
	snapshot, inputs := batesFixture(t, s)
	namespace, err := s.EnsureBatesNamespace(ctx, "PUBLISH", "", 6)
	require.NoError(t, err)
	recipe, err := canonical.Marshal(map[string]any{"contract": "bates-stamp/v1"})
	require.NoError(t, err)
	request := batesRequest(t, namespace, snapshot, inputs)
	request.RecipeSHA256 = digestCatalogJSON(recipe)
	allocation, err := s.ReserveBatesRange(ctx, request)
	require.NoError(t, err)
	_, pages, err := s.BatesPublicationPlan(ctx, allocation.AllocationID)
	require.NoError(t, err)
	publication := BatesArtifactPublication{ArtifactID: uuid.New().String(), AllocationID: allocation.AllocationID,
		BlobSHA256: fakeHash("f2"), Size: 10, PageCount: len(pages), RecipeJSON: recipe, Pages: pages}
	physical := BlobPhysical{Encoding: "raw", StoredBytes: 10, Created: true}
	var role string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
	role = pgx.Identifier{role}.Sanitize()
	// Fail after recording the new output blob and artifact header.
	_, err = setup.ExecContext(ctx, "REVOKE INSERT ON documents.bates_artifact_pages FROM "+role)
	require.NoError(t, err)
	_, err = s.PublishBatesArtifact(ctx, publication, physical)
	require.ErrorContains(t, err, "permission denied")
	_, err = s.BatesArtifact(ctx, allocation.AllocationID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.BlobInfo(ctx, publication.BlobSHA256)
	require.ErrorIs(t, err, ErrNotFound)
	stillReserved, err := s.BatesAllocation(ctx, allocation.AllocationID)
	require.NoError(t, err)
	require.Equal(t, allocation, stillReserved)
	_, err = setup.ExecContext(ctx, "GRANT INSERT ON documents.bates_artifact_pages TO "+role)
	require.NoError(t, err)
	first, err := s.PublishBatesArtifact(ctx, publication, physical)
	require.NoError(t, err)
	publication.BlobSHA256 = fakeHash("f3")
	replay, err := s.PublishBatesArtifact(ctx, publication, physical)
	require.NoError(t, err)
	require.Equal(t, first, replay, "the first verified publisher retains authority")
	_, err = s.BlobInfo(ctx, publication.BlobSHA256)
	require.ErrorIs(t, err, ErrNotFound)
	history, err := s.BatesArtifacts(ctx, "", 1)
	require.NoError(t, err)
	require.Equal(t, []BatesArtifact{first}, history)
	end, err := s.BatesArtifacts(ctx, first.ArtifactID, 1)
	require.NoError(t, err)
	require.Empty(t, end)
	checkRetentionMetadata(t, s)
}

func checkSealedDeliveryConcurrentReservationsPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
	stores := []*Store{newPostgresSchemaStore(t, setup, cfg), newPostgresSchemaStore(t, setup, cfg)}
	snapshot, request := numberedSnapshotFixture(t, stores[0])
	const count = 8
	var group sync.WaitGroup
	var results [count]BatesAllocation
	var errs [count]error
	for i := range count {
		group.Go(func() {
			candidate, operation := snapshot, request
			candidate.SnapshotID, operation.OperationID = uuid.New().String(), uuid.New().String()
			operation.SnapshotID = candidate.SnapshotID
			results[i], errs[i] = stores[i%len(stores)].ReserveSnapshotBatesRange(t.Context(), candidate, operation)
		})
	}
	group.Wait()
	for i := range results {
		require.NoError(t, errs[i])
		require.Equal(t, results[0], results[i])
	}
	var snapshots int
	require.NoError(t, stores[0].db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM collection_snapshots`).Scan(&snapshots))
	require.Equal(t, 1, snapshots)
	retainedSnapshotID := results[0].SnapshotID
	// Distinct starts can lose to a later reservation, but can never overlap.
	for i := range count {
		group.Go(func() {
			operation := request
			operation.OperationID, operation.StartAt = uuid.New().String(), int64(i+2)
			operation.SnapshotID = retainedSnapshotID
			results[i], errs[i] = stores[i%len(stores)].ReserveBatesRange(t.Context(), operation)
		})
	}
	group.Wait()
	seen := map[int64]bool{1: true}
	for i, result := range &results {
		if errs[i] != nil {
			require.ErrorIs(t, errs[i], ErrBatesReservationConflict)
			continue
		}
		require.Equal(t, int64(i+2), result.StartSequence)
		require.Equal(t, result.StartSequence, result.EndSequence)
		require.False(t, seen[result.StartSequence])
		seen[result.StartSequence] = true
	}
	require.Greater(t, len(seen), 1)
	checkRetentionMetadata(t, stores[0])
}

func checkBatesEvidenceSnapshotsPostgres(t *testing.T) {
	t.Helper()
	for _, operation := range []string{"candidates", "evidence"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			s := newPostgresQueryStore(t)
			ctx := t.Context()
			artifact, member := seedBatesSearchArtifact(t, s)
			var assignments []CustodianAssignment
			for i := range 2 {
				assignment, err := s.SetCustodian(ctx, CustodianRequest{Scope: CustodianScope{Kind: "document",
					NodeID: member.NodeID, ContentVersionID: member.ContentVersionID}, RawLabel: "Snapshot keeper",
					Rank: "additional", Basis: "operator_assigned", SourceRef: fmt.Sprintf("source-%d", i), IfMatchRevision: 1})
				require.NoError(t, err)
				assignments = append(assignments, assignment)
			}
			read := func(reader *Store) (BatesArtifactCandidatePage, error) {
				selector := BatesArtifactSelector{CustodianLabel: "snapshot keeper"}
				if operation == "evidence" {
					return reader.FindBatesArtifactEvidence(ctx, selector, artifact.ArtifactID, 1)
				}
				return reader.FindBatesArtifacts(ctx, selector, BatesArtifactPosition{}, 10)
			}
			before, err := read(s)
			require.NoError(t, err)
			require.Len(t, before.Items, 1)
			var changed bool
			var mutationErr error
			reader := sealedDeliveryReaderAfterSelect(t, s, func() {
				for _, assignment := range assignments {
					if mutationErr = s.RetireCustodian(ctx, assignment.AssignmentID, assignment.Revision); mutationErr != nil {
						return
					}
				}
				changed = true
			})
			during, err := read(reader)
			require.NoError(t, mutationErr)
			require.NoError(t, err)
			require.True(t, changed)
			require.Equal(t, before, during)
			after, err := read(s)
			if operation == "evidence" {
				require.ErrorIs(t, err, ErrNotFound)
			} else {
				require.NoError(t, err)
				require.Empty(t, after.Items)
				require.Greater(t, after.BindingEpoch, before.BindingEpoch)
			}
		})
	}
}

func checkBatesCollectionEvidenceFollowsFrozenSources(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "file", "Synthetic source")
	require.NoError(t, err)
	node, err := s.IngestFileExact(ctx, run, s.RootID(), "source.pdf", fakeHash("d1"), 10, "application/pdf", "source.pdf", "")
	require.NoError(t, err)
	occurrence := strings.Repeat("a", 32)
	snapshot, err := s.SealCollectionSnapshot(ctx, SnapshotSealRequest{SnapshotID: uuid.New().String(),
		SourceCollectionIDs: []string{run.ID()}, Members: []CollectionSnapshotMember{{
			Ordinal: 1, OccurrenceID: occurrence, NodeID: node.ID, ContentVersionID: node.CurrentVersionID,
			BlobSHA256: node.BlobHash, Size: 10, FamilyID: occurrence, FamilyOrder: 1, DisplayName: "frozen\x00name",
			FrozenFieldsJSON: "{}", DocumentKind: "other", SourcePageCount: 1, SelectedSourcePages: []int{1},
			SelectedPDFSHA256: node.BlobHash, Representations: []CollectionSnapshotRepresentation{{
				OccurrenceID: occurrence, Role: "native", Status: "available", TextAuthority: "none",
				ContentVersionID: node.CurrentVersionID, BlobSHA256: node.BlobHash, Size: 10,
				MediaType: "application/pdf", VerifiedPageCount: 1}},
		}}})
	require.NoError(t, err)
	namespace, err := s.EnsureBatesNamespace(ctx, "FROZEN", "", 6)
	require.NoError(t, err)
	recipe, err := canonical.Marshal(map[string]any{"contract": "bates-stamp/v1"})
	require.NoError(t, err)
	request := batesRequest(t, namespace, snapshot, []BatesPageInput{{OccurrenceID: occurrence,
		UnstampedSHA256: node.BlobHash, SourcePage: 1, VerifiedPageCount: 1}})
	request.RecipeSHA256 = digestCatalogJSON(recipe)
	allocation, err := s.ReserveBatesRange(ctx, request)
	require.NoError(t, err)
	_, pages, err := s.BatesPublicationPlan(ctx, allocation.AllocationID)
	require.NoError(t, err)
	artifact, err := s.PublishBatesArtifact(ctx, BatesArtifactPublication{ArtifactID: uuid.New().String(),
		AllocationID: allocation.AllocationID, BlobSHA256: fakeHash("d2"), Size: 10,
		PageCount: 1, RecipeJSON: recipe, Pages: pages}, BlobPhysical{Encoding: "raw", StoredBytes: 10, Created: true})
	require.NoError(t, err)
	assignment, err := s.SetCustodian(ctx, CustodianRequest{Scope: CustodianScope{Kind: "collection", IngestID: run.ID()},
		RawLabel: "Collection keeper", Rank: "primary", Basis: "operator_assigned", IfMatchRevision: 1})
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, node.ID, node.Revision)
	require.NoError(t, err)
	page, err := s.FindBatesArtifacts(ctx, BatesArtifactSelector{CustodianLabel: "collection keeper"}, BatesArtifactPosition{}, 10)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, artifact.ArtifactID, page.Items[0].ArtifactID)
	require.Len(t, page.Items[0].Evidence, 1)
	require.Equal(t, assignment.AssignmentID, page.Items[0].Evidence[0].AssignmentID)
	checkRetentionMetadata(t, s)
}

func checkBatesNamespaceSnapshotPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	first, err := s.EnsureBatesNamespace(ctx, "FIRST", "", 6)
	require.NoError(t, err)
	var mutationErr error
	var changed bool
	reader := sealedDeliveryReaderAfterSelect(t, s, func() {
		_, mutationErr = s.EnsureBatesNamespace(ctx, "SECOND", "", 6)
		changed = true
	})
	during, total, next, err := reader.BatesNamespaces(ctx, "", 10)
	require.NoError(t, mutationErr)
	require.True(t, changed)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Equal(t, []BatesNamespace{first}, during)
	require.Empty(t, next)
	after, total, next, err := s.BatesNamespaces(ctx, "", 1)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Len(t, after, 1)
	require.NotEmpty(t, next)
	last, total, next, err := s.BatesNamespaces(ctx, next, 1)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Len(t, last, 1)
	require.NotEqual(t, after[0].NamespaceID, last[0].NamespaceID)
	require.Empty(t, next)
}

func sealedDeliveryReaderAfterSelect(t *testing.T, s *Store, run func()) *Store {
	t.Helper()
	conn, err := s.db.Conn(t.Context())
	require.NoError(t, err)
	var cfg *pgx.ConnConfig
	err = conn.Raw(func(raw any) error {
		pg, ok := raw.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("unexpected PostgreSQL connection %T", raw)
		}
		cfg = pg.Conn().Config().Copy()
		return nil
	})
	require.NoError(t, conn.Close())
	require.NoError(t, err)
	cfg.Tracer = &afterFirstSelect{run: run}
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return &Store{db: db, postgresSchema: s.postgresSchema}
}
