package store

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/audit"
)

func TestAuditRecordsSQLite(t *testing.T) {
	t.Parallel()
	checkAuditRecords(t, newTestStore(t))
}

func TestAuditRecordsPostgres(t *testing.T) {
	t.Parallel()
	checkAuditRecords(t, newPostgresQueryStore(t))
}

func auditRecordFixture(t *testing.T) map[string][]storedAuditRecord {
	t.Helper()
	ctx := t.Context()
	source := newTestStore(t)
	seedInitialAuditAuthority(t, source, source.RootID())
	require.NoError(t, source.ValidateMetadata(ctx))
	records, err := loadInitialAuditRecords(ctx, source.db)
	require.NoError(t, err)
	return records
}

func checkAuditRecords(t *testing.T, destination *Store) {
	t.Helper()
	ctx := t.Context()
	records := auditRecordFixture(t)

	t.Run("canonical records and indexes", func(t *testing.T) {
		tx, err := destination.writeDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		for _, group := range records {
			for _, record := range group {
				require.NoError(t, insertAuditRecord(ctx, tx, record.record))
			}
		}
		// The loader checks every stored index against its
		// canonical record, including absent fields and exact integer values.
		stored, err := loadInitialAuditRecords(ctx, tx)
		require.NoError(t, err)
		require.Equal(t, records, stored)
		require.Equal(t, int64(0), *stored["event"][0].index.eventOrdinal)
		require.Equal(t, int64(1), *stored["canonical_mutation"][0].index.operationSequence)
		require.Equal(t, int64(1), *stored["scope_chain_entry"][0].index.entryCount)
		// A complete audit operation must also publish authority and membership
		// projections. Roll back this record-only transaction instead of
		// committing an incomplete vault as a fixture.
		require.NoError(t, tx.Rollback())
	})
}

func TestAuditRecordValidationSQLite(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	destination := newTestStore(t)
	records := auditRecordFixture(t)
	event := records["event"][0].record
	nested, err := auditNestedField(event, auditEventField)
	require.NoError(t, err)
	invalidNode := mustReplaceAuditRecordField(t, nested, metadataNodeIDField, audit.Unsigned(0))
	for _, tc := range []struct {
		name   string
		record audit.Record
		want   string
	}{
		{"zero mutation sequence", mustReplaceAuditRecordField(t, records["canonical_mutation"][0].record, "operation_sequence", audit.Unsigned(0)), "must be positive"},
		{"zero allocation sequence", mustReplaceAuditRecordField(t, records["allocation_entry"][0].record, "operation_sequence", audit.Unsigned(0)), "must be positive"},
		{"zero scope count", mustReplaceAuditRecordField(t, records["scope_chain_entry"][0].record, "entry_count", audit.Unsigned(0)), "must be positive"},
		{"zero event node", mustReplaceAuditRecordField(t, event, auditEventField, audit.Nested(invalidNode)), "must be positive"},
		{"overflow sequence", mustReplaceAuditRecordField(t, records["canonical_mutation"][0].record, "operation_sequence", audit.Unsigned(math.MaxInt64+1)), "exceeds storage integer range"},
		{"nested kind", audit.Record{Kind: auditEventIdentityKind, Fields: []audit.Field{
			{Name: auditOperationIDField, Value: mustAuditUUID(t, testAuditOperationID)},
			{Name: auditEventOrdinalField, Value: audit.Unsigned(0)},
		}}, "record kind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Keep the wrapper and digest valid so only the storage-domain
			// rule can reject the record before SQL, in both entry paths.
			raw, err := audit.MarshalJSONRecord(tc.record)
			require.NoError(t, err)
			value := metadataAuditRecord{Type: metadataAuditRecordType,
				Digest: mustAuditRecordDigest(t, tc.record), Record: raw}
			tx, err := destination.writeDB.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			require.ErrorContains(t, insertAuditRecord(ctx, tx, tc.record), tc.want)
			require.ErrorContains(t, importAuditRecord(ctx, tx, value), tc.want)
			var count int
			require.NoError(t, tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_records`).Scan(&count))
			require.Zero(t, count)
		})
	}
}
