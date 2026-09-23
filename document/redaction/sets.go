package redaction

import (
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
)

const (
	MaxInstructionsBytes = 64 << 10
	MaxChangesPerBatch   = 500
	MaxCommandBytes      = 1 << 20
	MaxProductionPage    = 200
	MaxProductionMembers = 100_000

	RecipeID300DPI  = "raster-redaction/v1-300dpi"
	RecipeID600DPI  = "raster-redaction/v1-600dpi"
	DefaultRecipeID = RecipeID300DPI

	DefaultOutputProfileID     = "redacted-dat-pdf-v1"
	DefaultDisclosureProfileID = "generated-fields-only-v1"
	BatesNumberingRecipeID     = "bates-sequential-v1"
)

var (
	productionCatalogIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[-_/][a-z0-9]+)*$`)
	productionRelationPattern  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
)

type Set struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Creator      string `json:"creator"`
	CreatedAt    string `json:"created_at"`
	HeadRevision int64  `json:"head_revision"`
}

type Draft struct {
	SetID                   string          `json:"set_id"`
	Revision                int64           `json:"revision"`
	ETag                    int64           `json:"etag"`
	InstructionsSHA256      string          `json:"instructions_sha256"`
	MemberHash              string          `json:"member_hash"`
	DecisionsSHA256         string          `json:"decisions_sha256"`
	RecipeID                string          `json:"recipe_id"`
	RecipeSHA256            string          `json:"recipe_sha256"`
	ProfileID               string          `json:"profile_id"`
	ProfileSHA256           string          `json:"profile_sha256"`
	DisclosureProfileID     string          `json:"disclosure_profile_id"`
	DisclosureProfileSHA256 string          `json:"disclosure_profile_sha256"`
	NumberingRecipeID       string          `json:"numbering_recipe_id"`
	NumberingRecipeSHA256   string          `json:"numbering_recipe_sha256"`
	Policy                  PolicySelection `json:"policy"`
	State                   string          `json:"state"`
	MembershipSealed        bool            `json:"membership_sealed"`
}

type CreateRequest struct {
	OperationID         string `json:"operation_id"`
	Name                string `json:"name"`
	Instructions        string `json:"instructions"`
	RecipeID            string `json:"recipe_id,omitzero"`
	ProfileID           string `json:"profile_id,omitzero"`
	DisclosureProfileID string `json:"disclosure_profile_id,omitzero"`
	NumberingRecipeID   string `json:"numbering_recipe_id,omitzero"`
	PolicyID            string `json:"policy_id,omitzero"`
	PolicyVersion       int64  `json:"policy_version,omitzero"`
}

type Change struct {
	Kind                string    `json:"kind"`
	MemberID            string    `json:"member_id,omitzero"`
	Member              *Member   `json:"member,omitzero"`
	Decision            *Decision `json:"decision,omitzero"`
	DecisionID          string    `json:"decision_id,omitzero"`
	Mode                string    `json:"mode,omitzero"`
	RecipeID            string    `json:"recipe_id,omitzero"`
	ProfileID           string    `json:"profile_id,omitzero"`
	DisclosureProfileID string    `json:"disclosure_profile_id,omitzero"`
	NumberingRecipeID   string    `json:"numbering_recipe_id,omitzero"`
	PolicyID            string    `json:"policy_id,omitzero"`
	PolicyVersion       int64     `json:"policy_version,omitzero"`
}

type ApplyRequest struct {
	OperationID string   `json:"operation_id"`
	ETag        int64    `json:"etag"`
	Changes     []Change `json:"changes"`
}

type InstructionsEditRequest struct {
	OperationID  string `json:"operation_id"`
	ETag         int64  `json:"etag"`
	Instructions string `json:"instructions"`
}

type MembershipSealRequest struct {
	OperationID string `json:"operation_id"`
	ETag        int64  `json:"etag"`
	Total       int    `json:"total"`
	MemberHash  string `json:"member_hash"`
}

type Receipt struct {
	OperationID   string `json:"operation_id"`
	SetID         string `json:"set_id"`
	Revision      int64  `json:"revision"`
	ETag          int64  `json:"etag"`
	RequestSHA256 string `json:"request_sha256"`
}

func ValidateCreateRequest(value CreateRequest) error {
	if !canonicalUUIDv4(value.OperationID) || invalidProductionText(value.Name, 256, false) ||
		invalidProductionText(value.Instructions, MaxInstructionsBytes, true) {
		return errors.New("invalid production-set create request")
	}
	if !validOptionalCatalogID(value.RecipeID) || !validOptionalCatalogID(value.ProfileID) ||
		!validOptionalCatalogID(value.DisclosureProfileID) || !validOptionalCatalogID(value.NumberingRecipeID) ||
		!validOptionalPolicyReference(value.PolicyID, value.PolicyVersion) {
		return errors.New("invalid production-set catalog reference")
	}
	return nil
}

func ValidateSet(value Set) error {
	createdAt, err := time.Parse(time.RFC3339Nano, value.CreatedAt)
	if !canonicalUUIDv4(value.ID) || invalidProductionText(value.Name, 256, false) ||
		invalidProductionActor(value.Creator) || err != nil || createdAt.Location() != time.UTC || value.HeadRevision < 1 {
		return errors.New("invalid production set")
	}
	return nil
}

func ValidateDraft(value Draft) error {
	if !canonicalUUIDv4(value.SetID) || value.Revision < 1 || value.ETag < 1 ||
		!canonical.IsSHA256Hex(value.InstructionsSHA256) || !canonical.IsSHA256Hex(value.MemberHash) ||
		!canonical.IsSHA256Hex(value.DecisionsSHA256) || !canonical.IsSHA256Hex(value.RecipeSHA256) ||
		!canonical.IsSHA256Hex(value.ProfileSHA256) || !canonical.IsSHA256Hex(value.DisclosureProfileSHA256) ||
		!canonical.IsSHA256Hex(value.NumberingRecipeSHA256) || !validPolicySelection(value.Policy) ||
		!validOptionalCatalogID(value.RecipeID) || !validOptionalCatalogID(value.ProfileID) ||
		!validOptionalCatalogID(value.DisclosureProfileID) || !validOptionalCatalogID(value.NumberingRecipeID) ||
		value.RecipeID == "" || value.ProfileID == "" || value.DisclosureProfileID == "" ||
		value.State != "draft" && value.State != "finalized" {
		return errors.New("invalid production draft")
	}
	return nil
}

func ValidateReceipt(value Receipt) error {
	if !canonicalUUIDv4(value.OperationID) || !canonicalUUIDv4(value.SetID) || value.Revision < 1 || value.ETag < 1 ||
		!canonical.IsSHA256Hex(value.RequestSHA256) {
		return errors.New("invalid production receipt")
	}
	return nil
}

func ValidateApplyRequest(value ApplyRequest) error {
	if !canonicalUUIDv4(value.OperationID) || value.ETag < 1 || len(value.Changes) < 1 || len(value.Changes) > MaxChangesPerBatch {
		return errors.New("invalid production-set change batch")
	}
	for index, change := range value.Changes {
		if err := ValidateChange(change); err != nil {
			return fmt.Errorf("change %d: %w", index, err)
		}
	}
	return nil
}

func ValidateInstructionsEditRequest(value InstructionsEditRequest) error {
	if !canonicalUUIDv4(value.OperationID) || value.ETag < 1 || invalidProductionText(value.Instructions, MaxInstructionsBytes, true) {
		return errors.New("invalid production-set instructions edit")
	}
	return nil
}

func ValidateMembershipSealRequest(value MembershipSealRequest) error {
	if !canonicalUUIDv4(value.OperationID) || value.ETag < 1 || value.Total < 1 || value.Total > MaxProductionMembers || !canonical.IsSHA256Hex(value.MemberHash) {
		return errors.New("invalid production membership seal")
	}
	return nil
}

func ValidateChange(value Change) error {
	switch value.Kind {
	case "member":
		if value.Member == nil || value.MemberID != "" && value.MemberID != value.Member.ID || hasNonMemberChangeFields(value) {
			return errors.New("invalid member change")
		}
		return ValidateMember(*value.Member)
	case "member_remove":
		if !canonicalUUIDv4(value.MemberID) || value.Member != nil || hasNonMemberChangeFields(value) {
			return errors.New("invalid member removal")
		}
	case "decision":
		if value.Decision == nil || value.DecisionID != "" && value.DecisionID != value.Decision.ID || hasNonDecisionChangeFields(value) {
			return errors.New("invalid decision change")
		}
		return ValidateDecision(*value.Decision)
	case "decision_remove":
		if !canonicalUUIDv4(value.DecisionID) || value.Decision != nil || hasNonDecisionChangeFields(value) {
			return errors.New("invalid decision removal")
		}
	case "mode":
		if !canonicalUUIDv4(value.MemberID) || !validMode(value.Mode) || value.Member != nil || value.Decision != nil || value.DecisionID != "" || value.RecipeID != "" || value.ProfileID != "" || value.DisclosureProfileID != "" || value.NumberingRecipeID != "" || value.PolicyID != "" || value.PolicyVersion != 0 {
			return errors.New("invalid member mode change")
		}
	case "recipe":
		if value.RecipeID != RecipeID300DPI && value.RecipeID != RecipeID600DPI || hasIdentityOrPayload(value) || value.ProfileID != "" || value.DisclosureProfileID != "" || value.NumberingRecipeID != "" || value.PolicyID != "" || value.PolicyVersion != 0 {
			return errors.New("invalid recipe change")
		}
	case "profile":
		if hasIdentityOrPayload(value) || value.RecipeID != "" || !validOptionalCatalogID(value.ProfileID) || !validOptionalCatalogID(value.DisclosureProfileID) || !validOptionalCatalogID(value.NumberingRecipeID) || value.PolicyID != "" || value.PolicyVersion != 0 {
			return errors.New("invalid profile change")
		}
	case "policy":
		if hasIdentityOrPayload(value) || value.RecipeID != "" || value.ProfileID != "" || value.DisclosureProfileID != "" || value.NumberingRecipeID != "" || !validRequiredPolicyReference(value.PolicyID, value.PolicyVersion) {
			return errors.New("invalid policy change")
		}
	default:
		return errors.New("unknown production change kind")
	}
	return nil
}

func ValidateMember(value Member) error {
	if !canonicalUUIDv4(value.ID) || !canonicalUUIDv4(value.VaultID) || !canonicalUUIDv4(value.SourceVersionID) ||
		value.NodeID < 1 || value.SourceSize < 0 || value.PDFSize < 1 || value.Ordinal < 1 || value.Ordinal > MaxProductionMembers ||
		!canonical.IsSHA256Hex(value.SourceSHA256) || !canonical.IsSHA256Hex(value.PDFSHA256) ||
		!canonical.IsSHA256Hex(value.MapSHA256) || !canonical.IsSHA256Hex(value.PageInventorySHA256) || !validMode(value.Mode) {
		return errors.New("invalid production member")
	}
	if err := ValidateFamilyContext(value.Family, value.SourceVersionID); err != nil {
		return err
	}
	if value.Reviewed != (canonical.IsSHA256Hex(value.ReviewBinding)) || !value.Reviewed && value.ReviewBinding != "" {
		return errors.New("invalid production member review binding")
	}
	return nil
}

func ValidateFamilyContext(value FamilyContext, sourceVersionID string) error {
	if !canonicalUUIDv4(value.RootVersionID) {
		return errors.New("invalid production member family root")
	}
	switch value.Kind {
	case "standalone", "email_message", "transcript":
		if value.RootVersionID != sourceVersionID || value.RelationOperationID != "" || value.RelationOrder != 0 {
			return errors.New("invalid self-contained production member family")
		}
	case "email_attachment":
		if !productionRelationPattern.MatchString(value.RelationOperationID) || value.RelationOrder < 1 {
			return errors.New("invalid production member family relation")
		}
	default:
		return errors.New("invalid production member family kind")
	}
	return nil
}

func validMode(value string) bool { return value == "redact_selected" || value == "keep_selected" }

func validOptionalCatalogID(value string) bool {
	return value == "" || len(value) <= 128 && productionCatalogIDPattern.MatchString(value)
}

func validPolicySelection(value PolicySelection) bool {
	return validRequiredPolicyReference(value.PolicyID, value.Version) && canonical.IsSHA256Hex(value.PolicySHA256)
}

func validOptionalPolicyReference(id string, version int64) bool {
	return id == "" && version == 0 || validRequiredPolicyReference(id, version)
}

func validRequiredPolicyReference(id string, version int64) bool {
	return canonicalUUIDv4(id) && version > 0
}

func invalidProductionText(value string, maximum int, emptyAllowed bool) bool {
	if !utf8.ValidString(value) || len(value) > maximum || !emptyAllowed && len(value) == 0 {
		return true
	}
	for _, char := range value {
		if unicode.IsControl(char) && char != '\n' && char != '\r' && char != '\t' {
			return true
		}
	}
	return false
}

func invalidProductionActor(value string) bool {
	if len(value) == 0 || len(value) > 256 || !utf8.ValidString(value) {
		return true
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return true
		}
	}
	return false
}

func hasIdentityOrPayload(value Change) bool {
	return value.MemberID != "" || value.Member != nil || value.Decision != nil || value.DecisionID != "" || value.Mode != ""
}

func hasNonMemberChangeFields(value Change) bool {
	return value.Decision != nil || value.DecisionID != "" || value.Mode != "" || value.RecipeID != "" || value.ProfileID != "" || value.DisclosureProfileID != "" || value.NumberingRecipeID != "" || value.PolicyID != "" || value.PolicyVersion != 0
}

func hasNonDecisionChangeFields(value Change) bool {
	return value.MemberID != "" || value.Member != nil || value.Mode != "" || value.RecipeID != "" || value.ProfileID != "" || value.DisclosureProfileID != "" || value.NumberingRecipeID != "" || value.PolicyID != "" || value.PolicyVersion != 0
}
