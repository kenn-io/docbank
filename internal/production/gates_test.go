package production

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/redactiontest"
)

func TestPreparedInputGatesAdmitAutonomousUncertainKeepBeforeNumbering(t *testing.T) {
	stored := syntheticStoredProductionInputs(t)
	store := &gateTestStore{stored: stored}
	request := syntheticGateRequest(t, stored)

	authority, err := RunPreparedInputGates(t.Context(), store, request)
	require.NoError(t, err)
	require.NotNil(t, authority.Receipt)
	require.True(t, documentproduction.GateResultsPassed(authority.GateResults))
	decisionCheck := gateCheck(t, authority, documentproduction.GateCheckDecisions)
	require.Equal(t, documentproduction.GateStatePass, decisionCheck.State)
	require.Equal(t, int64(1), decisionCheck.Evidence[0].Count, "keep uncertainty stays visible")

	reserved := 0
	err = ReserveAfterPreparedInput(authority, PreparedInputReference{
		OperationID: request.OperationID, PreparedSHA256: authority.Prepared.SHA256,
		ReceiptSHA256: authority.Receipt.SHA256,
	}, func(documentproduction.PreparedInputAuthority) error {
		reserved++
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, reserved)
}

func TestPreparedInputGatesRejectEveryBlockingStateBeforeNumbering(t *testing.T) {
	base := syntheticStoredProductionInputs(t)
	tests := map[string]func(*StoredProductionInputs){
		"stale etag": func(value *StoredProductionInputs) { value.Draft.ETag++ },
		"source substitution": func(value *StoredProductionInputs) {
			value.Members[0].Member.SourceSHA256 = gateSHA("substituted source")
		},
		"empty to nonempty annotations": func(value *StoredProductionInputs) {
			value.Members[0].Decisions = append(value.Members[0].Decisions, redaction.Decision{
				ID: "12121212-1212-4212-8212-121212121212", MemberID: value.Members[0].Member.ID,
				Action: "keep", Selector: redaction.Selector{Kind: "page", MapSHA256: value.Members[0].Member.MapSHA256, Pages: []int{1}},
			})
		},
		"stale review": func(value *StoredProductionInputs) { value.Members[0].Member.ReviewBinding = gateSHA("stale review") },
		"missing review": func(value *StoredProductionInputs) {
			value.Members[0].Member.Reviewed = false
			value.Members[0].Member.ReviewBinding = ""
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			stored := cloneStoredProductionInputs(base)
			request := syntheticGateRequest(t, stored)
			request.ExpectedETag = base.Draft.ETag
			request.ExpectedRevisionSHA256, _ = ProductionRevisionSHA256(base)
			mutate(&stored)
			store := &gateTestStore{stored: stored}
			authority, err := RunPreparedInputGates(t.Context(), store, request)
			require.Error(t, err)
			require.Nil(t, authority.Receipt)
			require.NotEmpty(t, authority.Audit.SHA256, "failed gate evidence is persisted")
			reserved := 0
			err = ReserveAfterPreparedInput(authority, PreparedInputReference{
				OperationID: request.OperationID, PreparedSHA256: authority.Prepared.SHA256, ReceiptSHA256: gateSHA("wrong receipt"),
			}, func(documentproduction.PreparedInputAuthority) error { reserved++; return nil })
			require.Error(t, err)
			require.Zero(t, reserved, "a failed gate must consume no number")
		})
	}
}

func TestPreparedInputPolicyWithheldPrivilegeAndApprovalGates(t *testing.T) {
	base := syntheticStoredProductionInputs(t)
	tests := map[string]func(*StoredProductionInputs){
		"missing required label": func(value *StoredProductionInputs) {
			value.Policy = gatePolicy(t, documentproduction.PolicyRule{
				ID: "label", Kind: documentproduction.PolicyRuleLabel,
				Predicate:     documentproduction.PolicyPredicate{Field: "member.id", Operator: documentproduction.PolicyOperatorPresent},
				RequiredLabel: "responsive",
			})
			value.Members[0].Facts.Labels = []string{}
		},
		"conflicting tags": func(value *StoredProductionInputs) {
			value.Policy = gatePolicy(t,
				documentproduction.PolicyRule{ID: "produce", Kind: documentproduction.PolicyRuleDisposition,
					Predicate: documentproduction.PolicyPredicate{Field: "tag", Operator: documentproduction.PolicyOperatorEquals, Values: []string{"both"}}, Disposition: documentproduction.PolicyDispositionProduce},
				documentproduction.PolicyRule{ID: "withhold", Kind: documentproduction.PolicyRuleDisposition,
					Predicate: documentproduction.PolicyPredicate{Field: "tag", Operator: documentproduction.PolicyOperatorEquals, Values: []string{"both"}}, Disposition: documentproduction.PolicyDispositionWithhold})
			value.Members[0].Facts.Fields = map[string][]string{"tag": {"both"}}
		},
		"broken family policy": func(value *StoredProductionInputs) {
			value.Policy = gatePolicy(t, documentproduction.PolicyRule{
				ID: "family", Kind: documentproduction.PolicyRuleFamily,
				Predicate:   documentproduction.PolicyPredicate{Field: "member.id", Operator: documentproduction.PolicyOperatorPresent},
				Disposition: documentproduction.PolicyDispositionProduce, FamilyMode: documentproduction.PolicyFamilyComplete,
			})
			value.Members[0].Facts.FamilyID = "synthetic-family"
			value.Members[0].Facts.FamilyComplete = false
		},
		"missing withheld member": func(value *StoredProductionInputs) {
			value.Policy = gatePolicy(t, documentproduction.PolicyRule{
				ID: "withhold", Kind: documentproduction.PolicyRuleDisposition,
				Predicate:   documentproduction.PolicyPredicate{Field: "member.id", Operator: documentproduction.PolicyOperatorPresent},
				Disposition: documentproduction.PolicyDispositionWithhold,
			})
			value.Withheld = nil
		},
		"required privilege receipt": func(value *StoredProductionInputs) {
			value.Policy.PrivilegeLog = documentproduction.PrivilegeLogRequirement{Required: true, RequireFrozenReceipt: true,
				RequiredFields: []string{"description"}, AllowedBases: []string{"synthetic"}}
			value.Policy = resealGatePolicy(t, value.Policy)
			value.PrivilegeLog = nil
		},
		"required approval": func(value *StoredProductionInputs) {
			value.Policy.Approval.Required = true
			value.Policy = resealGatePolicy(t, value.Policy)
			value.ApprovalGrant = nil
			value.ApprovalEvaluation = nil
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			stored := cloneStoredProductionInputs(base)
			mutate(&stored)
			stored.Draft.Policy = redaction.PolicySelection{PolicyID: stored.Policy.ID, Version: stored.Policy.Version, PolicySHA256: stored.Policy.SHA256}
			request := syntheticGateRequest(t, stored)
			store := &gateTestStore{stored: stored}
			authority, err := RunPreparedInputGates(t.Context(), store, request)
			require.Error(t, err)
			require.Nil(t, authority.Receipt)
		})
	}
}

func TestPreparedInputWithheldSelectionMatchesStoredDisposition(t *testing.T) {
	base := syntheticStoredProductionInputs(t)
	base.Policy = gatePolicy(t, documentproduction.PolicyRule{
		ID: "withhold", Kind: documentproduction.PolicyRuleDisposition,
		Predicate:   documentproduction.PolicyPredicate{Field: "member.id", Operator: documentproduction.PolicyOperatorPresent},
		Disposition: documentproduction.PolicyDispositionWithhold,
	})
	base.Draft.Policy = redaction.PolicySelection{PolicyID: base.Policy.ID, Version: base.Policy.Version, PolicySHA256: base.Policy.SHA256}
	base.Withheld = syntheticWithheldSelection(t, base)

	for name, mutate := range map[string]func(*StoredProductionInputs){
		"exact selection passes": func(*StoredProductionInputs) {},
		"source substitution fails": func(value *StoredProductionInputs) {
			value.Withheld.Members[0].SourceSHA256 = gateSHA("substituted withheld source")
			resealWithheldSelection(t, value.Withheld)
		},
		"extra member fails": func(value *StoredProductionInputs) {
			value.Withheld.Members = append(value.Withheld.Members, documentproduction.WithheldMember{
				ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Ordinal: 2,
				SourceVersionID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", SourceSHA256: gateSHA("extra source"),
				SourceSize: 2, FamilyOrder: 1,
				Family: redaction.FamilyContext{Kind: "standalone", RootVersionID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"},
			})
			resealWithheldSelection(t, value.Withheld)
		},
	} {
		t.Run(name, func(t *testing.T) {
			stored := cloneStoredProductionInputs(base)
			selection := *base.Withheld
			selection.Members = slices.Clone(base.Withheld.Members)
			stored.Withheld = &selection
			mutate(&stored)
			authority, err := RunPreparedInputGates(t.Context(), &gateTestStore{stored: stored}, syntheticGateRequest(t, stored))
			if name == "exact selection passes" {
				require.NoError(t, err)
				require.NotNil(t, authority.Receipt)
				return
			}
			require.Error(t, err)
			require.Nil(t, authority.Receipt)
			require.Equal(t, documentproduction.GateStateFail, gateCheck(t, authority, documentproduction.GateCheckWithheld).State)
		})
	}
}

func TestPreparedInputRequiredApprovalMatchesCurrentSubjectAndEvaluation(t *testing.T) {
	base := syntheticStoredProductionInputs(t)
	base.Policy.Approval.Required = true
	base.Policy = resealGatePolicy(t, base.Policy)
	base.Draft.Policy = redaction.PolicySelection{PolicyID: base.Policy.ID, Version: base.Policy.Version, PolicySHA256: base.Policy.SHA256}
	request := syntheticGateRequest(t, base)
	prepared, _, err := prepareStoredProduction(base, request.PreparedAt)
	require.NoError(t, err)
	record, err := PrepareApprovalRecord(RecordApprovalRequest{
		OperationID: "89898989-8989-4989-8989-898989898989",
		ApprovalID:  "90909090-9090-4090-8090-909090909090", Subject: prepared.ApprovalSubject,
	}, base.Policy, AuthenticatedApproval{Actor: "synthetic-reviewer", Authority: documentproduction.ApprovalAuthority{
		Contract: documentproduction.ApprovalAuthorityContractV1,
		Kind:     documentproduction.ApprovalAuthorityAuthenticatedHuman, PrincipalID: "synthetic-principal",
		AuthenticationMethod: "synthetic-authentication", AuthenticatedAt: "2026-09-22T15:00:00Z",
		EvidenceSHA256: gateSHA("synthetic approval authority"),
	}}, time.Date(2026, 9, 22, 15, 30, 0, 0, time.UTC))
	require.NoError(t, err)
	evaluation, err := EvaluateRequiredApproval(base.Policy, prepared.ApprovalSubject, &record.Grant, nil, request.PreparedAt)
	require.NoError(t, err)
	base.ApprovalSubject = &prepared.ApprovalSubject
	base.ApprovalGrant = &record.Grant
	base.ApprovalEvaluation = &evaluation

	for name, mutate := range map[string]func(*StoredProductionInputs){
		"current approval passes": func(*StoredProductionInputs) {},
		"stale subject fails": func(value *StoredProductionInputs) {
			subject := *value.ApprovalSubject
			subject.Members = slices.Clone(subject.Members)
			subject.Members[0].SourceVersionID = "91919191-9191-4191-8191-919191919191"
			value.ApprovalSubject = &subject
		},
		"stale evaluation fails": func(value *StoredProductionInputs) {
			changed := *value.ApprovalEvaluation
			changed.EvaluatedAt = "2026-09-22T15:59:00Z"
			changed.SHA256 = ""
			_, changed.SHA256, _ = documentproduction.CanonicalApprovalEvaluation(changed)
			value.ApprovalEvaluation = &changed
		},
	} {
		t.Run(name, func(t *testing.T) {
			stored := cloneStoredProductionInputs(base)
			mutate(&stored)
			authority, err := RunPreparedInputGates(t.Context(), &gateTestStore{stored: stored}, syntheticGateRequest(t, stored))
			if name == "current approval passes" {
				require.NoError(t, err)
				require.NotNil(t, authority.Receipt)
				return
			}
			require.Error(t, err)
			require.Nil(t, authority.Receipt)
			require.Equal(t, documentproduction.GateStateFail, gateCheck(t, authority, documentproduction.GateCheckApproval).State)
		})
	}
}

func TestPreparedInputBindingFailuresReportSourceStale(t *testing.T) {
	for name, mutate := range map[string]func(*StoredProductionInputs){
		"membership": func(value *StoredProductionInputs) { value.Draft.MemberHash = gateSHA("wrong membership") },
		"decisions":  func(value *StoredProductionInputs) { value.Draft.DecisionsSHA256 = gateSHA("wrong decisions") },
		"reviews":    func(value *StoredProductionInputs) { value.Members[0].Member.ReviewBinding = gateSHA("wrong review") },
	} {
		t.Run(name, func(t *testing.T) {
			stored := syntheticStoredProductionInputs(t)
			mutate(&stored)
			_, err := RunPreparedInputGates(t.Context(), &gateTestStore{stored: stored}, syntheticGateRequest(t, stored))
			requireProductionGateCode(t, err, documentproduction.ProblemSourceStale)
		})
	}
}

func TestPreparedInputWrongReceiptAndChangedOperationPayloadConsumeNoNumber(t *testing.T) {
	stored := syntheticStoredProductionInputs(t)
	store := &gateTestStore{stored: stored}
	request := syntheticGateRequest(t, stored)
	authority, err := RunPreparedInputGates(t.Context(), store, request)
	require.NoError(t, err)

	reserved := 0
	err = ReserveAfterPreparedInput(authority, PreparedInputReference{
		OperationID: request.OperationID, PreparedSHA256: authority.Prepared.SHA256,
		ReceiptSHA256: gateSHA("wrong receipt"),
	}, func(documentproduction.PreparedInputAuthority) error { reserved++; return nil })
	require.Error(t, err)
	require.Zero(t, reserved)

	changed := request
	changed.PreparedAt = changed.PreparedAt.Add(time.Second)
	_, err = RunPreparedInputGates(t.Context(), store, changed)
	requireProductionGateCode(t, err, documentproduction.ProblemChangedPayload)
	require.Zero(t, reserved)
}

type gateTestStore struct {
	stored      StoredProductionInputs
	authority   *documentproduction.PreparedInputAuthority
	requestHash string
}

func (s *gateTestStore) RunProductionGates(_ context.Context, request PreparedInputRequest, build PreparedInputBuilder) (documentproduction.PreparedInputAuthority, error) {
	requestHash, err := PreparedInputRequestSHA256(request)
	if err != nil {
		return documentproduction.PreparedInputAuthority{}, err
	}
	if s.authority != nil {
		if requestHash != s.requestHash {
			return documentproduction.PreparedInputAuthority{}, &documentproduction.Problem{Code: documentproduction.ProblemChangedPayload, Detail: "operation payload changed"}
		}
		return *s.authority, nil
	}
	authority, err := build(cloneStoredProductionInputs(s.stored))
	if err != nil {
		return documentproduction.PreparedInputAuthority{}, err
	}
	s.requestHash = requestHash
	s.authority = &authority
	return authority, nil
}

func syntheticStoredProductionInputs(t *testing.T) StoredProductionInputs {
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
	_, decisionSHA, err := redaction.CanonicalDecisions([]redaction.Decision{decision})
	require.NoError(t, err)
	member := redaction.Member{
		ID: decision.MemberID, VaultID: "33333333-3333-4333-8333-333333333333",
		SourceVersionID: "44444444-4444-4444-8444-444444444444", SourceSHA256: gateSHA("source"), SourceSize: 1,
		PDFSHA256: m.PDFSHA256, PDFSize: 1, NodeID: 1, Ordinal: 1,
		Family: redaction.FamilyContext{Kind: "standalone", RootVersionID: "44444444-4444-4444-8444-444444444444"},
		Mode:   "keep_selected", MapSHA256: m.SHA256, PageInventorySHA256: gateSHA("inventory"),
	}
	stored := StoredProductionInputs{
		Draft: redaction.Draft{
			SetID: "55555555-5555-4555-8555-555555555555", Revision: 1, ETag: 3,
			InstructionsSHA256: gateSHA("instructions"), DecisionsSHA256: decisionSHA,
			RecipeID: redaction.DefaultRecipeID, RecipeSHA256: resolved.RecipeSHA256,
			ProfileID: redaction.DefaultOutputProfileID, ProfileSHA256: gateSHA("profile"),
			DisclosureProfileID: redaction.DefaultDisclosureProfileID, DisclosureProfileSHA256: gateSHA("disclosure"),
			NumberingRecipeID: redaction.BatesNumberingRecipeID, NumberingRecipeSHA256: gateSHA("numbering"),
			Policy: redaction.PolicySelection{PolicyID: policy.ID, Version: policy.Version, PolicySHA256: policy.SHA256},
			State:  "draft", MembershipSealed: true,
		},
		Policy: policy,
		Members: []StoredPreparedMember{{Member: member, Decisions: []redaction.Decision{decision}, Resolved: resolved,
			Facts: documentproduction.PolicyMemberFacts{MemberID: member.ID, Fields: map[string][]string{}, Labels: []string{}}}},
	}
	_, stored.Draft.MemberHash, err = storedProductionMemberHash(stored.Members)
	require.NoError(t, err)
	review, err := redaction.ReviewBinding(reviewInputForStored(stored, stored.Members[0], decisionSHA, resolved.SHA256))
	require.NoError(t, err)
	stored.Members[0].Member.Reviewed = true
	stored.Members[0].Member.ReviewBinding = review
	return stored
}

func syntheticGateRequest(t *testing.T, stored StoredProductionInputs) PreparedInputRequest {
	t.Helper()
	revisionSHA, err := ProductionRevisionSHA256(stored)
	require.NoError(t, err)
	return PreparedInputRequest{
		OperationID: "66666666-6666-4666-8666-666666666666",
		ReceiptID:   "77777777-7777-4777-8777-777777777777",
		SetID:       stored.Draft.SetID, Revision: stored.Draft.Revision, ExpectedETag: stored.Draft.ETag,
		ExpectedRevisionSHA256: revisionSHA, PreparedAt: time.Date(2026, 9, 22, 16, 0, 0, 0, time.UTC),
	}
}

func gatePolicy(t *testing.T, rules ...documentproduction.PolicyRule) documentproduction.PolicyVersion {
	t.Helper()
	policy := documentproduction.PolicyVersion{
		Contract: documentproduction.PolicyContractV1, ID: "88888888-8888-4888-8888-888888888888",
		Version: 1, Name: "Synthetic gate policy", CreatedAt: "2026-09-22T15:00:00Z",
		Rules: rules, ConflictMode: documentproduction.PolicyConflictReject,
	}
	return resealGatePolicy(t, policy)
}

func resealGatePolicy(t *testing.T, policy documentproduction.PolicyVersion) documentproduction.PolicyVersion {
	t.Helper()
	policy.SHA256 = ""
	_, policy.SHA256, _ = documentproduction.CanonicalPolicyVersion(policy)
	require.NoError(t, documentproduction.ValidatePolicyVersion(policy))
	return policy
}

func cloneStoredProductionInputs(value StoredProductionInputs) StoredProductionInputs {
	value.Members = slices.Clone(value.Members)
	for index := range value.Members {
		value.Members[index].Decisions = slices.Clone(value.Members[index].Decisions)
		value.Members[index].Facts.Labels = slices.Clone(value.Members[index].Facts.Labels)
		value.Members[index].Facts.Fields = cloneGateFields(value.Members[index].Facts.Fields)
	}
	value.ApprovalEvents = slices.Clone(value.ApprovalEvents)
	value.Withheld = cloneWithheld(value.Withheld)
	value.PrivilegeLog = clonePrivilegeReceipt(value.PrivilegeLog)
	value.ApprovalEvaluation = cloneApprovalEvaluation(value.ApprovalEvaluation)
	if value.ApprovalSubject != nil {
		cloned := *value.ApprovalSubject
		cloned.Members = slices.Clone(value.ApprovalSubject.Members)
		value.ApprovalSubject = &cloned
	}
	if value.ApprovalGrant != nil {
		cloned := *value.ApprovalGrant
		value.ApprovalGrant = &cloned
	}
	return value
}

func syntheticWithheldSelection(t *testing.T, stored StoredProductionInputs) *documentproduction.WithheldSelection {
	t.Helper()
	member := stored.Members[0].Member
	selection := &documentproduction.WithheldSelection{
		Contract: documentproduction.WithheldSelectionContractV1,
		ID:       "78787878-7878-4878-8878-787878787878", SetID: stored.Draft.SetID,
		Revision: stored.Draft.Revision, PolicySHA256: stored.Policy.SHA256,
		Members: []documentproduction.WithheldMember{{ID: member.ID, Ordinal: member.Ordinal,
			SourceVersionID: member.SourceVersionID, SourceSHA256: member.SourceSHA256,
			SourceSize: member.SourceSize, FamilyOrder: 1, Family: member.Family}},
	}
	resealWithheldSelection(t, selection)
	return selection
}

func resealWithheldSelection(t *testing.T, selection *documentproduction.WithheldSelection) {
	t.Helper()
	selection.SHA256 = ""
	_, selection.SHA256, _ = documentproduction.CanonicalWithheldSelection(*selection)
	require.NoError(t, documentproduction.ValidateWithheldSelection(*selection))
}

func cloneGateFields(values map[string][]string) map[string][]string {
	cloned := make(map[string][]string, len(values))
	for key, value := range values {
		cloned[key] = slices.Clone(value)
	}
	return cloned
}

func gateCheck(t *testing.T, authority documentproduction.PreparedInputAuthority, id string) documentproduction.GateCheck {
	t.Helper()
	for _, check := range authority.GateResults.Checks {
		if check.ID == id {
			return check
		}
	}
	t.Fatalf("missing gate check %s", id)
	return documentproduction.GateCheck{}
}

func gateSHA(value string) string {
	digest, _ := canonicalDigest(value)
	return digest
}

func requireProductionGateCode(t *testing.T, err error, code documentproduction.ProblemCode) {
	t.Helper()
	var problem *documentproduction.Problem
	require.ErrorAs(t, err, &problem)
	require.Equal(t, code, problem.Code)
}
