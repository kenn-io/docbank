package production

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
)

func CanonicalPolicyVersion(value PolicyVersion) ([]byte, string, error) {
	value.SHA256 = ""
	value.Rules = slices.Clone(value.Rules)
	for index := range value.Rules {
		if err := validatePolicyRule(value.Rules[index]); err != nil {
			return nil, "", err
		}
		value.Rules[index].Predicate.Values = canonicalStrings(value.Rules[index].Predicate.Values)
	}
	slices.SortFunc(value.Rules, func(left, right PolicyRule) int { return compareString(left.ID, right.ID) })
	value.PrivilegeLog.RequiredFields = canonicalStrings(value.PrivilegeLog.RequiredFields)
	value.PrivilegeLog.AllowedBases = canonicalStrings(value.PrivilegeLog.AllowedBases)
	if err := validatePolicyVersion(value, false); err != nil {
		return nil, "", err
	}
	return encodeDigest(value, "production policy")
}

func CanonicalPlayersSnapshot(value PlayersSnapshot) ([]byte, string, error) {
	value.SHA256 = ""
	value.Players = slices.Clone(value.Players)
	for index := range value.Players {
		value.Players[index].Aliases = canonicalStrings(value.Players[index].Aliases)
	}
	slices.SortFunc(value.Players, func(left, right Player) int { return compareString(left.ID, right.ID) })
	if err := validatePlayersSnapshot(value, false); err != nil {
		return nil, "", err
	}
	return encodeDigest(value, "players snapshot")
}

// CanonicalPolicyMemberFacts validates and hashes one stored policy-fact
// projection. The digest is pinned alongside the exact source generation.
func CanonicalPolicyMemberFacts(value PolicyMemberFacts) ([]byte, string, error) {
	if err := validatePolicyMemberFacts(value); err != nil {
		return nil, "", err
	}
	return encodeDigest(value, "production policy member facts")
}

type productionGateEvidenceMember struct {
	MemberID    string                      `json:"member_id"`
	Ordinal     int64                       `json:"ordinal"`
	EvidencePin ProductionMemberEvidencePin `json:"evidence_pin"`
}

type productionGateEvidence struct {
	Contract string                         `json:"contract"`
	Members  []productionGateEvidenceMember `json:"members"`
}

// CanonicalProductionGateEvidence hashes the exact source and publication
// pins in occurrence order, independently of the caller's slice order.
func CanonicalProductionGateEvidence(members []PreparedMember) ([]byte, string, error) {
	if len(members) == 0 || len(members) > redaction.MaxProductionMembers {
		return nil, "", invalidProblem("invalid production gate evidence members")
	}
	ordered := slices.Clone(members)
	slices.SortFunc(ordered, func(left, right PreparedMember) int {
		if left.Member.Ordinal < right.Member.Ordinal {
			return -1
		}
		if left.Member.Ordinal > right.Member.Ordinal {
			return 1
		}
		return compareString(left.Member.ID, right.Member.ID)
	})
	value := productionGateEvidence{Contract: ProductionGateEvidenceContractV1,
		Members: make([]productionGateEvidenceMember, len(ordered))}
	seen := make(map[string]struct{}, len(ordered))
	for index, member := range ordered {
		if !canonicalUUID(member.Member.ID) || member.Member.Ordinal != int64(index+1) || member.EvidencePin == nil {
			return nil, "", invalidProblem("invalid production gate evidence member")
		}
		if _, duplicate := seen[member.Member.ID]; duplicate {
			return nil, "", invalidProblem("duplicate production gate evidence member")
		}
		seen[member.Member.ID] = struct{}{}
		if err := validateProductionMemberEvidencePin(*member.EvidencePin, true); err != nil {
			return nil, "", err
		}
		value.Members[index] = productionGateEvidenceMember{
			MemberID: member.Member.ID, Ordinal: member.Member.Ordinal, EvidencePin: *member.EvidencePin,
		}
	}
	return encodeDigest(value, "production gate evidence")
}

func CanonicalApprovalSubject(value ApprovalSubject) ([]byte, string, error) {
	value.Members = slices.Clone(value.Members)
	slices.SortFunc(value.Members, func(left, right ApprovalMember) int {
		if left.Ordinal < right.Ordinal {
			return -1
		}
		if left.Ordinal > right.Ordinal {
			return 1
		}
		return compareString(left.MemberID, right.MemberID)
	})
	if err := validateApprovalSubject(value); err != nil {
		return nil, "", err
	}
	return encodeDigest(value, "approval subject")
}

func CanonicalApprovalAuthority(value ApprovalAuthority) ([]byte, string, error) {
	if err := validateApprovalAuthority(value); err != nil {
		return nil, "", err
	}
	return encodeDigest(value, "approval authority")
}

func CanonicalApprovalGrant(value ApprovalGrant) ([]byte, string, error) {
	value.SHA256 = ""
	if err := validateApprovalGrant(value, false); err != nil {
		return nil, "", err
	}
	return encodeDigest(value, "approval grant")
}

func CanonicalApprovalEvents(values []ApprovalEvent) ([]byte, string, error) {
	events := slices.Clone(values)
	if events == nil {
		events = []ApprovalEvent{}
	}
	slices.SortFunc(events, compareApprovalEvents)
	seen := make(map[string]struct{}, len(events))
	for _, event := range events {
		if err := validateApprovalEvent(event); err != nil {
			return nil, "", err
		}
		if _, duplicate := seen[event.ID]; duplicate {
			return nil, "", invalidProblem("duplicate approval event")
		}
		seen[event.ID] = struct{}{}
	}
	return encodeDigest(events, "approval events")
}

func CanonicalApprovalEvaluation(value ApprovalEvaluation) ([]byte, string, error) {
	value.SHA256 = ""
	if err := validateApprovalEvaluation(value, false); err != nil {
		return nil, "", err
	}
	return encodeDigest(value, "approval evaluation")
}

func CanonicalWithheldSelection(value WithheldSelection) ([]byte, string, error) {
	value.SHA256 = ""
	value.Members = slices.Clone(value.Members)
	slices.SortFunc(value.Members, func(left, right WithheldMember) int {
		if left.Ordinal < right.Ordinal {
			return -1
		}
		if left.Ordinal > right.Ordinal {
			return 1
		}
		return compareString(left.ID, right.ID)
	})
	if err := validateWithheldSelection(value, false); err != nil {
		return nil, "", err
	}
	return encodeDigest(value, "withheld selection")
}

func CanonicalPrivilegeRows(values []PrivilegeRow) ([]byte, string, error) {
	rows := slices.Clone(values)
	if rows == nil {
		rows = []PrivilegeRow{}
	}
	for index := range rows {
		if err := validatePrivilegeRow(rows[index]); err != nil {
			return nil, "", err
		}
		rows[index].PersonIDs = canonicalStrings(rows[index].PersonIDs)
		rows[index].Fields = slices.Clone(rows[index].Fields)
		if rows[index].Fields == nil {
			rows[index].Fields = []PrivilegeField{}
		}
		slices.SortFunc(rows[index].Fields, func(left, right PrivilegeField) int { return compareString(left.Name, right.Name) })
	}
	slices.SortFunc(rows, comparePrivilegeRows)
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if err := validatePrivilegeRow(row); err != nil {
			return nil, "", err
		}
		if _, duplicate := seen[row.ID]; duplicate {
			return nil, "", invalidProblem("duplicate privilege row")
		}
		seen[row.ID] = struct{}{}
	}
	return encodeDigest(rows, "privilege rows")
}

func CanonicalPrivilegeLogInputs(input PrivilegeLogValidationInput) ([]byte, string, error) {
	encoded, digest, _, err := canonicalPrivilegeLogInputs(input)
	return encoded, digest, err
}

func canonicalPrivilegeLogInputs(input PrivilegeLogValidationInput) ([]byte, string, string, error) {
	if len(input.Rows) == 0 || len(input.Rows) > MaxPrivilegeRows || !canonicalUUID(input.LogID) || input.Revision < 1 ||
		!allSHA256(input.WithheldSelectionSHA256, input.PolicySHA256, input.PlayersSHA256) || validateTimestamp(input.ValidatedAt) != nil {
		return nil, "", "", invalidProblem("invalid privilege log validation input")
	}
	_, rowsDigest, err := CanonicalPrivilegeRows(input.Rows)
	if err != nil {
		return nil, "", "", err
	}
	identity := PrivilegeLogInputs{
		Contract: PrivilegeLogInputsContractV1, LogID: input.LogID, Revision: input.Revision,
		WithheldSelectionSHA256: input.WithheldSelectionSHA256, PolicySHA256: input.PolicySHA256,
		PlayersSHA256: input.PlayersSHA256, RowsSHA256: rowsDigest, ValidatedAt: input.ValidatedAt,
	}
	encoded, digest, err := encodeDigest(identity, "privilege log inputs")
	return encoded, digest, rowsDigest, err
}

func CanonicalPrivilegeLogReceipt(value PrivilegeLogReceipt) ([]byte, string, error) {
	value.SHA256 = ""
	if err := validatePrivilegeLogReceipt(value, false); err != nil {
		return nil, "", err
	}
	return encodeDigest(value, "privilege log receipt")
}

func CanonicalPrivilegeLogAttachment(value PrivilegeLogAttachmentReceipt) ([]byte, string, error) {
	value.SHA256 = ""
	value.References = slices.Clone(value.References)
	if value.References == nil {
		value.References = []PrivilegeOutputReference{}
	}
	slices.SortFunc(value.References, func(left, right PrivilegeOutputReference) int {
		if left.WithheldMemberID != right.WithheldMemberID {
			return compareString(left.WithheldMemberID, right.WithheldMemberID)
		}
		if left.AssignedNumber != right.AssignedNumber {
			return compareString(left.AssignedNumber, right.AssignedNumber)
		}
		return compareString(left.ArtifactSHA256, right.ArtifactSHA256)
	})
	if err := validatePrivilegeLogAttachment(value, false); err != nil {
		return nil, "", err
	}
	return encodeDigest(value, "privilege log attachment")
}

func CanonicalArtifactManifest(value ArtifactManifest) ([]byte, string, error) {
	value.SHA256 = ""
	value.Artifacts = slices.Clone(value.Artifacts)
	slices.SortFunc(value.Artifacts, compareArtifacts)
	if err := validateArtifactManifest(value, false); err != nil {
		return nil, "", err
	}
	return encodeDigest(value, "artifact manifest")
}

func CanonicalArtifactProvenanceReceipt(value ArtifactProvenanceReceipt) ([]byte, string, error) {
	value.SHA256 = ""
	value.Entries = slices.Clone(value.Entries)
	slices.SortFunc(value.Entries, compareArtifactProvenance)
	if err := validateArtifactProvenanceReceipt(value, false); err != nil {
		return nil, "", err
	}
	return encodeDigest(value, "artifact provenance receipt")
}

func CanonicalNumberReservation(value NumberReservation) ([]byte, string, error) {
	value.SHA256 = ""
	value.Numbers = slices.Clone(value.Numbers)
	slices.SortFunc(value.Numbers, func(left, right AssignedNumber) int {
		if left.MemberOrdinal != right.MemberOrdinal {
			if left.MemberOrdinal < right.MemberOrdinal {
				return -1
			}
			return 1
		}
		if left.Page != right.Page {
			if left.Page < right.Page {
				return -1
			}
			return 1
		}
		return compareString(left.Text, right.Text)
	})
	if err := validateNumberReservation(value, false); err != nil {
		return nil, "", err
	}
	return encodeDigest(value, "number reservation")
}

func CanonicalPreparedInputReceipt(value PreparedInputReceipt) ([]byte, string, error) {
	value.SHA256 = ""
	if err := validatePreparedInputReceipt(value, false); err != nil {
		return nil, "", err
	}
	return encodeDigest(value, "prepared input receipt")
}

func CanonicalProductionReceipt(value ProductionReceipt) ([]byte, string, error) {
	value.SHA256 = ""
	if err := validateProductionReceipt(value, false); err != nil {
		return nil, "", err
	}
	return encodeDigest(value, "production receipt")
}

func CanonicalRetentionReceipt(value RetentionReceipt) ([]byte, string, error) {
	value.SHA256 = ""
	if err := validateRetentionReceipt(value, false); err != nil {
		return nil, "", err
	}
	return encodeDigest(value, "retention receipt")
}

func CanonicalReproductionRequest(value ReproductionRequest) ([]byte, string, error) {
	value.ArtifactIDs = canonicalStrings(value.ArtifactIDs)
	if err := validateReproductionRequest(value); err != nil {
		return nil, "", err
	}
	return encodeDigest(value, "reproduction request")
}

func CanonicalReproductionReceipt(value ReproductionReceipt) ([]byte, string, error) {
	value.SHA256 = ""
	if err := validateReproductionReceipt(value, false); err != nil {
		return nil, "", err
	}
	return encodeDigest(value, "reproduction receipt")
}

func EvaluateApproval(grant ApprovalGrant, events []ApprovalEvent, subjectSHA256 string, at time.Time) (ApprovalEvaluation, error) {
	if err := ValidateApprovalGrant(grant); err != nil {
		return ApprovalEvaluation{}, err
	}
	if at.IsZero() || at.Location() != time.UTC || !canonical.IsSHA256Hex(subjectSHA256) {
		return ApprovalEvaluation{}, invalidProblem("invalid approval evaluation input")
	}
	grantedAt, _ := time.Parse(time.RFC3339Nano, grant.GrantedAt)
	if at.Before(grantedAt) {
		return ApprovalEvaluation{}, invalidProblem("approval evaluation predates grant")
	}
	_, eventsDigest, err := CanonicalApprovalEvents(events)
	if err != nil {
		return ApprovalEvaluation{}, err
	}
	state := ApprovalStateCurrent
	if grant.SubjectSHA256 != subjectSHA256 {
		state = ApprovalStateStaleSubject
	}
	ordered := slices.Clone(events)
	slices.SortFunc(ordered, compareApprovalEvents)
	for _, event := range ordered {
		if event.ApprovalID != grant.ID {
			return ApprovalEvaluation{}, invalidProblem("approval event targets another grant")
		}
		effectiveAt, _ := time.Parse(time.RFC3339Nano, event.EffectiveAt)
		if effectiveAt.Before(grantedAt) {
			return ApprovalEvaluation{}, invalidProblem("approval event predates grant")
		}
		if state == ApprovalStateCurrent {
			if effectiveAt.After(at) {
				continue
			}
			if event.Kind == ApprovalEventRevoke {
				state = ApprovalStateRevoked
			} else {
				state = ApprovalStateSuperseded
			}
		}
	}
	if state == ApprovalStateCurrent && grant.ExpiresAt != "" {
		expiresAt, _ := time.Parse(time.RFC3339Nano, grant.ExpiresAt)
		if !at.Before(expiresAt) {
			state = ApprovalStateExpired
		}
	}
	evaluation := ApprovalEvaluation{
		Contract: ApprovalEvaluationContractV1, ApprovalSHA256: grant.SHA256,
		EventsSHA256: eventsDigest, SubjectSHA256: subjectSHA256,
		EvaluatedAt: at.Format(time.RFC3339Nano), State: state,
	}
	_, evaluation.SHA256, err = CanonicalApprovalEvaluation(evaluation)
	return evaluation, err
}

func FreezePrivilegeLog(input PrivilegeLogFreezeInput) (PrivilegeLogReceipt, error) {
	if len(input.Rows) == 0 || len(input.Rows) > MaxPrivilegeRows || !canonicalUUID(input.LogID) || input.Revision < 1 ||
		!allSHA256(input.WithheldSelectionSHA256, input.PolicySHA256, input.PlayersSHA256) ||
		!optionalSHA256(input.ApprovalEvaluationSHA256) ||
		validateTimestamp(input.ValidatedAt) != nil || validateTimestamp(input.FrozenAt) != nil {
		return PrivilegeLogReceipt{}, invalidProblem("invalid privilege log freeze input")
	}
	validatedAt, _ := time.Parse(time.RFC3339Nano, input.ValidatedAt)
	frozenAt, _ := time.Parse(time.RFC3339Nano, input.FrozenAt)
	if frozenAt.Before(validatedAt) {
		return PrivilegeLogReceipt{}, invalidProblem("privilege log froze before validation")
	}
	_, inputsDigest, rowsDigest, err := canonicalPrivilegeLogInputs(input.PrivilegeLogValidationInput)
	if err != nil {
		return PrivilegeLogReceipt{}, err
	}
	receipt := PrivilegeLogReceipt{
		Contract: PrivilegeLogReceiptContractV1, LogID: input.LogID, Revision: input.Revision,
		State: PrivilegeLogStateFrozen, WithheldSelectionSHA256: input.WithheldSelectionSHA256,
		PolicySHA256: input.PolicySHA256, PlayersSHA256: input.PlayersSHA256,
		ApprovalEvaluationSHA256: input.ApprovalEvaluationSHA256, RowsSHA256: rowsDigest,
		InputsSHA256: inputsDigest, RowCount: len(input.Rows), ValidatedAt: input.ValidatedAt, FrozenAt: input.FrozenAt,
	}
	_, receipt.SHA256, err = CanonicalPrivilegeLogReceipt(receipt)
	return receipt, err
}

func PublicPrivilegeRows(values []PrivilegeRow) []PrivilegePublicRow {
	rows := slices.Clone(values)
	slices.SortFunc(rows, comparePrivilegeRows)
	public := make([]PrivilegePublicRow, 0, len(rows))
	for _, row := range rows {
		public = append(public, PrivilegePublicRow{ID: row.ID, WithheldMemberID: row.WithheldMemberID,
			FamilyOrder: row.FamilyOrder, SourceVersionID: row.SourceVersionID,
			Basis: row.Basis, PublicDescription: row.PublicDescription})
	}
	return public
}

func encodeDigest(value any, subject string) ([]byte, string, error) {
	encoded, err := canonical.Marshal(value)
	if err != nil {
		return nil, "", fmt.Errorf("canonicalize %s: %w", subject, err)
	}
	digest := sha256.Sum256(encoded)
	return encoded, hex.EncodeToString(digest[:]), nil
}

func canonicalStrings(values []string) []string {
	values = slices.Clone(values)
	if values == nil {
		values = []string{}
	}
	slices.Sort(values)
	return slices.Compact(values)
}

func comparePrivilegeRows(left, right PrivilegeRow) int {
	if left.FamilyOrder < right.FamilyOrder {
		return -1
	}
	if left.FamilyOrder > right.FamilyOrder {
		return 1
	}
	return compareString(left.ID, right.ID)
}

func compareApprovalEvents(left, right ApprovalEvent) int {
	leftTime, leftErr := time.Parse(time.RFC3339Nano, left.EffectiveAt)
	rightTime, rightErr := time.Parse(time.RFC3339Nano, right.EffectiveAt)
	if leftErr == nil && rightErr == nil {
		if compared := leftTime.Compare(rightTime); compared != 0 {
			return compared
		}
	} else if left.EffectiveAt != right.EffectiveAt {
		return compareString(left.EffectiveAt, right.EffectiveAt)
	}
	return compareString(left.ID, right.ID)
}

func compareArtifacts(left, right Artifact) int {
	if left.MemberOrdinal != right.MemberOrdinal {
		if left.MemberOrdinal < right.MemberOrdinal {
			return -1
		}
		return 1
	}
	if left.Page != right.Page {
		if left.Page < right.Page {
			return -1
		}
		return 1
	}
	if rank := artifactRoleRank(left.Role) - artifactRoleRank(right.Role); rank != 0 {
		return rank
	}
	if left.Path != right.Path {
		return compareString(left.Path, right.Path)
	}
	return compareString(left.ID, right.ID)
}

func compareArtifactProvenance(left, right ArtifactProvenance) int {
	if left.MemberOrdinal != right.MemberOrdinal {
		if left.MemberOrdinal < right.MemberOrdinal {
			return -1
		}
		return 1
	}
	if left.Page != right.Page {
		if left.Page < right.Page {
			return -1
		}
		return 1
	}
	if left.Volume != right.Volume {
		return compareString(left.Volume, right.Volume)
	}
	return compareString(left.ArtifactID, right.ArtifactID)
}

func compareString(left, right string) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}
