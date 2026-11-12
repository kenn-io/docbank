package store

import (
	"context"
	"database/sql"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/query"
)

// TestDocumentDateIndexMeasurement runs in the existing Linux CI test lanes.
// Its gotestsum JSON artifact retains the query plans and timings, including on
// success. The synthetic data has one event per file and no multi-day spans.
func TestDocumentDateIndexMeasurement(t *testing.T) {
	t.Parallel()
	if os.Getenv("CI") != "true" || runtime.GOOS != "linux" {
		t.Skip("100k synthetic index measurement runs only in Linux CI")
	}
	ctx := t.Context()
	s := newTestStore(t)
	node, err := s.CreateFile(ctx, s.RootID(), "scale-source.txt", fakeHash("scale-source"), 1, "text/plain")
	require.NoError(t, err)
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := bulkBenchmarkFileVersionsTx(ctx, tx, node, node.ID, 99999); err != nil {
			return err
		}
		statements := []string{
			`INSERT INTO document_event_generations (generation_id,content_version_id,contract_version,deriver_fingerprint,inputs_sha256,document_kind,canonical_json,checksum,event_count,created_at)
			 SELECT cv.version_id,cv.version_id,'document-events/v1','` + DocumentEventsDeriverFingerprint + `','synthetic','other',X'7B7D','synthetic',1,'2026-01-01T00:00:00Z' FROM content_versions cv`,
			`INSERT INTO document_event_heads (content_version_id,generation_id,input_epoch,published_at)
			 SELECT content_version_id,generation_id,1,'2026-01-01T00:00:00Z' FROM document_event_generations`,
			`INSERT INTO document_events (generation_id,event_id,source_key,date_kind,source_kind_raw,date_value,raw_value,precision,fraction_digits,timezone_kind,zone_text,axis_key,utc_key,claim_basis,parse_confidence,evidence_kind,evidence_id,evidence_sha256,evidence_locator,sensitive)
			 SELECT cv.version_id,'event','source','created','created',
			 CASE n.id%5 WHEN 3 THEN substr(date('2025-01-01',printf('+%d days',(n.id/5)%730)),1,7) WHEN 4 THEN substr(date('2025-01-01',printf('+%d days',(n.id/5)%730)),1,4) ELSE date('2025-01-01',printf('+%d days',(n.id/5)%730))||CASE WHEN n.id%5=2 THEN '' ELSE 'T12:00:00' END END, 'synthetic',
			 CASE n.id%5 WHEN 2 THEN 'date' WHEN 3 THEN 'month' WHEN 4 THEN 'year' ELSE 'second' END,0,
			 CASE WHEN n.id%5=0 THEN 'utc' ELSE 'omitted' END,'',
			 CASE n.id%5 WHEN 4 THEN substr(date('2025-01-01',printf('+%d days',(n.id/5)%730)),1,4)||'-01-01T00:00:00.000000000'
                 WHEN 3 THEN substr(date('2025-01-01',printf('+%d days',(n.id/5)%730)),1,7)||'-01T00:00:00.000000000'
                 WHEN 2 THEN date('2025-01-01',printf('+%d days',(n.id/5)%730))||'T00:00:00.000000000'
                 ELSE date('2025-01-01',printf('+%d days',(n.id/5)%730))||'T12:00:00.000000000' END,
			 CASE WHEN n.id%5=0 THEN date('2025-01-01',printf('+%d days',(n.id/5)%730))||'T12:00:00.000000000Z' END,
			 'source_asserted','exact','source_metadata','synthetic','synthetic',X'7B7D',0
			 FROM content_versions cv JOIN nodes n ON n.id=cv.node_id`,
			`INSERT INTO document_event_primaries (generation_id,scope_class,disclosure,event_id,rule_id,reason)
    SELECT generation_id,'vault','safe',event_id,'primary-rule/v1','created_date' FROM document_events`,
			`INSERT INTO document_event_primaries (generation_id,scope_class,disclosure,event_id,rule_id,reason)
    SELECT generation_id,'vault','full',event_id,'primary-rule/v1','created_date' FROM document_events`,
			`INSERT INTO document_date_keys (generation_id,event_id,content_version_id,date_kind,precision,floating,utc_key,axis_key,civil_date,civil_end_date,civil_month,civil_year,sensitive,primary_safe,primary_full)
			 SELECT e.generation_id,e.event_id,e.generation_id,e.date_kind,e.precision,
			 CASE WHEN e.utc_key IS NULL AND e.precision='second' THEN 1 ELSE 0 END,e.utc_key,e.axis_key,
             CASE WHEN e.precision IN ('year','month') THEN '' ELSE substr(e.date_value,1,10) END,
             CASE WHEN e.precision IN ('year','month') THEN '' ELSE substr(e.date_value,1,10) END,
             CASE WHEN e.precision='year' THEN '' ELSE substr(e.date_value,1,7) END,substr(e.date_value,1,4),0,1,1 FROM document_events e`,
		}
		statements = append(statements,
			`INSERT INTO document_event_state (singleton,contract_version,deriver_fingerprint,input_epoch,publication_epoch,updated_at) VALUES (1,'document-events/v1','`+DocumentEventsDeriverFingerprint+`',1,1,'2026-01-01T00:00:00Z') ON CONFLICT (singleton) DO NOTHING`,
			`INSERT INTO document_event_attempts (content_version_id,input_epoch,input_revision,inputs_sha256,state,diagnostic_json,attempted_at) SELECT content_version_id,1,0,'synthetic','indexed',X'7B7D','2026-01-01T00:00:00Z' FROM document_event_generations`)
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	}))
	var versions int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM content_versions`).Scan(&versions))
	require.Equal(t, 100000, versions)
	var currentFiles int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes n JOIN content_versions cv ON cv.version_id=n.current_version_id WHERE n.kind='file'`).Scan(&currentFiles))
	require.Equal(t, 100000, currentFiles)

	part, err := (queryCompiler{dateZone: "UTC"}).compileDatePredicate(dateScope{ranges: [][2]string{{"2026-03-01", "2026-03-01"}}})
	require.NoError(t, err)
	from := ` FROM nodes n JOIN content_versions cv ON cv.version_id=n.current_version_id WHERE n.kind='file' AND `
	sortKey := documentDateSortExpression(false)
	keySQL := `SELECT n.id` + from + part.sql + ` ORDER BY ` + sortKey + `, n.id`
	// The alternative uses test-only indexes over the retained event columns.
	// The synthetic corpus has one primary event per file and no multi-day ranges.
	baseline := `EXISTS (SELECT 1 FROM document_events e INDEXED BY measurement_events_lookup JOIN document_event_heads h ON h.generation_id=e.generation_id WHERE h.content_version_id=cv.version_id AND ` + currentDocumentDateHead + ` AND e.sensitive=0 AND ((e.utc_key IS NOT NULL AND e.utc_key>=? AND e.utc_key<?) OR (e.utc_key IS NULL AND e.precision NOT IN ('year','month') AND substr(e.date_value,1,10)<=? AND substr(e.date_value,1,10)>=?) OR (e.precision='month' AND substr(e.date_value,1,7)>=? AND substr(e.date_value,1,7)<=?) OR (e.precision='year' AND substr(e.date_value,1,4)>=? AND substr(e.date_value,1,4)<=?)))`
	baselineSort := strings.Replace(sortKey, "document_date_keys k", "document_events k INDEXED BY measurement_events_lookup", 1)
	require.NotEqual(t, sortKey, baselineSort)
	beforePrimaryRemoval := baselineSort
	baselineSort = strings.Replace(baselineSort, "k.primary_safe=1 AND ", "", 1)
	require.NotEqual(t, beforePrimaryRemoval, baselineSort)
	eventSQL := `SELECT n.id` + from + baseline + ` ORDER BY ` + baselineSort + `, n.id`
	// Force the same test-only covering index in both event lookups so the
	// reported alternative is actually indexed, independent of planner choice.
	_, err = s.db.ExecContext(ctx, `CREATE INDEX measurement_events_lookup ON document_events(generation_id,precision,utc_key,date_value,axis_key,sensitive,event_id)`)
	require.NoError(t, err)
	for run := range 3 {
		keys := measureDateLookup(ctx, t, s.db, "date_keys", keySQL, part.args...)
		events := measureDateLookup(ctx, t, s.db, "document_events_indexed", eventSQL, part.args...)
		require.NotEmpty(t, keys)
		require.Less(t, len(keys), versions)
		require.Equal(t, events, keys)
		started := time.Now()
		require.NoError(t, requireDocumentDateIndex(ctx, s.db))
		t.Logf("fresh gate run %d: %s", run+1, time.Since(started))
		value, err := query.Parse([]byte(`{"syntax":"advanced","text":"date:2026-03-01","timezone":"UTC","sort":{"field":"document_date","direction":"asc"}}`))
		require.NoError(t, err)
		started = time.Now()
		projection, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
		require.NoError(t, err)
		t.Logf("fresh full snapshot run %d: %d rows in %s", run+1, len(projection.Rows), time.Since(started))
		var snapshotIDs []int64
		for _, row := range projection.Rows {
			snapshotIDs = append(snapshotIDs, row.NodeID)
		}
		slices.Sort(keys)
		slices.Sort(snapshotIDs)
		require.Equal(t, keys, snapshotIDs)
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM document_event_attempts WHERE content_version_id=(SELECT current_version_id FROM nodes WHERE kind='file' ORDER BY id DESC LIMIT 1)`)
	require.NoError(t, err)
	value, err := query.Parse([]byte(`{"syntax":"advanced","text":"date:2026-03-01","timezone":"UTC"}`))
	require.NoError(t, err)
	for run := range 3 {
		started := time.Now()
		require.ErrorIs(t, requireDocumentDateIndex(ctx, s.db), ErrDocumentDatesPending)
		t.Logf("pending gate run %d: %s", run+1, time.Since(started))
		started = time.Now()
		_, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
		require.ErrorIs(t, err, ErrDocumentDatesPending)
		t.Logf("pending full snapshot run %d: %s", run+1, time.Since(started))
	}
	t.Log("Limits: synthetic single-event current files, test-only event indexes, no multi-day spans; timings are observations, not latency requirements.")
}

func measureDateLookup(ctx context.Context, t *testing.T, db *sql.DB, label, statement string, args ...any) []int64 {
	t.Helper()
	plan, err := db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+statement, args...)
	require.NoError(t, err)
	defer func() { _ = plan.Close() }()
	var details []string
	for plan.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, plan.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
		t.Logf("%s plan: %s", label, detail)
	}
	require.NoError(t, plan.Err())
	require.NoError(t, plan.Close())
	planText := strings.Join(details, " ")
	if label == "date_keys" {
		require.Contains(t, planText, "sqlite_autoindex_document_date_keys_1")
	} else {
		require.Contains(t, planText, "measurement_events_lookup")
		require.NotContains(t, planText, "document_date_keys_")
	}
	start := time.Now()
	rows, err := db.QueryContext(ctx, statement, args...)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	t.Logf("%s: %d matches in %s", label, len(ids), time.Since(start))
	return ids
}
