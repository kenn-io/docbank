package production

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/redactiontest"
)

func testHash(v string) string { s := sha256.Sum256([]byte(v)); return hex.EncodeToString(s[:]) }

type storedFixture struct {
	finalized FinalizedProduction
	job       Job
	published bool
	reserved  int
	canceled  bool
}

func (f *storedFixture) RenewProductionJobClaim(context.Context, JobClaim, time.Duration) (JobClaim, error) {
	return JobClaim{}, ErrJobStaleClaim
}
func (f *storedFixture) StageProductionArtifact(context.Context, JobClaim, documentproduction.Artifact) error {
	return nil
}

func (f *storedFixture) LoadFinalizedProduction(context.Context, string, int64) (FinalizedProduction, error) {
	return f.finalized, nil
}
func (f *storedFixture) AdmitProductionJob(_ context.Context, r JobRequest) (Job, error) {
	f.job = Job{ID: r.JobID, OperationID: r.OperationID, SetID: r.SetID, Revision: r.Revision, ETag: r.ETag, PreparedInputSHA256: r.PreparedInputSHA256}
	return f.job, nil
}
func (f *storedFixture) ClaimProductionJob(context.Context, string, string, time.Duration) (JobClaim, error) {
	return JobClaim{JobID: f.job.ID, Worker: "worker", Token: "claim", Epoch: 1}, nil
}
func (f *storedFixture) ReserveProductionJobNumbers(context.Context, Job, FinalizedProduction) (documentproduction.NumberReservation, error) {
	f.reserved++
	r := documentproduction.NumberReservation{Contract: documentproduction.NumberReservationContractV1, Authority: "synthetic-ledger/v1", ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", OperationID: f.job.ID, RevisionSHA256: f.finalized.Authority.Prepared.SHA256, State: "reserved", Numbers: []documentproduction.AssignedNumber{{MemberID: "44444444-4444-4444-8444-444444444444", MemberOrdinal: 1, Page: 1, Text: "SYN000001"}}}
	_, r.SHA256, _ = documentproduction.CanonicalNumberReservation(r)
	if err := documentproduction.ValidateNumberReservation(r); err != nil {
		panic(err)
	}
	return r, nil
}
func (f *storedFixture) PublishProductionJob(_ context.Context, _ JobClaim, j Job, r documentproduction.ProductionReceipt, m documentproduction.ArtifactManifest, e []redaction.Endorsement) (Job, error) {
	f.published = true
	j.Receipt = r
	j.Manifest = m
	j.Endorsements = e
	return j, nil
}
func (f *storedFixture) CancelProductionJob(context.Context, string) error {
	f.canceled = true
	return nil
}

type fixtureRenderer struct{}

func (fixtureRenderer) Render(_ context.Context, f FinalizedProduction, _ documentproduction.NumberReservation, _ []redaction.Endorsement, emit func(documentproduction.Artifact) error) error {
	return emit(documentproduction.Artifact{ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", MemberID: f.Authority.Prepared.Members[0].Member.ID, MemberOrdinal: 1, Page: 1, Role: documentproduction.ArtifactRoleRedactedPage, Path: "VOL001/SYN000001.png", SHA256: testHash("artifact"), Size: 10, MediaType: "image/png", Volume: "VOL001"})
}

type blockingRenderer struct{}

func (blockingRenderer) Render(ctx context.Context, _ FinalizedProduction, _ documentproduction.NumberReservation, _ []redaction.Endorsement, _ func(documentproduction.Artifact) error) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestRunCancelsAndDoesNotPublishAfterRenewalLoss(t *testing.T) {
	f := newStoredFixture(t)
	req := JobRequest{JobID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", OperationID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", SetID: f.finalized.Draft.SetID, Revision: f.finalized.Draft.Revision, ETag: f.finalized.Draft.ETag, PreparedInputSHA256: f.finalized.Authority.Receipt.SHA256, RevisionSHA256: f.finalized.Authority.Prepared.SHA256, NumberingProfileSHA256: testHash("layout")}
	_, err := runWithRenewalInterval(t.Context(), f, req, "worker", blockingRenderer{}, time.Millisecond)
	if err == nil || f.published {
		t.Fatalf("err=%v published=%v", err, f.published)
	}
	if !errors.Is(err, ErrJobStaleClaim) {
		t.Fatalf("renewal error hidden: %v", err)
	}
}

func TestRunUsesStoredFinalizedPreparedAuthorityAndConcreteReceipt(t *testing.T) {
	f := newStoredFixture(t)
	if err := documentproduction.ValidatePreparedInputAuthority(f.finalized.Authority); err != nil {
		t.Fatalf("fixture authority: %v", err)
	}
	req := JobRequest{JobID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", OperationID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", SetID: f.finalized.Draft.SetID, Revision: f.finalized.Draft.Revision, ETag: f.finalized.Draft.ETag, PreparedInputSHA256: f.finalized.Authority.Receipt.SHA256, RevisionSHA256: f.finalized.Authority.Prepared.SHA256, NumberingProfileSHA256: testHash("layout")}
	job, err := Run(t.Context(), f, req, "worker", fixtureRenderer{})
	if err != nil {
		t.Fatal(err)
	}
	if !f.published || f.reserved != 1 {
		t.Fatalf("published=%v reserved=%d", f.published, f.reserved)
	}
	if job.Receipt.NumberReservationSHA256 == "" || job.Receipt.ArtifactManifestSHA256 == "" || job.Receipt.PreparedInputSHA256 != f.finalized.Authority.Receipt.SHA256 {
		t.Fatalf("receipt did not bind concrete authority: %+v", job.Receipt)
	}
}

func TestRunRejectsChangedFinalizedETagOrPreparedInputs(t *testing.T) {
	for name, mutate := range map[string]func(*storedFixture){
		"etag": func(f *storedFixture) { f.finalized.Draft.ETag++ },
		"source": func(f *storedFixture) {
			f.finalized.Authority.Prepared.Members[0].Member.SourceSHA256 = testHash("changed-source")
		},
		"decisions": func(f *storedFixture) { f.finalized.Authority.Prepared.DecisionsSHA256 = testHash("changed-decisions") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newStoredFixture(t)
			req := JobRequest{JobID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", OperationID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", SetID: f.finalized.Draft.SetID, Revision: f.finalized.Draft.Revision, ETag: 11, PreparedInputSHA256: f.finalized.Authority.Receipt.SHA256, RevisionSHA256: f.finalized.Authority.Prepared.SHA256, NumberingProfileSHA256: testHash("layout")}
			mutate(f)
			if _, err := Run(t.Context(), f, req, "worker", fixtureRenderer{}); err == nil {
				t.Fatal("changed finalized authority was accepted")
			}
		})
	}
}

func newStoredFixture(t *testing.T) *storedFixture {
	t.Helper()
	policy, err := documentproduction.GenericPolicyVersion()
	if err != nil {
		t.Fatal(err)
	}
	m := redactiontest.Map("A")
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
	prepared := documentproduction.PreparedProduction{Contract: documentproduction.PreparedProductionContractV1, SetID: setID, Revision: 7, ETag: 11, MembershipSealed: true, InstructionsSHA256: testHash("instructions"), DecisionsSHA256: decisionsSHA, RecipeSHA256: resolved.RecipeSHA256, OutputProfileSHA256: testHash("output"), DisclosureProfileSHA256: testHash("disclosure"), NumberingPolicySHA256: testHash("numbering"), Policy: policy, PreparedAt: "2026-09-22T16:00:00Z", Members: []documentproduction.PreparedMember{{Member: redaction.Member{ID: memberID, VaultID: setID, Ordinal: 1, SourceVersionID: versionID, SourceSHA256: testHash("source"), SourceSize: 10, PDFSHA256: testHash("pdf"), PDFSize: 20, MapSHA256: m.SHA256, PageInventorySHA256: testHash("pages"), Mode: "redact_selected", NodeID: 1, Family: redaction.FamilyContext{Kind: "standalone", RootVersionID: versionID}, Reviewed: true, ReviewBinding: testHash("review")}, Decisions: []redaction.Decision{}, Resolved: resolved, Frames: []documentproduction.PreparedFrame{{Page: 1, SHA256: m.Pages[0].FrameSHA256, Width: m.Pages[0].Width, Height: m.Pages[0].Height}}, Facts: documentproduction.PolicyMemberFacts{MemberID: memberID, Fields: map[string][]string{}, Labels: []string{}}, DecisionsSHA256: decisionsSHA, ResolvedSHA256: resolved.SHA256, ReviewBinding: testHash("review"), Disposition: documentproduction.PolicyDispositionProduce}}}
	_, prepared.MemberHash, _ = documentproduction.PreparedMemberHash(prepared.Members)
	prepared.ApprovalSubject = documentproduction.ApprovalSubject{Contract: documentproduction.ApprovalSubjectContractV1, SetID: setID, Revision: 7, Members: []documentproduction.ApprovalMember{{MemberID: memberID, Ordinal: 1, SourceVersionID: versionID, SourceSHA256: testHash("source"), SourceSize: 10, PDFSHA256: testHash("pdf"), PageInventorySHA256: testHash("pages"), MapSHA256: m.SHA256, DecisionsSHA256: decisionsSHA, ResolvedSHA256: resolved.SHA256}}, InstructionsSHA256: prepared.InstructionsSHA256, RecipeSHA256: prepared.RecipeSHA256, OutputProfileSHA256: prepared.OutputProfileSHA256, DisclosureProfileSHA256: prepared.DisclosureProfileSHA256, NumberingPolicySHA256: prepared.NumberingPolicySHA256, Policy: documentproduction.PolicySelection{PolicyID: policy.ID, Version: policy.Version, PolicySHA256: policy.SHA256}}
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
