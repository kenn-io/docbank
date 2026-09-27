package docbank

import (
	"context"
	"time"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	productionservice "go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

type ProductionMemberPage = api.ProductionMemberPage
type ProductionDecisionPage = api.ProductionDecisionPage
type ProductionRecipeCatalog = api.ProductionRecipeCatalog
type ProductionPolicyPage = api.ProductionPolicyPage
type ProductionApprovalPublic = api.ProductionApprovalPublic
type ProductionPrivilegePublicPage = api.ProductionPrivilegePublicPage
type ProductionPrivilegeValidationRequest = api.ProductionPrivilegeValidationRequest
type ProductionPrivilegeValidation = api.ProductionPrivilegeValidation
type ProductionPrivilegeExport = productionservice.PrivilegeLogExport
type ProductionPrivilegeDraftCreateRequest = api.ProductionPrivilegeDraftCreateRequest
type ProductionPrivilegeRowsReplaceRequest = api.ProductionPrivilegeRowsReplaceRequest
type ProductionPrivilegeDraftGeneration = api.ProductionPrivilegeDraftGeneration

// ProductionApprovalRequest names the exact subject and private evidence to
// record in an embedded vault. It does not contain authentication authority.
type ProductionApprovalRequest struct {
	OperationID string
	ApprovalID  string
	Subject     documentproduction.ApprovalSubject
	Evidence    string
}

// ProductionApprovalAuthentication must be derived by the embedding host from
// a verified human authentication event. A caller-supplied actor label alone
// is not proof of approval.
type ProductionApprovalAuthentication struct {
	Actor     string
	Authority documentproduction.ApprovalAuthority
}

func (v *Vault) ProductionPolicyVersions(ctx context.Context, cursor string, limit int) (ProductionPolicyPage, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionPolicyPage{}, ErrClosed
	}
	if limit == 0 {
		limit = 25
	}
	page, err := v.metadata.ListProductionPolicies(ctx, cursor, limit)
	if err != nil {
		return ProductionPolicyPage{}, err
	}
	return ProductionPolicyPage{Items: page.Items, NextCursor: page.NextCursor}, nil
}

// CreateProductionPolicyVersion stores one immutable policy in this embedded vault.
// Reusing an operation ID with different policy content is a conflict.
func (v *Vault) CreateProductionPolicyVersion(ctx context.Context, operationID string,
	policy documentproduction.PolicyVersion) (documentproduction.PolicyVersion, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return documentproduction.PolicyVersion{}, ErrClosed
	}
	prepared, err := productionservice.PreparePolicyVersion(operationID, policy)
	if err != nil {
		return documentproduction.PolicyVersion{}, err
	}
	var stored documentproduction.PolicyVersion
	err = embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		stored, err = v.metadata.PutProductionPolicy(ctx, prepared)
		return err
	})
	return stored, err
}

func (v *Vault) ProductionPolicyVersion(ctx context.Context, policyID string,
	version int64) (documentproduction.PolicyVersion, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return documentproduction.PolicyVersion{}, ErrClosed
	}
	return v.metadata.ProductionPolicy(ctx, policyID, version)
}

// ProductionApproval returns the public approval projection for this embedded vault.
func (v *Vault) ProductionApproval(ctx context.Context, approvalID string) (ProductionApprovalPublic, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionApprovalPublic{}, ErrClosed
	}
	grant, events, err := v.metadata.ProductionApprovalPublic(ctx, approvalID)
	if err != nil {
		return ProductionApprovalPublic{}, err
	}
	return ProductionApprovalPublic{Grant: grant, Events: events}, nil
}

// ProductionPrivilegeLog reads a bounded page of public rows from one frozen
// privilege-log revision in this embedded vault.
func (v *Vault) ProductionPrivilegeLog(ctx context.Context, logID string, revision int64,
	cursor string, limit int) (ProductionPrivilegePublicPage, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionPrivilegePublicPage{}, ErrClosed
	}
	if limit == 0 {
		limit = 25
	}
	page, err := v.metadata.ProductionPrivilegePublicPage(ctx, logID, revision, cursor, limit)
	if err != nil {
		return ProductionPrivilegePublicPage{}, err
	}
	return ProductionPrivilegePublicPage{Receipt: page.Receipt,
		Rows: page.Rows, NextCursor: page.NextCursor}, nil
}

// ExportProductionPrivilegeLog returns verified public bytes from one frozen
// log in this embedded vault. Formats are json, csv, xlsx, and pdf.
func (v *Vault) ExportProductionPrivilegeLog(ctx context.Context, logID string, revision int64,
	format string) (ProductionPrivilegeExport, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionPrivilegeExport{}, ErrClosed
	}
	return v.metadata.ExportProductionPrivilegeLog(ctx, logID, revision, format)
}

// CreateProductionPrivilegeLogDraft derives produced members from this vault's
// sealed revision and stores the supplied private rows in the same transaction.
func (v *Vault) CreateProductionPrivilegeLogDraft(ctx context.Context, logID string, revision int64,
	request ProductionPrivilegeDraftCreateRequest) (ProductionPrivilegeDraftGeneration, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionPrivilegeDraftGeneration{}, ErrClosed
	}
	var generation int64
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		generation, err = v.metadata.CreateStoredPrivilegeLogDraft(ctx, store.StoredPrivilegeLogDraftRequest{
			SetID: request.SetID, SetRevision: request.SetRevision,
			Draft: productionservice.PrivilegeLogDraftRequest{
				OperationID: request.OperationID, LogID: logID, Revision: revision,
				PredecessorLogID:         request.PredecessorLogID,
				PredecessorReceiptSHA256: request.PredecessorReceiptSHA256,
			},
			PlayersSHA256: request.PlayersSHA256, Rows: request.Rows,
		})
		return err
	})
	if err != nil {
		return ProductionPrivilegeDraftGeneration{}, err
	}
	return ProductionPrivilegeDraftGeneration{LogID: logID, Revision: revision, Generation: generation}, nil
}

// ReplaceProductionPrivilegeLogRows advances a mutable draft generation and
// invalidates its prior validation and approval binding.
func (v *Vault) ReplaceProductionPrivilegeLogRows(ctx context.Context, logID string, revision int64,
	request ProductionPrivilegeRowsReplaceRequest) (ProductionPrivilegeDraftGeneration, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionPrivilegeDraftGeneration{}, ErrClosed
	}
	var generation int64
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		generation, err = v.metadata.ReplacePrivilegeLogRows(ctx, store.PrivilegeLogRowUpdate{
			OperationID: request.OperationID, LogID: logID, Revision: revision,
			ExpectedGeneration: request.ExpectedGeneration, Rows: request.Rows,
		})
		return err
	})
	if err != nil {
		return ProductionPrivilegeDraftGeneration{}, err
	}
	return ProductionPrivilegeDraftGeneration{LogID: logID, Revision: revision, Generation: generation}, nil
}

// ValidateProductionPrivilegeLog checks the stored draft rows and pinned
// authority in this embedded vault. An exact operation retry returns the same
// validation; a changed generation or payload conflicts.
func (v *Vault) ValidateProductionPrivilegeLog(ctx context.Context, logID string, revision int64,
	request ProductionPrivilegeValidationRequest) (ProductionPrivilegeValidation, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionPrivilegeValidation{}, ErrClosed
	}
	validatedAt, err := time.Parse(time.RFC3339Nano, request.ValidatedAt)
	if err != nil || validatedAt.Location() != time.UTC ||
		validatedAt.Format(time.RFC3339Nano) != request.ValidatedAt {
		return ProductionPrivilegeValidation{}, &documentproduction.Problem{
			Code: documentproduction.ProblemInvalidContract, Detail: "validated_at must be canonical UTC",
		}
	}
	serviceRequest := productionservice.PrivilegeLogValidationRequest{
		OperationID: request.OperationID, LogID: logID, Revision: revision,
		ExpectedGeneration: request.ExpectedGeneration, ValidatedAt: validatedAt,
	}
	var prepared productionservice.PreparedPrivilegeLogValidation
	err = embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		prepared, err = productionservice.ValidateStoredPrivilegeLog(ctx, v.metadata, serviceRequest)
		return err
	})
	if err != nil {
		return ProductionPrivilegeValidation{}, err
	}
	return ProductionPrivilegeValidation{DraftGeneration: prepared.DraftGeneration,
		Validation: prepared.Validation}, nil
}

// RecordProductionApproval stores one immutable approval using the host's
// verified human authority. Retries with the same operation and subject return
// the original grant. Only the public grant crosses this API boundary.
func (v *Vault) RecordProductionApproval(ctx context.Context, request ProductionApprovalRequest,
	authenticated ProductionApprovalAuthentication) (documentproduction.ApprovalPublicGrant, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return documentproduction.ApprovalPublicGrant{}, ErrClosed
	}
	policy, err := v.metadata.ProductionPolicy(ctx, request.Subject.Policy.PolicyID, request.Subject.Policy.Version)
	if err != nil {
		return documentproduction.ApprovalPublicGrant{}, err
	}
	record, err := productionservice.PrepareApprovalRecord(productionservice.RecordApprovalRequest{
		OperationID: request.OperationID, ApprovalID: request.ApprovalID,
		Subject: request.Subject, Evidence: request.Evidence,
	}, policy, productionservice.AuthenticatedApproval{
		Actor: authenticated.Actor, Authority: authenticated.Authority,
	}, time.Now().UTC())
	if err != nil {
		return documentproduction.ApprovalPublicGrant{}, err
	}
	var grant documentproduction.ApprovalGrant
	err = embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		grant, err = v.metadata.PutProductionApproval(ctx, record)
		return err
	})
	if err != nil {
		return documentproduction.ApprovalPublicGrant{}, err
	}
	return documentproduction.PublicApprovalGrant(grant), nil
}

func (v *Vault) ProductionRecipes(ctx context.Context) (ProductionRecipeCatalog, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionRecipeCatalog{}, ErrClosed
	}
	return api.QualifiedProductionRecipes()
}

// CreateProductionSet creates an idempotent first draft in this embedded vault.
// Actor is the embedding application's authenticated principal.
func (v *Vault) CreateProductionSet(ctx context.Context, actor string, request redaction.CreateRequest) (redaction.Set, redaction.Draft, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Set{}, redaction.Draft{}, ErrClosed
	}
	var set redaction.Set
	var draft redaction.Draft
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		set, draft, err = v.metadata.CreateProductionSet(ctx, actor, request)
		return err
	})
	return set, draft, err
}

func (v *Vault) ProductionSet(ctx context.Context, setID string) (redaction.Set, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Set{}, ErrClosed
	}
	return v.metadata.ProductionSet(ctx, setID)
}

func (v *Vault) ProductionDraft(ctx context.Context, setID string, revision int64) (redaction.Draft, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Draft{}, ErrClosed
	}
	return v.metadata.ProductionDraft(ctx, setID, revision)
}

// ForkProductionDraft copies one retained revision into a new editable draft.
// Review declarations and the membership seal are reset by the Store.
func (v *Vault) ForkProductionDraft(ctx context.Context, actor, setID string, revision int64,
	operationID string) (redaction.Draft, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Draft{}, ErrClosed
	}
	var draft redaction.Draft
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		draft, err = v.metadata.ForkProductionDraft(ctx, actor, setID, revision, operationID)
		return err
	})
	return draft, err
}

func (v *Vault) ProductionMembers(ctx context.Context, setID string, revision int64, cursor string, limit int) (ProductionMemberPage, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionMemberPage{}, ErrClosed
	}
	if limit == 0 {
		limit = 100
	}
	items, next, err := v.metadata.ProductionMembers(ctx, setID, revision, cursor, limit)
	if err != nil {
		return ProductionMemberPage{}, err
	}
	page := ProductionMemberPage{Items: make([]api.ProductionMember, len(items)), NextCursor: next}
	for i, item := range items {
		page.Items[i] = api.ProductionMember(item)
	}
	return page, nil
}

func (v *Vault) ProductionDecisions(ctx context.Context, setID string, revision int64, cursor string, limit int) (ProductionDecisionPage, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionDecisionPage{}, ErrClosed
	}
	if limit == 0 {
		limit = 100
	}
	items, next, err := v.metadata.ProductionDecisions(ctx, setID, revision, cursor, limit)
	if err != nil {
		return ProductionDecisionPage{}, err
	}
	page := ProductionDecisionPage{Items: make([]api.ProductionDecision, len(items)), NextCursor: next}
	for i, item := range items {
		page.Items[i] = api.ProductionDecision(item)
	}
	return page, nil
}

func (v *Vault) EditProductionInstructions(ctx context.Context, actor, setID string, revision, etag int64,
	request api.ProductionInstructionsRequest) (redaction.Receipt, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Receipt{}, ErrClosed
	}
	var receipt redaction.Receipt
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		receipt, err = v.metadata.EditProductionInstructions(ctx, actor, setID, revision, request.Domain(etag))
		return err
	})
	return receipt, err
}

func (v *Vault) ApplyProductionChanges(ctx context.Context, actor, setID string, revision, etag int64,
	request api.ProductionChangesRequest) (redaction.Receipt, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Receipt{}, ErrClosed
	}
	var receipt redaction.Receipt
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		receipt, err = v.metadata.ApplyProductionChanges(ctx, actor, setID, revision, request.Domain(etag))
		return err
	})
	return receipt, err
}

func (v *Vault) SealProductionMembership(ctx context.Context, actor, setID string, revision, etag int64,
	request api.ProductionMembershipSealRequest) (redaction.Receipt, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Receipt{}, ErrClosed
	}
	var receipt redaction.Receipt
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		receipt, err = v.metadata.SealProductionMembership(ctx, actor, setID, revision, request.Domain(etag))
		return err
	})
	return receipt, err
}

func (v *Vault) ReviewProductionMember(ctx context.Context, actor, setID string, revision, etag int64,
	memberID string, request api.ProductionMemberReviewRequest) (redaction.Receipt, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Receipt{}, ErrClosed
	}
	var receipt redaction.Receipt
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		receipt, err = v.metadata.ReviewProductionMember(ctx, actor, setID, revision, request.Domain(etag, memberID))
		return err
	})
	return receipt, err
}
