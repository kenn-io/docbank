package api

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"net/http"
	"slices"
	"strconv"
	"time"
)

const MaxOperationSourceIDs = 4096

type Operation string

const (
	OperationRead               Operation = "read"
	OperationContribute         Operation = "contribute"
	OperationProcessing         Operation = "processing"
	OperationAnalyze            Operation = "analyze"
	OperationMetadataMutation   Operation = "metadata_mutation"
	OperationRelationshipReview Operation = "relationship_review"
	OperationExportShare        Operation = "export_share"
	OperationFederation         Operation = "federation"
	OperationAdmin              Operation = "admin"
)

var (
	ErrOperationDenied        = errors.New("operation is not permitted")
	ErrOperationNotFound      = errors.New("protected entity was not found")
	ErrOperationGrantExpired  = errors.New("operation grant expired")
	ErrOperationGrantRevoked  = errors.New("operation grant was revoked")
	ErrOperationScopeTooLarge = errors.New("operation source scope exceeds the limit")
)

// Principal is the authenticated, immutable grant snapshot carried by a
// request. Non-local grants are reloaded from GrantAuthority for every
// authorization, including resource reads and cache disclosures.
type Principal struct {
	SubjectID      string
	CredentialKind string
	Audience       string
	Operations     []Operation
	SourceIDs      []string
	GrantRevision  uint64
	ExpiresAt      time.Time
	Local          bool
}

// PrincipalAuthenticator validates a non-local credential and returns its
// immutable grant snapshot. The policy still reloads the current grant before
// every authorization decision.
type PrincipalAuthenticator func(*http.Request) (Principal, bool)

// LocalAdminPrincipal preserves the daemon's existing master-key behavior.
// It is constructed only after local API-key authentication succeeds.
func LocalAdminPrincipal() Principal {
	return Principal{
		SubjectID: "local:admin", CredentialKind: "daemon",
		Audience: "docbank:local", Operations: []Operation{OperationAdmin}, Local: true,
	}
}

// GrantAuthority returns the current grant for an authenticated subject.
// Implementations must fail closed when the subject is unknown or revoked.
type GrantAuthority interface {
	CurrentGrant(ctx context.Context, subjectID string) (Principal, error)
}

type OperationAuditOutcome string

const (
	OperationOutcomeAllowed  OperationAuditOutcome = "allowed"
	OperationOutcomeDenied   OperationAuditOutcome = "denied"
	OperationOutcomeNotFound OperationAuditOutcome = "not_found"
	OperationOutcomeExpired  OperationAuditOutcome = "expired"
	OperationOutcomeRevoked  OperationAuditOutcome = "revoked"
)

// OperationAuditEvent deliberately excludes source identities, source text,
// credential material, and request payloads.
type OperationAuditEvent struct {
	Actor         string
	Operation     Operation
	Outcome       OperationAuditOutcome
	GrantRevision uint64
	SourceCount   int
}

type OperationAudit interface {
	RecordOperation(ctx context.Context, event OperationAuditEvent)
}

type OperationPolicyOptions struct {
	Authority GrantAuthority
	Audit     OperationAudit
	Now       func() time.Time
}

type OperationPolicy struct {
	authority GrantAuthority
	audit     OperationAudit
	now       func() time.Time
}

func NewOperationPolicy(options OperationPolicyOptions) *OperationPolicy {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &OperationPolicy{authority: options.Authority, audit: options.Audit, now: now}
}

type OperationAuthorizationRequest struct {
	Principal  Principal
	Operation  Operation
	SourceIDs  []string
	RequireAll bool
	Protected  bool
}

type OperationAuthorization struct {
	SourceIDs     []string
	CacheKey      string
	GrantRevision uint64
	Unrestricted  bool
}

// Authorize applies the operation grant and exact source fence at the shared
// service boundary. A caller must invoke it again before disclosing cached or
// asynchronously produced data.
func (p *OperationPolicy) Authorize(
	ctx context.Context, request OperationAuthorizationRequest,
) (OperationAuthorization, error) {
	if p == nil {
		return OperationAuthorization{}, ErrOperationDenied
	}
	requested, requestedCount, err := normalizeSourceIDs(request.SourceIDs)
	if err != nil {
		return p.deny(ctx, request, request.Principal.GrantRevision, OperationOutcomeDenied, err)
	}
	if requestedCount > MaxOperationSourceIDs {
		return p.deny(ctx, request, request.Principal.GrantRevision,
			OperationOutcomeDenied, ErrOperationScopeTooLarge)
	}

	if request.Principal.Local {
		if !localOperationAllowed(request.Principal, request.Operation) {
			return p.deny(ctx, request, 0, OperationOutcomeDenied, ErrOperationDenied)
		}
		decision := OperationAuthorization{SourceIDs: requested, Unrestricted: true}
		decision.CacheKey = operationCacheKey(request.Principal.SubjectID, 0, request.Operation, requested)
		p.record(ctx, request.Principal.SubjectID, request.Operation, OperationOutcomeAllowed, 0, len(requested))
		return decision, nil
	}

	if !validPrincipal(request.Principal) || p.authority == nil {
		return p.deny(ctx, request, request.Principal.GrantRevision, OperationOutcomeDenied, ErrOperationDenied)
	}
	now := p.now().UTC()
	if !now.Before(request.Principal.ExpiresAt.UTC()) {
		return p.deny(ctx, request, request.Principal.GrantRevision,
			OperationOutcomeExpired, ErrOperationGrantExpired)
	}
	current, err := p.authority.CurrentGrant(ctx, request.Principal.SubjectID)
	if err != nil || !validPrincipal(current) ||
		current.SubjectID != request.Principal.SubjectID ||
		current.CredentialKind != request.Principal.CredentialKind ||
		current.Audience != request.Principal.Audience ||
		current.GrantRevision != request.Principal.GrantRevision {
		return p.deny(ctx, request, request.Principal.GrantRevision,
			OperationOutcomeRevoked, ErrOperationGrantRevoked)
	}
	if !now.Before(current.ExpiresAt.UTC()) {
		return p.deny(ctx, request, current.GrantRevision,
			OperationOutcomeExpired, ErrOperationGrantExpired)
	}
	if !hasOperation(request.Principal.Operations, request.Operation) ||
		!hasOperation(current.Operations, request.Operation) {
		return p.deny(ctx, request, current.GrantRevision, OperationOutcomeDenied, ErrOperationDenied)
	}

	allowed := IntersectSourceIDs(current.SourceIDs, request.Principal.SourceIDs)
	resolved := allowed
	if request.SourceIDs != nil {
		resolved = IntersectSourceIDs(requested, allowed)
	}
	if request.RequireAll && len(resolved) != requestedCount {
		outcome, cause := OperationOutcomeDenied, ErrOperationDenied
		if request.Protected {
			outcome, cause = OperationOutcomeNotFound, ErrOperationNotFound
		}
		return p.deny(ctx, request, current.GrantRevision, outcome, cause)
	}
	decision := OperationAuthorization{
		SourceIDs: resolved, GrantRevision: current.GrantRevision,
		CacheKey: operationCacheKey(current.SubjectID, current.GrantRevision, request.Operation, resolved),
	}
	p.record(ctx, current.SubjectID, request.Operation, OperationOutcomeAllowed,
		current.GrantRevision, len(resolved))
	return decision, nil
}

func (p *OperationPolicy) deny(
	ctx context.Context, request OperationAuthorizationRequest, revision uint64,
	outcome OperationAuditOutcome, cause error,
) (OperationAuthorization, error) {
	p.record(ctx, request.Principal.SubjectID, request.Operation, outcome, revision, 0)
	return OperationAuthorization{}, cause
}

func (p *OperationPolicy) record(
	ctx context.Context, actor string, operation Operation, outcome OperationAuditOutcome,
	revision uint64, sourceCount int,
) {
	if p.audit != nil {
		p.audit.RecordOperation(ctx, OperationAuditEvent{
			Actor: actor, Operation: operation, Outcome: outcome,
			GrantRevision: revision, SourceCount: sourceCount,
		})
	}
}

func validPrincipal(principal Principal) bool {
	return principal.SubjectID != "" && principal.CredentialKind != "" && principal.Audience != "" &&
		principal.GrantRevision > 0 && !principal.ExpiresAt.IsZero()
}

func localOperationAllowed(principal Principal, operation Operation) bool {
	return hasOperation(principal.Operations, OperationAdmin) || hasOperation(principal.Operations, operation)
}

func hasOperation(operations []Operation, operation Operation) bool {
	return slices.Contains(operations, operation)
}

func normalizeSourceIDs(ids []string) ([]string, int, error) {
	if ids == nil {
		return nil, 0, nil
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			return nil, 0, ErrOperationDenied
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, len(out), nil
}

func operationCacheKey(subject string, revision uint64, operation Operation, sourceIDs []string) string {
	ids := slices.Clone(sourceIDs)
	slices.Sort(ids)
	hash := sha256.New()
	writeCacheKeyPart(hash, subject)
	writeCacheKeyPart(hash, strconv.FormatUint(revision, 10))
	writeCacheKeyPart(hash, string(operation))
	for _, id := range ids {
		writeCacheKeyPart(hash, id)
	}
	return "policy:v1:" + hex.EncodeToString(hash.Sum(nil))
}

func writeCacheKeyPart(hash hash.Hash, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = hash.Write(size[:])
	_, _ = hash.Write([]byte(value))
}

type operationPrincipalContextKey struct{}

func ContextWithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, operationPrincipalContextKey{}, principal)
}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(operationPrincipalContextKey{}).(Principal)
	return principal, ok
}

// IntersectSourceIDs returns each requested source at most once, preserving
// request order, when it is present in the exact allowed source set.
func IntersectSourceIDs(requested, allowed []string) []string {
	permitted := make(map[string]bool, len(allowed))
	for _, id := range allowed {
		permitted[id] = true
	}
	out := make([]string, 0)
	for _, id := range requested {
		if permitted[id] {
			out = append(out, id)
			delete(permitted, id)
		}
	}
	return out
}
