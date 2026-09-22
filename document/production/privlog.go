package production

import (
	"slices"

	"go.kenn.io/docbank/document/redaction"
)

// PrivilegeLogValidation is the canonical pre-approval authority derived from
// persisted rows and their exact policy, player, and withheld snapshots.
type PrivilegeLogValidation struct {
	Inputs       PrivilegeLogInputs `json:"inputs"`
	RowsSHA256   string             `json:"rows_sha256"`
	InputsSHA256 string             `json:"inputs_sha256"`
}

// OrderPrivilegeRows returns stable parent-then-attachment order. Families use
// their earliest selected occurrence as the group key; members then use exact
// family order, source-version identity, and row ID as deterministic ties.
func OrderPrivilegeRows(withheld WithheldSelection, rows []PrivilegeRow) ([]PrivilegeRow, error) {
	if err := ValidateWithheldSelection(withheld); err != nil {
		return nil, err
	}
	if _, _, err := CanonicalPrivilegeRows(rows); err != nil {
		return nil, err
	}
	if len(rows) != len(withheld.Members) {
		return nil, invalidProblem("privilege rows do not cover withheld selection")
	}
	type memberOrder struct {
		member      WithheldMember
		familyFirst int64
	}
	familyFirst := make(map[string]int64)
	for _, member := range withheld.Members {
		first, exists := familyFirst[member.Family.RootVersionID]
		if !exists || member.Ordinal < first {
			familyFirst[member.Family.RootVersionID] = member.Ordinal
		}
	}
	members := make(map[string]memberOrder, len(withheld.Members))
	for _, member := range withheld.Members {
		members[member.ID] = memberOrder{member: member, familyFirst: familyFirst[member.Family.RootVersionID]}
	}
	ordered := slices.Clone(rows)
	seen := make(map[string]struct{}, len(ordered))
	for _, row := range ordered {
		member, exists := members[row.WithheldMemberID]
		if !exists || row.SourceVersionID != member.member.SourceVersionID || row.FamilyOrder != member.member.FamilyOrder {
			return nil, invalidProblem("privilege row differs from withheld member")
		}
		if _, duplicate := seen[row.WithheldMemberID]; duplicate {
			return nil, invalidProblem("duplicate privilege row withheld member")
		}
		seen[row.WithheldMemberID] = struct{}{}
	}
	slices.SortFunc(ordered, func(left, right PrivilegeRow) int {
		leftOrder, rightOrder := members[left.WithheldMemberID], members[right.WithheldMemberID]
		if leftOrder.familyFirst != rightOrder.familyFirst {
			if leftOrder.familyFirst < rightOrder.familyFirst {
				return -1
			}
			return 1
		}
		if left.FamilyOrder != right.FamilyOrder {
			if left.FamilyOrder < right.FamilyOrder {
				return -1
			}
			return 1
		}
		if left.SourceVersionID != right.SourceVersionID {
			return compareString(left.SourceVersionID, right.SourceVersionID)
		}
		return compareString(left.ID, right.ID)
	})
	return ordered, nil
}

// ValidatePrivilegeLog validates a stored privilege-log revision. Callers must
// pass rows loaded from persistence, not replacement rows supplied by a freeze
// request. The resulting inputs digest deliberately excludes later approval
// and produced-number references.
func ValidatePrivilegeLog(input PrivilegeLogValidationInput, withheld WithheldSelection, policy PolicyVersion, players PlayersSnapshot, produced []redaction.Member) (PrivilegeLogValidation, error) {
	if err := ValidatePolicyVersion(policy); err != nil {
		return PrivilegeLogValidation{}, err
	}
	if err := ValidateWithheldSelection(withheld); err != nil {
		return PrivilegeLogValidation{}, err
	}
	if err := ValidatePlayersSnapshot(players); err != nil {
		return PrivilegeLogValidation{}, err
	}
	if !policy.PrivilegeLog.Required || withheld.PolicySHA256 != policy.SHA256 ||
		input.WithheldSelectionSHA256 != withheld.SHA256 ||
		input.PolicySHA256 != policy.SHA256 || input.PlayersSHA256 != players.SHA256 {
		return PrivilegeLogValidation{}, invalidProblem("privilege log authority does not match stored inputs")
	}
	if err := ValidateSelectionPartition(produced, withheld); err != nil {
		return PrivilegeLogValidation{}, err
	}
	_, inputsDigest, rowsDigest, err := canonicalPrivilegeLogInputs(input)
	if err != nil {
		return PrivilegeLogValidation{}, err
	}

	violations := newPrivilegeViolationSet()
	playersByID := make(map[string]struct{}, len(players.Players))
	for _, player := range players.Players {
		playersByID[player.ID] = struct{}{}
	}
	rowsByMember := make(map[string]PrivilegeRow, len(input.Rows))
	for _, row := range input.Rows {
		if _, duplicate := rowsByMember[row.WithheldMemberID]; duplicate {
			if err := violations.add(row.ID); err != nil {
				return PrivilegeLogValidation{}, err
			}
		}
		rowsByMember[row.WithheldMemberID] = row
		if !slices.Contains(policy.PrivilegeLog.AllowedBases, row.Basis) || !requiredPrivilegeFieldsPresent(row.Fields, policy.PrivilegeLog.RequiredFields) {
			if err := violations.add(row.ID); err != nil {
				return PrivilegeLogValidation{}, err
			}
		}
		for _, personID := range row.PersonIDs {
			if _, exists := playersByID[personID]; !exists {
				if err := violations.add(row.ID); err != nil {
					return PrivilegeLogValidation{}, err
				}
			}
		}
	}

	families := make(map[string][]WithheldMember)
	withheldIDs := make(map[string]struct{}, len(withheld.Members))
	for _, member := range withheld.Members {
		withheldIDs[member.ID] = struct{}{}
		families[member.Family.RootVersionID] = append(families[member.Family.RootVersionID], member)
		row, exists := rowsByMember[member.ID]
		if !exists {
			if err := violations.add(member.ID); err != nil {
				return PrivilegeLogValidation{}, err
			}
			continue
		}
		if row.SourceVersionID != member.SourceVersionID || row.FamilyOrder != member.FamilyOrder {
			if err := violations.add(row.ID); err != nil {
				return PrivilegeLogValidation{}, err
			}
		}
	}
	for _, row := range input.Rows {
		if _, exists := withheldIDs[row.WithheldMemberID]; !exists {
			if err := violations.add(row.ID); err != nil {
				return PrivilegeLogValidation{}, err
			}
		}
	}
	for _, members := range families {
		slices.SortFunc(members, func(left, right WithheldMember) int {
			if left.FamilyOrder < right.FamilyOrder {
				return -1
			}
			if left.FamilyOrder > right.FamilyOrder {
				return 1
			}
			return 0
		})
		for index, member := range members {
			if member.FamilyOrder != int64(index+1) {
				if err := violations.add(member.ID); err != nil {
					return PrivilegeLogValidation{}, err
				}
			}
		}
	}
	if err := violations.problem(); err != nil {
		return PrivilegeLogValidation{}, err
	}
	return PrivilegeLogValidation{
		Inputs: PrivilegeLogInputs{
			Contract: PrivilegeLogInputsContractV1, LogID: input.LogID, Revision: input.Revision,
			WithheldSelectionSHA256: input.WithheldSelectionSHA256, PolicySHA256: input.PolicySHA256,
			PlayersSHA256: input.PlayersSHA256, RowsSHA256: rowsDigest, ValidatedAt: input.ValidatedAt,
		},
		RowsSHA256: rowsDigest, InputsSHA256: inputsDigest,
	}, nil
}

func requiredPrivilegeFieldsPresent(fields []PrivilegeField, required []string) bool {
	values := make(map[string]string, len(fields))
	for _, field := range fields {
		values[field.Name] = field.Value
	}
	for _, name := range required {
		if values[name] == "" {
			return false
		}
	}
	return true
}

type privilegeViolationSet map[string]struct{}

func newPrivilegeViolationSet() privilegeViolationSet {
	return make(privilegeViolationSet, MaxProblemIDs+1)
}

func (values privilegeViolationSet) add(id string) error {
	values[id] = struct{}{}
	if len(values) > MaxProblemIDs {
		return &Problem{Code: ProblemLimit, Detail: "privilege log validation failures exceed bounds"}
	}
	return nil
}

func (values privilegeViolationSet) problem() error {
	if len(values) == 0 {
		return nil
	}
	ids := make([]string, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return &Problem{Code: ProblemPolicyUnsatisfied, Detail: "stored privilege log does not satisfy selected policy", IDs: ids}
}
