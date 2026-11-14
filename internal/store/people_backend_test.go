package store

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkPeopleSnapshotsPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	t.Run("person detail", func(t *testing.T) {
		t.Parallel()
		s := newPostgresQueryStore(t)
		ctx := t.Context()
		person, err := s.CreatePerson(ctx, "Ada", "operator")
		require.NoError(t, err)
		person, identity := addTestPersonIdentity(t, s, person, "email", "ada@example.test", "synthetic-email")
		before, err := s.PersonDetailPage(ctx, person.PersonID, PersonAssertionPage{})
		require.NoError(t, err)
		var writeErr error
		reader := postgresSnapshotReader(t, s, func() {
			writeErr = s.RemovePersonIdentity(ctx, person.PersonID, identity.IdentityID, person.Revision)
		})
		during, err := reader.PersonDetailPage(ctx, person.PersonID, PersonAssertionPage{})
		require.NoError(t, writeErr)
		require.NoError(t, err)
		require.Equal(t, before, during)
		after, err := s.PersonDetailPage(ctx, person.PersonID, PersonAssertionPage{})
		require.NoError(t, err)
		require.Empty(t, after.Identities)
		require.Greater(t, after.Revision, before.Revision)
	})
	t.Run("candidate count and page", func(t *testing.T) {
		t.Parallel()
		s := newPostgresQueryStore(t)
		ctx := t.Context()
		_, version := seedPeopleVersion(t, s)
		candidate := openCandidateForTest(t, s, candidateForTest(t, "name_alias:ada", "Ada", "", []PersonCandidateOccurrence{{ContentVersionID: version, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "synthetic-author"}}))
		var writeErr error
		reader := postgresSnapshotReader(t, s, func() {
			_, writeErr = s.DecidePersonCandidate(ctx, nil, CandidateDecision{CandidateID: candidate.CandidateID, Action: "reject", ExpectedRevision: candidate.Revision})
		})
		items, total, err := reader.PersonCandidates(ctx, "open", 10, 0)
		require.NoError(t, writeErr)
		require.NoError(t, err)
		require.EqualValues(t, 1, total)
		require.Equal(t, []PersonMatchCandidate{candidate}, items)
		items, total, err = s.PersonCandidates(ctx, "open", 10, 0)
		require.NoError(t, err)
		require.Zero(t, total)
		require.Empty(t, items)
	})
	t.Run("custodian count and page", func(t *testing.T) {
		t.Parallel()
		s := newPostgresQueryStore(t)
		ctx := t.Context()
		node, version := seedPeopleVersion(t, s)
		scope := CustodianScope{Kind: "document", NodeID: node, ContentVersionID: version}
		assignment, err := s.SetCustodian(ctx, CustodianRequest{Scope: scope, RawLabel: "Records Team", Rank: "primary", Basis: "operator_assigned", IfMatchRevision: 1})
		require.NoError(t, err)
		var writeErr error
		reader := postgresSnapshotReader(t, s, func() { writeErr = s.RetireCustodian(ctx, assignment.AssignmentID, assignment.Revision) })
		items, total, err := reader.Custodians(ctx, scope, false, 10, 0)
		require.NoError(t, writeErr)
		require.NoError(t, err)
		require.EqualValues(t, 1, total)
		require.Equal(t, []CustodianAssignment{assignment}, items)
		items, total, err = s.Custodians(ctx, scope, false, 10, 0)
		require.NoError(t, err)
		require.Zero(t, total)
		require.Empty(t, items)
	})
	t.Run("external alias resolution", func(t *testing.T) {
		t.Parallel()
		s := newPostgresQueryStore(t)
		ctx := t.Context()
		for _, uid := range []string{"old", "new"} {
			person, err := s.CreatePerson(ctx, uid, "operator")
			require.NoError(t, err)
			_, err = s.LinkExternalIdentity(ctx, PersonExternalIdentity{PersonID: person.PersonID, System: "msgvault", ArchiveID: "synthetic", UID: uid, UIDKind: "vcard_uid", UIDState: "current"}, person.Revision)
			require.NoError(t, err)
		}
		before, err := s.ResolveExternalPersonUID(ctx, "msgvault", "synthetic", "old")
		require.NoError(t, err)
		var writeErr error
		reader := postgresSnapshotReader(t, s, func() {
			writeErr = s.RecordExternalUIDAliases(ctx, "msgvault", "synthetic", "new", []string{"old"})
		})
		during, err := reader.ResolveExternalPersonUID(ctx, "msgvault", "synthetic", "old")
		require.NoError(t, writeErr)
		require.NoError(t, err)
		require.Equal(t, before, during)
		after, err := s.ResolveExternalPersonUID(ctx, "msgvault", "synthetic", "old")
		require.NoError(t, err)
		require.True(t, after.Forwarded)
		require.Equal(t, "new", after.ResolvedUID)
		require.NotEqual(t, before.PersonID, after.PersonID)
	})
	t.Run("actor keys", func(t *testing.T) {
		t.Parallel()
		s := newPostgresQueryStore(t)
		ctx := t.Context()
		person, err := s.CreatePerson(ctx, "Ada", "operator")
		require.NoError(t, err)
		request := PersonActorKeysRequest{PersonID: person.PersonID}
		before, err := s.ActorKeysForPerson(ctx, request)
		require.NoError(t, err)
		var writeErr error
		reader := postgresSnapshotReader(t, s, func() {
			_, writeErr = s.AddPersonIdentity(ctx, person.PersonID, person.Revision, PersonIdentity{Kind: "email", ValueDisplay: "ada@example.test", Origin: "operator", EvidenceKind: "operator_assertion", EvidenceID: "synthetic-email", Confidence: "operator_asserted"})
		})
		during, err := reader.ActorKeysForPerson(ctx, request)
		require.NoError(t, writeErr)
		require.NoError(t, err)
		require.Equal(t, before, during)
		after, err := s.ActorKeysForPerson(ctx, request)
		require.NoError(t, err)
		require.Contains(t, after.Items, "email:ada@example.test")
		require.Greater(t, after.Total, before.Total)
	})
}

func checkPeopleRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	for _, operation := range []string{"name", "undo", "merge", "split", "candidate"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			setup, owner := pgtest.NewDatabase(t)
			s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
			ctx := t.Context()
			person, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			person, identity := addTestPersonIdentity(t, s, person, "email", "ada@example.test", "synthetic-email")
			other, err := s.CreatePerson(ctx, "Ada old", "operator")
			require.NoError(t, err)
			node, face := faceForDecisionTest(t, s, "")
			var undoID string
			if operation == "undo" {
				undoID = decideFaceForTest(t, s, node, face.FaceID, person.PersonID, "name").ReceiptID
			}
			detail, err := s.PersonDetailPage(ctx, person.PersonID, PersonAssertionPage{})
			require.NoError(t, err)
			faces, assertions, photoNames, err := s.PhotoFacePersonNames(ctx, node.CurrentVersionID)
			require.NoError(t, err)
			var epoch, receipts, people int64
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&epoch))
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_change_receipts`).Scan(&receipts))
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM persons`).Scan(&people))
			table := "photo_change_receipts"
			switch operation {
			case "merge":
				table = "person_merges"
			case "split":
				table = "person_splits"
			case "candidate":
				table = "person_candidate_faces"
			}
			var role string
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
			_, err = setup.ExecContext(ctx, "REVOKE INSERT ON documents."+table+" FROM "+pgx.Identifier{role}.Sanitize())
			require.NoError(t, err)
			switch operation {
			case "name":
				_, err = s.DecidePhotoFace(ctx, node.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: face.FaceID, PersonID: person.PersonID, Action: "name", Revision: face.Revision})
			case "undo":
				_, err = s.UndoPhotoFaceDecision(ctx, undoID)
			case "merge":
				_, err = s.MergePersons(ctx, other.PersonID, person.PersonID, uuid.NewString(), other.Revision, detail.Revision)
			case "split":
				_, err = s.SplitPerson(ctx, PersonSplitRequest{PersonID: person.PersonID, OperationID: uuid.NewString(), DisplayName: "Separate person", Revision: detail.Revision, IdentityIDs: []string{identity.IdentityID}})
			case "candidate":
				input := candidateForTest(t, "face:"+face.FaceID, "Ada", person.PersonID, []PersonCandidateOccurrence{{ContentVersionID: node.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: face.FaceID}})
				input.Reason = personCandidateReasonFaceMatch
				err = s.withLogicalTx(ctx, func(tx *sql.Tx) error { _, _, err := s.OpenPersonCandidate(ctx, tx, input); return err })
			}
			require.ErrorContains(t, err, "permission denied for table "+table)
			afterDetail, err := s.PersonDetailPage(ctx, person.PersonID, PersonAssertionPage{})
			require.NoError(t, err)
			require.Equal(t, detail, afterDetail)
			afterOther, through, err := s.PersonByID(ctx, other.PersonID)
			require.NoError(t, err)
			require.Empty(t, through)
			require.Equal(t, other, afterOther)
			afterFaces, afterAssertions, afterNames, err := s.PhotoFacePersonNames(ctx, node.CurrentVersionID)
			require.NoError(t, err)
			require.Equal(t, faces, afterFaces)
			require.Equal(t, assertions, afterAssertions)
			require.Equal(t, photoNames, afterNames)
			for query, want := range map[string]int64{
				`SELECT binding_epoch FROM document_people_state WHERE singleton=1`: epoch,
				`SELECT COUNT(*) FROM photo_change_receipts`:                        receipts,
				`SELECT COUNT(*) FROM persons`:                                      people,
				`SELECT COUNT(*) FROM person_match_candidates`:                      0,
				`SELECT COUNT(*) FROM person_candidate_faces`:                       0,
				`SELECT COUNT(*) FROM person_merges`:                                0,
				`SELECT COUNT(*) FROM person_splits`:                                0,
			} {
				var got int64
				require.NoError(t, s.db.QueryRowContext(ctx, query).Scan(&got))
				require.Equal(t, want, got, query)
			}
			checkRetentionMetadata(t, s)
		})
	}
}

func checkPeopleRetainedPackageCustodyPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	source := newTestStore(t)
	source.vaultID = s.vaultID
	ctx := t.Context()
	_, err := source.writeDB.ExecContext(ctx, `UPDATE vault_metadata SET vault_uid=$1`, source.vaultID)
	require.NoError(t, err)
	pkg, node := seedReceivedPackage(t, source, "synthetic-package")
	commitReceivedLabel(t, source, pkg, node.CurrentVersionID, "SYN000001")
	// Package production remains SQLite-only. Exercise native custody writes
	// against the validated retained catalog, not against fabricated package rows.
	copyStoreCatalogFixture(t, source, setup, postgresEnrollmentCatalogs)
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	key, err := PackageRecordKey("VOL001.dat", 1, "DOC-A")
	require.NoError(t, err)
	scope := CustodianScope{Kind: "package", PackageID: pkg.PackageID}
	def, err := s.SetCustodian(ctx, CustodianRequest{Scope: scope, RawLabel: "Default owner", Rank: "primary", Basis: "package_column", SourceRef: "Custodian", IfMatchRevision: 1})
	require.NoError(t, err)
	scope.PackageRecordID, scope.HasPackageRecordID = key, true
	record, err := s.SetCustodian(ctx, CustodianRequest{Scope: scope, RawLabel: "Sender claim", Rank: "primary", Basis: "package_column", SourceRef: "Custodian", IfMatchRevision: 1})
	require.NoError(t, err)
	alias, err := s.CreatePerson(ctx, "Former name", "operator")
	require.NoError(t, err)
	_, err = s.MergePersons(ctx, person.PersonID, alias.PersonID, uuid.NewString(), person.Revision, alias.Revision)
	require.NoError(t, err)
	resolved, err := s.ResolveCustodian(ctx, record.AssignmentID, alias.PersonID, record.Revision)
	require.NoError(t, err)
	require.Equal(t, record.AssignmentID, resolved.AssignmentID)
	require.NotNil(t, resolved.PersonID)
	require.Equal(t, person.PersonID, *resolved.PersonID)
	require.EqualValues(t, 2, resolved.Revision)
	require.Equal(t, "Sender claim", resolved.RawLabel)
	require.Equal(t, "primary", resolved.Rank)
	require.Equal(t, "package_column", resolved.Basis)
	require.Equal(t, "Custodian", resolved.SourceRef)
	_, err = s.ResolveCustodian(ctx, record.AssignmentID, person.PersonID, record.Revision)
	require.ErrorIs(t, err, ErrStaleRevision)
	record = resolved
	rows, total, err := s.Custodians(ctx, scope, false, 10, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Equal(t, []CustodianAssignment{record}, rows)
	_, err = s.SetOperatorPackageCustodian(ctx, CustodianRequest{Scope: scope, RawLabel: "Operator owner", Rank: "primary", Basis: "operator_assigned", IfMatchRevision: 1})
	require.ErrorIs(t, err, ErrCustodianConflict)
	scope.PackageRecordID, scope.HasPackageRecordID = "", false
	all, total, err := s.Custodians(ctx, scope, false, 10, 0)
	require.NoError(t, err)
	require.EqualValues(t, 2, total)
	require.ElementsMatch(t, []CustodianAssignment{def, record}, all)
	require.NoError(t, s.RetireCustodian(ctx, record.AssignmentID, record.Revision))
	all, total, err = s.Custodians(ctx, scope, false, 10, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Equal(t, []CustodianAssignment{def}, all)
	checkRetentionMetadata(t, s)
}
