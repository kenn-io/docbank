package production

import (
	"encoding/hex"
	"errors"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
)

func validatePolicyVersion(value PolicyVersion, requireDigest bool) error {
	if value.Contract != PolicyContractV1 || !canonicalUUID(value.ID) || value.Version < 1 ||
		invalidText(value.Name, 256, false) || validateTimestamp(value.CreatedAt) != nil ||
		len(value.Rules) == 0 || len(value.Rules) > MaxPolicyRules ||
		requireDigest && !canonical.IsSHA256Hex(value.SHA256) || !requireDigest && value.SHA256 != "" {
		return invalidProblem("invalid production policy")
	}
	seen := make(map[string]struct{}, len(value.Rules))
	for _, rule := range value.Rules {
		if err := validatePolicyRule(rule); err != nil {
			return err
		}
		if _, duplicate := seen[rule.ID]; duplicate {
			return invalidProblem("duplicate production policy rule")
		}
		seen[rule.ID] = struct{}{}
	}
	if invalidOptionalText(value.Output.ConfidentialityLabel, 256) || invalidOptionalText(value.Output.EndorsementKind, 64) ||
		invalidOptionalText(value.Output.EndorsementText, MaxPolicyTextBytes) ||
		value.ConflictMode != PolicyConflictReject ||
		value.Approval.MaxAgeSeconds < 0 || value.Approval.MaxAgeSeconds > int64((365*24*time.Hour)/time.Second) ||
		!value.Approval.Required && (value.Approval.EvidenceRequired || value.Approval.MaxAgeSeconds != 0) ||
		len(value.PrivilegeLog.RequiredFields) > MaxPrivilegeFields || len(value.PrivilegeLog.AllowedBases) > MaxPrivilegeFields ||
		!value.PrivilegeLog.Required && (value.PrivilegeLog.RequireFrozenReceipt || len(value.PrivilegeLog.RequiredFields) != 0 || len(value.PrivilegeLog.AllowedBases) != 0) ||
		value.PrivilegeLog.Required && (!value.PrivilegeLog.RequireFrozenReceipt || len(value.PrivilegeLog.RequiredFields) == 0 || len(value.PrivilegeLog.AllowedBases) == 0) {
		return invalidProblem("invalid production policy requirements")
	}
	for _, field := range append(slices.Clone(value.PrivilegeLog.RequiredFields), value.PrivilegeLog.AllowedBases...) {
		if invalidText(field, 128, false) {
			return invalidProblem("invalid production policy vocabulary")
		}
	}
	return nil
}

func validatePolicyRule(value PolicyRule) error {
	if invalidText(value.ID, 128, false) || invalidText(value.Predicate.Field, 256, false) ||
		len(value.Predicate.Values) > MaxPolicyValues || !oneOf(value.Kind,
		PolicyRuleScope, PolicyRuleDate, PolicyRuleFamily, PolicyRuleDisposition, PolicyRuleLabel) ||
		!oneOf(value.Predicate.Operator, PolicyOperatorEquals, PolicyOperatorOneOf, PolicyOperatorPresent, PolicyOperatorDateBetween) ||
		invalidOptionalText(value.RequiredLabel, 256) {
		return invalidProblem("invalid production policy rule")
	}
	for _, item := range value.Predicate.Values {
		if invalidText(item, 1024, false) {
			return invalidProblem("invalid production policy predicate")
		}
	}
	switch value.Predicate.Operator {
	case PolicyOperatorEquals:
		if len(value.Predicate.Values) != 1 || value.Predicate.From != "" || value.Predicate.Through != "" {
			return invalidProblem("invalid equals predicate")
		}
	case PolicyOperatorOneOf:
		if len(value.Predicate.Values) == 0 || value.Predicate.From != "" || value.Predicate.Through != "" {
			return invalidProblem("invalid one-of predicate")
		}
	case PolicyOperatorPresent:
		if len(value.Predicate.Values) != 0 || value.Predicate.From != "" || value.Predicate.Through != "" {
			return invalidProblem("invalid present predicate")
		}
	case PolicyOperatorDateBetween:
		if len(value.Predicate.Values) != 0 || !canonicalDate(value.Predicate.From) || !canonicalDate(value.Predicate.Through) || value.Predicate.From > value.Predicate.Through {
			return invalidProblem("invalid date predicate")
		}
	}
	if value.Disposition != "" && !oneOf(value.Disposition, PolicyDispositionProduce, PolicyDispositionWithhold) ||
		value.FamilyMode != "" && !oneOf(value.FamilyMode, PolicyFamilyMember, PolicyFamilyComplete) {
		return invalidProblem("invalid production policy effect")
	}
	if value.Kind == PolicyRuleFamily && value.FamilyMode == "" || value.Kind != PolicyRuleFamily && value.FamilyMode != "" ||
		value.Kind == PolicyRuleLabel && value.RequiredLabel == "" || value.Kind != PolicyRuleLabel && value.RequiredLabel != "" ||
		value.Kind == PolicyRuleDisposition && value.Disposition == "" {
		return invalidProblem("invalid production policy union")
	}
	return nil
}

func validateApprovalSubject(value ApprovalSubject) error {
	if value.Contract != ApprovalSubjectContractV1 || !canonicalUUID(value.SetID) || value.Revision < 1 ||
		len(value.Members) == 0 || len(value.Members) > redaction.MaxProductionMembers ||
		!allSHA256(value.InstructionsSHA256, value.RecipeSHA256, value.OutputProfileSHA256,
			value.DisclosureProfileSHA256, value.NumberingPolicySHA256, value.Policy.PolicySHA256) ||
		!canonicalUUID(value.Policy.PolicyID) || value.Policy.Version < 1 ||
		!optionalSHA256(value.WithheldSelectionSHA256) || !optionalSHA256(value.PrivilegeLogInputsSHA256) {
		return invalidProblem("invalid approval subject")
	}
	seenIDs := make(map[string]struct{}, len(value.Members))
	seenOrdinals := make(map[int64]struct{}, len(value.Members))
	for _, member := range value.Members {
		if !canonicalUUID(member.MemberID) || !canonicalUUID(member.SourceVersionID) || member.Ordinal < 1 ||
			member.SourceSize < 0 || !allSHA256(member.SourceSHA256, member.PDFSHA256, member.PageInventorySHA256,
			member.MapSHA256, member.DecisionsSHA256, member.ResolvedSHA256) {
			return invalidProblem("invalid approval member")
		}
		if _, duplicate := seenIDs[member.MemberID]; duplicate {
			return invalidProblem("duplicate approval member")
		}
		if _, duplicate := seenOrdinals[member.Ordinal]; duplicate {
			return invalidProblem("duplicate approval member ordinal")
		}
		seenIDs[member.MemberID], seenOrdinals[member.Ordinal] = struct{}{}, struct{}{}
	}
	return nil
}

func validateApprovalGrant(value ApprovalGrant, requireDigest bool) error {
	if value.Contract != ApprovalGrantContractV1 || !canonicalUUID(value.ID) || !canonical.IsSHA256Hex(value.SubjectSHA256) ||
		invalidText(value.Actor, 256, false) || value.AuthorityKind != ApprovalAuthorityAuthenticatedHuman ||
		!canonical.IsSHA256Hex(value.AuthoritySHA256) || invalidOptionalText(value.Evidence, MaxApprovalEvidenceBytes) ||
		validateTimestamp(value.GrantedAt) != nil || value.ExpiresAt != "" && validateTimestamp(value.ExpiresAt) != nil ||
		requireDigest && !canonical.IsSHA256Hex(value.SHA256) || !requireDigest && value.SHA256 != "" {
		return invalidProblem("invalid approval grant")
	}
	if value.ExpiresAt != "" {
		grantedAt, _ := time.Parse(time.RFC3339Nano, value.GrantedAt)
		expiresAt, _ := time.Parse(time.RFC3339Nano, value.ExpiresAt)
		if !expiresAt.After(grantedAt) {
			return invalidProblem("approval expiry must follow grant")
		}
	}
	return nil
}

func validateApprovalAuthority(value ApprovalAuthority) error {
	if value.Contract != ApprovalAuthorityContractV1 || value.Kind != ApprovalAuthorityAuthenticatedHuman ||
		invalidText(value.PrincipalID, 256, false) || invalidText(value.AuthenticationMethod, 128, false) ||
		validateTimestamp(value.AuthenticatedAt) != nil || !canonical.IsSHA256Hex(value.EvidenceSHA256) {
		return invalidProblem("invalid approval authority")
	}
	return nil
}

func validateApprovalEvent(value ApprovalEvent) error {
	if value.Contract != ApprovalEventContractV1 || !canonicalUUID(value.ID) || !canonicalUUID(value.ApprovalID) ||
		!oneOf(value.Kind, ApprovalEventRevoke, ApprovalEventSupersede) || validateTimestamp(value.EffectiveAt) != nil ||
		invalidOptionalText(value.Reason, MaxApprovalEvidenceBytes) {
		return invalidProblem("invalid approval event")
	}
	if value.Kind == ApprovalEventSupersede && !canonicalUUID(value.ReplacementApprovalID) ||
		value.Kind == ApprovalEventRevoke && value.ReplacementApprovalID != "" || value.ReplacementApprovalID == value.ApprovalID {
		return invalidProblem("invalid approval event union")
	}
	return nil
}

func validateApprovalEvaluation(value ApprovalEvaluation, requireDigest bool) error {
	if value.Contract != ApprovalEvaluationContractV1 || !allSHA256(value.ApprovalSHA256, value.EventsSHA256, value.SubjectSHA256) ||
		validateTimestamp(value.EvaluatedAt) != nil || !oneOf(value.State, ApprovalStateCurrent, ApprovalStateExpired,
		ApprovalStateRevoked, ApprovalStateSuperseded, ApprovalStateStaleSubject) ||
		requireDigest && !canonical.IsSHA256Hex(value.SHA256) || !requireDigest && value.SHA256 != "" {
		return invalidProblem("invalid approval evaluation")
	}
	return nil
}

func validateWithheldSelection(value WithheldSelection, requireDigest bool) error {
	if value.Contract != WithheldSelectionContractV1 || !canonicalUUID(value.ID) || !canonicalUUID(value.SetID) ||
		value.Revision < 1 || !canonical.IsSHA256Hex(value.PolicySHA256) || len(value.Members) == 0 ||
		len(value.Members) > redaction.MaxProductionMembers || requireDigest && !canonical.IsSHA256Hex(value.SHA256) ||
		!requireDigest && value.SHA256 != "" {
		return invalidProblem("invalid withheld selection")
	}
	seenIDs, seenOrdinals := map[string]struct{}{}, map[int64]struct{}{}
	seenFamilyOrder := make(map[struct {
		root  string
		order int64
	}]struct{}, len(value.Members))
	for _, member := range value.Members {
		if !canonicalUUID(member.ID) || !canonicalUUID(member.SourceVersionID) || member.Ordinal < 1 || member.SourceSize < 0 ||
			member.FamilyOrder < 1 || !canonical.IsSHA256Hex(member.SourceSHA256) {
			return invalidProblem("invalid withheld member")
		}
		if err := redaction.ValidateFamilyContext(member.Family, member.SourceVersionID); err != nil {
			return invalidProblem("invalid withheld family context")
		}
		if _, duplicate := seenIDs[member.ID]; duplicate {
			return invalidProblem("duplicate withheld member")
		}
		if _, duplicate := seenOrdinals[member.Ordinal]; duplicate {
			return invalidProblem("duplicate withheld member ordinal")
		}
		familyOrder := struct {
			root  string
			order int64
		}{member.Family.RootVersionID, member.FamilyOrder}
		if _, duplicate := seenFamilyOrder[familyOrder]; duplicate {
			return invalidProblem("duplicate withheld family order")
		}
		seenIDs[member.ID], seenOrdinals[member.Ordinal] = struct{}{}, struct{}{}
		seenFamilyOrder[familyOrder] = struct{}{}
	}
	return nil
}

func validatePlayersSnapshot(value PlayersSnapshot, requireDigest bool) error {
	if value.Contract != PlayersSnapshotContractV1 || !canonicalUUID(value.ID) || value.Revision < 1 ||
		len(value.Players) == 0 || len(value.Players) > MaxPrivilegeRows ||
		requireDigest && !canonical.IsSHA256Hex(value.SHA256) || !requireDigest && value.SHA256 != "" {
		return invalidProblem("invalid players snapshot")
	}
	seen := make(map[string]struct{}, len(value.Players))
	for _, player := range value.Players {
		if !canonicalUUID(player.ID) || invalidText(player.DisplayName, 256, false) || len(player.Aliases) > MaxPrivilegeFields ||
			!canonical.IsSHA256Hex(player.EvidenceSHA256) {
			return invalidProblem("invalid player")
		}
		for _, alias := range player.Aliases {
			if invalidText(alias, 256, false) {
				return invalidProblem("invalid player alias")
			}
		}
		if _, duplicate := seen[player.ID]; duplicate {
			return invalidProblem("duplicate player")
		}
		seen[player.ID] = struct{}{}
	}
	return nil
}

func validatePrivilegeRow(value PrivilegeRow) error {
	if !canonicalUUID(value.ID) || !canonicalUUID(value.WithheldMemberID) || !canonicalUUID(value.SourceVersionID) ||
		value.FamilyOrder < 1 || invalidText(value.Basis, 128, false) || invalidText(value.PublicDescription, MaxPolicyTextBytes, false) ||
		invalidOptionalText(value.PrivateRationale, MaxPolicyTextBytes) || !canonical.IsSHA256Hex(value.EvidenceSHA256) ||
		len(value.PersonIDs) == 0 || len(value.PersonIDs) > MaxPrivilegeFields || len(value.Fields) > MaxPrivilegeFields {
		return invalidProblem("invalid privilege row")
	}
	seenPeople := make(map[string]struct{}, len(value.PersonIDs))
	for _, id := range value.PersonIDs {
		if !canonicalUUID(id) {
			return invalidProblem("invalid privilege row person")
		}
		if _, duplicate := seenPeople[id]; duplicate {
			return invalidProblem("duplicate privilege row person")
		}
		seenPeople[id] = struct{}{}
	}
	seenFields := make(map[string]struct{}, len(value.Fields))
	for _, field := range value.Fields {
		if invalidText(field.Name, 128, false) || invalidText(field.Value, MaxPolicyTextBytes, false) {
			return invalidProblem("invalid privilege row field")
		}
		if _, duplicate := seenFields[field.Name]; duplicate {
			return invalidProblem("duplicate privilege row field")
		}
		seenFields[field.Name] = struct{}{}
	}
	return nil
}

func validatePrivilegeLogReceipt(value PrivilegeLogReceipt, requireDigest bool) error {
	if value.Contract != PrivilegeLogReceiptContractV1 || !canonicalUUID(value.LogID) || value.Revision < 1 ||
		value.State != PrivilegeLogStateFrozen || !allSHA256(value.WithheldSelectionSHA256, value.PolicySHA256,
		value.PlayersSHA256, value.RowsSHA256, value.InputsSHA256) || !optionalSHA256(value.ApprovalEvaluationSHA256) ||
		value.RowCount < 1 || value.RowCount > MaxPrivilegeRows || validateTimestamp(value.ValidatedAt) != nil ||
		validateTimestamp(value.FrozenAt) != nil ||
		requireDigest && !canonical.IsSHA256Hex(value.SHA256) || !requireDigest && value.SHA256 != "" {
		return invalidProblem("invalid frozen privilege log receipt")
	}
	validatedAt, _ := time.Parse(time.RFC3339Nano, value.ValidatedAt)
	frozenAt, _ := time.Parse(time.RFC3339Nano, value.FrozenAt)
	if frozenAt.Before(validatedAt) {
		return invalidProblem("privilege log froze before validation")
	}
	return nil
}

func validatePrivilegeLogAttachment(value PrivilegeLogAttachmentReceipt, requireDigest bool) error {
	if value.Contract != PrivilegeLogAttachmentContractV1 || !canonicalUUID(value.ID) ||
		!allSHA256(value.PrivilegeLogReceiptSHA256, value.ProductionReceiptSHA256) || len(value.References) > MaxOutputReferences ||
		validateTimestamp(value.CreatedAt) != nil || requireDigest && !canonical.IsSHA256Hex(value.SHA256) ||
		!requireDigest && value.SHA256 != "" {
		return invalidProblem("invalid privilege log attachment")
	}
	seen := make(map[string]struct{}, len(value.References))
	for _, reference := range value.References {
		if !canonicalUUID(reference.WithheldMemberID) || invalidText(reference.AssignedNumber, 256, false) ||
			!canonical.IsSHA256Hex(reference.ArtifactSHA256) {
			return invalidProblem("invalid privilege output reference")
		}
		key := reference.WithheldMemberID + "\x00" + reference.AssignedNumber + "\x00" + reference.ArtifactSHA256
		if _, duplicate := seen[key]; duplicate {
			return invalidProblem("duplicate privilege output reference")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateArtifactManifest(value ArtifactManifest, requireDigest bool) error {
	if value.Contract != ArtifactManifestContractV1 || len(value.Artifacts) == 0 || len(value.Artifacts) > MaxArtifacts ||
		requireDigest && !canonical.IsSHA256Hex(value.SHA256) || !requireDigest && value.SHA256 != "" {
		return invalidProblem("invalid artifact manifest")
	}
	seenIDs, seenPaths := map[string]struct{}{}, map[string]struct{}{}
	for _, artifact := range value.Artifacts {
		if !canonicalUUID(artifact.ID) || !oneOf(artifact.Role, ArtifactRoleRedactedPDF, ArtifactRoleRedactedText,
			ArtifactRoleRedactedPage, ArtifactRoleDAT, ArtifactRoleOPT, ArtifactRoleLFP, ArtifactRoleManifest, ArtifactRoleArchive, ArtifactRoleQC) ||
			invalidArtifactPath(artifact.Path) || !canonical.IsSHA256Hex(artifact.SHA256) || artifact.Size < 0 ||
			invalidText(artifact.MediaType, 256, false) || invalidOptionalText(artifact.Volume, 128) || artifact.MemberOrdinal < 0 || artifact.Page < 0 {
			return invalidProblem("invalid production artifact")
		}
		memberRole := oneOf(artifact.Role, ArtifactRoleRedactedPDF, ArtifactRoleRedactedText, ArtifactRoleRedactedPage)
		if memberRole && (!canonicalUUID(artifact.MemberID) || artifact.MemberOrdinal < 1) || !memberRole && (artifact.MemberID != "" || artifact.MemberOrdinal != 0 || artifact.Page != 0) ||
			artifact.Role == ArtifactRoleRedactedPage && artifact.Page < 1 || artifact.Role != ArtifactRoleRedactedPage && artifact.Page != 0 {
			return invalidProblem("invalid production artifact union")
		}
		if _, duplicate := seenIDs[artifact.ID]; duplicate {
			return invalidProblem("duplicate production artifact ID")
		}
		if _, duplicate := seenPaths[artifact.Path]; duplicate {
			return invalidProblem("duplicate production artifact path")
		}
		seenIDs[artifact.ID], seenPaths[artifact.Path] = struct{}{}, struct{}{}
	}
	return nil
}

func validateArtifactProvenanceReceipt(value ArtifactProvenanceReceipt, requireDigest bool) error {
	if value.Contract != ArtifactProvenanceContractV1 || !canonicalUUID(value.ID) ||
		!allSHA256(value.ProductionReceiptSHA256, value.ArtifactManifestSHA256) || len(value.Entries) == 0 ||
		len(value.Entries) > MaxArtifacts || validateTimestamp(value.CreatedAt) != nil ||
		requireDigest && !canonical.IsSHA256Hex(value.SHA256) || !requireDigest && value.SHA256 != "" {
		return invalidProblem("invalid artifact provenance receipt")
	}
	seen := make(map[string]struct{}, len(value.Entries))
	for _, entry := range value.Entries {
		if !canonicalUUID(entry.ArtifactID) || !canonical.IsSHA256Hex(entry.ArtifactSHA256) ||
			!canonicalUUID(entry.SourceVersionID) || !canonicalUUID(entry.MemberID) || entry.MemberOrdinal < 1 ||
			entry.Page < 0 || invalidText(entry.Volume, 128, false) {
			return invalidProblem("invalid artifact provenance entry")
		}
		if _, duplicate := seen[entry.ArtifactID]; duplicate {
			return invalidProblem("duplicate artifact provenance entry")
		}
		seen[entry.ArtifactID] = struct{}{}
	}
	return nil
}

func validateNumberReservation(value NumberReservation, requireDigest bool) error {
	if value.Contract != NumberReservationContractV1 || invalidText(value.Authority, 128, false) ||
		!canonicalUUID(value.ID) || !canonicalUUID(value.OperationID) || !canonical.IsSHA256Hex(value.RevisionSHA256) ||
		value.State != "reserved" || len(value.Numbers) == 0 || len(value.Numbers) > MaxArtifacts ||
		requireDigest && !canonical.IsSHA256Hex(value.SHA256) || !requireDigest && value.SHA256 != "" {
		return invalidProblem("invalid number reservation receipt")
	}
	seenTexts := make(map[string]struct{}, len(value.Numbers))
	seenPages := make(map[struct {
		member string
		page   int
	}]struct{}, len(value.Numbers))
	for _, number := range value.Numbers {
		if !canonicalUUID(number.MemberID) || number.MemberOrdinal < 1 || number.Page < 1 ||
			invalidText(number.Text, 256, false) {
			return invalidProblem("invalid assigned number")
		}
		pageKey := struct {
			member string
			page   int
		}{number.MemberID, number.Page}
		if _, duplicate := seenTexts[number.Text]; duplicate {
			return invalidProblem("duplicate assigned number")
		}
		if _, duplicate := seenPages[pageKey]; duplicate {
			return invalidProblem("duplicate numbered page")
		}
		seenTexts[number.Text], seenPages[pageKey] = struct{}{}, struct{}{}
	}
	return nil
}

func validatePreparedInputReceipt(value PreparedInputReceipt, requireDigest bool) error {
	if value.Contract != PreparedInputContractV1 || !canonicalUUID(value.ID) ||
		!allSHA256(value.ApprovalSubjectSHA256, value.GateResultsSHA256) ||
		!optionalSHA256(value.ApprovalEvaluationSHA256) || !optionalSHA256(value.WithheldSelectionSHA256) ||
		!optionalSHA256(value.PrivilegeLogReceiptSHA256) || validateTimestamp(value.CreatedAt) != nil ||
		requireDigest && !canonical.IsSHA256Hex(value.SHA256) || !requireDigest && value.SHA256 != "" {
		return invalidProblem("invalid prepared input receipt")
	}
	return nil
}

func validateProductionReceipt(value ProductionReceipt, requireDigest bool) error {
	if value.Contract != ProductionReceiptContractV1 || !canonicalUUID(value.ID) || !canonicalUUID(value.JobID) ||
		!canonicalUUID(value.SetID) || value.Revision < 1 || !allSHA256(value.RevisionSHA256, value.PolicySHA256,
		value.NumberReservationSHA256, value.LayoutSHA256, value.EndorsementsSHA256, value.ArtifactManifestSHA256) ||
		!canonical.IsSHA256Hex(value.PreparedInputSHA256) || !optionalSHA256(value.ApprovalEvaluationSHA256) ||
		!optionalSHA256(value.WithheldSelectionSHA256) || !optionalSHA256(value.PrivilegeLogReceiptSHA256) ||
		validateTimestamp(value.CreatedAt) != nil || requireDigest && !canonical.IsSHA256Hex(value.SHA256) ||
		!requireDigest && value.SHA256 != "" {
		return invalidProblem("invalid production receipt")
	}
	return nil
}

func validateRetentionReceipt(value RetentionReceipt, requireDigest bool) error {
	if value.Contract != RetentionReceiptContractV1 || !canonicalUUID(value.ID) ||
		!allSHA256(value.ProductionReceiptSHA256, value.ArtifactManifestSHA256, value.MetadataSHA256,
			value.BlobRootsSHA256, value.AuditSHA256) || validateTimestamp(value.RetainedAt) != nil ||
		requireDigest && !canonical.IsSHA256Hex(value.SHA256) || !requireDigest && value.SHA256 != "" {
		return invalidProblem("invalid retention receipt")
	}
	return nil
}

func validateReproductionRequest(value ReproductionRequest) error {
	if value.Contract != ReproductionRequestContractV1 || !canonicalUUID(value.OperationID) ||
		!allSHA256(value.OriginalProductionReceiptSHA256, value.DeliveryPolicySHA256) ||
		len(value.ArtifactIDs) == 0 || len(value.ArtifactIDs) > MaxArtifacts {
		return invalidProblem("invalid reproduction request")
	}
	for _, id := range value.ArtifactIDs {
		if !canonicalUUID(id) {
			return invalidProblem("invalid reproduction artifact ID")
		}
	}
	return nil
}

func validateReproductionReceipt(value ReproductionReceipt, requireDigest bool) error {
	if value.Contract != ReproductionReceiptContractV1 || !canonicalUUID(value.ID) ||
		!allSHA256(value.OriginalProductionReceiptSHA256, value.OriginalNumberReservationSHA256,
			value.ArtifactManifestSHA256, value.PackageQCSHA256, value.DeliveryPolicySHA256) ||
		value.NumberAllocationCount != 0 || validateTimestamp(value.CreatedAt) != nil ||
		requireDigest && !canonical.IsSHA256Hex(value.SHA256) || !requireDigest && value.SHA256 != "" {
		return invalidProblem("invalid reproduction receipt")
	}
	return nil
}

// ValidateProductionReceipt checks a stored historical receipt against its
// admission-time authorities. It deliberately has no event-log parameter.
func ValidateProductionReceipt(value ProductionReceipt) error {
	if err := validateProductionReceipt(value, true); err != nil {
		return err
	}
	_, digest, err := CanonicalProductionReceipt(value)
	if err != nil {
		return err
	}
	if digest != value.SHA256 {
		return changedPayloadProblem(value.ID)
	}
	return nil
}

func ValidatePolicyVersion(value PolicyVersion) error {
	return validateStoredDigest(value.ID, value.SHA256, func() (string, error) {
		if err := validatePolicyVersion(value, true); err != nil {
			return "", err
		}
		_, digest, err := CanonicalPolicyVersion(value)
		return digest, err
	})
}

func ValidatePlayersSnapshot(value PlayersSnapshot) error {
	return validateStoredDigest(value.ID, value.SHA256, func() (string, error) {
		if err := validatePlayersSnapshot(value, true); err != nil {
			return "", err
		}
		_, digest, err := CanonicalPlayersSnapshot(value)
		return digest, err
	})
}

func ValidateApprovalGrant(value ApprovalGrant) error {
	return validateStoredDigest(value.ID, value.SHA256, func() (string, error) {
		if err := validateApprovalGrant(value, true); err != nil {
			return "", err
		}
		_, digest, err := CanonicalApprovalGrant(value)
		return digest, err
	})
}

func ValidateApprovalEvaluation(value ApprovalEvaluation) error {
	return validateStoredDigest("", value.SHA256, func() (string, error) {
		if err := validateApprovalEvaluation(value, true); err != nil {
			return "", err
		}
		_, digest, err := CanonicalApprovalEvaluation(value)
		return digest, err
	})
}

func ValidateWithheldSelection(value WithheldSelection) error {
	return validateStoredDigest(value.ID, value.SHA256, func() (string, error) {
		if err := validateWithheldSelection(value, true); err != nil {
			return "", err
		}
		_, digest, err := CanonicalWithheldSelection(value)
		return digest, err
	})
}

func ValidatePrivilegeLogReceipt(value PrivilegeLogReceipt) error {
	return validateStoredDigest(value.LogID, value.SHA256, func() (string, error) {
		if err := validatePrivilegeLogReceipt(value, true); err != nil {
			return "", err
		}
		_, digest, err := CanonicalPrivilegeLogReceipt(value)
		return digest, err
	})
}

func ValidatePrivilegeLogAttachment(value PrivilegeLogAttachmentReceipt) error {
	return validateStoredDigest(value.ID, value.SHA256, func() (string, error) {
		if err := validatePrivilegeLogAttachment(value, true); err != nil {
			return "", err
		}
		_, digest, err := CanonicalPrivilegeLogAttachment(value)
		return digest, err
	})
}

func ValidateArtifactManifest(value ArtifactManifest) error {
	return validateStoredDigest("", value.SHA256, func() (string, error) {
		if err := validateArtifactManifest(value, true); err != nil {
			return "", err
		}
		_, digest, err := CanonicalArtifactManifest(value)
		return digest, err
	})
}

func ValidateArtifactProvenanceReceipt(value ArtifactProvenanceReceipt) error {
	return validateStoredDigest(value.ID, value.SHA256, func() (string, error) {
		if err := validateArtifactProvenanceReceipt(value, true); err != nil {
			return "", err
		}
		_, digest, err := CanonicalArtifactProvenanceReceipt(value)
		return digest, err
	})
}

func ValidateNumberReservation(value NumberReservation) error {
	return validateStoredDigest(value.ID, value.SHA256, func() (string, error) {
		if err := validateNumberReservation(value, true); err != nil {
			return "", err
		}
		_, digest, err := CanonicalNumberReservation(value)
		return digest, err
	})
}

func ValidatePreparedInputReceipt(value PreparedInputReceipt) error {
	return validateStoredDigest(value.ID, value.SHA256, func() (string, error) {
		if err := validatePreparedInputReceipt(value, true); err != nil {
			return "", err
		}
		_, digest, err := CanonicalPreparedInputReceipt(value)
		return digest, err
	})
}

func ValidateRetentionReceipt(value RetentionReceipt) error {
	return validateStoredDigest(value.ID, value.SHA256, func() (string, error) {
		if err := validateRetentionReceipt(value, true); err != nil {
			return "", err
		}
		_, digest, err := CanonicalRetentionReceipt(value)
		return digest, err
	})
}

func ValidateReproductionReceipt(value ReproductionReceipt) error {
	return validateStoredDigest(value.ID, value.SHA256, func() (string, error) {
		if err := validateReproductionReceipt(value, true); err != nil {
			return "", err
		}
		_, digest, err := CanonicalReproductionReceipt(value)
		return digest, err
	})
}

func ValidateProblem(value Problem) error {
	if !oneOf(string(value.Code), string(ProblemInvalidContract), string(ProblemPolicyUnsatisfied),
		string(ProblemApprovalRequired), string(ProblemApprovalStale), string(ProblemPrivilegeLogRequired),
		string(ProblemPrivilegeLogStale), string(ProblemChangedPayload), string(ProblemSourceStale),
		string(ProblemArtifactMissing), string(ProblemArtifactMismatch), string(ProblemRetentionRequired), string(ProblemLimit)) ||
		invalidText(value.Detail, MaxProblemDetailBytes, false) || invalidOptionalText(value.SubjectID, 256) || len(value.IDs) > MaxProblemIDs {
		return invalidProblem("invalid bounded production problem")
	}
	for _, id := range value.IDs {
		if invalidText(id, 256, false) {
			return invalidProblem("invalid bounded production problem ID")
		}
	}
	return nil
}

func validateStoredDigest(subjectID, supplied string, calculate func() (string, error)) error {
	digest, err := calculate()
	if err != nil {
		return err
	}
	if digest != supplied {
		return changedPayloadProblem(subjectID)
	}
	return nil
}

func ValidateSelectionPartition(produced []redaction.Member, withheld WithheldSelection) error {
	if withheld.SHA256 == "" {
		if err := validateWithheldSelection(withheld, false); err != nil {
			return err
		}
	} else if err := ValidateWithheldSelection(withheld); err != nil {
		return err
	}
	withheldIDs := make(map[string]struct{}, len(withheld.Members))
	for _, member := range withheld.Members {
		withheldIDs[member.ID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(produced))
	for _, member := range produced {
		if !canonicalUUID(member.ID) || !canonicalUUID(member.SourceVersionID) || member.Ordinal < 1 {
			return invalidProblem("invalid produced selection member")
		}
		if _, duplicate := seen[member.ID]; duplicate {
			return invalidProblem("duplicate produced selection member")
		}
		if _, conflict := withheldIDs[member.ID]; conflict {
			return &Problem{Code: ProblemPolicyUnsatisfied, Detail: "produced and withheld selections overlap", SubjectID: member.ID}
		}
		seen[member.ID] = struct{}{}
	}
	return nil
}

func ApprovalGateProblem(required bool, evaluation *ApprovalEvaluation, subjectSHA256 string) error {
	if !required {
		return nil
	}
	if !canonical.IsSHA256Hex(subjectSHA256) {
		return invalidProblem("invalid approval gate subject")
	}
	if evaluation == nil {
		return &Problem{Code: ProblemApprovalRequired, Detail: "current approval is required"}
	}
	if err := validateApprovalEvaluation(*evaluation, true); err != nil {
		return err
	}
	_, digest, err := CanonicalApprovalEvaluation(*evaluation)
	if err != nil {
		return err
	}
	if digest != evaluation.SHA256 {
		return changedPayloadProblem(evaluation.SubjectSHA256)
	}
	if evaluation.State != ApprovalStateCurrent || evaluation.SubjectSHA256 != subjectSHA256 {
		return &Problem{Code: ProblemApprovalStale, Detail: "approval is not current"}
	}
	return nil
}

func PrivilegeLogGateProblem(required bool, receipt *PrivilegeLogReceipt, policySHA256, withheldSHA256, playersSHA256, approvalEvaluationSHA256, inputsSHA256 string) error {
	if !required {
		return nil
	}
	if receipt == nil {
		return &Problem{Code: ProblemPrivilegeLogRequired, Detail: "validated frozen privilege log is required"}
	}
	if err := ValidatePrivilegeLogReceipt(*receipt); err != nil {
		return err
	}
	if !allSHA256(policySHA256, withheldSHA256, playersSHA256, inputsSHA256) || !optionalSHA256(approvalEvaluationSHA256) {
		return invalidProblem("invalid privilege log gate authority")
	}
	if receipt.PolicySHA256 != policySHA256 || receipt.WithheldSelectionSHA256 != withheldSHA256 ||
		receipt.PlayersSHA256 != playersSHA256 || receipt.ApprovalEvaluationSHA256 != approvalEvaluationSHA256 ||
		receipt.InputsSHA256 != inputsSHA256 {
		return &Problem{Code: ProblemPrivilegeLogStale, Detail: "privilege log inputs changed after freeze", SubjectID: receipt.LogID}
	}
	return nil
}

func PolicyUnsatisfiedProblem(ids []string) error {
	ids = canonicalStrings(ids)
	if len(ids) > MaxProblemIDs {
		return &Problem{Code: ProblemLimit, Detail: "policy conflict list exceeds bounds"}
	}
	for _, id := range ids {
		if invalidText(id, 256, false) {
			return invalidProblem("invalid policy conflict ID")
		}
	}
	return &Problem{Code: ProblemPolicyUnsatisfied, Detail: "selected production policy is unsatisfied", IDs: ids}
}

func artifactRoleRank(value string) int {
	for index, role := range []string{ArtifactRoleRedactedPDF, ArtifactRoleRedactedText, ArtifactRoleRedactedPage,
		ArtifactRoleDAT, ArtifactRoleOPT, ArtifactRoleLFP, ArtifactRoleManifest, ArtifactRoleArchive, ArtifactRoleQC} {
		if value == role {
			return index
		}
	}
	return 100
}

func canonicalUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	compact := value[0:8] + value[9:13] + value[14:18] + value[19:23] + value[24:36]
	raw, err := hex.DecodeString(compact)
	return err == nil && len(raw) == 16 && hex.EncodeToString(raw) == compact && raw[6]>>4 == 4 && raw[8]>>6 == 2
}

func canonicalDate(value string) bool {
	parsed, err := time.Parse(time.DateOnly, value)
	return err == nil && parsed.Format(time.DateOnly) == value
}

func validateTimestamp(value string) error {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.Location() != time.UTC || parsed.Format(time.RFC3339Nano) != value {
		return errors.New("timestamp is not canonical UTC RFC3339")
	}
	return nil
}

func invalidText(value string, maximum int, emptyAllowed bool) bool {
	if !utf8.ValidString(value) || len(value) > maximum || !emptyAllowed && value == "" {
		return true
	}
	for _, char := range value {
		if unicode.IsControl(char) && char != '\n' && char != '\r' && char != '\t' {
			return true
		}
	}
	return false
}

func invalidOptionalText(value string, maximum int) bool {
	return value != "" && invalidText(value, maximum, true)
}

func invalidArtifactPath(value string) bool {
	return invalidText(value, MaxArtifactPathBytes, false) || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") ||
		isWindowsDrivePath(value) || path.Clean(value) != value || value == "." || value == ".." || strings.HasPrefix(value, "../")
}

func isWindowsDrivePath(value string) bool {
	return len(value) >= 2 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':'
}

func allSHA256(values ...string) bool {
	for _, value := range values {
		if !canonical.IsSHA256Hex(value) {
			return false
		}
	}
	return true
}

func optionalSHA256(value string) bool { return value == "" || canonical.IsSHA256Hex(value) }

func oneOf(value string, allowed ...string) bool { return slices.Contains(allowed, value) }

func invalidProblem(detail string) *Problem {
	return &Problem{Code: ProblemInvalidContract, Detail: boundedDetail(detail)}
}

func changedPayloadProblem(subjectID string) *Problem {
	return &Problem{Code: ProblemChangedPayload, Detail: "stored payload does not match its canonical digest", SubjectID: subjectID}
}

func boundedDetail(value string) string {
	if len(value) <= MaxProblemDetailBytes {
		return value
	}
	cut := MaxProblemDetailBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}
