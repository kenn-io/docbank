package store

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkDocumentPeopleAuthorityInputs(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	version := seedDocumentPeopleEvent(t, s, "authority.txt", "a1", nil)
	person, err := s.CreatePerson(ctx, "Records Team", "operator")
	require.NoError(t, err)
	assignment, err := s.SetCustodian(ctx, CustodianRequest{
		Scope:    CustodianScope{Kind: "document", NodeID: version.NodeID, ContentVersionID: version.ID},
		PersonID: person.PersonID, RawLabel: "Records Team", Rank: "primary", Basis: "operator_assigned", IfMatchRevision: 1,
	})
	require.NoError(t, err)
	assertion, err := s.AssertDocumentPerson(ctx, PersonDocumentAssertion{
		ContentVersionID: version.ID, PersonID: person.PersonID, Role: "author", Action: "suppress", Revision: 1,
	})
	require.NoError(t, err)
	input, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
	require.NoError(t, err)
	require.Equal(t, []CustodianAssignment{assignment}, input.Custodians)
	require.Equal(t, []PersonDocumentAssertion{assertion}, input.Assertions)
	require.Equal(t, person, input.Persons[person.PersonID])
}

func checkDocumentPeopleSnapshotsPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	t.Run("version attribution", func(t *testing.T) {
		t.Parallel()
		s := newPostgresQueryStore(t)
		ctx := t.Context()
		version := seedDocumentPeopleEvent(t, s, "snapshot.txt", "a1", nil)
		input, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
		require.NoError(t, err)
		publication := documentPeoplePublicationForInput(t, input)
		before, err := s.PublishDocumentPeople(ctx, publication)
		require.NoError(t, err)
		var writeErr error
		reader := postgresSnapshotReader(t, s, func() {
			_, writeErr = s.RebuildDocumentPeople(ctx, "00000000-0000-4000-8000-000000000001")
		})
		edges, during, err := reader.DocumentPeopleForVersion(ctx, version.ID)
		require.NoError(t, writeErr)
		require.NoError(t, err)
		require.Equal(t, before, during)
		require.Equal(t, publication.People.Edges, edges)
		_, _, err = s.DocumentPeopleForVersion(ctx, version.ID)
		require.ErrorIs(t, err, ErrNotFound, "the next read must withhold stale attribution")
	})
	t.Run("coverage", func(t *testing.T) {
		t.Parallel()
		s := newPostgresQueryStore(t)
		ctx := t.Context()
		version := seedDocumentPeopleEvent(t, s, "coverage.txt", "a1", nil)
		input, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
		require.NoError(t, err)
		publication := documentPeoplePublicationForInput(t, input)
		_, err = s.PublishDocumentPeople(ctx, publication)
		require.NoError(t, err)
		before, err := s.DocumentPeopleCoverage(ctx)
		require.NoError(t, err)
		publication.Resolution.OverLimit = true
		publication.People.Edges = []document.DocumentPersonEdgeV1{}
		var writeErr error
		reader := postgresSnapshotReader(t, s, func() {
			_, writeErr = s.PublishDocumentPeople(ctx, publication)
		})
		during, err := reader.DocumentPeopleCoverage(ctx)
		require.NoError(t, writeErr)
		require.NoError(t, err)
		require.Equal(t, before, during)
		after, err := s.DocumentPeopleCoverage(ctx)
		require.NoError(t, err)
		require.Zero(t, after.Published)
		require.EqualValues(t, 1, after.Unavailable)
		require.Greater(t, after.PublicationEpoch, before.PublicationEpoch)
	})
}

func checkDocumentPeopleRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	for _, operation := range []string{"provision", "publish", "rebuild"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			setup, owner := pgtest.NewDatabase(t)
			s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
			ctx := t.Context()
			version := seedDocumentPeopleEvent(t, s, "rollback.txt", "a1", nil)
			var publication DocumentPeoplePublication
			var original DocumentPeopleHead
			if operation == "publish" {
				input, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
				require.NoError(t, err)
				publication = documentPeoplePublicationForInput(t, input)
				original, err = s.PublishDocumentPeople(ctx, publication)
				require.NoError(t, err)
			}
			before, err := s.DocumentPeopleCoverage(ctx)
			require.NoError(t, err)
			_, cursor, err := s.VaultChanges(ctx, "", 500)
			require.NoError(t, err)
			var role string
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
			table := map[string]string{"provision": "person_identities", "publish": "document_people_heads", "rebuild": "document_people_builds"}[operation]
			_, err = setup.ExecContext(ctx, "REVOKE INSERT ON documents."+table+" FROM "+pgx.Identifier{role}.Sanitize())
			require.NoError(t, err)
			switch operation {
			case "provision":
				input, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
				require.ErrorContains(t, err, "permission denied for table "+table)
				require.Empty(t, input.ContentVersionID)
				for _, table := range []string{"persons", "person_identities"} {
					var count int
					require.NoError(t, s.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count))
					require.Zero(t, count)
				}
			case "publish":
				replacement := publication
				replacement.People.Edges = []document.DocumentPersonEdgeV1{}
				_, err := s.PublishDocumentPeople(ctx, replacement)
				require.ErrorContains(t, err, "permission denied for table "+table)
				edges, head, err := s.DocumentPeopleForVersion(ctx, version.ID)
				require.NoError(t, err)
				require.Equal(t, original, head)
				require.Equal(t, publication.People.Edges, edges)
				var generations, retainedEdges int
				require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM document_people_generations`).Scan(&generations))
				require.Equal(t, 1, generations)
				require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM document_people`).Scan(&retainedEdges))
				require.Equal(t, 1, retainedEdges)
			case "rebuild":
				const operationID = "00000000-0000-4000-8000-000000000001"
				_, err := s.RebuildDocumentPeople(ctx, operationID)
				require.ErrorContains(t, err, "permission denied for table "+table)
				_, err = s.DocumentPeopleBuild(ctx, operationID)
				require.ErrorIs(t, err, ErrNotFound)
			}
			after, err := s.DocumentPeopleCoverage(ctx)
			require.NoError(t, err)
			require.Equal(t, before, after)
			changes, _, err := s.VaultChanges(ctx, cursor, 500)
			require.NoError(t, err)
			require.Empty(t, changes)
		})
	}
}
