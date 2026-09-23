package production

import (
	"context"
	"errors"
	"slices"
	"time"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
)

// StoredPreparedMember is loaded from storage inside the gate transaction.
// Decisions is authoritative even when it is an explicitly empty slice.
type StoredPreparedMember struct {
	Member      redaction.Member
	Decisions   []redaction.Decision
	Resolved    redaction.Resolved
	Facts       documentproduction.PolicyMemberFacts
	EvidencePin *documentproduction.ProductionMemberEvidencePin
}

// StoredProductionInputs is the complete stored authority required by every
// pre-numbering gate. Callers never pass this value to RunPreparedInputGates.
type StoredProductionInputs struct {
	Draft              redaction.Draft
	Policy             documentproduction.PolicyVersion
	Members            []StoredPreparedMember
	Withheld           *documentproduction.WithheldSelection
	PrivilegeLog       *documentproduction.PrivilegeLogReceipt
	ApprovalSubject    *documentproduction.ApprovalSubject
	ApprovalGrant      *documentproduction.ApprovalGrant
	ApprovalEvents     []documentproduction.ApprovalEvent
	ApprovalEvaluation *documentproduction.ApprovalEvaluation
}

type PreparedInputRequest struct {
	OperationID            string
	ReceiptID              string
	SetID                  string
	Revision               int64
	ExpectedETag           int64
	ExpectedRevisionSHA256 string
	PreparedAt             time.Time
}

type PreparedInputBuilder func(StoredProductionInputs) (documentproduction.PreparedInputAuthority, error)

// PreparedInputStore must load StoredProductionInputs and persist the builder
// result in one writer transaction. A callback error commits nothing. Exact
// operation retries return the original authority; changed payloads fail.
type PreparedInputStore interface {
	RunProductionGates(ctx context.Context, request PreparedInputRequest, build PreparedInputBuilder) (documentproduction.PreparedInputAuthority, error)
}

type PreparedInputReference struct {
	OperationID    string
	PreparedSHA256 string
	ReceiptSHA256  string
}

// RunPreparedInputGates seals only store-loaded inputs. Failed policy gates
// are returned after the store has persisted their complete evidence and audit.
func RunPreparedInputGates(ctx context.Context, store PreparedInputStore, request PreparedInputRequest) (documentproduction.PreparedInputAuthority, error) {
	if store == nil || !canonicalUUIDv4(request.OperationID) || !canonicalUUIDv4(request.ReceiptID) ||
		!canonicalUUIDv4(request.SetID) || request.Revision < 1 || request.ExpectedETag < 1 ||
		!canonicalSHA256(request.ExpectedRevisionSHA256) || !canonicalUTCTime(request.PreparedAt) {
		return documentproduction.PreparedInputAuthority{}, invalidProductionContract("invalid prepared-input request")
	}
	requestSHA256, err := PreparedInputRequestSHA256(request)
	if err != nil {
		return documentproduction.PreparedInputAuthority{}, err
	}
	authority, err := store.RunProductionGates(ctx, request, func(stored StoredProductionInputs) (documentproduction.PreparedInputAuthority, error) {
		return buildPreparedInputAuthority(stored, request, requestSHA256)
	})
	if err != nil {
		return documentproduction.PreparedInputAuthority{}, err
	}
	if err := documentproduction.ValidatePreparedInputAuthority(authority); err != nil {
		return documentproduction.PreparedInputAuthority{}, err
	}
	if !documentproduction.GateResultsPassed(authority.GateResults) {
		return authority, preparedInputGateProblem(authority)
	}
	return authority, nil
}

func PreparedInputRequestSHA256(request PreparedInputRequest) (string, error) {
	return semanticDigest(struct {
		ReceiptID              string `json:"receipt_id"`
		SetID                  string `json:"set_id"`
		Revision               int64  `json:"revision"`
		ExpectedETag           int64  `json:"expected_etag"`
		ExpectedRevisionSHA256 string `json:"expected_revision_sha256"`
		PreparedAt             string `json:"prepared_at"`
	}{request.ReceiptID, request.SetID, request.Revision, request.ExpectedETag,
		request.ExpectedRevisionSHA256, request.PreparedAt.Format(time.RFC3339Nano)})
}

// ProductionRevisionSHA256 is the observation token returned before a gate
// run. It includes empty decision sets and every conditional authority.
func ProductionRevisionSHA256(stored StoredProductionInputs) (string, error) {
	normalized := cloneStoredInputs(stored)
	slices.SortFunc(normalized.Members, func(left, right StoredPreparedMember) int {
		if left.Member.Ordinal < right.Member.Ordinal {
			return -1
		}
		if left.Member.Ordinal > right.Member.Ordinal {
			return 1
		}
		return compareStrings(left.Member.ID, right.Member.ID)
	})
	return semanticDigest(normalized)
}

// StoredApprovalSubject derives the exact subject that a selected grant must
// cover from Store-loaded authority. It does not evaluate or record a gate.
func StoredApprovalSubject(stored StoredProductionInputs) (documentproduction.ApprovalSubject, error) {
	prepared, _, err := prepareStoredProduction(stored, time.Unix(0, 0).UTC())
	if err != nil {
		return documentproduction.ApprovalSubject{}, err
	}
	return prepared.ApprovalSubject, nil
}

// ReserveAfterPreparedInput is the sole gate-to-numbering handoff. The
// reservation callback is never invoked for a failed, stale or wrong receipt.
func ReserveAfterPreparedInput(authority documentproduction.PreparedInputAuthority, reference PreparedInputReference, reserve func(documentproduction.PreparedInputAuthority) error) error {
	if reserve == nil || !canonicalUUIDv4(reference.OperationID) || !canonicalSHA256(reference.PreparedSHA256) ||
		!canonicalSHA256(reference.ReceiptSHA256) {
		return invalidProductionContract("invalid prepared-input numbering reference")
	}
	if err := documentproduction.ValidatePreparedInputAuthority(authority); err != nil {
		return err
	}
	if authority.Receipt == nil || !documentproduction.GateResultsPassed(authority.GateResults) ||
		authority.Audit.OperationID != reference.OperationID || authority.Prepared.SHA256 != reference.PreparedSHA256 ||
		authority.Receipt.SHA256 != reference.ReceiptSHA256 || authority.Audit.PreparedInputSHA256 != reference.ReceiptSHA256 {
		return &documentproduction.Problem{Code: documentproduction.ProblemChangedPayload,
			Detail: "prepared-input receipt does not match numbering request"}
	}
	return reserve(authority)
}

func buildPreparedInputAuthority(stored StoredProductionInputs, request PreparedInputRequest, requestSHA256 string) (documentproduction.PreparedInputAuthority, error) {
	prepared, facts, err := prepareStoredProduction(stored, request.PreparedAt)
	if err != nil {
		return documentproduction.PreparedInputAuthority{}, err
	}
	currentRevisionSHA256, err := ProductionRevisionSHA256(stored)
	if err != nil {
		return documentproduction.PreparedInputAuthority{}, err
	}
	checks := newGateChecks()
	revisionOK := stored.Draft.SetID == request.SetID && stored.Draft.Revision == request.Revision &&
		stored.Draft.ETag == request.ExpectedETag && currentRevisionSHA256 == request.ExpectedRevisionSHA256
	setGateCheck(&checks, documentproduction.GateCheckRevision, revisionOK,
		documentproduction.GateEvidence{Kind: "revision_snapshot", SubjectID: stored.Draft.SetID,
			SHA256: currentRevisionSHA256, Count: stored.Draft.ETag})

	membershipOK := stored.Draft.MembershipSealed && stored.Draft.MemberHash == prepared.MemberHash &&
		stored.Draft.DecisionsSHA256 == prepared.DecisionsSHA256 && len(prepared.Members) != 0
	setGateCheck(&checks, documentproduction.GateCheckMembership, membershipOK,
		documentproduction.GateEvidence{Kind: "ordered_occurrences", SHA256: prepared.MemberHash, Count: int64(len(prepared.Members))})

	sourcesOK, renditionsOK, framesOK := validatePreparedMemberInputs(prepared)
	setGateCheck(&checks, documentproduction.GateCheckSources, sourcesOK,
		documentproduction.GateEvidence{Kind: "source_versions", SHA256: prepared.MemberHash, Count: int64(len(prepared.Members))})
	setGateCheck(&checks, documentproduction.GateCheckRenditions, renditionsOK,
		documentproduction.GateEvidence{Kind: "renditions", SHA256: prepared.MemberHash, Count: int64(len(prepared.Members))})
	frameCount := int64(0)
	for _, member := range prepared.Members {
		frameCount += int64(len(member.Frames))
	}
	setGateCheck(&checks, documentproduction.GateCheckFrames, framesOK,
		documentproduction.GateEvidence{Kind: "page_frames", SHA256: prepared.MemberHash, Count: frameCount})

	decisionOK, uncertainty := validatePreparedDecisions(prepared)
	setGateCheck(&checks, documentproduction.GateCheckDecisions, decisionOK,
		documentproduction.GateEvidence{Kind: "decision_snapshot", SHA256: prepared.DecisionsSHA256,
			Count: int64(len(uncertainty)), IDs: boundedGateIDs(uncertainty)})

	reviewsOK := validatePreparedReviews(stored, prepared)
	setGateCheck(&checks, documentproduction.GateCheckReviews, reviewsOK,
		documentproduction.GateEvidence{Kind: "review_bindings", SHA256: prepared.MemberHash, Count: int64(len(prepared.Members))})

	policyEvaluation, policyErr := documentproduction.EvaluatePolicy(stored.Policy, facts)
	policyOK := policyErr == nil && completePolicyEvaluation(policyEvaluation, len(prepared.Members))
	policyEvidence := documentproduction.GateEvidence{Kind: "policy_evaluation", SHA256: stored.Policy.SHA256,
		Count: int64(len(prepared.Members)), IDs: gateProblemIDs(policyErr)}
	setGateCheck(&checks, documentproduction.GateCheckPolicy, policyOK, policyEvidence)
	if policyOK {
		for index := range prepared.Members {
			prepared.Members[index].Disposition = policyEvaluation.Members[index].Disposition
		}
	}

	withheldRequired := policyOK && policyHasWithheld(policyEvaluation)
	withheldOK := policyOK && validateWithheldGate(prepared, policyEvaluation)
	withheldState := documentproduction.GateStateNotApplicable
	if withheldRequired || stored.Withheld != nil {
		withheldState = gateState(withheldOK)
	}
	setGateCheckState(&checks, documentproduction.GateCheckWithheld, withheldState, withheldRequired,
		documentproduction.GateEvidence{Kind: "withheld_selection", SHA256: optionalWithheldDigest(stored.Withheld),
			Count: int64(withheldCount(stored.Withheld))})

	privilegeRequired := stored.Policy.PrivilegeLog.Required || stored.Policy.PrivilegeLog.RequireFrozenReceipt
	privilegeErr := validatePrivilegeGate(prepared, privilegeRequired)
	privilegeState := documentproduction.GateStateNotApplicable
	if privilegeRequired || stored.PrivilegeLog != nil {
		privilegeState = gateState(privilegeErr == nil)
	}
	setGateCheckState(&checks, documentproduction.GateCheckPrivilegeLog, privilegeState, privilegeRequired,
		documentproduction.GateEvidence{Kind: "privilege_log", SHA256: optionalPrivilegeDigest(stored.PrivilegeLog)})

	approvalRequired := stored.Policy.Approval.Required
	approvalErr := validatePreparedApproval(stored, prepared.ApprovalSubject, request.PreparedAt)
	approvalState := documentproduction.GateStateNotApplicable
	if approvalRequired || stored.ApprovalGrant != nil || stored.ApprovalEvaluation != nil {
		approvalState = gateState(approvalErr == nil)
	}
	setGateCheckState(&checks, documentproduction.GateCheckApproval, approvalState, approvalRequired,
		documentproduction.GateEvidence{Kind: "approval_evaluation", SHA256: optionalApprovalDigest(stored.ApprovalEvaluation)})

	prepared.ApprovalEvaluation = cloneApprovalEvaluation(stored.ApprovalEvaluation)
	_, prepared.SHA256, err = documentproduction.CanonicalPreparedProduction(prepared)
	if err != nil {
		return documentproduction.PreparedInputAuthority{}, err
	}
	results := documentproduction.GateResults{
		Contract: documentproduction.GateResultsContractV1, PreparedSHA256: prepared.SHA256,
		EvaluatedAt: request.PreparedAt.Format(time.RFC3339Nano), Checks: checks,
	}
	_, results.SHA256, err = documentproduction.CanonicalGateResults(results)
	if err != nil {
		return documentproduction.PreparedInputAuthority{}, err
	}

	var receipt *documentproduction.PreparedInputReceipt
	if documentproduction.GateResultsPassed(results) {
		subjectSHA256, subjectErr := canonicalApprovalSubjectDigest(prepared.ApprovalSubject)
		if subjectErr != nil {
			return documentproduction.PreparedInputAuthority{}, subjectErr
		}
		value := documentproduction.PreparedInputReceipt{
			Contract: documentproduction.PreparedInputContractV1, ID: request.ReceiptID,
			ApprovalSubjectSHA256: subjectSHA256, ApprovalEvaluationSHA256: optionalApprovalDigest(stored.ApprovalEvaluation),
			WithheldSelectionSHA256:   optionalWithheldDigest(stored.Withheld),
			PrivilegeLogReceiptSHA256: optionalPrivilegeDigest(stored.PrivilegeLog),
			GateResultsSHA256:         results.SHA256, CreatedAt: request.PreparedAt.Format(time.RFC3339Nano),
		}
		_, value.SHA256, err = documentproduction.CanonicalPreparedInputReceipt(value)
		if err != nil {
			return documentproduction.PreparedInputAuthority{}, err
		}
		receipt = &value
	}
	audit := documentproduction.GateAudit{
		Contract: documentproduction.GateAuditContractV1, OperationID: request.OperationID,
		RequestSHA256: requestSHA256, PreparedSHA256: prepared.SHA256, GateResultsSHA256: results.SHA256,
		RecordedAt: request.PreparedAt.Format(time.RFC3339Nano),
	}
	if receipt != nil {
		audit.PreparedInputSHA256 = receipt.SHA256
	}
	_, audit.SHA256, err = documentproduction.CanonicalGateAudit(audit)
	if err != nil {
		return documentproduction.PreparedInputAuthority{}, err
	}
	authority := documentproduction.PreparedInputAuthority{Prepared: prepared, GateResults: results, Receipt: receipt, Audit: audit}
	return authority, documentproduction.ValidatePreparedInputAuthority(authority)
}

func prepareStoredProduction(stored StoredProductionInputs, preparedAt time.Time) (documentproduction.PreparedProduction, []documentproduction.PolicyMemberFacts, error) {
	if redaction.ValidateDraft(stored.Draft) != nil || documentproduction.ValidatePolicyVersion(stored.Policy) != nil ||
		stored.Draft.Policy.PolicyID != stored.Policy.ID || stored.Draft.Policy.Version != stored.Policy.Version ||
		stored.Draft.Policy.PolicySHA256 != stored.Policy.SHA256 || len(stored.Members) == 0 {
		return documentproduction.PreparedProduction{}, nil, invalidProductionContract("invalid stored production authority")
	}
	members := slices.Clone(stored.Members)
	slices.SortFunc(members, func(left, right StoredPreparedMember) int {
		if left.Member.Ordinal < right.Member.Ordinal {
			return -1
		}
		if left.Member.Ordinal > right.Member.Ordinal {
			return 1
		}
		return compareStrings(left.Member.ID, right.Member.ID)
	})
	preparedMembers := make([]documentproduction.PreparedMember, len(members))
	facts := make([]documentproduction.PolicyMemberFacts, len(members))
	approvalMembers := make([]documentproduction.ApprovalMember, len(members))
	allDecisions := make([]redaction.Decision, 0)
	hasEvidencePins := false
	for index, storedMember := range members {
		decisions := slices.Clone(storedMember.Decisions)
		_, decisionsSHA256, err := redaction.CanonicalDecisions(decisions)
		if err != nil {
			return documentproduction.PreparedProduction{}, nil, err
		}
		_, resolvedSHA256, err := redaction.CanonicalResolved(storedMember.Resolved)
		if err != nil || storedMember.Resolved.SHA256 != resolvedSHA256 {
			return documentproduction.PreparedProduction{}, nil, invalidProductionContract("stored resolved plan digest is invalid")
		}
		frames := make([]documentproduction.PreparedFrame, len(storedMember.Resolved.Pages))
		for pageIndex, page := range storedMember.Resolved.Pages {
			frames[pageIndex] = documentproduction.PreparedFrame{Page: page.Number, SHA256: page.FrameSHA256, Width: page.Width, Height: page.Height}
		}
		var evidencePin *documentproduction.ProductionMemberEvidencePin
		if storedMember.EvidencePin != nil {
			pinCopy := *storedMember.EvidencePin
			evidencePin = &pinCopy
			hasEvidencePins = true
		}
		preparedMembers[index] = documentproduction.PreparedMember{
			Member: storedMember.Member, Decisions: decisions, Resolved: storedMember.Resolved,
			Frames: frames, Facts: storedMember.Facts, EvidencePin: evidencePin, DecisionsSHA256: decisionsSHA256,
			ResolvedSHA256: resolvedSHA256, ReviewBinding: storedMember.Member.ReviewBinding,
		}
		facts[index] = storedMember.Facts
		approvalMembers[index] = documentproduction.ApprovalMember{
			MemberID: storedMember.Member.ID, Ordinal: storedMember.Member.Ordinal,
			SourceVersionID: storedMember.Member.SourceVersionID, SourceSHA256: storedMember.Member.SourceSHA256,
			SourceSize: storedMember.Member.SourceSize, PDFSHA256: storedMember.Member.PDFSHA256,
			PageInventorySHA256: storedMember.Member.PageInventorySHA256, MapSHA256: storedMember.Member.MapSHA256,
			DecisionsSHA256: decisionsSHA256, ResolvedSHA256: resolvedSHA256,
		}
		allDecisions = append(allDecisions, decisions...)
	}
	_, memberHash, err := documentproduction.PreparedMemberHash(preparedMembers)
	if err != nil {
		return documentproduction.PreparedProduction{}, nil, err
	}
	_, decisionsSHA256, err := redaction.CanonicalDecisions(allDecisions)
	if err != nil {
		return documentproduction.PreparedProduction{}, nil, err
	}
	subjectContract := documentproduction.ApprovalSubjectContractV1
	preparedContract := documentproduction.PreparedProductionContractV1
	var gateEvidenceSHA256 string
	if hasEvidencePins {
		_, gateEvidenceSHA256, err = documentproduction.CanonicalProductionGateEvidence(preparedMembers)
		if err != nil {
			return documentproduction.PreparedProduction{}, nil, err
		}
		subjectContract = documentproduction.ApprovalSubjectContractV2
		preparedContract = documentproduction.PreparedProductionContractV2
	}
	subject := documentproduction.ApprovalSubject{
		Contract: subjectContract, SetID: stored.Draft.SetID,
		Revision: stored.Draft.Revision, Members: approvalMembers,
		InstructionsSHA256: stored.Draft.InstructionsSHA256, RecipeSHA256: stored.Draft.RecipeSHA256,
		OutputProfileSHA256:     stored.Draft.ProfileSHA256,
		DisclosureProfileSHA256: stored.Draft.DisclosureProfileSHA256,
		NumberingPolicySHA256:   stored.Draft.NumberingRecipeSHA256, Policy: stored.Draft.Policy,
		GateEvidenceSHA256:       gateEvidenceSHA256,
		WithheldSelectionSHA256:  optionalWithheldDigest(stored.Withheld),
		PrivilegeLogInputsSHA256: optionalPrivilegeInputsDigest(stored.PrivilegeLog),
	}
	prepared := documentproduction.PreparedProduction{
		Contract: preparedContract, SetID: stored.Draft.SetID,
		Revision: stored.Draft.Revision, ETag: stored.Draft.ETag, MembershipSealed: stored.Draft.MembershipSealed,
		InstructionsSHA256: stored.Draft.InstructionsSHA256, MemberHash: memberHash, DecisionsSHA256: decisionsSHA256,
		RecipeSHA256: stored.Draft.RecipeSHA256, OutputProfileSHA256: stored.Draft.ProfileSHA256,
		DisclosureProfileSHA256: stored.Draft.DisclosureProfileSHA256,
		NumberingPolicySHA256:   stored.Draft.NumberingRecipeSHA256, Policy: stored.Policy,
		Members: preparedMembers, WithheldSelection: cloneWithheld(stored.Withheld),
		PrivilegeLogReceipt: clonePrivilegeReceipt(stored.PrivilegeLog), ApprovalSubject: subject,
		ApprovalEvaluation: cloneApprovalEvaluation(stored.ApprovalEvaluation),
		PreparedAt:         preparedAt.Format(time.RFC3339Nano),
	}
	return prepared, facts, nil
}

func validatePreparedMemberInputs(prepared documentproduction.PreparedProduction) (bool, bool, bool) {
	sources, renditions, frames := true, true, true
	for _, member := range prepared.Members {
		if redaction.ValidateMember(member.Member) != nil || !canonicalUUIDv4(member.Member.SourceVersionID) ||
			!canonicalSHA256(member.Member.SourceSHA256) || member.Member.SourceSize < 0 {
			sources = false
		}
		if !canonicalSHA256(member.Member.PDFSHA256) || member.Member.PDFSize < 1 ||
			!canonicalSHA256(member.Member.MapSHA256) || !canonicalSHA256(member.Member.PageInventorySHA256) ||
			member.Resolved.MapSHA256 != member.Member.MapSHA256 {
			renditions = false
		}
		if len(member.Frames) == 0 || len(member.Frames) != len(member.Resolved.Pages) {
			frames = false
			continue
		}
		for index, frame := range member.Frames {
			page := member.Resolved.Pages[index]
			if frame.Page != index+1 || frame.Page != page.Number || frame.SHA256 != page.FrameSHA256 ||
				frame.Width != page.Width || frame.Height != page.Height || frame.Width < 1 || frame.Height < 1 {
				frames = false
			}
		}
	}
	return sources, renditions, frames
}

func validatePreparedDecisions(prepared documentproduction.PreparedProduction) (bool, []string) {
	all := make([]redaction.Decision, 0)
	uncertainty := make([]string, 0)
	for _, member := range prepared.Members {
		_, digest, err := redaction.CanonicalDecisions(member.Decisions)
		if err != nil || digest != member.DecisionsSHA256 {
			return false, uncertainty
		}
		for _, decision := range member.Decisions {
			if decision.MemberID != member.Member.ID || decision.Selector.MapSHA256 != member.Member.MapSHA256 {
				return false, uncertainty
			}
		}
		uncertainty = append(uncertainty, member.Resolved.UncertainDecisionIDs...)
		all = append(all, member.Decisions...)
	}
	_, digest, err := redaction.CanonicalDecisions(all)
	return err == nil && digest == prepared.DecisionsSHA256, uncertainty
}

func validatePreparedReviews(stored StoredProductionInputs, prepared documentproduction.PreparedProduction) bool {
	for index, member := range prepared.Members {
		binding, err := redaction.ReviewBinding(reviewInputForPrepared(stored.Draft, prepared, member))
		if err != nil || !member.Member.Reviewed || member.Member.ReviewBinding != binding || member.ReviewBinding != binding ||
			member.Member.Ordinal != int64(index+1) {
			return false
		}
	}
	return true
}

func reviewInputForPrepared(draft redaction.Draft, prepared documentproduction.PreparedProduction, member documentproduction.PreparedMember) redaction.ReviewInput {
	return redaction.ReviewInput{
		SetID: draft.SetID, MemberID: member.Member.ID, VaultID: member.Member.VaultID,
		SourceVersionID: member.Member.SourceVersionID, Revision: draft.Revision,
		Ordinal: member.Member.Ordinal, NodeID: member.Member.NodeID, SourceSize: member.Member.SourceSize,
		PDFSize: member.Member.PDFSize, SourceSHA256: member.Member.SourceSHA256, PDFSHA256: member.Member.PDFSHA256,
		PageInventorySHA256: member.Member.PageInventorySHA256, MapSHA256: member.Member.MapSHA256,
		Mode: member.Member.Mode, MemberHash: prepared.MemberHash, InstructionsSHA256: draft.InstructionsSHA256,
		RecipeSHA256: draft.RecipeSHA256, DecisionsSHA256: member.DecisionsSHA256, ResolvedSHA256: member.ResolvedSHA256,
	}
}

func reviewInputForStored(stored StoredProductionInputs, member StoredPreparedMember, decisionsSHA256, resolvedSHA256 string) redaction.ReviewInput {
	return redaction.ReviewInput{
		SetID: stored.Draft.SetID, MemberID: member.Member.ID, VaultID: member.Member.VaultID,
		SourceVersionID: member.Member.SourceVersionID, Revision: stored.Draft.Revision,
		Ordinal: member.Member.Ordinal, NodeID: member.Member.NodeID, SourceSize: member.Member.SourceSize,
		PDFSize: member.Member.PDFSize, SourceSHA256: member.Member.SourceSHA256, PDFSHA256: member.Member.PDFSHA256,
		PageInventorySHA256: member.Member.PageInventorySHA256, MapSHA256: member.Member.MapSHA256,
		Mode: member.Member.Mode, MemberHash: stored.Draft.MemberHash, InstructionsSHA256: stored.Draft.InstructionsSHA256,
		RecipeSHA256: stored.Draft.RecipeSHA256, DecisionsSHA256: decisionsSHA256, ResolvedSHA256: resolvedSHA256,
	}
}

func validateWithheldGate(prepared documentproduction.PreparedProduction, evaluation documentproduction.PolicyEvaluation) bool {
	expected := make(map[string]documentproduction.PreparedMember)
	for index, result := range evaluation.Members {
		if result.Disposition == documentproduction.PolicyDispositionWithhold {
			expected[result.MemberID] = prepared.Members[index]
		}
	}
	if len(expected) == 0 {
		return prepared.WithheldSelection == nil
	}
	if prepared.WithheldSelection == nil || prepared.WithheldSelection.SetID != prepared.SetID ||
		prepared.WithheldSelection.Revision != prepared.Revision || prepared.WithheldSelection.PolicySHA256 != prepared.Policy.SHA256 ||
		len(prepared.WithheldSelection.Members) != len(expected) {
		return false
	}
	for _, withheld := range prepared.WithheldSelection.Members {
		member, ok := expected[withheld.ID]
		if !ok || withheld.Ordinal != member.Member.Ordinal || withheld.SourceVersionID != member.Member.SourceVersionID ||
			withheld.SourceSHA256 != member.Member.SourceSHA256 || withheld.SourceSize != member.Member.SourceSize ||
			withheld.Family != member.Member.Family {
			return false
		}
		delete(expected, withheld.ID)
	}
	return len(expected) == 0
}

func validatePrivilegeGate(prepared documentproduction.PreparedProduction, required bool) error {
	if !required && prepared.PrivilegeLogReceipt == nil {
		return nil
	}
	withheldSHA256 := optionalWithheldDigest(prepared.WithheldSelection)
	playersSHA256, frozenApprovalSHA256, inputsSHA256 := "", "", prepared.ApprovalSubject.PrivilegeLogInputsSHA256
	if prepared.PrivilegeLogReceipt != nil {
		playersSHA256 = prepared.PrivilegeLogReceipt.PlayersSHA256
		// The receipt binds the evaluation made at freeze. Admission evaluates
		// the current grant and events separately at PreparedAt.
		frozenApprovalSHA256 = prepared.PrivilegeLogReceipt.ApprovalEvaluationSHA256
	}
	return documentproduction.PrivilegeLogGateProblem(required, prepared.PrivilegeLogReceipt,
		prepared.Policy.SHA256, withheldSHA256, playersSHA256, frozenApprovalSHA256, inputsSHA256)
}

func validatePreparedApproval(stored StoredProductionInputs, subject documentproduction.ApprovalSubject, at time.Time) error {
	if !stored.Policy.Approval.Required {
		if stored.ApprovalSubject != nil || stored.ApprovalGrant != nil || len(stored.ApprovalEvents) != 0 || stored.ApprovalEvaluation != nil {
			return invalidProductionContract("unexpected approval authority")
		}
		return nil
	}
	if stored.ApprovalSubject == nil {
		return &documentproduction.Problem{Code: documentproduction.ProblemApprovalRequired, Detail: "current approval is required"}
	}
	expected, err := canonicalApprovalSubjectDigest(subject)
	if err != nil {
		return err
	}
	actual, err := canonicalApprovalSubjectDigest(*stored.ApprovalSubject)
	if err != nil || actual != expected {
		return &documentproduction.Problem{Code: documentproduction.ProblemApprovalStale, Detail: "approval subject is stale"}
	}
	evaluation, err := EvaluateRequiredApproval(stored.Policy, subject, stored.ApprovalGrant, stored.ApprovalEvents, at)
	if err != nil {
		return err
	}
	if stored.ApprovalEvaluation == nil || stored.ApprovalEvaluation.SHA256 != evaluation.SHA256 {
		return &documentproduction.Problem{Code: documentproduction.ProblemApprovalStale, Detail: "approval evaluation is stale"}
	}
	return nil
}

func newGateChecks() []documentproduction.GateCheck {
	ids := documentproduction.RequiredGateCheckIDs()
	checks := make([]documentproduction.GateCheck, len(ids))
	for index, id := range ids {
		checks[index] = documentproduction.GateCheck{ID: id, State: documentproduction.GateStateNotChecked,
			Evidence: []documentproduction.GateEvidence{{Kind: "not_checked", Detail: "check did not run"}}}
	}
	return checks
}

func setGateCheck(checks *[]documentproduction.GateCheck, id string, passed bool, evidence documentproduction.GateEvidence) {
	setGateCheckState(checks, id, gateState(passed), true, evidence)
}

func setGateCheckState(checks *[]documentproduction.GateCheck, id, state string, required bool, evidence documentproduction.GateEvidence) {
	for index := range *checks {
		if (*checks)[index].ID == id {
			(*checks)[index].Required = required
			(*checks)[index].State = state
			(*checks)[index].Evidence = []documentproduction.GateEvidence{evidence}
			return
		}
	}
}

func gateState(passed bool) string {
	if passed {
		return documentproduction.GateStatePass
	}
	return documentproduction.GateStateFail
}

func preparedInputGateProblem(authority documentproduction.PreparedInputAuthority) error {
	for _, check := range authority.GateResults.Checks {
		if check.State != documentproduction.GateStateFail && (!check.Required || check.State == documentproduction.GateStatePass) {
			continue
		}
		switch check.ID {
		case documentproduction.GateCheckRevision, documentproduction.GateCheckMembership,
			documentproduction.GateCheckSources, documentproduction.GateCheckRenditions,
			documentproduction.GateCheckFrames, documentproduction.GateCheckDecisions,
			documentproduction.GateCheckReviews:
			return &documentproduction.Problem{Code: documentproduction.ProblemSourceStale, Detail: "stored production inputs changed before numbering"}
		case documentproduction.GateCheckApproval:
			return &documentproduction.Problem{Code: documentproduction.ProblemApprovalRequired, Detail: "current approval is required"}
		case documentproduction.GateCheckPrivilegeLog:
			return &documentproduction.Problem{Code: documentproduction.ProblemPrivilegeLogRequired, Detail: "validated frozen privilege log is required"}
		default:
			return documentproduction.PolicyUnsatisfiedProblem(check.Evidence[0].IDs)
		}
	}
	return invalidProductionContract("prepared-input gates did not pass")
}

func completePolicyEvaluation(value documentproduction.PolicyEvaluation, count int) bool {
	if len(value.Members) != count {
		return false
	}
	for _, member := range value.Members {
		if member.Disposition != documentproduction.PolicyDispositionProduce && member.Disposition != documentproduction.PolicyDispositionWithhold {
			return false
		}
	}
	return true
}

func policyHasWithheld(value documentproduction.PolicyEvaluation) bool {
	for _, member := range value.Members {
		if member.Disposition == documentproduction.PolicyDispositionWithhold {
			return true
		}
	}
	return false
}

func gateProblemIDs(err error) []string {
	if problem, ok := errors.AsType[*documentproduction.Problem](err); ok {
		return boundedGateIDs(problem.IDs)
	}
	return []string{}
}

func boundedGateIDs(values []string) []string {
	values = slices.Clone(values)
	slices.Sort(values)
	values = slices.Compact(values)
	if len(values) > documentproduction.MaxProblemIDs {
		values = values[:documentproduction.MaxProblemIDs]
	}
	if values == nil {
		return []string{}
	}
	return values
}

func storedProductionMemberHash(values []StoredPreparedMember) ([]byte, string, error) {
	prepared := make([]documentproduction.PreparedMember, len(values))
	for index := range values {
		prepared[index] = documentproduction.PreparedMember{Member: values[index].Member}
	}
	return documentproduction.PreparedMemberHash(prepared)
}

func canonicalApprovalSubjectDigest(value documentproduction.ApprovalSubject) (string, error) {
	_, digest, err := documentproduction.CanonicalApprovalSubject(value)
	return digest, err
}

func optionalWithheldDigest(value *documentproduction.WithheldSelection) string {
	if value == nil {
		return ""
	}
	return value.SHA256
}

func optionalPrivilegeDigest(value *documentproduction.PrivilegeLogReceipt) string {
	if value == nil {
		return ""
	}
	return value.SHA256
}

func optionalPrivilegeInputsDigest(value *documentproduction.PrivilegeLogReceipt) string {
	if value == nil {
		return ""
	}
	return value.InputsSHA256
}

func optionalApprovalDigest(value *documentproduction.ApprovalEvaluation) string {
	if value == nil {
		return ""
	}
	return value.SHA256
}

func withheldCount(value *documentproduction.WithheldSelection) int {
	if value == nil {
		return 0
	}
	return len(value.Members)
}

func cloneStoredInputs(value StoredProductionInputs) StoredProductionInputs {
	value.Members = slices.Clone(value.Members)
	for index := range value.Members {
		value.Members[index].Decisions = slices.Clone(value.Members[index].Decisions)
		value.Members[index].Facts.Labels = slices.Clone(value.Members[index].Facts.Labels)
	}
	value.ApprovalEvents = slices.Clone(value.ApprovalEvents)
	return value
}

func cloneWithheld(value *documentproduction.WithheldSelection) *documentproduction.WithheldSelection {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.Members = slices.Clone(value.Members)
	return &cloned
}

func clonePrivilegeReceipt(value *documentproduction.PrivilegeLogReceipt) *documentproduction.PrivilegeLogReceipt {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneApprovalEvaluation(value *documentproduction.ApprovalEvaluation) *documentproduction.ApprovalEvaluation {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func compareStrings(left, right string) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}
