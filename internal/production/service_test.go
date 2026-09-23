package production

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/redactiontest"
)

func testHash(v string) string { s := sha256.Sum256([]byte(v)); return hex.EncodeToString(s[:]) }

type storedFixture struct {
	finalized FinalizedProduction
	reserved  int
}

func (f *storedFixture) LoadFinalizedProduction(_ context.Context, _ string, _ int64) (FinalizedProduction, error) {
	return f.finalized, nil
}

func newStoredFixture(t *testing.T) *storedFixture {
	t.Helper()
	return newStoredFixtureWithText(t, "A")
}

func newStoredFixtureWithText(t *testing.T, text string) *storedFixture {
	t.Helper()
	return newStoredFixtureWithSource(t, text, nil)
}

func newStoredFixtureWithSource(t *testing.T, text string, pdf []byte) *storedFixture {
	t.Helper()
	policy, err := documentproduction.GenericPolicyVersion()
	if err != nil {
		t.Fatal(err)
	}
	m := redactiontest.Map(text)
	pdfSHA, pdfSize := testHash("pdf"), int64(20)
	if len(pdf) != 0 {
		pdfSHA, pdfSize = testHash(string(pdf)), int64(len(pdf))
		m.PDFSHA256 = pdfSHA
		_, m.SHA256, err = redaction.CanonicalTextMap(m)
		if err != nil {
			t.Fatal(err)
		}
	}
	recipe, err := pdfproduction.QualifiedRecipeForDPI(300)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := redaction.Resolve(m, "redact_selected", nil, recipe)
	if err != nil {
		t.Fatal(err)
	}
	_, decisionsSHA, _ := redaction.CanonicalDecisions(nil)
	memberID := "44444444-4444-4444-8444-444444444444"
	versionID := "66666666-6666-4666-8666-666666666666"
	setID := "11111111-1111-4111-8111-111111111111"
	prepared := documentproduction.PreparedProduction{Contract: documentproduction.PreparedProductionContractV1, SetID: setID, Revision: 7, ETag: 11, MembershipSealed: true, InstructionsSHA256: testHash("instructions"), DecisionsSHA256: decisionsSHA, RecipeSHA256: resolved.RecipeSHA256, OutputProfileSHA256: testHash("output"), DisclosureProfileSHA256: testHash("disclosure"), NumberingPolicySHA256: testHash("numbering"), Policy: policy, PreparedAt: "2026-09-22T16:00:00Z", Members: []documentproduction.PreparedMember{{Member: redaction.Member{ID: memberID, VaultID: setID, Ordinal: 1, SourceVersionID: versionID, SourceSHA256: testHash("source"), SourceSize: 10, PDFSHA256: pdfSHA, PDFSize: pdfSize, MapSHA256: m.SHA256, PageInventorySHA256: testHash("pages"), Mode: "redact_selected", NodeID: 1, Family: redaction.FamilyContext{Kind: "standalone", RootVersionID: versionID}, Reviewed: true, ReviewBinding: testHash("review")}, Decisions: []redaction.Decision{}, Resolved: resolved, Frames: []documentproduction.PreparedFrame{{Page: 1, SHA256: m.Pages[0].FrameSHA256, Width: m.Pages[0].Width, Height: m.Pages[0].Height}}, Facts: documentproduction.PolicyMemberFacts{MemberID: memberID, Fields: map[string][]string{}, Labels: []string{}}, DecisionsSHA256: decisionsSHA, ResolvedSHA256: resolved.SHA256, ReviewBinding: testHash("review"), Disposition: documentproduction.PolicyDispositionProduce}}}
	_, prepared.MemberHash, _ = documentproduction.PreparedMemberHash(prepared.Members)
	prepared.ApprovalSubject = documentproduction.ApprovalSubject{Contract: documentproduction.ApprovalSubjectContractV1, SetID: setID, Revision: 7, Members: []documentproduction.ApprovalMember{{MemberID: memberID, Ordinal: 1, SourceVersionID: versionID, SourceSHA256: testHash("source"), SourceSize: 10, PDFSHA256: pdfSHA, PageInventorySHA256: testHash("pages"), MapSHA256: m.SHA256, DecisionsSHA256: decisionsSHA, ResolvedSHA256: resolved.SHA256}}, InstructionsSHA256: prepared.InstructionsSHA256, RecipeSHA256: prepared.RecipeSHA256, OutputProfileSHA256: prepared.OutputProfileSHA256, DisclosureProfileSHA256: prepared.DisclosureProfileSHA256, NumberingPolicySHA256: prepared.NumberingPolicySHA256, Policy: documentproduction.PolicySelection{PolicyID: policy.ID, Version: policy.Version, PolicySHA256: policy.SHA256}}
	_, prepared.SHA256, err = documentproduction.CanonicalPreparedProduction(prepared)
	if err != nil {
		t.Fatal(err)
	}
	results := documentproduction.GateResults{Contract: documentproduction.GateResultsContractV1, PreparedSHA256: prepared.SHA256, EvaluatedAt: "2026-09-22T16:00:00Z", Checks: passingFixtureChecks()}
	_, results.SHA256, _ = documentproduction.CanonicalGateResults(results)
	_, subjectSHA, _ := documentproduction.CanonicalApprovalSubject(prepared.ApprovalSubject)
	receipt := documentproduction.PreparedInputReceipt{Contract: documentproduction.PreparedInputContractV1, ID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", ApprovalSubjectSHA256: subjectSHA, GateResultsSHA256: results.SHA256, CreatedAt: "2026-09-22T16:00:00Z"}
	_, receipt.SHA256, _ = documentproduction.CanonicalPreparedInputReceipt(receipt)
	authority := documentproduction.PreparedInputAuthority{Prepared: prepared, GateResults: results, Receipt: &receipt, Audit: documentproduction.GateAudit{Contract: documentproduction.GateAuditContractV1, OperationID: "ffffffff-ffff-4fff-8fff-ffffffffffff", PreparedSHA256: prepared.SHA256, GateResultsSHA256: results.SHA256, PreparedInputSHA256: receipt.SHA256, RecordedAt: "2026-09-22T16:00:00Z"}}
	_, authority.Audit.SHA256, _ = documentproduction.CanonicalGateAudit(authority.Audit)
	return &storedFixture{finalized: FinalizedProduction{Draft: redaction.Draft{SetID: setID, Revision: 7, ETag: 11, State: "finalized"}, Authority: authority}}
}
func passingFixtureChecks() []documentproduction.GateCheck {
	ids := documentproduction.RequiredGateCheckIDs()
	out := make([]documentproduction.GateCheck, 0, len(ids))
	for _, id := range ids {
		out = append(out, documentproduction.GateCheck{ID: id, Required: true, State: documentproduction.GateStatePass, Evidence: []documentproduction.GateEvidence{{Kind: "digest", SHA256: testHash("evidence")}}})
	}
	return out
}
