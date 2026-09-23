package store

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"
)

// ErrSourceGrantUnavailable means a queued scoped job cannot prove that its
// original source grant remains current. It must not publish a derivative.
var ErrSourceGrantUnavailable = errors.New("processing source grant is unavailable")

// SourceGrantBinding is the immutable, credential-free grant snapshot captured
// after request authorization. Nil bindings are reserved for local admin work.
type SourceGrantBinding struct {
	SubjectID      string    `json:"subject_id"`
	CredentialKind string    `json:"credential_kind"`
	Audience       string    `json:"audience"`
	GrantRevision  uint64    `json:"grant_revision"`
	ExpiresAt      time.Time `json:"expires_at"`
	SourceID       string    `json:"source_id"`
}

// SourceGrantAuthorizer checks the persisted snapshot against current grant
// authority, including the processing operation and exact source fence.
type SourceGrantAuthorizer interface {
	AuthorizeSourceGrant(ctx context.Context, binding SourceGrantBinding) error
}

type SourceGrantAuthorizeFunc func(context.Context, SourceGrantBinding) error

func (fn SourceGrantAuthorizeFunc) AuthorizeSourceGrant(ctx context.Context, binding SourceGrantBinding) error {
	if fn == nil {
		return ErrSourceGrantUnavailable
	}
	return fn(ctx, binding)
}

func validateSourceGrantBinding(binding *SourceGrantBinding, sourceID string) error {
	if binding == nil {
		return nil
	}
	if binding.SubjectID == "" || binding.CredentialKind == "" || binding.Audience == "" ||
		binding.GrantRevision == 0 || binding.ExpiresAt.IsZero() ||
		binding.SourceID != sourceID || validateUUIDv4(binding.SourceID) != nil {
		return ErrSourceGrantUnavailable
	}
	return nil
}

func encodeSourceGrantBinding(binding *SourceGrantBinding, sourceID string) (any, string, error) {
	if err := validateSourceGrantBinding(binding, sourceID); err != nil {
		return nil, "", err
	}
	if binding == nil {
		return nil, "", nil
	}
	encoded, err := json.Marshal(binding, json.Deterministic(true))
	if err != nil {
		return nil, "", fmt.Errorf("encoding source grant: %w", err)
	}
	return string(encoded), digestCatalogJSON(encoded), nil
}

func decodeSourceGrantBinding(raw string, sourceID string) (*SourceGrantBinding, error) {
	var binding SourceGrantBinding
	if err := json.Unmarshal([]byte(raw), &binding, json.RejectUnknownMembers(true)); err != nil {
		return nil, ErrSourceGrantUnavailable
	}
	if err := validateSourceGrantBinding(&binding, sourceID); err != nil {
		return nil, err
	}
	return &binding, nil
}

func authorizeSourceGrant(ctx context.Context, binding *SourceGrantBinding,
	sourceID string, at time.Time, authorizer SourceGrantAuthorizer,
) error {
	if err := validateSourceGrantBinding(binding, sourceID); err != nil {
		return err
	}
	if binding == nil {
		return nil
	}
	if authorizer == nil || !at.UTC().Before(binding.ExpiresAt.UTC()) {
		return ErrSourceGrantUnavailable
	}
	if err := authorizer.AuthorizeSourceGrant(ctx, *binding); err != nil {
		return ErrSourceGrantUnavailable
	}
	return nil
}

// RecheckSourceGrant is the pre-egress counterpart of the publication check.
// Publication must independently repeat this check in its head-flip transaction.
func RecheckSourceGrant(ctx context.Context, binding *SourceGrantBinding,
	sourceID string, at time.Time, authorizer SourceGrantAuthorizer,
) error {
	return authorizeSourceGrant(ctx, binding, sourceID, at, authorizer)
}
