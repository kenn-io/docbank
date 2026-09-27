package production

import (
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	// GenericPolicyID identifies Docbank's repository-owned autonomous policy.
	// It makes no legal determination: every selected member is produced after
	// the ordinary redaction gates pass, and no approval or privilege log is
	// required.
	GenericPolicyID            = "00000000-0000-4000-8000-000000000001"
	GenericPolicyVersionNumber = int64(1)
)

// PolicyMemberFacts are explicit stored facts supplied to policy evaluation.
// EvaluatePolicy never derives scope, disposition, privilege, or approval from
// document content.
type PolicyMemberFacts struct {
	MemberID       string              `json:"member_id"`
	FamilyID       string              `json:"family_id,omitzero"`
	FamilyComplete bool                `json:"family_complete"`
	Fields         map[string][]string `json:"fields"`
	Labels         []string            `json:"labels"`
}

type PolicyMemberEvaluation struct {
	MemberID       string   `json:"member_id"`
	Disposition    string   `json:"disposition,omitzero"`
	MatchedRuleIDs []string `json:"matched_rule_ids"`
}

// PolicyEvaluation carries only mechanical rule results and configured output
// requirements. It is not a relevance, privilege, or other legal conclusion.
type PolicyEvaluation struct {
	Members      []PolicyMemberEvaluation `json:"members"`
	Output       PolicyOutput             `json:"output"`
	Approval     ApprovalRequirement      `json:"approval"`
	PrivilegeLog PrivilegeLogRequirement  `json:"privilege_log"`
}

// ApprovalPublicGrant is the allowlisted public projection of an approval.
// Private authority evidence and its digest remain in the private record.
type ApprovalPublicGrant struct {
	ID            string `json:"id"`
	SubjectSHA256 string `json:"subject_sha256"`
	Actor         string `json:"actor"`
	GrantedAt     string `json:"granted_at"`
	ExpiresAt     string `json:"expires_at,omitzero"`
	SHA256        string `json:"sha256"`
}

// ApprovalPublicEvent omits the private reason from public output.
type ApprovalPublicEvent struct {
	ID                    string `json:"id"`
	ApprovalID            string `json:"approval_id"`
	Kind                  string `json:"kind"`
	EffectiveAt           string `json:"effective_at"`
	ReplacementApprovalID string `json:"replacement_approval_id,omitzero"`
}

// GenericPolicyVersion returns the immutable generic redaction policy.
func GenericPolicyVersion() (PolicyVersion, error) {
	value := PolicyVersion{
		Contract:  PolicyContractV1,
		ID:        GenericPolicyID,
		Version:   GenericPolicyVersionNumber,
		Name:      "Generic redaction",
		CreatedAt: "1970-01-01T00:00:00Z",
		Rules: []PolicyRule{{
			ID:          "produce-selected-members",
			Kind:        PolicyRuleDisposition,
			Predicate:   PolicyPredicate{Field: "member.id", Operator: PolicyOperatorPresent},
			Disposition: PolicyDispositionProduce,
		}},
		ConflictMode: PolicyConflictReject,
	}
	_, digest, err := CanonicalPolicyVersion(value)
	if err != nil {
		return PolicyVersion{}, err
	}
	value.SHA256 = digest
	return value, nil
}

// EvaluatePolicy applies a versioned policy to explicit facts. Members retain
// caller occurrence order. Rule order is never conflict precedence.
func EvaluatePolicy(policy PolicyVersion, members []PolicyMemberFacts) (PolicyEvaluation, error) {
	if policy.SHA256 == "" {
		if _, _, err := CanonicalPolicyVersion(policy); err != nil {
			return PolicyEvaluation{}, err
		}
	} else if err := ValidatePolicyVersion(policy); err != nil {
		return PolicyEvaluation{}, err
	}
	if len(members) == 0 || len(members) > MaxPrivilegeRows {
		return PolicyEvaluation{}, &Problem{Code: ProblemInvalidContract, Detail: "invalid production policy facts"}
	}

	result := PolicyEvaluation{
		Members:      make([]PolicyMemberEvaluation, len(members)),
		Output:       policy.Output,
		Approval:     policy.Approval,
		PrivilegeLog: policy.PrivilegeLog,
	}
	ruleDispositions := make([]map[string]string, len(members))
	familyIndexes := make(map[string][]int)
	violations := newPolicyViolationSet()
	for index, member := range members {
		if err := validatePolicyMemberFacts(member); err != nil {
			return PolicyEvaluation{}, err
		}
		result.Members[index] = PolicyMemberEvaluation{MemberID: member.MemberID, MatchedRuleIDs: []string{}}
		ruleDispositions[index] = make(map[string]string)
		if member.FamilyID != "" {
			familyIndexes[member.FamilyID] = append(familyIndexes[member.FamilyID], index)
		}
	}

	for _, rule := range policy.Rules {
		appliedFamilies := make(map[string]struct{})
		for index, member := range members {
			matched, err := policyPredicateMatches(rule.Predicate, member)
			if err != nil {
				return PolicyEvaluation{}, err
			}
			if !matched {
				continue
			}
			if rule.Kind == PolicyRuleLabel && !slices.Contains(member.Labels, rule.RequiredLabel) {
				if err := violations.add(rule.ID); err != nil {
					return PolicyEvaluation{}, err
				}
			}
			targets := []int{index}
			if rule.Kind == PolicyRuleFamily && rule.FamilyMode == PolicyFamilyComplete {
				if member.FamilyID == "" || !member.FamilyComplete {
					if err := violations.add(rule.ID); err != nil {
						return PolicyEvaluation{}, err
					}
					continue
				}
				if _, applied := appliedFamilies[member.FamilyID]; applied {
					continue
				}
				appliedFamilies[member.FamilyID] = struct{}{}
				targets = familyIndexes[member.FamilyID]
			}
			for _, target := range targets {
				result.Members[target].MatchedRuleIDs = append(result.Members[target].MatchedRuleIDs, rule.ID)
				if rule.Disposition != "" {
					ruleDispositions[target][rule.ID] = rule.Disposition
				}
			}
		}
	}

	for index := range result.Members {
		var produce, withhold []string
		for ruleID, disposition := range ruleDispositions[index] {
			switch disposition {
			case PolicyDispositionProduce:
				produce = append(produce, ruleID)
			case PolicyDispositionWithhold:
				withhold = append(withhold, ruleID)
			}
		}
		result.Members[index].MatchedRuleIDs = canonicalStrings(result.Members[index].MatchedRuleIDs)
		switch {
		case len(produce) != 0 && len(withhold) != 0:
			if err := violations.add(produce...); err != nil {
				return PolicyEvaluation{}, err
			}
			if err := violations.add(withhold...); err != nil {
				return PolicyEvaluation{}, err
			}
		case len(produce) != 0:
			result.Members[index].Disposition = PolicyDispositionProduce
		case len(withhold) != 0:
			result.Members[index].Disposition = PolicyDispositionWithhold
		}
	}
	if err := violations.problem(); err != nil {
		return PolicyEvaluation{}, err
	}
	return result, nil
}

func PublicApprovalGrant(value ApprovalGrant) ApprovalPublicGrant {
	return ApprovalPublicGrant{
		ID: value.ID, SubjectSHA256: value.SubjectSHA256, Actor: value.Actor,
		GrantedAt: value.GrantedAt, ExpiresAt: value.ExpiresAt, SHA256: value.SHA256,
	}
}

func PublicApprovalEvents(values []ApprovalEvent) []ApprovalPublicEvent {
	result := make([]ApprovalPublicEvent, 0, len(values))
	for _, value := range values {
		result = append(result, ApprovalPublicEvent{
			ID: value.ID, ApprovalID: value.ApprovalID, Kind: value.Kind,
			EffectiveAt: value.EffectiveAt, ReplacementApprovalID: value.ReplacementApprovalID,
		})
	}
	return result
}

func validatePolicyMemberFacts(value PolicyMemberFacts) error {
	if invalidPolicyFactText(value.MemberID, 256, false) || invalidPolicyFactText(value.FamilyID, 256, true) ||
		len(value.Fields) > MaxPolicyValues || len(value.Labels) > MaxPolicyValues {
		return &Problem{Code: ProblemInvalidContract, Detail: "invalid production policy facts"}
	}
	for field, values := range value.Fields {
		if invalidPolicyFactText(field, 256, false) || len(values) > MaxPolicyValues {
			return &Problem{Code: ProblemInvalidContract, Detail: "invalid production policy facts"}
		}
		for _, fact := range values {
			if invalidPolicyFactText(fact, MaxPolicyTextBytes, false) {
				return &Problem{Code: ProblemInvalidContract, Detail: "invalid production policy facts"}
			}
		}
	}
	for _, label := range value.Labels {
		if invalidPolicyFactText(label, 256, false) {
			return &Problem{Code: ProblemInvalidContract, Detail: "invalid production policy facts"}
		}
	}
	return nil
}

func invalidPolicyFactText(value string, limit int, optional bool) bool {
	return !utf8.ValidString(value) || len(value) > limit || !optional && value == "" || strings.TrimSpace(value) != value
}

type policyViolationSet map[string]struct{}

func newPolicyViolationSet() policyViolationSet {
	return make(policyViolationSet, MaxProblemIDs+1)
}

func (values policyViolationSet) add(ids ...string) error {
	for _, id := range ids {
		values[id] = struct{}{}
		if len(values) > MaxProblemIDs {
			return PolicyUnsatisfiedProblem(values.ids())
		}
	}
	return nil
}

func (values policyViolationSet) problem() error {
	if len(values) == 0 {
		return nil
	}
	return PolicyUnsatisfiedProblem(values.ids())
}

func (values policyViolationSet) ids() []string {
	result := make([]string, 0, len(values))
	for id := range values {
		result = append(result, id)
	}
	return result
}

func policyPredicateMatches(predicate PolicyPredicate, member PolicyMemberFacts) (bool, error) {
	values := member.Fields[predicate.Field]
	switch predicate.Field {
	case "member.id":
		values = []string{member.MemberID}
	case "family.id":
		values = nil
		if member.FamilyID != "" {
			values = []string{member.FamilyID}
		}
	case "label", "labels":
		values = member.Labels
	}
	switch predicate.Operator {
	case PolicyOperatorEquals:
		return slices.Contains(values, predicate.Values[0]), nil
	case PolicyOperatorOneOf:
		for _, value := range values {
			if slices.Contains(predicate.Values, value) {
				return true, nil
			}
		}
	case PolicyOperatorPresent:
		return len(values) != 0, nil
	case PolicyOperatorDateBetween:
		for _, value := range values {
			if !canonicalDate(value) {
				return false, &Problem{Code: ProblemInvalidContract, Detail: "invalid production policy date fact"}
			}
		}
		for _, value := range values {
			if value >= predicate.From && value <= predicate.Through {
				return true, nil
			}
		}
	}
	return false, nil
}
