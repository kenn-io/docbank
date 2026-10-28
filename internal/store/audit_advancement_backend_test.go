package store

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/audit"
)

func TestAuditAdvancementPostgres(t *testing.T) {
	t.Parallel()
	source, destination, publish, _, child, _ := newAuditHistoryPostgresFixture(t)
	publish()
	ctx := t.Context()
	before, err := destination.AuditStatus(ctx, &child.ID)
	require.NoError(t, err)
	original, err := loadInitialAuditRecords(ctx, destination.db)
	require.NoError(t, err)
	known := make(map[string]bool)
	for _, group := range original {
		for _, record := range group {
			known[record.digest] = true
		}
	}
	moved, _, err := source.Move(ctx, child.ID, *child.ParentID, "advanced", child.Revision)
	require.NoError(t, err)
	require.NoError(t, source.ValidateMetadata(ctx))
	want, err := source.AuditStatus(ctx, &child.ID)
	require.NoError(t, err)
	records, err := loadInitialAuditRecords(ctx, source.db)
	require.NoError(t, err)
	var mutation, allocation storedAuditRecord
	var pending []audit.Record
	for _, group := range records {
		for _, record := range group {
			if known[record.digest] {
				continue
			}
			switch record.record.Kind {
			case "scope_chain_entry": // The native advancement helper builds this.
				continue
			case "allocation_entry":
				allocation = record
				continue
			case "canonical_mutation":
				mutation = record
			}
			pending = append(pending, record.record)
		}
	}
	require.NotNil(t, mutation.index.operationSequence)
	require.NotNil(t, mutation.index.operationID)
	mutationHash, err := audit.DigestHex(mutation.digest)
	require.NoError(t, err)
	parent, err := source.NodeByID(ctx, *child.ParentID)
	require.NoError(t, err)

	// Supply the native rename's resulting node projections. Complete PostgreSQL
	// move operations still need the remaining catalogs and their maintenance.
	discard := errors.New("discard audit advancement")
	for _, commit := range []bool{false, true} {
		err := destination.withStorageTx(ctx, func(tx *sql.Tx) error {
			authority, scopes, _, err := destination.loadAuditedNodeAuthority(ctx, tx, child.ID)
			if err != nil {
				return err
			}
			for _, node := range []Node{moved, parent} {
				if _, err := tx.ExecContext(ctx, `UPDATE nodes SET name=$1,revision=$2,modified_at=$3 WHERE id=$4`,
					node.Name, node.Revision, node.ModifiedAt, node.ID); err != nil {
					return err
				}
			}
			for _, record := range pending {
				if err := insertAuditRecord(ctx, tx, record); err != nil {
					return err
				}
			}
			values, err := makeAuditedMutationValues(source.vaultID, authority.lineageID, *mutation.index.operationID, moved.ModifiedAt)
			if err != nil {
				return err
			}
			if err := advanceAuditedMutationScopes(ctx, tx, values, scopes, mutationHash); err != nil {
				return err
			}
			if err := advanceAuditAuthority(ctx, tx, authority, *mutation.index.operationSequence, allocation.record); err != nil {
				return err
			}
			if !commit {
				return discard
			}
			return nil
		})
		expected, expectedRecords := before, original
		if commit {
			require.NoError(t, err)
			expected, expectedRecords = want, records
		} else {
			require.ErrorIs(t, err, discard)
		}
		got, err := destination.AuditStatus(ctx, &child.ID)
		require.NoError(t, err)
		require.Equal(t, expected, got)
		gotRecords, err := loadInitialAuditRecords(ctx, destination.db)
		require.NoError(t, err)
		require.Len(t, gotRecords, len(expectedRecords))
		for kind, group := range expectedRecords {
			require.ElementsMatch(t, group, gotRecords[kind], kind)
		}
	}
}

func TestAuditHeadValidationSQLite(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedInitialAuditAuthority(t, s, s.RootID())
	ctx := t.Context()
	authority, scope, err := loadInitialAuditProjection(ctx, s.db)
	require.NoError(t, err)
	for _, tc := range []struct {
		name, want string
		write      func(*sql.Tx) error
	}{
		{"zero sequence", "operation sequence must be positive", func(tx *sql.Tx) error {
			return updateAuditAuthorityHead(ctx, tx, 0, 1, authority.allocationHead)
		}},
		{"zero allocation count", "allocation entry count must be positive", func(tx *sql.Tx) error {
			return updateAuditAuthorityHead(ctx, tx, 1, 0, authority.allocationHead)
		}},
		{"inconsistent counters", "allocation count disagrees", func(tx *sql.Tx) error {
			return updateAuditAuthorityHead(ctx, tx, 2, 1, authority.allocationHead)
		}},
		{"zero scope count", "scope entry count must be positive", func(tx *sql.Tx) error {
			return updateAuditScopeHead(ctx, tx, scope.scopeID, 0, scope.chainHead)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := s.writeDB.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			// Match the Go rejection rather than a SQLite CHECK error.
			require.ErrorContains(t, tc.write(tx), tc.want)
			gotAuthority, gotScope, err := loadInitialAuditProjection(ctx, tx)
			require.NoError(t, err)
			require.Equal(t, authority, gotAuthority)
			require.Equal(t, scope, gotScope)
		})
	}
}
