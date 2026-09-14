package store

import (
	"context"

	"go.kenn.io/docbank/document"
)

// GrantProcessingConsent is the typed shared boundary over existing consent
// authority. Calling it is distinct from submitting work or publishing files.
func (s *Store) GrantProcessingConsent(ctx context.Context, r document.ProcessingConsentRequest) (document.ProcessingConsentReceipt, error) {
	grant, err := s.GrantConsent(ctx, ProcessingConsentGrantRequest(r))
	if err != nil {
		return document.ProcessingConsentReceipt{}, err
	}
	return document.ProcessingConsentReceipt{
		GrantID: grant.ID, VaultID: grant.VaultID, ProcessingIncarnationID: grant.ProcessingIncarnationID,
		RevocationFence: grant.RevocationFence, IssuedAt: grant.IssuedAt,
	}, nil
}

// RevokeProcessingConsent advances the named principal and scope's revocation fence.
func (s *Store) RevokeProcessingConsent(ctx context.Context, r document.ProcessingConsentRevocationRequest) (document.ProcessingConsentRevocationReceipt, error) {
	receipt, err := s.RevokeConsent(ctx, ProcessingConsentRevocationRequest(r))
	if err != nil {
		return document.ProcessingConsentRevocationReceipt{}, err
	}
	return document.ProcessingConsentRevocationReceipt{
		RevocationID: receipt.ID, ProcessingIncarnationID: receipt.ProcessingIncarnationID,
		Fence: receipt.Fence, RevokedAt: receipt.RevokedAt,
	}, nil
}
