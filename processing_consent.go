package docbank

import (
	"context"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

var ErrInvalidProcessingConsentRequest = store.ErrInvalidProcessingConsentRequest

// GrantProcessingConsent explicitly authorizes exact provider input and output
// classes for one principal, scope, profile and disclosure fingerprint.
func (v *Vault) GrantProcessingConsent(ctx context.Context, r document.ProcessingConsentRequest) (document.ProcessingConsentReceipt, error) {
	if err := v.begin(); err != nil {
		return document.ProcessingConsentReceipt{}, err
	}
	defer v.lifecycle.RUnlock()
	v.mutation.Lock()
	defer v.mutation.Unlock()
	return v.metadata.GrantProcessingConsent(ctx, r)
}
func (v *Vault) RevokeProcessingConsent(ctx context.Context, r document.ProcessingConsentRevocationRequest) (document.ProcessingConsentRevocationReceipt, error) {
	if err := v.begin(); err != nil {
		return document.ProcessingConsentRevocationReceipt{}, err
	}
	defer v.lifecycle.RUnlock()
	v.mutation.Lock()
	defer v.mutation.Unlock()
	return v.metadata.RevokeProcessingConsent(ctx, r)
}
