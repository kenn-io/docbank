package production

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
	"unicode/utf8"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/canonical"
)

// AuthenticatedApproval is constructed from trusted request authentication,
// not decoded from an approval request. Actor is recorded as provenance; the
// canonical authenticated-human Authority is the proof used by the service.
type AuthenticatedApproval struct {
	Actor     string
	Authority documentproduction.ApprovalAuthority
}

// RecordApprovalRequest intentionally has no actor or authority fields.
type RecordApprovalRequest struct {
	OperationID string
	ApprovalID  string
	Subject     documentproduction.ApprovalSubject
	Evidence    string
}

// ApprovalRecord is the private immutable record prepared for an atomic store
// write. CanonicalSubject and Authority contain private authority material and
// must not be returned through general list, audit, error, or artifact output.
type ApprovalRecord struct {
	OperationID      string
	RequestSHA256    string
	SubjectSHA256    string
	CanonicalSubject []byte
	Subject          documentproduction.ApprovalSubject
	Authority        documentproduction.ApprovalAuthority
	AuthoritySHA256  string
	Grant            documentproduction.ApprovalGrant
}

type ApprovalEventRequest struct {
	OperationID           string
	EventID               string
	Kind                  string
	EffectiveAt           string
	ReplacementApprovalID string
	Reason                string
}

type ApprovalEventRecord struct {
	OperationID   string
	RequestSHA256 string
	Event         documentproduction.ApprovalEvent
}

// PrepareApprovalRecord binds a trusted authenticated-human authority to the
// exact canonical subject. It performs no content-based or legal inference.
func PrepareApprovalRecord(request RecordApprovalRequest, policy documentproduction.PolicyVersion, authenticated AuthenticatedApproval, grantedAt time.Time) (ApprovalRecord, error) {
	if !canonicalUUIDv4(request.OperationID) || !canonicalUUIDv4(request.ApprovalID) ||
		grantedAt.IsZero() || grantedAt.Location() != time.UTC {
		return ApprovalRecord{}, invalidProductionContract("invalid approval request")
	}
	if err := validateStoredPolicy(policy); err != nil {
		return ApprovalRecord{}, err
	}
	if err := validateApprovalEvidence(request.Evidence, policy.Approval.EvidenceRequired); err != nil {
		return ApprovalRecord{}, err
	}
	subjectBytes, subjectDigest, err := documentproduction.CanonicalApprovalSubject(request.Subject)
	if err != nil {
		return ApprovalRecord{}, err
	}
	if !subjectSelectsPolicy(request.Subject, policy) {
		return ApprovalRecord{}, invalidProductionContract("approval subject selects another policy")
	}
	authorityBytes, authorityDigest, err := documentproduction.CanonicalApprovalAuthority(authenticated.Authority)
	if err != nil {
		return ApprovalRecord{}, err
	}
	_ = authorityBytes // validation and digest are the authority boundary.
	authenticatedAt, _ := time.Parse(time.RFC3339Nano, authenticated.Authority.AuthenticatedAt)
	if authenticatedAt.After(grantedAt) {
		return ApprovalRecord{}, invalidProductionContract("approval predates authentication")
	}

	grant := documentproduction.ApprovalGrant{
		Contract: documentproduction.ApprovalGrantContractV1,
		ID:       request.ApprovalID, SubjectSHA256: subjectDigest,
		Actor: authenticated.Actor, AuthorityKind: documentproduction.ApprovalAuthorityAuthenticatedHuman,
		AuthoritySHA256: authorityDigest, Evidence: request.Evidence,
		GrantedAt: grantedAt.Format(time.RFC3339Nano),
	}
	if policy.Approval.MaxAgeSeconds != 0 {
		grant.ExpiresAt = grantedAt.Add(time.Duration(policy.Approval.MaxAgeSeconds) * time.Second).Format(time.RFC3339Nano)
	}
	_, grantDigest, err := documentproduction.CanonicalApprovalGrant(grant)
	if err != nil {
		return ApprovalRecord{}, err
	}
	grant.SHA256 = grantDigest
	requestDigest, err := approvalRequestDigest(request.ApprovalID, subjectDigest, request.Evidence)
	if err != nil {
		return ApprovalRecord{}, err
	}
	return ApprovalRecord{
		OperationID: request.OperationID, RequestSHA256: requestDigest,
		SubjectSHA256: subjectDigest, CanonicalSubject: subjectBytes, Subject: request.Subject,
		Authority: authenticated.Authority, AuthoritySHA256: authorityDigest, Grant: grant,
	}, nil
}

// PrepareApprovalEvent creates an append-only lifecycle record. ApprovalID is
// taken from the validated grant, never accepted from caller input.
func PrepareApprovalEvent(grant documentproduction.ApprovalGrant, request ApprovalEventRequest) (ApprovalEventRecord, error) {
	if !canonicalUUIDv4(request.OperationID) || !canonicalUUIDv4(request.EventID) {
		return ApprovalEventRecord{}, invalidProductionContract("invalid approval event request")
	}
	if err := documentproduction.ValidateApprovalGrant(grant); err != nil {
		return ApprovalEventRecord{}, err
	}
	event := documentproduction.ApprovalEvent{
		Contract: documentproduction.ApprovalEventContractV1,
		ID:       request.EventID, ApprovalID: grant.ID, Kind: request.Kind,
		EffectiveAt: request.EffectiveAt, ReplacementApprovalID: request.ReplacementApprovalID,
		Reason: request.Reason,
	}
	if _, _, err := documentproduction.CanonicalApprovalEvents([]documentproduction.ApprovalEvent{event}); err != nil {
		return ApprovalEventRecord{}, err
	}
	grantedAt, _ := time.Parse(time.RFC3339Nano, grant.GrantedAt)
	effectiveAt, _ := time.Parse(time.RFC3339Nano, event.EffectiveAt)
	if effectiveAt.Before(grantedAt) {
		return ApprovalEventRecord{}, invalidProductionContract("approval event predates grant")
	}
	requestDigest, err := semanticDigest(struct {
		EventID               string `json:"event_id"`
		ApprovalSHA256        string `json:"approval_sha256"`
		Kind                  string `json:"kind"`
		EffectiveAt           string `json:"effective_at"`
		ReplacementApprovalID string `json:"replacement_approval_id,omitzero"`
		Reason                string `json:"reason,omitzero"`
	}{event.ID, grant.SHA256, event.Kind, event.EffectiveAt, event.ReplacementApprovalID, event.Reason})
	if err != nil {
		return ApprovalEventRecord{}, err
	}
	return ApprovalEventRecord{OperationID: request.OperationID, RequestSHA256: requestDigest, Event: event}, nil
}

// EvaluateRequiredApproval evaluates admission against the exact current
// subject. Lifecycle changes govern future admissions; callers persist the
// returned current evaluation digest in a historical production receipt.
func EvaluateRequiredApproval(policy documentproduction.PolicyVersion, subject documentproduction.ApprovalSubject, grant *documentproduction.ApprovalGrant, events []documentproduction.ApprovalEvent, at time.Time) (documentproduction.ApprovalEvaluation, error) {
	if err := validateStoredPolicy(policy); err != nil {
		return documentproduction.ApprovalEvaluation{}, err
	}
	_, subjectDigest, err := documentproduction.CanonicalApprovalSubject(subject)
	if err != nil {
		return documentproduction.ApprovalEvaluation{}, err
	}
	if !subjectSelectsPolicy(subject, policy) {
		return documentproduction.ApprovalEvaluation{}, invalidProductionContract("approval subject selects another policy")
	}
	if !policy.Approval.Required {
		return documentproduction.ApprovalEvaluation{}, nil
	}
	if grant == nil {
		return documentproduction.ApprovalEvaluation{}, documentproduction.ApprovalGateProblem(true, nil, subjectDigest)
	}
	if policy.Approval.EvidenceRequired && grant.Evidence == "" {
		return documentproduction.ApprovalEvaluation{}, &documentproduction.Problem{
			Code: documentproduction.ProblemApprovalRequired, Detail: "current approval evidence is required",
		}
	}
	if !approvalLifetimeMatches(*grant, policy.Approval.MaxAgeSeconds) {
		return documentproduction.ApprovalEvaluation{}, &documentproduction.Problem{
			Code: documentproduction.ProblemApprovalStale, Detail: "approval lifetime differs from selected policy",
		}
	}
	evaluation, err := documentproduction.EvaluateApproval(*grant, events, subjectDigest, at)
	if err != nil {
		return documentproduction.ApprovalEvaluation{}, err
	}
	if err := documentproduction.ApprovalGateProblem(true, &evaluation, subjectDigest); err != nil {
		return evaluation, err
	}
	return evaluation, nil
}

func validateStoredPolicy(policy documentproduction.PolicyVersion) error {
	return documentproduction.ValidatePolicyVersion(policy)
}

func subjectSelectsPolicy(subject documentproduction.ApprovalSubject, policy documentproduction.PolicyVersion) bool {
	return subject.Policy.PolicyID == policy.ID && subject.Policy.Version == policy.Version &&
		subject.Policy.PolicySHA256 == policy.SHA256
}

func validateApprovalEvidence(value string, required bool) error {
	if !utf8.ValidString(value) || len(value) > documentproduction.MaxApprovalEvidenceBytes || strings.TrimSpace(value) != value || required && value == "" {
		return invalidProductionContract("invalid approval evidence")
	}
	return nil
}

func approvalLifetimeMatches(grant documentproduction.ApprovalGrant, maxAgeSeconds int64) bool {
	if maxAgeSeconds == 0 {
		return grant.ExpiresAt == ""
	}
	grantedAt, grantedErr := time.Parse(time.RFC3339Nano, grant.GrantedAt)
	expiresAt, expiresErr := time.Parse(time.RFC3339Nano, grant.ExpiresAt)
	return grantedErr == nil && expiresErr == nil &&
		expiresAt.Equal(grantedAt.Add(time.Duration(maxAgeSeconds)*time.Second))
}

func approvalRequestDigest(approvalID, subjectSHA256, evidence string) (string, error) {
	return semanticDigest(struct {
		ApprovalID    string `json:"approval_id"`
		SubjectSHA256 string `json:"subject_sha256"`
		Evidence      string `json:"evidence,omitzero"`
	}{approvalID, subjectSHA256, evidence})
}

func semanticDigest(value any) (string, error) {
	encoded, err := canonical.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
