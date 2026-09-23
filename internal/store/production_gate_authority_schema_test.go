package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProductionGateAuthoritySchema(t *testing.T) {
	s := newTestStore(t)

	var version int
	require.NoError(t, s.db.QueryRow(`SELECT schema_version FROM vault_metadata WHERE singleton=1`).Scan(&version))
	require.Equal(t, 29, version)

	wantColumns := map[string][]string{
		"production_member_policy_facts": {
			"set_id", "revision", "member_id", "source_sha256", "generation_id",
			"evidence_sha256", "allowlist_version", "facts_sha256", "canonical_json",
		},
		"production_revision_email_publications": {
			"set_id", "revision", "root_version_id", "operation_id", "request_digest", "receipt_sha256",
		},
		"production_revision_gate_authority": {
			"set_id", "revision", "approval_id", "approval_grant_sha256", "approval_subject_sha256",
			"privilege_log_id", "privilege_log_revision", "privilege_log_receipt_sha256",
		},
	}
	for table, expected := range wantColumns {
		rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
		require.NoError(t, err)
		defer func() { require.NoError(t, rows.Close()) }()
		var got []string
		for rows.Next() {
			var column string
			require.NoError(t, rows.Scan(&column))
			got = append(got, column)
		}
		require.NoError(t, rows.Err())
		require.Equal(t, expected, got, table)
	}

	var grantUnique, receiptUnique int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM pragma_index_list('production_approval_grants')
		WHERE "unique"=1`).Scan(&grantUnique))
	require.GreaterOrEqual(t, grantUnique, 2)
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM pragma_index_list('production_privilege_log_receipts')
		WHERE "unique"=1`).Scan(&receiptUnique))
	require.GreaterOrEqual(t, receiptUnique, 2)
}

func TestProductionGateEvidencePinsAreImmutable(t *testing.T) {
	s := newTestStore(t)
	conn, err := s.db.Conn(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=OFF`)
	require.NoError(t, err)

	rows := []struct {
		table  string
		insert string
		errMsg string
	}{
		{"production_member_policy_facts", `INSERT INTO production_member_policy_facts
			(set_id,revision,member_id,source_sha256,generation_id,evidence_sha256,allowlist_version,facts_sha256,canonical_json)
			VALUES('set',1,'member','source','generation','evidence','allowlist','facts','{}')`, "are immutable"},
		{"production_revision_email_publications", `INSERT INTO production_revision_email_publications
			(set_id,revision,root_version_id,operation_id,request_digest,receipt_sha256)
			VALUES('set',1,'root','operation','request','receipt')`, "are immutable"},
		{"production_revision_gate_authority", `INSERT INTO production_revision_gate_authority
			(set_id,revision) VALUES('set',1)`, "is immutable"},
	}
	for _, row := range rows {
		t.Run(row.table, func(t *testing.T) {
			_, err := conn.ExecContext(t.Context(), row.insert)
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), `UPDATE `+row.table+` SET revision=revision`)
			require.ErrorContains(t, err, row.errMsg)
			_, err = conn.ExecContext(t.Context(), `DELETE FROM `+row.table)
			require.ErrorContains(t, err, row.errMsg)
		})
	}
}

func TestProductionRevisionGateAuthorityRequiresCompleteSelectorGroups(t *testing.T) {
	valid := productionRevisionGateAuthority{
		SetID: "50000000-0000-4000-8000-000000000001", Revision: 1,
		ApprovalID:          "50000000-0000-4000-8000-000000000002",
		ApprovalGrantSHA256: strings.Repeat("a", 64), ApprovalSubjectSHA256: strings.Repeat("b", 64),
		PrivilegeLogID: "50000000-0000-4000-8000-000000000003", PrivilegeLogRevision: 1,
		PrivilegeLogReceiptSHA256: strings.Repeat("c", 64),
	}
	require.NoError(t, validateProductionRevisionGateAuthority(valid))

	approvalHalf := valid
	approvalHalf.ApprovalGrantSHA256 = ""
	require.Error(t, validateProductionRevisionGateAuthority(approvalHalf))
	approvalSubjectMissing := valid
	approvalSubjectMissing.ApprovalSubjectSHA256 = ""
	require.Error(t, validateProductionRevisionGateAuthority(approvalSubjectMissing))
	privilegeHalf := valid
	privilegeHalf.PrivilegeLogReceiptSHA256 = ""
	require.Error(t, validateProductionRevisionGateAuthority(privilegeHalf))
	privilegeRevisionMissing := valid
	privilegeRevisionMissing.PrivilegeLogRevision = 0
	require.Error(t, validateProductionRevisionGateAuthority(privilegeRevisionMissing))

	require.NoError(t, validateProductionRevisionGateAuthority(productionRevisionGateAuthority{
		SetID: "50000000-0000-4000-8000-000000000001", Revision: 1,
	}))
}
