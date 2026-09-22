package production

import (
	"slices"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
)

const (
	PreparedProductionContractV1 = "production-prepared-production/v1"
	GateResultsContractV1        = "production-gate-results/v1"
	GateAuditContractV1          = "production-gate-audit/v1"
)

const (
	GateStatePass          = "pass"
	GateStateFail          = "fail"
	GateStateNotChecked    = "not_checked"
	GateStateNotApplicable = "not_applicable"
)

const (
	GateCheckRevision     = "stored_revision"
	GateCheckMembership   = "membership"
	GateCheckSources      = "sources"
	GateCheckRenditions   = "renditions"
	GateCheckFrames       = "frames"
	GateCheckDecisions    = "decisions"
	GateCheckReviews      = "reviews"
	GateCheckPolicy       = "policy"
	GateCheckWithheld     = "withheld_selection"
	GateCheckPrivilegeLog = "privilege_log"
	GateCheckApproval     = "approval"
)

var requiredGateCheckIDs = []string{
	GateCheckRevision,
	GateCheckMembership,
	GateCheckSources,
	GateCheckRenditions,
	GateCheckFrames,
	GateCheckDecisions,
	GateCheckReviews,
	GateCheckPolicy,
	GateCheckWithheld,
	GateCheckPrivilegeLog,
	GateCheckApproval,
}

// RequiredGateCheckIDs returns the complete, ordered pre-numbering gate set.
func RequiredGateCheckIDs() []string { return slices.Clone(requiredGateCheckIDs) }

// PreparedFrame seals the physical page frame used by selectors and rendering.
type PreparedFrame struct {
	Page   int    `json:"page"`
	SHA256 string `json:"sha256"`
	Width  int64  `json:"width"`
	Height int64  `json:"height"`
}

// PreparedMember is one occurrence in production order. The same source
// version may appear more than once under distinct Member.ID and Ordinal.
type PreparedMember struct {
	Member          redaction.Member     `json:"member"`
	Decisions       []redaction.Decision `json:"decisions"`
	Resolved        redaction.Resolved   `json:"resolved"`
	Frames          []PreparedFrame      `json:"frames"`
	Facts           PolicyMemberFacts    `json:"facts"`
	DecisionsSHA256 string               `json:"decisions_sha256"`
	ResolvedSHA256  string               `json:"resolved_sha256"`
	ReviewBinding   string               `json:"review_binding"`
	Disposition     string               `json:"disposition"`
}

// PreparedProduction is the immutable pre-numbering authority derived from
// stored rows. It includes empty decision sets and every occurrence in order.
type PreparedProduction struct {
	Contract                string               `json:"contract"`
	SetID                   string               `json:"set_id"`
	Revision                int64                `json:"revision"`
	ETag                    int64                `json:"etag"`
	MembershipSealed        bool                 `json:"membership_sealed"`
	InstructionsSHA256      string               `json:"instructions_sha256"`
	MemberHash              string               `json:"member_hash"`
	DecisionsSHA256         string               `json:"decisions_sha256"`
	RecipeSHA256            string               `json:"recipe_sha256"`
	OutputProfileSHA256     string               `json:"output_profile_sha256"`
	DisclosureProfileSHA256 string               `json:"disclosure_profile_sha256"`
	NumberingPolicySHA256   string               `json:"numbering_policy_sha256"`
	Policy                  PolicyVersion        `json:"policy"`
	Members                 []PreparedMember     `json:"members"`
	WithheldSelection       *WithheldSelection   `json:"withheld_selection,omitzero"`
	PrivilegeLogReceipt     *PrivilegeLogReceipt `json:"privilege_log_receipt,omitzero"`
	ApprovalSubject         ApprovalSubject      `json:"approval_subject"`
	ApprovalEvaluation      *ApprovalEvaluation  `json:"approval_evaluation,omitzero"`
	PreparedAt              string               `json:"prepared_at"`
	SHA256                  string               `json:"sha256,omitzero"`
}

type GateEvidence struct {
	Kind      string   `json:"kind"`
	SubjectID string   `json:"subject_id,omitzero"`
	SHA256    string   `json:"sha256,omitzero"`
	Detail    string   `json:"detail,omitzero"`
	Count     int64    `json:"count,omitzero"`
	IDs       []string `json:"ids,omitzero"`
}

type GateCheck struct {
	ID       string         `json:"id"`
	Required bool           `json:"required"`
	State    string         `json:"state"`
	Evidence []GateEvidence `json:"evidence"`
}

type GateResults struct {
	Contract       string      `json:"contract"`
	PreparedSHA256 string      `json:"prepared_sha256"`
	EvaluatedAt    string      `json:"evaluated_at"`
	Checks         []GateCheck `json:"checks"`
	SHA256         string      `json:"sha256,omitzero"`
}

// GateAudit binds the operation request to the stored gate result and, only
// for a passing run, the prepared-input receipt admitted to numbering.
type GateAudit struct {
	Contract            string `json:"contract"`
	OperationID         string `json:"operation_id"`
	RequestSHA256       string `json:"request_sha256,omitzero"`
	PreparedSHA256      string `json:"prepared_sha256"`
	GateResultsSHA256   string `json:"gate_results_sha256"`
	PreparedInputSHA256 string `json:"prepared_input_sha256,omitzero"`
	RecordedAt          string `json:"recorded_at"`
	SHA256              string `json:"sha256,omitzero"`
}

// PreparedInputAuthority is persisted as one canonical operation response so
// the sealed input, every finding and its audit binding commit atomically.
type PreparedInputAuthority struct {
	Prepared    PreparedProduction    `json:"prepared"`
	GateResults GateResults           `json:"gate_results"`
	Receipt     *PreparedInputReceipt `json:"receipt,omitzero"`
	Audit       GateAudit             `json:"audit"`
}

func PreparedMemberHash(values []PreparedMember) ([]byte, string, error) {
	members := slices.Clone(values)
	slices.SortFunc(members, comparePreparedMembers)
	identities := make([]redaction.Member, len(members))
	for index := range members {
		identities[index] = members[index].Member
		identities[index].Reviewed = false
		identities[index].ReviewBinding = ""
	}
	return encodeDigest(identities, "prepared production members")
}

func CanonicalPreparedProduction(value PreparedProduction) ([]byte, string, error) {
	value.SHA256 = ""
	value.Members = clonePreparedMembers(value.Members)
	slices.SortFunc(value.Members, comparePreparedMembers)
	if err := validatePreparedProduction(value, false); err != nil {
		return nil, "", err
	}
	return encodeDigest(value, "prepared production")
}

func ValidatePreparedProduction(value PreparedProduction) error {
	return validateStoredDigest(value.SetID, value.SHA256, func() (string, error) {
		if err := validatePreparedProduction(value, true); err != nil {
			return "", err
		}
		_, digest, err := CanonicalPreparedProduction(value)
		return digest, err
	})
}

func CanonicalGateResults(value GateResults) ([]byte, string, error) {
	value.SHA256 = ""
	value.Checks = cloneGateChecks(value.Checks)
	slices.SortFunc(value.Checks, func(left, right GateCheck) int {
		return gateCheckRank(left.ID) - gateCheckRank(right.ID)
	})
	for index := range value.Checks {
		slices.SortFunc(value.Checks[index].Evidence, compareGateEvidence)
	}
	if err := validateGateResults(value, false); err != nil {
		return nil, "", err
	}
	return encodeDigest(value, "production gate results")
}

func ValidateGateResults(value GateResults) error {
	return validateStoredDigest(value.PreparedSHA256, value.SHA256, func() (string, error) {
		if err := validateGateResults(value, true); err != nil {
			return "", err
		}
		_, digest, err := CanonicalGateResults(value)
		return digest, err
	})
}

func GateResultsPassed(value GateResults) bool {
	if ValidateGateResults(value) != nil {
		return false
	}
	for _, check := range value.Checks {
		if check.State == GateStateFail || check.Required && check.State != GateStatePass {
			return false
		}
	}
	return true
}

func CanonicalGateAudit(value GateAudit) ([]byte, string, error) {
	value.SHA256 = ""
	if err := validateGateAudit(value, false); err != nil {
		return nil, "", err
	}
	return encodeDigest(value, "production gate audit")
}

func ValidateGateAudit(value GateAudit) error {
	return validateStoredDigest(value.OperationID, value.SHA256, func() (string, error) {
		if err := validateGateAudit(value, true); err != nil {
			return "", err
		}
		_, digest, err := CanonicalGateAudit(value)
		return digest, err
	})
}

func ValidatePreparedInputAuthority(value PreparedInputAuthority) error {
	if err := ValidatePreparedProduction(value.Prepared); err != nil {
		return err
	}
	if err := ValidateGateResults(value.GateResults); err != nil {
		return err
	}
	if err := ValidateGateAudit(value.Audit); err != nil {
		return err
	}
	if value.GateResults.PreparedSHA256 != value.Prepared.SHA256 ||
		value.Audit.PreparedSHA256 != value.Prepared.SHA256 ||
		value.Audit.GateResultsSHA256 != value.GateResults.SHA256 {
		return changedPayloadProblem(value.Prepared.SetID)
	}
	passed := GateResultsPassed(value.GateResults)
	if passed != (value.Receipt != nil) {
		return invalidProblem("prepared-input receipt does not match gate outcome")
	}
	if value.Receipt == nil {
		if value.Audit.PreparedInputSHA256 != "" {
			return changedPayloadProblem(value.Prepared.SetID)
		}
		return nil
	}
	if err := ValidatePreparedInputReceipt(*value.Receipt); err != nil {
		return err
	}
	withheldSHA256, privilegeSHA256, approvalSHA256 := "", "", ""
	if value.Prepared.WithheldSelection != nil {
		withheldSHA256 = value.Prepared.WithheldSelection.SHA256
	}
	if value.Prepared.PrivilegeLogReceipt != nil {
		privilegeSHA256 = value.Prepared.PrivilegeLogReceipt.SHA256
	}
	if value.Prepared.ApprovalEvaluation != nil {
		approvalSHA256 = value.Prepared.ApprovalEvaluation.SHA256
	}
	if value.Receipt.GateResultsSHA256 != value.GateResults.SHA256 ||
		value.Receipt.ApprovalSubjectSHA256 != approvalSubjectDigest(value.Prepared.ApprovalSubject) ||
		value.Receipt.WithheldSelectionSHA256 != withheldSHA256 ||
		value.Receipt.PrivilegeLogReceiptSHA256 != privilegeSHA256 ||
		value.Receipt.ApprovalEvaluationSHA256 != approvalSHA256 ||
		value.Audit.PreparedInputSHA256 != value.Receipt.SHA256 {
		return changedPayloadProblem(value.Receipt.ID)
	}
	if value.Prepared.ApprovalEvaluation != nil &&
		value.Prepared.ApprovalEvaluation.SubjectSHA256 != value.Receipt.ApprovalSubjectSHA256 {
		return changedPayloadProblem(value.Receipt.ID)
	}
	if value.Prepared.PrivilegeLogReceipt != nil &&
		(value.Prepared.PrivilegeLogReceipt.PolicySHA256 != value.Prepared.Policy.SHA256 ||
			value.Prepared.PrivilegeLogReceipt.WithheldSelectionSHA256 != withheldSHA256 ||
			value.Prepared.PrivilegeLogReceipt.ApprovalEvaluationSHA256 != approvalSHA256 ||
			value.Prepared.PrivilegeLogReceipt.InputsSHA256 != value.Prepared.ApprovalSubject.PrivilegeLogInputsSHA256) {
		return changedPayloadProblem(value.Receipt.ID)
	}
	return nil
}

func validatePreparedProduction(value PreparedProduction, requireDigest bool) error {
	if value.Contract != PreparedProductionContractV1 || !canonicalUUID(value.SetID) || value.Revision < 1 || value.ETag < 1 ||
		!value.MembershipSealed || !allSHA256(value.InstructionsSHA256, value.MemberHash, value.DecisionsSHA256,
		value.RecipeSHA256, value.OutputProfileSHA256, value.DisclosureProfileSHA256, value.NumberingPolicySHA256) ||
		len(value.Members) == 0 || len(value.Members) > MaxPrivilegeRows || validateTimestamp(value.PreparedAt) != nil ||
		requireDigest && !canonical.IsSHA256Hex(value.SHA256) || !requireDigest && value.SHA256 != "" ||
		ValidatePolicyVersion(value.Policy) != nil {
		return invalidProblem("invalid prepared production")
	}
	if value.ApprovalSubject.SetID != value.SetID || value.ApprovalSubject.Revision != value.Revision ||
		value.ApprovalSubject.InstructionsSHA256 != value.InstructionsSHA256 ||
		value.ApprovalSubject.RecipeSHA256 != value.RecipeSHA256 ||
		value.ApprovalSubject.OutputProfileSHA256 != value.OutputProfileSHA256 ||
		value.ApprovalSubject.DisclosureProfileSHA256 != value.DisclosureProfileSHA256 ||
		value.ApprovalSubject.NumberingPolicySHA256 != value.NumberingPolicySHA256 ||
		value.ApprovalSubject.Policy.PolicySHA256 != value.Policy.SHA256 {
		return invalidProblem("prepared approval subject does not match production")
	}
	if err := validatePreparedConditionalAuthorities(value); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(value.Members))
	allDecisions := make([]redaction.Decision, 0)
	for index, member := range value.Members {
		if member.Member.Ordinal != int64(index+1) || member.Facts.MemberID != member.Member.ID ||
			member.ReviewBinding != member.Member.ReviewBinding ||
			(member.Disposition != "" && member.Disposition != PolicyDispositionProduce && member.Disposition != PolicyDispositionWithhold) {
			return invalidProblem("invalid prepared member")
		}
		expectedApprovalMember := ApprovalMember{
			MemberID: member.Member.ID, Ordinal: member.Member.Ordinal,
			SourceVersionID: member.Member.SourceVersionID, SourceSHA256: member.Member.SourceSHA256,
			SourceSize: member.Member.SourceSize, PDFSHA256: member.Member.PDFSHA256,
			PageInventorySHA256: member.Member.PageInventorySHA256, MapSHA256: member.Member.MapSHA256,
			DecisionsSHA256: member.DecisionsSHA256, ResolvedSHA256: member.ResolvedSHA256,
		}
		if len(value.ApprovalSubject.Members) != len(value.Members) || value.ApprovalSubject.Members[index] != expectedApprovalMember {
			return invalidProblem("prepared approval members do not match production")
		}
		if _, duplicate := seen[member.Member.ID]; duplicate {
			return invalidProblem("duplicate prepared occurrence")
		}
		seen[member.Member.ID] = struct{}{}
		if err := redaction.ValidateMember(member.Member); err != nil {
			return invalidProblem("invalid prepared member authority")
		}
		_, decisionsDigest, err := redaction.CanonicalDecisions(member.Decisions)
		if err != nil || decisionsDigest != member.DecisionsSHA256 {
			return invalidProblem("prepared decisions do not match digest")
		}
		for _, decision := range member.Decisions {
			if decision.MemberID != member.Member.ID || decision.Selector.MapSHA256 != member.Member.MapSHA256 {
				return invalidProblem("prepared decision targets another occurrence")
			}
		}
		if _, resolvedDigest, err := redaction.CanonicalResolved(member.Resolved); err != nil || resolvedDigest != member.ResolvedSHA256 ||
			member.Resolved.SHA256 != member.ResolvedSHA256 || member.Resolved.MapSHA256 != member.Member.MapSHA256 ||
			member.Resolved.RecipeSHA256 != value.RecipeSHA256 {
			return invalidProblem("prepared resolved plan does not match authority")
		}
		if _, err := redaction.Text(member.Resolved); err != nil {
			return invalidProblem("invalid prepared resolved plan")
		}
		if len(member.Frames) != len(member.Resolved.Pages) {
			return invalidProblem("prepared frames do not match resolved pages")
		}
		for pageIndex, frame := range member.Frames {
			page := member.Resolved.Pages[pageIndex]
			if frame.Page != page.Number || frame.SHA256 != page.FrameSHA256 || frame.Width != page.Width ||
				frame.Height != page.Height || !canonical.IsSHA256Hex(frame.SHA256) {
				return invalidProblem("prepared frame does not match resolved page")
			}
		}
		allDecisions = append(allDecisions, member.Decisions...)
	}
	_, memberDigest, err := PreparedMemberHash(value.Members)
	if err != nil || memberDigest != value.MemberHash {
		return invalidProblem("prepared member hash does not match occurrences")
	}
	_, decisionsDigest, err := redaction.CanonicalDecisions(allDecisions)
	if err != nil || decisionsDigest != value.DecisionsSHA256 {
		return invalidProblem("prepared decision hash does not match decisions")
	}
	return nil
}

func validatePreparedConditionalAuthorities(value PreparedProduction) error {
	if err := validateApprovalSubject(value.ApprovalSubject); err != nil {
		return invalidProblem("invalid prepared approval subject")
	}
	withheldSHA256 := ""
	if value.WithheldSelection != nil {
		if err := ValidateWithheldSelection(*value.WithheldSelection); err != nil {
			return err
		}
		withheldSHA256 = value.WithheldSelection.SHA256
	}
	privilegeInputsSHA256 := ""
	if value.PrivilegeLogReceipt != nil {
		if err := ValidatePrivilegeLogReceipt(*value.PrivilegeLogReceipt); err != nil {
			return err
		}
		privilegeInputsSHA256 = value.PrivilegeLogReceipt.InputsSHA256
	}
	if value.ApprovalEvaluation != nil {
		if err := ValidateApprovalEvaluation(*value.ApprovalEvaluation); err != nil {
			return err
		}
	}
	if value.ApprovalSubject.WithheldSelectionSHA256 != withheldSHA256 ||
		value.ApprovalSubject.PrivilegeLogInputsSHA256 != privilegeInputsSHA256 {
		return changedPayloadProblem(value.SetID)
	}
	return nil
}

func validateGateResults(value GateResults, requireDigest bool) error {
	if value.Contract != GateResultsContractV1 || !canonical.IsSHA256Hex(value.PreparedSHA256) ||
		validateTimestamp(value.EvaluatedAt) != nil || len(value.Checks) != len(requiredGateCheckIDs) ||
		requireDigest && !canonical.IsSHA256Hex(value.SHA256) || !requireDigest && value.SHA256 != "" {
		return invalidProblem("invalid production gate results")
	}
	for index, check := range value.Checks {
		if check.ID != requiredGateCheckIDs[index] ||
			!oneOf(check.State, GateStatePass, GateStateFail, GateStateNotChecked, GateStateNotApplicable) ||
			check.Required && check.State == GateStateNotChecked || len(check.Evidence) == 0 {
			return invalidProblem("invalid production gate check")
		}
		for _, evidence := range check.Evidence {
			if invalidText(evidence.Kind, 128, false) || invalidOptionalText(evidence.SubjectID, 256) ||
				!optionalSHA256(evidence.SHA256) || invalidOptionalText(evidence.Detail, MaxProblemDetailBytes) ||
				evidence.Count < 0 || len(evidence.IDs) > MaxProblemIDs {
				return invalidProblem("invalid production gate evidence")
			}
			for _, id := range evidence.IDs {
				if invalidText(id, 256, false) {
					return invalidProblem("invalid production gate evidence ID")
				}
			}
		}
	}
	return nil
}

func validateGateAudit(value GateAudit, requireDigest bool) error {
	if value.Contract != GateAuditContractV1 || !canonicalUUID(value.OperationID) ||
		!optionalSHA256(value.RequestSHA256) || !allSHA256(value.PreparedSHA256, value.GateResultsSHA256) ||
		!optionalSHA256(value.PreparedInputSHA256) || validateTimestamp(value.RecordedAt) != nil ||
		requireDigest && !canonical.IsSHA256Hex(value.SHA256) || !requireDigest && value.SHA256 != "" {
		return invalidProblem("invalid production gate audit")
	}
	return nil
}

func approvalSubjectDigest(value ApprovalSubject) string {
	_, digest, err := CanonicalApprovalSubject(value)
	if err != nil {
		return ""
	}
	return digest
}

func clonePreparedMembers(values []PreparedMember) []PreparedMember {
	values = slices.Clone(values)
	if values == nil {
		return []PreparedMember{}
	}
	for index := range values {
		values[index].Decisions = slices.Clone(values[index].Decisions)
		values[index].Frames = slices.Clone(values[index].Frames)
		values[index].Facts.Labels = slices.Clone(values[index].Facts.Labels)
	}
	return values
}

func cloneGateChecks(values []GateCheck) []GateCheck {
	values = slices.Clone(values)
	if values == nil {
		return []GateCheck{}
	}
	for index := range values {
		values[index].Evidence = slices.Clone(values[index].Evidence)
		for evidenceIndex := range values[index].Evidence {
			values[index].Evidence[evidenceIndex].IDs = slices.Clone(values[index].Evidence[evidenceIndex].IDs)
		}
	}
	return values
}

func comparePreparedMembers(left, right PreparedMember) int {
	if left.Member.Ordinal < right.Member.Ordinal {
		return -1
	}
	if left.Member.Ordinal > right.Member.Ordinal {
		return 1
	}
	return compareString(left.Member.ID, right.Member.ID)
}

func compareGateEvidence(left, right GateEvidence) int {
	if left.Kind != right.Kind {
		return compareString(left.Kind, right.Kind)
	}
	if left.SubjectID != right.SubjectID {
		return compareString(left.SubjectID, right.SubjectID)
	}
	if left.SHA256 != right.SHA256 {
		return compareString(left.SHA256, right.SHA256)
	}
	return compareString(left.Detail, right.Detail)
}

func gateCheckRank(id string) int {
	for index, candidate := range requiredGateCheckIDs {
		if id == candidate {
			return index
		}
	}
	return len(requiredGateCheckIDs)
}
