package store

import (
	"context"
	"errors"
	"fmt"
	"go.kenn.io/docbank/document"
)

var ErrInvalidProcessingConsentRequest = errors.New("invalid processing consent request")

// GrantProcessingConsent is the typed shared boundary over existing consent
// authority. Calling it is distinct from submitting work or publishing files.
func (s *Store) GrantProcessingConsent(ctx context.Context, r document.ProcessingConsentRequest) (document.ProcessingConsentReceipt, error) {
	if _, err := normalizeConsentAuthority(ProviderOperationAuthorizationRequest{Principal: r.Principal, Scope: r.Scope, ProfileFingerprint: r.ProfileFingerprint, DisclosureFingerprint: r.DisclosureFingerprint, InputClasses: r.InputClasses, RetainedArtifactClasses: r.RetainedArtifactClasses}); err != nil {
		return document.ProcessingConsentReceipt{}, fmt.Errorf("%w: %w", ErrInvalidProcessingConsentRequest, err)
	}
	grant, err := s.GrantConsent(ctx, ProcessingConsentGrantRequest{Principal: r.Principal, Scope: r.Scope, ProfileFingerprint: r.ProfileFingerprint, DisclosureFingerprint: r.DisclosureFingerprint, InputClasses: r.InputClasses, RetainedArtifactClasses: r.RetainedArtifactClasses, ExpiresAt: r.ExpiresAt})
	if err != nil {
		return document.ProcessingConsentReceipt{}, err
	}
	return document.ProcessingConsentReceipt{GrantID: grant.ID, VaultID: grant.VaultID, ProcessingIncarnationID: grant.ProcessingIncarnationID, RevocationFence: grant.RevocationFence, IssuedAt: grant.IssuedAt}, nil
}
func (s *Store) RevokeProcessingConsent(ctx context.Context, r document.ProcessingConsentRevocationRequest) (document.ProcessingConsentRevocationReceipt, error) {
	for label, value := range map[string]string{"principal": r.Principal, "scope": r.Scope} {
		if _, err := normalizeConsentLabel(label, value); err != nil {
			return document.ProcessingConsentRevocationReceipt{}, fmt.Errorf("%w: %w", ErrInvalidProcessingConsentRequest, err)
		}
	}
	receipt, err := s.RevokeConsent(ctx, ProcessingConsentRevocationRequest{Principal: r.Principal, Scope: r.Scope})
	if err != nil {
		return document.ProcessingConsentRevocationReceipt{}, err
	}
	return document.ProcessingConsentRevocationReceipt{RevocationID: receipt.ID, ProcessingIncarnationID: receipt.ProcessingIncarnationID, Fence: receipt.Fence, RevokedAt: receipt.RevokedAt}, nil
}
