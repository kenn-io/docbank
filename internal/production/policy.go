package production

import (
	"errors"

	"github.com/google/uuid"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
)

// PreparedPolicyVersion is the exact immutable record handed to persistence.
// Storage owns uniqueness and idempotency for OperationID plus RequestSHA256.
type PreparedPolicyVersion struct {
	OperationID   string
	RequestSHA256 string
	PolicySHA256  string
	Canonical     []byte
	Policy        documentproduction.PolicyVersion
}

// PolicyVersionResolver reads an immutable user-supplied policy version.
type PolicyVersionResolver func(id string, version int64) (documentproduction.PolicyVersion, error)

// PreparePolicyVersion canonicalizes a user-supplied version before any write.
// The returned digest is both the immutable policy identity and the semantic
// request identity used to distinguish exact retries from changed payloads.
func PreparePolicyVersion(operationID string, value documentproduction.PolicyVersion) (PreparedPolicyVersion, error) {
	if !canonicalUUIDv4(operationID) {
		return PreparedPolicyVersion{}, invalidProductionContract("invalid policy operation ID")
	}
	if value.SHA256 != "" {
		return PreparedPolicyVersion{}, invalidProductionContract("caller supplied a policy self digest")
	}
	encoded, digest, err := documentproduction.CanonicalPolicyVersion(value)
	if err != nil {
		return PreparedPolicyVersion{}, err
	}
	value.SHA256 = digest
	return PreparedPolicyVersion{
		OperationID: operationID, RequestSHA256: digest, PolicySHA256: digest,
		Canonical: encoded, Policy: value,
	}, nil
}

// ResolveCreatePolicy converts an omitted policy reference into the immutable
// repository-owned generic policy. A custom reference is resolved from stored
// authority and always returned as a complete draft selection.
func ResolveCreatePolicy(request redaction.CreateRequest, resolve PolicyVersionResolver) (documentproduction.PolicyVersion, redaction.PolicySelection, error) {
	if err := redaction.ValidateCreateRequest(request); err != nil {
		return documentproduction.PolicyVersion{}, redaction.PolicySelection{}, err
	}
	var (
		policy documentproduction.PolicyVersion
		err    error
	)
	if request.PolicyID == "" {
		policy, err = documentproduction.GenericPolicyVersion()
	} else if resolve == nil {
		err = errors.New("production policy resolver is required")
	} else {
		policy, err = resolve(request.PolicyID, request.PolicyVersion)
	}
	if err != nil {
		return documentproduction.PolicyVersion{}, redaction.PolicySelection{}, err
	}
	if err := documentproduction.ValidatePolicyVersion(policy); err != nil {
		return documentproduction.PolicyVersion{}, redaction.PolicySelection{}, err
	}
	if request.PolicyID != "" && (policy.ID != request.PolicyID || policy.Version != request.PolicyVersion) {
		return documentproduction.PolicyVersion{}, redaction.PolicySelection{}, invalidProductionContract("resolved production policy differs from request")
	}
	selection := redaction.PolicySelection{
		PolicyID: policy.ID, Version: policy.Version, PolicySHA256: policy.SHA256,
	}
	return policy, selection, nil
}

func canonicalUUIDv4(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.Version() == 4 && parsed.String() == value
}

func invalidProductionContract(detail string) *documentproduction.Problem {
	return &documentproduction.Problem{Code: documentproduction.ProblemInvalidContract, Detail: detail}
}
