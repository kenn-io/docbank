package store

import (
	"context"
	"database/sql"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDocumentDateIndexMeasurement runs in the existing Linux CI test lanes.
// Its gotestsum JSON artifact retains the query plans and timings, including on
// success. This measures selective lookups, not end-to-end snapshot latency.
func TestDocumentDateIndexMeasurement(t *testing.T) {
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
			 SELECT cv.version_id,cv.version_id,'document-events/v1','synthetic','synthetic','other',X'7B7D','synthetic',1,'2026-01-01T00:00:00Z' FROM content_versions cv`,
			`INSERT INTO document_event_heads (content_version_id,generation_id,input_epoch,published_at)
			 SELECT content_version_id,generation_id,1,'2026-01-01T00:00:00Z' FROM document_event_generations`,
			`INSERT INTO document_events (generation_id,event_id,source_key,date_kind,source_kind_raw,date_value,raw_value,precision,fraction_digits,timezone_kind,zone_text,axis_key,utc_key,claim_basis,parse_confidence,evidence_kind,evidence_id,evidence_sha256,evidence_locator,sensitive)
			 SELECT cv.version_id,'event','source','created','created',
			 date('2026-01-01',printf('+%d days',n.id%365))||CASE WHEN n.id%3=2 THEN '' ELSE 'T12:00:00' END, 'synthetic',
			 CASE WHEN n.id%3=2 THEN 'date' ELSE 'second' END,0,
			 CASE WHEN n.id%3=0 THEN 'utc' ELSE 'omitted' END,'',
			 date('2026-01-01',printf('+%d days',n.id%365))||'T12:00:00.000000000',
			 CASE WHEN n.id%3=0 THEN date('2026-01-01',printf('+%d days',n.id%365))||'T12:00:00.000000000Z' END,
			 'source_asserted','exact','source_metadata','synthetic','synthetic',X'7B7D',0
			 FROM content_versions cv JOIN nodes n ON n.id=cv.node_id`,
			`INSERT INTO document_date_keys (generation_id,event_id,content_version_id,date_kind,precision,floating,utc_key,axis_key,civil_date,civil_end_date,civil_month,civil_year,sensitive,primary_safe,primary_full)
			 SELECT e.generation_id,e.event_id,e.generation_id,e.date_kind,e.precision,
			 CASE WHEN e.utc_key IS NULL THEN 1 ELSE 0 END,e.utc_key,e.axis_key,substr(e.date_value,1,10),substr(e.date_value,1,10),
			 substr(e.date_value,1,7),substr(e.date_value,1,4),0,1,1 FROM document_events e`,
		}
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

	for _, tc := range []struct{ name, keyIndex, keyPredicate, eventPredicate string }{
		{"exact", "document_date_keys_utc", `k.utc_key>='2026-03-01T00:00:00.000000000Z' AND k.utc_key<'2026-03-02T00:00:00.000000000Z'`, `e.utc_key>='2026-03-01T00:00:00.000000000Z' AND e.utc_key<'2026-03-02T00:00:00.000000000Z'`},
		{"floating", "document_date_keys_civil", `k.civil_date='2026-03-01' AND k.utc_key IS NULL AND k.precision='second'`, `substr(e.date_value,1,10)='2026-03-01' AND e.utc_key IS NULL AND e.precision='second'`},
		{"civil", "document_date_keys_civil", `k.civil_date='2026-03-01' AND k.precision='date'`, `e.date_value='2026-03-01' AND e.precision='date'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keySQL := `SELECT n.id FROM document_date_keys k INDEXED BY ` + tc.keyIndex + ` JOIN document_event_heads h ON h.generation_id=k.generation_id JOIN content_versions cv ON cv.version_id=h.content_version_id JOIN nodes n ON n.current_version_id=cv.version_id WHERE ` + tc.keyPredicate + ` ORDER BY n.id`
			eventSQL := `SELECT n.id FROM document_events e JOIN document_event_heads h ON h.generation_id=e.generation_id JOIN content_versions cv ON cv.version_id=h.content_version_id JOIN nodes n ON n.current_version_id=cv.version_id WHERE ` + tc.eventPredicate + ` ORDER BY n.id`
			keys := measureDateLookup(t, ctx, s.db, "date_keys", keySQL)
			events := measureDateLookup(t, ctx, s.db, "document_events", eventSQL)
			require.NotEmpty(t, keys)
			require.Equal(t, events, keys)
		})
	}
}

func measureDateLookup(t *testing.T, ctx context.Context, db *sql.DB, label, statement string) []int64 {
	t.Helper()
	plan, err := db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+statement)
	require.NoError(t, err)
	for plan.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, plan.Scan(&id, &parent, &unused, &detail))
		t.Logf("%s plan: %s", label, detail)
	}
	require.NoError(t, plan.Err())
	require.NoError(t, plan.Close())
	start := time.Now()
	rows, err := db.QueryContext(ctx, statement)
	require.NoError(t, err)
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
