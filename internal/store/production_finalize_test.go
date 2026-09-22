package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
	productionservice "go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/redactiontest"
)

func TestProductionGateStorePersistsReceiptResultsAndAuditAtomically(t *testing.T) {
	s := newTestStore(t)
	stored := productionGateStoredFixture(t)
	request := productionGateRequest(t, stored)
	loads := 0
	gateStore, err := NewProductionGateStore(s, func(_ context.Context, _ *sql.Tx, _ productionservice.PreparedInputRequest) (productionservice.StoredProductionInputs, error) {
		loads++
		return stored, nil
	})
	require.NoError(t, err)

	authority, err := productionservice.RunPreparedInputGates(t.Context(), gateStore, request)
	require.NoError(t, err)
	require.NotNil(t, authority.Receipt)
	require.NotEmpty(t, authority.GateResults.SHA256)
	require.NotEmpty(t, authority.Audit.SHA256)
	require.Equal(t, authority.Receipt.SHA256, authority.Audit.PreparedInputSHA256)

	loaded, err := s.PreparedInputAuthority(t.Context(), request.OperationID)
	require.NoError(t, err)
	require.Equal(t, authority.Prepared.SHA256, loaded.Prepared.SHA256)
	require.Equal(t, authority.Receipt.SHA256, loaded.Receipt.SHA256)
	require.Equal(t, authority.Audit.SHA256, loaded.Audit.SHA256)
	replayed, err := productionservice.RunPreparedInputGates(t.Context(), gateStore, request)
	require.NoError(t, err)
	require.Equal(t, authority.Receipt.SHA256, replayed.Receipt.SHA256)
	require.Equal(t, 1, loads, "an exact retry returns the committed authority without re-reading live rows")

	changed := request
	changed.PreparedAt = changed.PreparedAt.Add(time.Second)
	_, err = productionservice.RunPreparedInputGates(t.Context(), gateStore, changed)
	requireProductionProblem(t, err, documentproduction.ProblemChangedPayload)
	require.Equal(t, 1, loads)

	var operations int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM production_operation_receipts WHERE operation_id=?`, request.OperationID).Scan(&operations))
	require.Equal(t, 1, operations, "receipt, results and audit have one atomic immutable row")
}

func TestProductionGateStoreMetadataRoundTripPreservesGateAuthority(t *testing.T) {
	s := newTestStore(t)
	stored := productionGateStoredFixture(t)
	request := productionGateRequest(t, stored)
	gateStore, err := NewProductionGateStore(s, func(_ context.Context, _ *sql.Tx, _ productionservice.PreparedInputRequest) (productionservice.StoredProductionInputs, error) {
		return stored, nil
	})
	require.NoError(t, err)

	authority, err := productionservice.RunPreparedInputGates(t.Context(), gateStore, request)
	require.NoError(t, err)

	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &backup))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(backup.Bytes())))
	require.NoError(t, restored.ValidateMetadata(t.Context()))
	loaded, err := restored.PreparedInputAuthority(t.Context(), request.OperationID)
	require.NoError(t, err)
	require.Equal(t, authority.Audit.SHA256, loaded.Audit.SHA256)
}

func TestProductionGateStoreMetadataRejectsDetachedGateAudit(t *testing.T) {
	s := newTestStore(t)
	stored := productionGateStoredFixture(t)
	request := productionGateRequest(t, stored)
	gateStore, err := NewProductionGateStore(s, func(_ context.Context, _ *sql.Tx, _ productionservice.PreparedInputRequest) (productionservice.StoredProductionInputs, error) {
		return stored, nil
	})
	require.NoError(t, err)
	_, err = productionservice.RunPreparedInputGates(t.Context(), gateStore, request)
	require.NoError(t, err)

	otherOperationID := "99999999-9999-4999-8999-999999999999"
	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &backup))
	detached := detachedPreparedInputOperationMetadata(t, backup.Bytes(), request.OperationID, otherOperationID)
	restored := newTestStore(t)
	require.ErrorContains(t, restored.ImportMetadata(t.Context(), bytes.NewReader(detached)), "prepared-input operation receipt is detached")
}

func detachedPreparedInputOperationMetadata(t *testing.T, backup []byte, operationID, detachedOperationID string) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSuffix(backup, []byte("\n")), []byte("\n"))
	changed := false
	for index, line := range lines {
		var authority metadataProductionAuthority
		require.NoError(t, json.Unmarshal(line, &authority))
		if authority.Kind != productionMetadataOperation {
			continue
		}
		var operation productionMetadataOperationRow
		require.NoError(t, json.Unmarshal(authority.CanonicalJSON, &operation))
		if operation.OperationID != operationID {
			continue
		}
		operation.OperationID = detachedOperationID
		canonicalJSON, err := canonical.Marshal(operation)
		require.NoError(t, err)
		authority.Key = detachedOperationID
		authority.CanonicalJSON = canonicalJSON
		authority.Checksum = digestProductionBytes(canonicalJSON)
		lines[index], err = canonical.Marshal(authority)
		require.NoError(t, err)
		changed = true
	}
	require.True(t, changed)
	return append(bytes.Join(lines, []byte("\n")), '\n')
}

func TestProductionGateStorePersistsFailedConcurrentEditWithoutReceipt(t *testing.T) {
	s := newTestStore(t)
	observed := productionGateStoredFixture(t)
	request := productionGateRequest(t, observed)
	changed := observed
	changed.Draft.ETag++
	changed.Members = slices.Clone(observed.Members)
	changed.Members[0].Decisions = append(slices.Clone(changed.Members[0].Decisions), redaction.Decision{
		ID: "12121212-1212-4212-8212-121212121212", MemberID: changed.Members[0].Member.ID,
		Action: "keep", Selector: redaction.Selector{Kind: "page", MapSHA256: changed.Members[0].Member.MapSHA256, Pages: []int{1}},
	})
	gateStore, err := NewProductionGateStore(s, func(_ context.Context, _ *sql.Tx, _ productionservice.PreparedInputRequest) (productionservice.StoredProductionInputs, error) {
		return changed, nil
	})
	require.NoError(t, err)

	authority, err := productionservice.RunPreparedInputGates(t.Context(), gateStore, request)
	require.Error(t, err)
	require.Nil(t, authority.Receipt)
	require.NotEmpty(t, authority.Audit.SHA256)

	loaded, err := s.PreparedInputAuthority(t.Context(), request.OperationID)
	require.NoError(t, err)
	require.Nil(t, loaded.Receipt)
	require.Equal(t, authority.GateResults, loaded.GateResults)
}

func TestProductionGateStoreRollsBackReceiptAndAuditTogether(t *testing.T) {
	s := newTestStore(t)
	stored := productionGateStoredFixture(t)
	request := productionGateRequest(t, stored)
	gateStore, err := NewProductionGateStore(s, func(_ context.Context, _ *sql.Tx, _ productionservice.PreparedInputRequest) (productionservice.StoredProductionInputs, error) {
		return stored, nil
	})
	require.NoError(t, err)
	installProductionInsertFailure(t, s, "synthetic_fail_gate_receipt", "production_operation_receipts", "synthetic gate receipt failure")
	_, err = productionservice.RunPreparedInputGates(t.Context(), gateStore, request)
	requireProductionStorageError(t, err, "synthetic gate receipt failure")
	dropProductionTrigger(t, s, "synthetic_fail_gate_receipt")

	var operations int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM production_operation_receipts WHERE operation_id=?`, request.OperationID).Scan(&operations))
	require.Zero(t, operations)
	_, err = s.PreparedInputAuthority(t.Context(), request.OperationID)
	require.ErrorIs(t, err, ErrNotFound)
}

func productionGateStoredFixture(t *testing.T) productionservice.StoredProductionInputs {
	t.Helper()
	policy, err := documentproduction.GenericPolicyVersion()
	require.NoError(t, err)
	m := redactiontest.Map("A")
	recipe, err := pdfproduction.QualifiedRecipeForDPI(300)
	require.NoError(t, err)
	decision := redaction.Decision{
		ID: "11111111-1111-4111-8111-111111111111", MemberID: "22222222-2222-4222-8222-222222222222",
		Action: "keep", Uncertain: true,
		Selector: redaction.Selector{Kind: "page", MapSHA256: m.SHA256, Pages: []int{1}},
	}
	resolved, err := redaction.Resolve(m, "keep_selected", []redaction.Decision{decision}, recipe)
	require.NoError(t, err)
	_, decisionsSHA256, err := redaction.CanonicalDecisions([]redaction.Decision{decision})
	require.NoError(t, err)
	member := redaction.Member{
		ID: decision.MemberID, VaultID: "33333333-3333-4333-8333-333333333333",
		SourceVersionID: "44444444-4444-4444-8444-444444444444", SourceSHA256: productionPersistenceSHA("1"), SourceSize: 1,
		PDFSHA256: m.PDFSHA256, PDFSize: 1, NodeID: 1, Ordinal: 1,
		Family: redaction.FamilyContext{Kind: "standalone", RootVersionID: "44444444-4444-4444-8444-444444444444"},
		Mode:   "keep_selected", MapSHA256: m.SHA256, PageInventorySHA256: productionPersistenceSHA("2"),
	}
	stored := productionservice.StoredProductionInputs{
		Draft: redaction.Draft{
			SetID: "55555555-5555-4555-8555-555555555555", Revision: 1, ETag: 3,
			InstructionsSHA256: productionPersistenceSHA("3"), DecisionsSHA256: decisionsSHA256,
			RecipeID: redaction.DefaultRecipeID, RecipeSHA256: resolved.RecipeSHA256,
			ProfileID: redaction.DefaultOutputProfileID, ProfileSHA256: productionPersistenceSHA("4"),
			DisclosureProfileID: redaction.DefaultDisclosureProfileID, DisclosureProfileSHA256: productionPersistenceSHA("5"),
			NumberingRecipeID: redaction.BatesNumberingRecipeID, NumberingRecipeSHA256: productionPersistenceSHA("6"),
			Policy: redaction.PolicySelection{PolicyID: policy.ID, Version: policy.Version, PolicySHA256: policy.SHA256},
			State:  "draft", MembershipSealed: true,
		},
		Policy: policy,
		Members: []productionservice.StoredPreparedMember{{
			Member: member, Decisions: []redaction.Decision{decision}, Resolved: resolved,
			Facts: documentproduction.PolicyMemberFacts{MemberID: member.ID, Fields: map[string][]string{}, Labels: []string{}},
		}},
	}
	_, stored.Draft.MemberHash, err = documentproduction.PreparedMemberHash([]documentproduction.PreparedMember{{Member: member}})
	require.NoError(t, err)
	review, err := redaction.ReviewBinding(redaction.ReviewInput{
		SetID: stored.Draft.SetID, MemberID: member.ID, VaultID: member.VaultID, SourceVersionID: member.SourceVersionID,
		Revision: stored.Draft.Revision, Ordinal: member.Ordinal, NodeID: member.NodeID, SourceSize: member.SourceSize,
		PDFSize: member.PDFSize, SourceSHA256: member.SourceSHA256, PDFSHA256: member.PDFSHA256,
		PageInventorySHA256: member.PageInventorySHA256, MapSHA256: member.MapSHA256, Mode: member.Mode,
		MemberHash: stored.Draft.MemberHash, InstructionsSHA256: stored.Draft.InstructionsSHA256,
		RecipeSHA256: stored.Draft.RecipeSHA256, DecisionsSHA256: decisionsSHA256, ResolvedSHA256: resolved.SHA256,
	})
	require.NoError(t, err)
	stored.Members[0].Member.Reviewed = true
	stored.Members[0].Member.ReviewBinding = review
	return stored
}

func productionGateRequest(t *testing.T, stored productionservice.StoredProductionInputs) productionservice.PreparedInputRequest {
	t.Helper()
	revisionSHA256, err := productionservice.ProductionRevisionSHA256(stored)
	require.NoError(t, err)
	return productionservice.PreparedInputRequest{
		OperationID: "66666666-6666-4666-8666-666666666666",
		ReceiptID:   "77777777-7777-4777-8777-777777777777",
		SetID:       stored.Draft.SetID, Revision: stored.Draft.Revision, ExpectedETag: stored.Draft.ETag,
		ExpectedRevisionSHA256: revisionSHA256,
		PreparedAt:             time.Date(2026, 9, 22, 16, 0, 0, 0, time.UTC),
	}
}
