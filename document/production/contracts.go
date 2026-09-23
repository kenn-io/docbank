// Package production defines immutable, storage-neutral contracts for
// production policy, approval, privilege, artifact, retention and
// reproduction authority.
package production

import "go.kenn.io/docbank/document/redaction"

type PolicySelection = redaction.PolicySelection

const (
	PolicyContractV1                 = "production-policy/v1"
	ApprovalSubjectContractV1        = "production-approval-subject/v1"
	ApprovalSubjectContractV2        = "production-approval-subject/v2"
	ApprovalAuthorityContractV1      = "production-approval-authority/v1"
	ApprovalGrantContractV1          = "production-approval-grant/v1"
	ApprovalEventContractV1          = "production-approval-event/v1"
	ApprovalEvaluationContractV1     = "production-approval-evaluation/v1"
	WithheldSelectionContractV1      = "production-withheld-selection/v1"
	PrivilegeLogInputsContractV1     = "production-privilege-log-inputs/v1"
	PrivilegeLogReceiptContractV1    = "production-privilege-log-receipt/v1"
	PrivilegeLogAttachmentContractV1 = "production-privilege-log-attachment/v1"
	PlayersSnapshotContractV1        = "production-players-snapshot/v1"
	NumberReservationContractV1      = "production-number-reservation/v1"
	ArtifactManifestContractV1       = "production-artifact-manifest/v1"
	ArtifactProvenanceContractV1     = "production-artifact-provenance/v1"
	PreparedInputContractV1          = "production-prepared-input/v1"
	ProductionGateEvidenceContractV1 = "production-gate-evidence/v1"
	PolicyFactsAllowlistV1           = "production-policy-facts-allowlist/v1"
	ProductionReceiptContractV1      = "production-receipt/v1"
	RetentionReceiptContractV1       = "production-retention-receipt/v1"
	ReproductionRequestContractV1    = "production-reproduction-request/v1"
	ReproductionReceiptContractV1    = "production-reproduction-receipt/v1"
)

const (
	MaxPolicyRules           = 500
	MaxPolicyValues          = 500
	MaxPolicyTextBytes       = 64 << 10
	MaxApprovalEvidenceBytes = 4 << 10
	MaxProblemDetailBytes    = 512
	MaxProblemIDs            = 100
	MaxPrivilegeRows         = 100_000
	MaxPrivilegeFields       = 100
	MaxArtifacts             = 1_000_000
	MaxArtifactPathBytes     = 4 << 10
	MaxOutputReferences      = 100_000
	MaxSurfaceOperations     = 200
)

const (
	GeneratedClientTransport = "internal/daemonconn.Connection.API"
	GeneratedClientPackage   = "internal/apiclient"

	NumberingStampRecipeContract = "bates-stamp/v1"
	LoadfileDATProfileID         = "dat-concordance-v1"
	LoadfileOPTProfileID         = "opt-standard-v1"
	LoadfileLFPProfileID         = "lfp-ipro-v1"
)

const (
	PolicyRuleScope       = "scope"
	PolicyRuleDate        = "date"
	PolicyRuleFamily      = "family"
	PolicyRuleDisposition = "disposition"
	PolicyRuleLabel       = "label"

	PolicyOperatorEquals      = "equals"
	PolicyOperatorOneOf       = "one_of"
	PolicyOperatorPresent     = "present"
	PolicyOperatorDateBetween = "date_between"

	PolicyDispositionProduce  = "produce"
	PolicyDispositionWithhold = "withhold"

	PolicyFamilyMember   = "member"
	PolicyFamilyComplete = "complete_family"

	PolicyConflictReject = "reject"
)

type PolicyPredicate struct {
	Field    string   `json:"field"`
	Operator string   `json:"operator"`
	Values   []string `json:"values,omitzero"`
	From     string   `json:"from,omitzero"`
	Through  string   `json:"through,omitzero"`
}

type PolicyRule struct {
	ID            string          `json:"id"`
	Kind          string          `json:"kind"`
	Predicate     PolicyPredicate `json:"predicate"`
	Disposition   string          `json:"disposition,omitzero"`
	FamilyMode    string          `json:"family_mode,omitzero"`
	RequiredLabel string          `json:"required_label,omitzero"`
}

type PolicyOutput struct {
	ConfidentialityLabel string `json:"confidentiality_label,omitzero"`
	EndorsementKind      string `json:"endorsement_kind,omitzero"`
	EndorsementText      string `json:"endorsement_text,omitzero"`
}

type ApprovalRequirement struct {
	Required         bool  `json:"required"`
	EvidenceRequired bool  `json:"evidence_required"`
	MaxAgeSeconds    int64 `json:"max_age_seconds,omitzero"`
}

type PrivilegeLogRequirement struct {
	Required             bool     `json:"required"`
	RequireFrozenReceipt bool     `json:"require_frozen_receipt"`
	RequiredFields       []string `json:"required_fields"`
	AllowedBases         []string `json:"allowed_bases"`
}

// PolicyVersion is immutable. SHA256, when present on a stored value, is the
// digest returned by CanonicalPolicyVersion and is excluded from that digest.
type PolicyVersion struct {
	Contract     string                  `json:"contract"`
	ID           string                  `json:"id"`
	Version      int64                   `json:"version"`
	Name         string                  `json:"name"`
	CreatedAt    string                  `json:"created_at"`
	Rules        []PolicyRule            `json:"rules"`
	Output       PolicyOutput            `json:"output"`
	Approval     ApprovalRequirement     `json:"approval"`
	PrivilegeLog PrivilegeLogRequirement `json:"privilege_log"`
	ConflictMode string                  `json:"conflict_mode"`
	SHA256       string                  `json:"sha256,omitzero"`
}

type ApprovalMember struct {
	MemberID            string `json:"member_id"`
	Ordinal             int64  `json:"ordinal"`
	SourceVersionID     string `json:"source_version_id"`
	SourceSHA256        string `json:"source_sha256"`
	SourceSize          int64  `json:"source_size"`
	PDFSHA256           string `json:"pdf_sha256"`
	PageInventorySHA256 string `json:"page_inventory_sha256"`
	MapSHA256           string `json:"map_sha256"`
	DecisionsSHA256     string `json:"decisions_sha256"`
	ResolvedSHA256      string `json:"resolved_sha256"`
}

// ProductionMemberEvidencePin binds one occurrence's policy projection and
// optional email-family publication to immutable source evidence. A v2 member
// with an empty pin explicitly records that a generic non-email policy needs
// neither metadata facts nor an email publication selector.
type ProductionMemberEvidencePin struct {
	AllowlistVersion              string `json:"allowlist_version,omitzero"`
	SourceMetadataGenerationID    string `json:"source_metadata_generation_id,omitzero"`
	SourceMetadataEvidenceSHA256  string `json:"source_metadata_evidence_sha256,omitzero"`
	PolicyFactsSHA256             string `json:"policy_facts_sha256,omitzero"`
	EmailRootVersionID            string `json:"email_root_version_id,omitzero"`
	EmailPublicationOperationID   string `json:"email_publication_operation_id,omitzero"`
	EmailPublicationRequestSHA256 string `json:"email_publication_request_sha256,omitzero"`
	EmailPublicationReceiptSHA256 string `json:"email_publication_receipt_sha256,omitzero"`
}

// ApprovalAuthority is derived from the trusted authentication context. It
// contains no credential or reusable session secret.
type ApprovalAuthority struct {
	Contract             string `json:"contract"`
	Kind                 string `json:"kind"`
	PrincipalID          string `json:"principal_id"`
	AuthenticationMethod string `json:"authentication_method"`
	AuthenticatedAt      string `json:"authenticated_at"`
	EvidenceSHA256       string `json:"evidence_sha256"`
}

// ApprovalSubject is the complete immutable subject an approval can cover.
// Members are encoded in ordinal order; the caller's slice order is not
// authority. WithheldSelectionSHA256 and PrivilegeLogInputsSHA256 are empty
// only when the selected policy does not require those authorities. Approval
// binds validated log inputs, not the later receipt that binds the evaluation,
// so neither digest depends on itself.
type ApprovalSubject struct {
	Contract                 string           `json:"contract"`
	SetID                    string           `json:"set_id"`
	Revision                 int64            `json:"revision"`
	Members                  []ApprovalMember `json:"members"`
	InstructionsSHA256       string           `json:"instructions_sha256"`
	RecipeSHA256             string           `json:"recipe_sha256"`
	OutputProfileSHA256      string           `json:"output_profile_sha256"`
	DisclosureProfileSHA256  string           `json:"disclosure_profile_sha256"`
	NumberingPolicySHA256    string           `json:"numbering_policy_sha256"`
	Policy                   PolicySelection  `json:"policy"`
	GateEvidenceSHA256       string           `json:"gate_evidence_sha256,omitzero"`
	WithheldSelectionSHA256  string           `json:"withheld_selection_sha256,omitzero"`
	PrivilegeLogInputsSHA256 string           `json:"privilege_log_inputs_sha256,omitzero"`
}

// ApprovalGrant records service-derived authenticated-human authority. API
// callers may supply evidence, but never Actor, AuthorityKind or
// AuthoritySHA256 as proof of approval.
type ApprovalGrant struct {
	Contract        string `json:"contract"`
	ID              string `json:"id"`
	SubjectSHA256   string `json:"subject_sha256"`
	Actor           string `json:"actor"`
	AuthorityKind   string `json:"authority_kind"`
	AuthoritySHA256 string `json:"authority_sha256"`
	Evidence        string `json:"evidence,omitzero"`
	GrantedAt       string `json:"granted_at"`
	ExpiresAt       string `json:"expires_at,omitzero"`
	SHA256          string `json:"sha256,omitzero"`
}

const ApprovalAuthorityAuthenticatedHuman = "authenticated_human"

const (
	ApprovalEventRevoke    = "revoke"
	ApprovalEventSupersede = "supersede"

	ApprovalStateCurrent      = "current"
	ApprovalStateExpired      = "expired"
	ApprovalStateRevoked      = "revoked"
	ApprovalStateSuperseded   = "superseded"
	ApprovalStateStaleSubject = "stale_subject"
)

type ApprovalEvent struct {
	Contract              string `json:"contract"`
	ID                    string `json:"id"`
	ApprovalID            string `json:"approval_id"`
	Kind                  string `json:"kind"`
	EffectiveAt           string `json:"effective_at"`
	ReplacementApprovalID string `json:"replacement_approval_id,omitzero"`
	Reason                string `json:"reason,omitzero"`
}

// ApprovalEvaluation freezes the approval state used to admit one future
// operation. Later expiry, revocation or supersession affects later
// operations, not a ProductionReceipt that already binds a current evaluation.
type ApprovalEvaluation struct {
	Contract       string `json:"contract"`
	ApprovalSHA256 string `json:"approval_sha256"`
	EventsSHA256   string `json:"events_sha256"`
	SubjectSHA256  string `json:"subject_sha256"`
	EvaluatedAt    string `json:"evaluated_at"`
	State          string `json:"state"`
	SHA256         string `json:"sha256,omitzero"`
}

type WithheldMember struct {
	ID              string                  `json:"id"`
	Ordinal         int64                   `json:"ordinal"`
	SourceVersionID string                  `json:"source_version_id"`
	SourceSHA256    string                  `json:"source_sha256"`
	SourceSize      int64                   `json:"source_size"`
	FamilyOrder     int64                   `json:"family_order"`
	Family          redaction.FamilyContext `json:"family"`
}

// WithheldSelection is separate authority from production membership. Its
// exact source versions cannot be represented by silently omitting members.
type WithheldSelection struct {
	Contract     string           `json:"contract"`
	ID           string           `json:"id"`
	SetID        string           `json:"set_id"`
	Revision     int64            `json:"revision"`
	PolicySHA256 string           `json:"policy_sha256"`
	Members      []WithheldMember `json:"members"`
	SHA256       string           `json:"sha256,omitzero"`
}

type PrivilegeRow struct {
	ID                string           `json:"id"`
	WithheldMemberID  string           `json:"withheld_member_id"`
	FamilyOrder       int64            `json:"family_order"`
	SourceVersionID   string           `json:"source_version_id"`
	Basis             string           `json:"basis"`
	PublicDescription string           `json:"public_description"`
	PrivateRationale  string           `json:"private_rationale"`
	EvidenceSHA256    string           `json:"evidence_sha256"`
	PersonIDs         []string         `json:"person_ids"`
	Fields            []PrivilegeField `json:"fields"`
}

type PrivilegeField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Player struct {
	ID             string   `json:"id"`
	DisplayName    string   `json:"display_name"`
	Aliases        []string `json:"aliases"`
	EvidenceSHA256 string   `json:"evidence_sha256"`
}

type PlayersSnapshot struct {
	Contract string   `json:"contract"`
	ID       string   `json:"id"`
	Revision int64    `json:"revision"`
	Players  []Player `json:"players"`
	SHA256   string   `json:"sha256,omitzero"`
}

type PrivilegePublicRow struct {
	ID                string `json:"id"`
	WithheldMemberID  string `json:"withheld_member_id"`
	FamilyOrder       int64  `json:"family_order"`
	SourceVersionID   string `json:"source_version_id"`
	Basis             string `json:"basis"`
	PublicDescription string `json:"public_description"`
}

type PrivilegeLogValidationInput struct {
	LogID                   string         `json:"log_id"`
	Revision                int64          `json:"revision"`
	WithheldSelectionSHA256 string         `json:"withheld_selection_sha256"`
	PolicySHA256            string         `json:"policy_sha256"`
	PlayersSHA256           string         `json:"players_sha256"`
	ValidatedAt             string         `json:"validated_at"`
	Rows                    []PrivilegeRow `json:"rows"`
}

type PrivilegeLogFreezeInput struct {
	PrivilegeLogValidationInput

	ApprovalEvaluationSHA256 string `json:"approval_evaluation_sha256,omitzero"`
	FrozenAt                 string `json:"frozen_at"`
}

// PrivilegeLogInputs is the validated pre-approval authority. Its digest can
// be approved before the frozen receipt binds the resulting evaluation.
type PrivilegeLogInputs struct {
	Contract                string `json:"contract"`
	LogID                   string `json:"log_id"`
	Revision                int64  `json:"revision"`
	WithheldSelectionSHA256 string `json:"withheld_selection_sha256"`
	PolicySHA256            string `json:"policy_sha256"`
	PlayersSHA256           string `json:"players_sha256"`
	RowsSHA256              string `json:"rows_sha256"`
	ValidatedAt             string `json:"validated_at"`
}

const PrivilegeLogStateFrozen = "frozen"

// PrivilegeLogReceipt freezes validated stored rows and inputs. Assigned
// production numbers are deliberately absent and can only appear in a later
// PrivilegeLogAttachmentReceipt.
type PrivilegeLogReceipt struct {
	Contract                 string `json:"contract"`
	LogID                    string `json:"log_id"`
	Revision                 int64  `json:"revision"`
	State                    string `json:"state"`
	WithheldSelectionSHA256  string `json:"withheld_selection_sha256"`
	PolicySHA256             string `json:"policy_sha256"`
	PlayersSHA256            string `json:"players_sha256"`
	ApprovalEvaluationSHA256 string `json:"approval_evaluation_sha256,omitzero"`
	RowsSHA256               string `json:"rows_sha256"`
	InputsSHA256             string `json:"inputs_sha256"`
	RowCount                 int    `json:"row_count"`
	ValidatedAt              string `json:"validated_at"`
	FrozenAt                 string `json:"frozen_at"`
	SHA256                   string `json:"sha256,omitzero"`
}

type PrivilegeOutputReference struct {
	WithheldMemberID string `json:"withheld_member_id"`
	AssignedNumber   string `json:"assigned_number"`
	ArtifactSHA256   string `json:"artifact_sha256"`
}

type PrivilegeLogAttachmentReceipt struct {
	Contract                  string                     `json:"contract"`
	ID                        string                     `json:"id"`
	PrivilegeLogReceiptSHA256 string                     `json:"privilege_log_receipt_sha256"`
	ProductionReceiptSHA256   string                     `json:"production_receipt_sha256"`
	References                []PrivilegeOutputReference `json:"references"`
	CreatedAt                 string                     `json:"created_at"`
	SHA256                    string                     `json:"sha256,omitzero"`
}

type AssignedNumber struct {
	MemberID      string `json:"member_id"`
	MemberOrdinal int64  `json:"member_ordinal"`
	Page          int    `json:"page"`
	Text          string `json:"text"`
}

// NumberReservation is a passive receipt from the one integrated numbering
// ledger. Authority records that ledger's qualified implementation contract.
// This package intentionally defines no allocator or Reserve method.
type NumberReservation struct {
	Contract       string           `json:"contract"`
	Authority      string           `json:"authority"`
	ID             string           `json:"id"`
	OperationID    string           `json:"operation_id"`
	RevisionSHA256 string           `json:"revision_sha256"`
	State          string           `json:"state"`
	Numbers        []AssignedNumber `json:"numbers"`
	SHA256         string           `json:"sha256,omitzero"`
}

const (
	ArtifactRoleRedactedPDF  = "redacted_pdf"
	ArtifactRoleRedactedText = "redacted_text"
	ArtifactRoleRedactedPage = "redacted_page"
	ArtifactRoleDAT          = "dat"
	ArtifactRoleOPT          = "opt"
	ArtifactRoleLFP          = "lfp"
	ArtifactRoleManifest     = "manifest"
	ArtifactRoleArchive      = "archive"
	ArtifactRoleQC           = "qc"
)

type Artifact struct {
	ID            string `json:"id"`
	MemberID      string `json:"member_id,omitzero"`
	MemberOrdinal int64  `json:"member_ordinal,omitzero"`
	Page          int    `json:"page,omitzero"`
	Role          string `json:"role"`
	Path          string `json:"path"`
	SHA256        string `json:"sha256"`
	Size          int64  `json:"size"`
	MediaType     string `json:"media_type"`
	Volume        string `json:"volume,omitzero"`
}

type ArtifactManifest struct {
	Contract  string     `json:"contract"`
	Artifacts []Artifact `json:"artifacts"`
	SHA256    string     `json:"sha256,omitzero"`
}

type ArtifactProvenance struct {
	ArtifactID      string `json:"artifact_id"`
	ArtifactSHA256  string `json:"artifact_sha256"`
	SourceVersionID string `json:"source_version_id"`
	MemberID        string `json:"member_id"`
	MemberOrdinal   int64  `json:"member_ordinal"`
	Page            int    `json:"page,omitzero"`
	Volume          string `json:"volume"`
}

type ArtifactProvenanceReceipt struct {
	Contract                string               `json:"contract"`
	ID                      string               `json:"id"`
	ProductionReceiptSHA256 string               `json:"production_receipt_sha256"`
	ArtifactManifestSHA256  string               `json:"artifact_manifest_sha256"`
	Entries                 []ArtifactProvenance `json:"entries"`
	CreatedAt               string               `json:"created_at"`
	SHA256                  string               `json:"sha256,omitzero"`
}

type PreparedInputReceipt struct {
	Contract                  string `json:"contract"`
	ID                        string `json:"id"`
	ApprovalSubjectSHA256     string `json:"approval_subject_sha256"`
	ApprovalEvaluationSHA256  string `json:"approval_evaluation_sha256,omitzero"`
	WithheldSelectionSHA256   string `json:"withheld_selection_sha256,omitzero"`
	PrivilegeLogReceiptSHA256 string `json:"privilege_log_receipt_sha256,omitzero"`
	GateResultsSHA256         string `json:"gate_results_sha256"`
	CreatedAt                 string `json:"created_at"`
	SHA256                    string `json:"sha256,omitzero"`
}

// ProductionReceipt is append-only historical authority. Validation checks
// the approval evaluation that was current at admission; it never consults
// later approval events.
type ProductionReceipt struct {
	Contract                  string `json:"contract"`
	ID                        string `json:"id"`
	JobID                     string `json:"job_id"`
	SetID                     string `json:"set_id"`
	Revision                  int64  `json:"revision"`
	RevisionSHA256            string `json:"revision_sha256"`
	PreparedInputSHA256       string `json:"prepared_input_sha256"`
	ApprovalEvaluationSHA256  string `json:"approval_evaluation_sha256,omitzero"`
	PolicySHA256              string `json:"policy_sha256"`
	WithheldSelectionSHA256   string `json:"withheld_selection_sha256,omitzero"`
	PrivilegeLogReceiptSHA256 string `json:"privilege_log_receipt_sha256,omitzero"`
	NumberReservationSHA256   string `json:"number_reservation_sha256"`
	LayoutSHA256              string `json:"layout_sha256"`
	EndorsementsSHA256        string `json:"endorsements_sha256"`
	ArtifactManifestSHA256    string `json:"artifact_manifest_sha256"`
	CreatedAt                 string `json:"created_at"`
	SHA256                    string `json:"sha256,omitzero"`
}

type RetentionReceipt struct {
	Contract                string `json:"contract"`
	ID                      string `json:"id"`
	ProductionReceiptSHA256 string `json:"production_receipt_sha256"`
	ArtifactManifestSHA256  string `json:"artifact_manifest_sha256"`
	MetadataSHA256          string `json:"metadata_sha256"`
	BlobRootsSHA256         string `json:"blob_roots_sha256"`
	AuditSHA256             string `json:"audit_sha256"`
	RetainedAt              string `json:"retained_at"`
	SHA256                  string `json:"sha256,omitzero"`
}

type ReproductionRequest struct {
	Contract                        string   `json:"contract"`
	OperationID                     string   `json:"operation_id"`
	OriginalProductionReceiptSHA256 string   `json:"original_production_receipt_sha256"`
	ArtifactIDs                     []string `json:"artifact_ids"`
	DeliveryPolicySHA256            string   `json:"delivery_policy_sha256"`
}

type ReproductionReceipt struct {
	Contract                        string `json:"contract"`
	ID                              string `json:"id"`
	OriginalProductionReceiptSHA256 string `json:"original_production_receipt_sha256"`
	OriginalNumberReservationSHA256 string `json:"original_number_reservation_sha256"`
	ArtifactManifestSHA256          string `json:"artifact_manifest_sha256"`
	PackageQCSHA256                 string `json:"package_qc_sha256"`
	DeliveryPolicySHA256            string `json:"delivery_policy_sha256"`
	NumberAllocationCount           int    `json:"number_allocation_count"`
	CreatedAt                       string `json:"created_at"`
	SHA256                          string `json:"sha256,omitzero"`
}

type ProblemCode string

const (
	ProblemInvalidContract      ProblemCode = "invalid_contract"
	ProblemPolicyUnsatisfied    ProblemCode = "policy_unsatisfied"
	ProblemApprovalRequired     ProblemCode = "approval_required"
	ProblemApprovalStale        ProblemCode = "approval_stale"
	ProblemPrivilegeLogRequired ProblemCode = "privilege_log_required"
	ProblemPrivilegeLogStale    ProblemCode = "privilege_log_stale"
	ProblemChangedPayload       ProblemCode = "changed_payload"
	ProblemSourceStale          ProblemCode = "source_stale"
	ProblemArtifactMissing      ProblemCode = "artifact_missing"
	ProblemArtifactMismatch     ProblemCode = "artifact_mismatch"
	ProblemRetentionRequired    ProblemCode = "retention_required"
	ProblemLimit                ProblemCode = "limit"
)

type Problem struct { //nolint:errname // Problem is the frozen public contract.
	Code      ProblemCode `json:"code"`
	Detail    string      `json:"detail"`
	SubjectID string      `json:"subject_id,omitzero"`
	IDs       []string    `json:"ids,omitzero"`
}

func (p *Problem) Error() string {
	if p == nil {
		return ""
	}
	return string(p.Code)
}
