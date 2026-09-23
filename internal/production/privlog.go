package production

import (
	"context"
	"time"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
)

// PreparedWithheldSelection is the immutable, version-pinned authority handed
// to coordinator-owned persistence.
type PreparedWithheldSelection struct {
	OperationID     string
	RequestSHA256   string
	SelectionSHA256 string
	Canonical       []byte
	Selection       documentproduction.WithheldSelection
}

// PrepareWithheldSelection canonicalizes exact source versions and enforces
// that an occurrence is not simultaneously produced and withheld.
func PrepareWithheldSelection(operationID string, value documentproduction.WithheldSelection, produced []redaction.Member) (PreparedWithheldSelection, error) {
	if !canonicalUUIDv4(operationID) || value.SHA256 != "" {
		return PreparedWithheldSelection{}, invalidProductionContract("invalid withheld selection request")
	}
	encoded, digest, err := documentproduction.CanonicalWithheldSelection(value)
	if err != nil {
		return PreparedWithheldSelection{}, err
	}
	value.SHA256 = digest
	if err := documentproduction.ValidateSelectionPartition(produced, value); err != nil {
		return PreparedWithheldSelection{}, err
	}
	return PreparedWithheldSelection{
		OperationID: operationID, RequestSHA256: digest, SelectionSHA256: digest,
		Canonical: encoded, Selection: value,
	}, nil
}

type PrivilegeLogDraftRequest struct {
	OperationID              string
	LogID                    string
	Revision                 int64
	PredecessorLogID         string
	PredecessorReceiptSHA256 string
}

type PreparedPrivilegeLogDraft struct {
	OperationID              string
	RequestSHA256            string
	LogID                    string
	Revision                 int64
	PredecessorLogID         string
	PredecessorReceiptSHA256 string
	WithheldSelectionSHA256  string
	PolicySHA256             string
}

// PreparePrivilegeLogDraft creates a new draft identity. A correction must
// link an immutable predecessor receipt and use a different log ID.
func PreparePrivilegeLogDraft(request PrivilegeLogDraftRequest, withheld documentproduction.WithheldSelection, policy documentproduction.PolicyVersion) (PreparedPrivilegeLogDraft, error) {
	linked := request.PredecessorLogID != "" || request.PredecessorReceiptSHA256 != ""
	if !canonicalUUIDv4(request.OperationID) || !canonicalUUIDv4(request.LogID) || request.Revision < 1 ||
		linked && (!canonicalUUIDv4(request.PredecessorLogID) || !canonicalSHA256(request.PredecessorReceiptSHA256) || request.PredecessorLogID == request.LogID) {
		return PreparedPrivilegeLogDraft{}, invalidProductionContract("invalid privilege log draft request")
	}
	if err := documentproduction.ValidateWithheldSelection(withheld); err != nil {
		return PreparedPrivilegeLogDraft{}, err
	}
	if err := documentproduction.ValidatePolicyVersion(policy); err != nil {
		return PreparedPrivilegeLogDraft{}, err
	}
	if withheld.PolicySHA256 != policy.SHA256 {
		return PreparedPrivilegeLogDraft{}, invalidProductionContract("withheld selection uses another policy")
	}
	digest, err := semanticDigest(struct {
		LogID                    string `json:"log_id"`
		Revision                 int64  `json:"revision"`
		PredecessorLogID         string `json:"predecessor_log_id,omitzero"`
		PredecessorReceiptSHA256 string `json:"predecessor_receipt_sha256,omitzero"`
		WithheldSelectionSHA256  string `json:"withheld_selection_sha256"`
		PolicySHA256             string `json:"policy_sha256"`
	}{request.LogID, request.Revision, request.PredecessorLogID, request.PredecessorReceiptSHA256, withheld.SHA256, policy.SHA256})
	if err != nil {
		return PreparedPrivilegeLogDraft{}, err
	}
	return PreparedPrivilegeLogDraft{
		OperationID: request.OperationID, RequestSHA256: digest, LogID: request.LogID, Revision: request.Revision,
		PredecessorLogID: request.PredecessorLogID, PredecessorReceiptSHA256: request.PredecessorReceiptSHA256,
		WithheldSelectionSHA256: withheld.SHA256, PolicySHA256: policy.SHA256,
	}, nil
}

// StoredPrivilegeLog is the complete authority that coordinator-owned storage
// must load inside validation or freeze transactions. Rows are private.
type StoredPrivilegeLog struct {
	LogID              string
	Revision           int64
	Generation         int64
	PredecessorID      string
	Rows               []documentproduction.PrivilegeRow
	Withheld           documentproduction.WithheldSelection
	Policy             documentproduction.PolicyVersion
	Players            documentproduction.PlayersSnapshot
	Produced           []redaction.Member
	Validation         *PreparedPrivilegeLogValidation
	ApprovalSubject    *documentproduction.ApprovalSubject
	ApprovalGrant      *documentproduction.ApprovalGrant
	ApprovalEvents     []documentproduction.ApprovalEvent
	ApprovalEvaluation *documentproduction.ApprovalEvaluation
}

type PrivilegeLogValidationRequest struct {
	OperationID        string
	LogID              string
	Revision           int64
	ExpectedGeneration int64
	ValidatedAt        time.Time
}

// PreparedPrivilegeLogValidation is persisted with the draft generation it
// validated. Any later row or authority edit makes it stale.
type PreparedPrivilegeLogValidation struct {
	OperationID     string
	RequestSHA256   string
	DraftGeneration int64
	Validation      documentproduction.PrivilegeLogValidation
}

type PrivilegeLogFreezeRequest struct {
	OperationID                      string
	LogID                            string
	Revision                         int64
	ExpectedGeneration               int64
	ExpectedInputsSHA256             string
	ExpectedApprovalEvaluationSHA256 string
	FrozenAt                         time.Time
}

type PrivilegeLogValidationBuilder func(StoredPrivilegeLog) (PreparedPrivilegeLogValidation, error)
type PrivilegeLogFreezeBuilder func(StoredPrivilegeLog) (documentproduction.PrivilegeLogReceipt, string, error)

// PrivilegeLogStore is the coordinator-facing persistence boundary. Each
// method must load StoredPrivilegeLog and commit the callback result in one
// writer transaction. A callback error commits nothing. Operation receipts
// make exact retries return the original result and changed payloads fail.
type PrivilegeLogStore interface {
	ValidatePrivilegeLog(ctx context.Context, request PrivilegeLogValidationRequest, build PrivilegeLogValidationBuilder) (PreparedPrivilegeLogValidation, error)
	FreezePrivilegeLog(ctx context.Context, request PrivilegeLogFreezeRequest, build PrivilegeLogFreezeBuilder) (documentproduction.PrivilegeLogReceipt, error)
}

// ValidateStoredPrivilegeLog derives validation authority only from rows and
// snapshots loaded by the store inside its transaction.
func ValidateStoredPrivilegeLog(ctx context.Context, store PrivilegeLogStore, request PrivilegeLogValidationRequest) (PreparedPrivilegeLogValidation, error) {
	if store == nil || !canonicalUUIDv4(request.OperationID) || !canonicalUUIDv4(request.LogID) ||
		request.Revision < 1 || request.ExpectedGeneration < 1 || !canonicalUTCTime(request.ValidatedAt) {
		return PreparedPrivilegeLogValidation{}, invalidProductionContract("invalid privilege log validation request")
	}
	return store.ValidatePrivilegeLog(ctx, request, func(stored StoredPrivilegeLog) (PreparedPrivilegeLogValidation, error) {
		if err := matchStoredPrivilegeDraft(stored, request.LogID, request.Revision, request.ExpectedGeneration); err != nil {
			return PreparedPrivilegeLogValidation{}, err
		}
		input := documentproduction.PrivilegeLogValidationInput{
			LogID: stored.LogID, Revision: stored.Revision,
			WithheldSelectionSHA256: stored.Withheld.SHA256, PolicySHA256: stored.Policy.SHA256,
			PlayersSHA256: stored.Players.SHA256, ValidatedAt: request.ValidatedAt.Format(time.RFC3339Nano),
			Rows: stored.Rows,
		}
		validation, err := documentproduction.ValidatePrivilegeLog(input, stored.Withheld, stored.Policy, stored.Players, stored.Produced)
		if err != nil {
			return PreparedPrivilegeLogValidation{}, err
		}
		requestDigest, err := privilegeLogValidationRequestDigest(request)
		if err != nil {
			return PreparedPrivilegeLogValidation{}, err
		}
		return PreparedPrivilegeLogValidation{
			OperationID: request.OperationID, RequestSHA256: requestDigest,
			DraftGeneration: stored.Generation, Validation: validation,
		}, nil
	})
}

// FreezeStoredPrivilegeLog re-reads and revalidates the stored draft inside the
// store's writer transaction before inserting the immutable receipt.
func FreezeStoredPrivilegeLog(ctx context.Context, store PrivilegeLogStore, request PrivilegeLogFreezeRequest) (documentproduction.PrivilegeLogReceipt, error) {
	if store == nil || !canonicalUUIDv4(request.OperationID) || !canonicalUUIDv4(request.LogID) ||
		request.Revision < 1 || request.ExpectedGeneration < 1 || !canonicalUTCTime(request.FrozenAt) ||
		!canonicalSHA256(request.ExpectedInputsSHA256) || !optionalCanonicalSHA256(request.ExpectedApprovalEvaluationSHA256) {
		return documentproduction.PrivilegeLogReceipt{}, invalidProductionContract("invalid privilege log freeze request")
	}
	return store.FreezePrivilegeLog(ctx, request, func(stored StoredPrivilegeLog) (documentproduction.PrivilegeLogReceipt, string, error) {
		if err := matchStoredPrivilegeDraft(stored, request.LogID, request.Revision, request.ExpectedGeneration); err != nil {
			return documentproduction.PrivilegeLogReceipt{}, "", err
		}
		if stored.Validation == nil || stored.Validation.DraftGeneration != stored.Generation {
			return documentproduction.PrivilegeLogReceipt{}, "", stalePrivilegeLog(stored.LogID)
		}
		validatedAt, err := time.Parse(time.RFC3339Nano, stored.Validation.Validation.Inputs.ValidatedAt)
		if err != nil {
			return documentproduction.PrivilegeLogReceipt{}, "", invalidProductionContract("stored privilege validation time is invalid")
		}
		input := documentproduction.PrivilegeLogValidationInput{
			LogID: stored.LogID, Revision: stored.Revision,
			WithheldSelectionSHA256: stored.Withheld.SHA256, PolicySHA256: stored.Policy.SHA256,
			PlayersSHA256: stored.Players.SHA256, ValidatedAt: validatedAt.Format(time.RFC3339Nano), Rows: stored.Rows,
		}
		current, err := documentproduction.ValidatePrivilegeLog(input, stored.Withheld, stored.Policy, stored.Players, stored.Produced)
		if err != nil {
			return documentproduction.PrivilegeLogReceipt{}, "", err
		}
		if current.InputsSHA256 != stored.Validation.Validation.InputsSHA256 ||
			current.RowsSHA256 != stored.Validation.Validation.RowsSHA256 ||
			current.InputsSHA256 != request.ExpectedInputsSHA256 {
			return documentproduction.PrivilegeLogReceipt{}, "", stalePrivilegeLog(stored.LogID)
		}
		approvalDigest, err := privilegeApprovalDigest(stored, current.InputsSHA256, request.FrozenAt)
		if err != nil {
			return documentproduction.PrivilegeLogReceipt{}, "", err
		}
		if approvalDigest != request.ExpectedApprovalEvaluationSHA256 {
			return documentproduction.PrivilegeLogReceipt{}, "", stalePrivilegeLog(stored.LogID)
		}
		receipt, err := documentproduction.FreezePrivilegeLog(documentproduction.PrivilegeLogFreezeInput{
			PrivilegeLogValidationInput: input, ApprovalEvaluationSHA256: approvalDigest,
			FrozenAt: request.FrozenAt.Format(time.RFC3339Nano),
		})
		if err != nil {
			return documentproduction.PrivilegeLogReceipt{}, "", err
		}
		requestDigest, err := privilegeLogFreezeRequestDigest(request)
		return receipt, requestDigest, err
	})
}

func privilegeApprovalDigest(stored StoredPrivilegeLog, inputsSHA256 string, frozenAt time.Time) (string, error) {
	if !stored.Policy.Approval.Required {
		if stored.ApprovalSubject != nil || stored.ApprovalGrant != nil || len(stored.ApprovalEvents) != 0 || stored.ApprovalEvaluation != nil {
			return "", invalidProductionContract("unexpected privilege log approval")
		}
		return "", nil
	}
	if stored.ApprovalSubject == nil || stored.ApprovalGrant == nil || stored.ApprovalEvaluation == nil ||
		!subjectSelectsPolicy(*stored.ApprovalSubject, stored.Policy) ||
		stored.ApprovalSubject.SetID != stored.Withheld.SetID || stored.ApprovalSubject.Revision != stored.Withheld.Revision ||
		stored.ApprovalSubject.WithheldSelectionSHA256 != stored.Withheld.SHA256 ||
		stored.ApprovalSubject.PrivilegeLogInputsSHA256 != inputsSHA256 {
		return "", &documentproduction.Problem{Code: documentproduction.ProblemApprovalStale, Detail: "privilege log approval subject is stale"}
	}
	current, err := EvaluateRequiredApproval(stored.Policy, *stored.ApprovalSubject, stored.ApprovalGrant, stored.ApprovalEvents, frozenAt)
	if err != nil {
		return "", err
	}
	if current.SHA256 != stored.ApprovalEvaluation.SHA256 {
		return "", &documentproduction.Problem{Code: documentproduction.ProblemApprovalStale, Detail: "privilege log approval evaluation is stale"}
	}
	return current.SHA256, nil
}

func matchStoredPrivilegeDraft(stored StoredPrivilegeLog, logID string, revision, generation int64) error {
	if stored.LogID != logID || stored.Revision != revision || stored.Generation != generation {
		return stalePrivilegeLog(logID)
	}
	return nil
}

func stalePrivilegeLog(logID string) *documentproduction.Problem {
	return &documentproduction.Problem{Code: documentproduction.ProblemPrivilegeLogStale, Detail: "privilege log authority changed", SubjectID: logID}
}

func privilegeLogValidationRequestDigest(request PrivilegeLogValidationRequest) (string, error) {
	return semanticDigest(struct {
		LogID              string `json:"log_id"`
		Revision           int64  `json:"revision"`
		ExpectedGeneration int64  `json:"expected_generation"`
		ValidatedAt        string `json:"validated_at"`
	}{request.LogID, request.Revision, request.ExpectedGeneration, request.ValidatedAt.Format(time.RFC3339Nano)})
}

func privilegeLogFreezeRequestDigest(request PrivilegeLogFreezeRequest) (string, error) {
	return semanticDigest(struct {
		LogID                            string `json:"log_id"`
		Revision                         int64  `json:"revision"`
		ExpectedGeneration               int64  `json:"expected_generation"`
		ExpectedInputsSHA256             string `json:"expected_inputs_sha256"`
		ExpectedApprovalEvaluationSHA256 string `json:"expected_approval_evaluation_sha256,omitzero"`
		FrozenAt                         string `json:"frozen_at"`
	}{request.LogID, request.Revision, request.ExpectedGeneration, request.ExpectedInputsSHA256,
		request.ExpectedApprovalEvaluationSHA256, request.FrozenAt.Format(time.RFC3339Nano)})
}

func canonicalUTCTime(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}

func canonicalSHA256(value string) bool {
	return len(value) == 64 && value == lowerHex(value)
}

func optionalCanonicalSHA256(value string) bool { return value == "" || canonicalSHA256(value) }

func lowerHex(value string) string {
	for _, character := range value {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' {
			continue
		}
		return ""
	}
	return value
}
