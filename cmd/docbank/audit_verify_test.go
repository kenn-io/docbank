package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestWriteAuditVerificationExplainsEvidenceAndProblems(t *testing.T) {
	evidence := &api.AuditEvidence{
		VaultID:                    "11111111-1111-4111-8111-111111111111",
		LineageID:                  "22222222-2222-4222-8222-222222222222",
		OperationSequenceHighWater: 4, AllocationEntryCount: 4,
		AllocationHead: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Scopes: []api.AuditScopeEvidence{{
			ID: "33333333-3333-4333-8333-333333333333", EntryCount: 3,
			ChainHead: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		}},
	}
	var out bytes.Buffer
	require.NoError(t, writeAuditVerification(&out, api.AuditVerifyReport{
		Enabled: true, Evidence: evidence, ProtectedBlobs: 2,
		ProtectedBytes: 42, VerifiedBlobs: 1,
		EvidenceCheck: &api.AuditEvidenceCheck{Extends: true},
		Problems: []api.VerifyProblem{{
			Hash:    "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			Problem: "missing",
		}},
	}))
	assert.Contains(t, out.String(), "audit authority verified")
	assert.Contains(t, out.String(), "allocation lineage 22222222-2222-4222-8222-222222222222")
	assert.Contains(t, out.String(), "scope 33333333-3333-4333-8333-333333333333: entry 3")
	assert.Contains(t, out.String(), "1 of 2 protected blob(s) verified, 42 unique byte(s)")
	assert.Contains(t, out.String(), "recorded evidence is an exact prefix")
	assert.Contains(t, out.String(), "missing: cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
}

func TestReadExpectedAuditEvidenceRequiresSuccessfulReport(t *testing.T) {
	evidence := &api.AuditEvidence{
		VaultID:                    "11111111-1111-4111-8111-111111111111",
		LineageID:                  "22222222-2222-4222-8222-222222222222",
		OperationSequenceHighWater: 1, AllocationEntryCount: 1,
		AllocationHead: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Scopes: []api.AuditScopeEvidence{{
			ID: "33333333-3333-4333-8333-333333333333", EntryCount: 1,
			ChainHead: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		}},
	}
	path := filepath.Join(t.TempDir(), "audit-evidence.json")
	raw, err := json.Marshal(api.AuditVerifyReport{
		Enabled: true, Evidence: evidence, ProtectedBlobs: 1, VerifiedBlobs: 1,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	got, err := readExpectedAuditEvidence(path)
	require.NoError(t, err)
	assert.Equal(t, evidence, got)

	raw, err = json.Marshal(api.AuditVerifyReport{
		Enabled: true, Evidence: evidence, ProtectedBlobs: 1,
		Problems: []api.VerifyProblem{{
			Hash:    "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			Problem: "missing",
		}},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	_, err = readExpectedAuditEvidence(path)
	require.ErrorContains(t, err, "not a successful active verification")
}

func TestWriteAuditVerificationExplainsDormantVault(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, writeAuditVerification(&out, api.AuditVerifyReport{}))
	assert.Equal(t, "audit is not enabled; no audit authority to verify\n", out.String())
}
