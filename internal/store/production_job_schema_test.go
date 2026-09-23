package store

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	docsqlite "go.kenn.io/docbank/sqlite"
)

func TestOpenRejectsCurrentDatabaseMissingProductionJobAuthority(t *testing.T) {
	for _, driver := range v090UpgradeDrivers() {
		for _, table := range []string{"production_finalized_revisions", "production_jobs", "production_job_artifacts"} {
			t.Run(driver.name+"/"+table, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "docbank.db")
				store, err := Open(path, driver.driver)
				require.NoError(t, err)
				require.NoError(t, store.Close())

				db, err := driver.driver.Open(path, docsqlite.OpenOptions{
					Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
				})
				require.NoError(t, err)
				_, err = db.Exec(`DROP TABLE ` + table)
				require.NoError(t, err)
				require.NoError(t, db.Close())

				reopened, err := Open(path, driver.driver)
				if reopened != nil {
					require.NoError(t, reopened.Close())
				}
				require.ErrorContains(t, err, "unexpected "+table+" layout")
			})
		}
	}
}

func TestOpenRejectsCurrentDatabaseMissingProductionSetAuthority(t *testing.T) {
	for _, driver := range v090UpgradeDrivers() {
		for _, table := range []string{
			"production_text_maps", "production_text_map_frames", "production_catalog_entries",
			"production_sets", "production_revisions", "production_members",
			"production_member_policy_facts", "production_revision_email_publications", "production_revision_gate_authority",
			"production_decisions", "production_operations", "production_audit_evidence",
		} {
			t.Run(driver.name+"/"+table, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "docbank.db")
				store, err := Open(path, driver.driver)
				require.NoError(t, err)
				require.NoError(t, store.Close())
				db, err := driver.driver.Open(path, docsqlite.OpenOptions{
					Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
				})
				require.NoError(t, err)
				_, err = db.Exec(`DROP TABLE ` + table)
				require.NoError(t, err)
				require.NoError(t, db.Close())
				reopened, err := Open(path, driver.driver)
				if reopened != nil {
					require.NoError(t, reopened.Close())
				}
				require.ErrorContains(t, err, "unexpected "+table+" layout")
			})
		}
	}
}

func TestProductionFinalizationRequiresStoredRevision(t *testing.T) {
	s := newTestStore(t)
	const id = "11111111-1111-4111-8111-111111111111"
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	_, err := s.db.Exec(`INSERT INTO production_finalized_revisions(set_id,revision,etag,draft_sha256,prepared_sha256,prepared_input_sha256,draft_json,prepared_input_json,numbering_namespace_id,numbering_snapshot_id,numbering_recipe_sha256,numbering_start_at,finalized_at) VALUES(?,1,1,?,?,?,'{}','{}',?,?,?,0,'2026-01-01T00:00:00Z')`, id, digest, digest, digest, id, id, digest)
	require.Error(t, err, "a gate receipt alone must not create finalized authority")
}

func TestImportMetadataRejectsExistingProductionJobAuthority(t *testing.T) {
	source := newTestStore(t)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))

	const id = "11111111-1111-4111-8111-111111111111"
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tests := []struct {
		name, insert, countQuery string
	}{
		{
			name:       "finalized revision",
			insert:     `INSERT INTO production_finalized_revisions(set_id,revision,etag,draft_sha256,prepared_sha256,prepared_input_sha256,draft_json,prepared_input_json,numbering_namespace_id,numbering_snapshot_id,numbering_recipe_sha256,numbering_start_at,finalized_at) VALUES('` + id + `',1,1,'` + digest + `','` + digest + `','` + digest + `','{}','{}','` + id + `','` + id + `','` + digest + `',0,'2026-01-01T00:00:00Z')`,
			countQuery: `SELECT COUNT(*) FROM production_finalized_revisions`,
		},
		{
			name:       "job",
			insert:     `INSERT INTO production_jobs(job_id,operation_id,owner,state,set_id,revision,etag,revision_sha256,prepared_input_sha256,request_json,created_at,updated_at) VALUES('` + id + `','22222222-2222-4222-8222-222222222222','production','queued','` + id + `',1,1,'` + digest + `','` + digest + `','{}','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,
			countQuery: `SELECT COUNT(*) FROM production_jobs`,
		},
		{
			name:       "staged artifact",
			insert:     `INSERT INTO production_job_artifacts(job_id,artifact_id,artifact_json,created_at) VALUES('` + id + `','33333333-3333-4333-8333-333333333333','{}','2026-01-01T00:00:00Z')`,
			countQuery: `SELECT COUNT(*) FROM production_job_artifacts`,
		},
		{
			name:       "production_text_maps",
			insert:     `INSERT INTO production_text_maps(map_sha256,source_node_id,version_id,source_sha256,source_size,pdf_sha256,pdf_size,page_document_sha256,evidence_sha256,page_inventory_sha256,canonical_json,created_at) VALUES('` + digest + `',1,'` + id + `','` + digest + `',1,'` + digest + `',1,'` + digest + `','` + digest + `','` + digest + `','{}','2026-01-01T00:00:00Z')`,
			countQuery: `SELECT COUNT(*) FROM production_text_maps`,
		},
		{
			name:       "production_text_map_frames",
			insert:     `INSERT INTO production_text_map_frames(map_sha256,version_id,page,frame_sha256,width,height) VALUES('` + digest + `','` + id + `',1,'` + digest + `',1,1)`,
			countQuery: `SELECT COUNT(*) FROM production_text_map_frames`,
		},
		{
			name:       "production_catalog_entries",
			insert:     `INSERT INTO production_catalog_entries(kind,id,sha256,canonical_json,created_at) VALUES('recipe','` + id + `','` + digest + `','{}','2026-01-01T00:00:00Z')`,
			countQuery: `SELECT COUNT(*) FROM production_catalog_entries`,
		},
		{
			name:       "production_sets",
			insert:     `INSERT INTO production_sets(id,name,creator,head_revision,created_at) VALUES('` + id + `','Synthetic set','test',1,'2026-01-01T00:00:00Z')`,
			countQuery: `SELECT COUNT(*) FROM production_sets`,
		},
		{
			name: "production_revisions",
			insert: `INSERT INTO production_revisions(
				set_id,revision,state,etag,instructions,instructions_sha256,member_hash,decisions_sha256,membership_sealed,
				recipe_kind,recipe_id,recipe_sha256,profile_kind,profile_id,profile_sha256,
				disclosure_profile_kind,disclosure_profile_id,disclosure_profile_sha256,
				numbering_recipe_kind,numbering_recipe_id,numbering_recipe_sha256,
				policy_id,policy_version,policy_sha256,created_at
			) VALUES ('` + id + `',1,'draft',1,'{}','` + digest + `','` + digest + `','` + digest + `',0,
				'recipe','` + id + `','` + digest + `','profile','` + id + `','` + digest + `',
				'disclosure','` + id + `','` + digest + `',
				'numbering','` + id + `','` + digest + `',
				'` + id + `',1,'` + digest + `','2026-01-01T00:00:00Z')`,
			countQuery: `SELECT COUNT(*) FROM production_revisions`,
		},
		{
			name: "production_members",
			insert: `INSERT INTO production_members(
				set_id,revision,member_id,ordinal,vault_id,node_id,version_id,source_sha256,source_size,
				pdf_sha256,pdf_size,map_sha256,page_inventory_sha256,family_context_json,canonical_json
			) VALUES ('` + id + `',1,'` + id + `',1,'` + id + `',1,'` + id + `','` + digest + `',1,
				'` + digest + `',1,'` + digest + `','` + digest + `','{}','{}')`,
			countQuery: `SELECT COUNT(*) FROM production_members`,
		},
		{
			name:       "production_decisions",
			insert:     `INSERT INTO production_decisions(set_id,revision,decision_id,member_id,actor,created_at,canonical_json) VALUES('` + id + `',1,'` + id + `','` + id + `','test','2026-01-01T00:00:00Z','{}')`,
			countQuery: `SELECT COUNT(*) FROM production_decisions`,
		},
		{
			name:       "production_operations",
			insert:     `INSERT INTO production_operations(operation_id,set_id,actor,kind,request_sha256,receipt_json,created_at) VALUES('` + id + `','` + id + `','test','synthetic','` + digest + `','{}','2026-01-01T00:00:00Z')`,
			countQuery: `SELECT COUNT(*) FROM production_operations`,
		},
		{
			name:       "production_audit_evidence",
			insert:     `INSERT INTO production_audit_evidence(operation_id,set_id,revision,actor,kind,request_sha256,receipt_sha256,created_at) VALUES('` + id + `','` + id + `',1,'test','synthetic','` + digest + `','` + digest + `','2026-01-01T00:00:00Z')`,
			countQuery: `SELECT COUNT(*) FROM production_audit_evidence`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := newTestStore(t)
			conn, err := target.db.Conn(t.Context())
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=OFF`)
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), test.insert)
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=ON`)
			require.NoError(t, err)
			require.NoError(t, conn.Close())

			err = target.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes()))
			require.ErrorContains(t, err, "not pristine")
			var rows int
			require.NoError(t, target.db.QueryRowContext(t.Context(), test.countQuery).Scan(&rows))
			require.Equal(t, 1, rows, "rejected import must preserve existing authority")
		})
	}
}

func TestProductionJobReceiptDigestIsPublishedOnlyOnce(t *testing.T) {
	s := newTestStore(t)
	const id = "11111111-1111-4111-8111-111111111111"
	const unpublishedID = "33333333-3333-4333-8333-333333333333"
	const first = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const second = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	conn, err := s.db.Conn(t.Context())
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=OFF`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `INSERT INTO production_finalized_revisions(set_id,revision,etag,draft_sha256,prepared_sha256,prepared_input_sha256,draft_json,prepared_input_json,numbering_namespace_id,numbering_snapshot_id,numbering_recipe_sha256,numbering_start_at,finalized_at) VALUES(?,1,1,?,?,?,'{}','{}',?,?,?,0,'2026-01-01T00:00:00Z')`, id, first, first, second, id, id, first)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `INSERT INTO production_jobs(job_id,operation_id,owner,state,set_id,revision,etag,revision_sha256,prepared_input_sha256,request_json,created_at,updated_at) VALUES(?,?,'production','queued',?,1,1,?,?,'{}','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`, id, "22222222-2222-4222-8222-222222222222", id, first, second)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `INSERT INTO production_jobs(job_id,operation_id,owner,state,set_id,revision,etag,revision_sha256,prepared_input_sha256,request_json,created_at,updated_at) VALUES(?,?,'production','queued',?,1,1,?,?,'{}','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`, unpublishedID, "44444444-4444-4444-8444-444444444444", id, first, second)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=ON`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	result, err := s.db.Exec(`DELETE FROM production_jobs WHERE job_id=?`, unpublishedID)
	require.NoError(t, err, "an unpublished job is not sealed")
	deleted, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	var receipt string
	require.NoError(t, s.db.QueryRow(`SELECT receipt_sha256 FROM production_jobs WHERE job_id=?`, id).Scan(&receipt))
	require.Empty(t, receipt)
	_, err = s.db.Exec(`UPDATE production_jobs SET receipt_sha256=? WHERE job_id=?`, first, id)
	require.ErrorContains(t, err, "receipt digest is immutable")

	_, err = s.db.Exec(`UPDATE production_jobs SET state='succeeded',receipt_sha256=?,receipt_json='{"receipt":1}',artifact_manifest_json='{"manifest":1}',endorsements_json='[]' WHERE job_id=?`, first, id)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE production_jobs SET receipt_sha256=? WHERE job_id=?`, second, id)
	require.ErrorContains(t, err, "receipt digest is immutable")
	for _, update := range []string{
		`UPDATE production_jobs SET receipt_json='{"receipt":2}' WHERE job_id=?`,
		`UPDATE production_jobs SET artifact_manifest_json='{"manifest":2}' WHERE job_id=?`,
		`UPDATE production_jobs SET endorsements_json='[1]' WHERE job_id=?`,
		`UPDATE production_jobs SET state='failed' WHERE job_id=?`,
	} {
		_, err = s.db.Exec(update, id)
		require.ErrorContains(t, err, "published production payload is immutable")
	}
	_, err = s.db.Exec(`DELETE FROM production_jobs WHERE job_id=?`, id)
	require.ErrorContains(t, err, "published production job is immutable")
	require.NoError(t, s.db.QueryRow(`SELECT receipt_sha256 FROM production_jobs WHERE job_id=?`, id).Scan(&receipt))
	require.Equal(t, first, receipt)
}
