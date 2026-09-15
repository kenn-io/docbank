package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"time"

	"go.kenn.io/docbank/internal/canonical"
)

// MediaConsentReceipt is the replay-safe, sanitized journal result for an
// acquisition consent mutation.
type MediaConsentReceipt struct {
	OperationID string `json:"operation_id"`
	OriginID    string `json:"origin_id"`
	GrantID     string `json:"grant_id,omitempty"`
	Fence       int64  `json:"fence"`
	RevokedAt   string `json:"revoked_at,omitempty"`
}

func (s *Store) GrantMediaAcquisitionConsent(
	ctx context.Context, op MediaOperation, originID string,
	request ProcessingConsentGrantRequest,
) (MediaConsentReceipt, error) {
	authority, err := normalizeConsentAuthority(ProviderOperationAuthorizationRequest{
		Principal: request.Principal, Scope: request.Scope,
		ProfileFingerprint: request.ProfileFingerprint, DisclosureFingerprint: request.DisclosureFingerprint,
		InputClasses: request.InputClasses, RetainedArtifactClasses: request.RetainedArtifactClasses,
	})
	if err != nil {
		return MediaConsentReceipt{}, err
	}
	id, err := newUUIDv4()
	if err != nil {
		return MediaConsentReceipt{}, err
	}
	issuedAt := time.Now().UTC()
	var expiresRaw any
	var expiresAt *time.Time
	if request.ExpiresAt != nil {
		value := request.ExpiresAt.UTC()
		expiresRaw, expiresAt = value.Format(timestampLayout), &value
	}
	grant := ProcessingConsentGrant{ID: id, ConsentSetID: id, VaultID: s.vaultID, Principal: authority.principal,
		Scope: authority.scope, ProfileFingerprint: authority.profile,
		DisclosureFingerprint: authority.disclosure, InputClasses: authority.inputs,
		RetainedArtifactClasses: authority.retained, IssuedAt: issuedAt, ExpiresAt: expiresAt}
	receiptRaw, err := s.withMediaOperation(ctx, op, func(tx *sql.Tx) (string, error) {
		if err := s.grantConsentTx(ctx, tx, authority, issuedAt.Format(timestampLayout), expiresRaw, &grant); err != nil {
			return "", err
		}
		encoded, err := canonical.Marshal(MediaConsentReceipt{OperationID: op.ID, OriginID: originID,
			GrantID: grant.ID, Fence: grant.RevocationFence})
		return string(encoded), err
	})
	if err != nil {
		return MediaConsentReceipt{}, fmt.Errorf("granting media acquisition consent: %w", err)
	}
	return decodeMediaConsentReceipt(receiptRaw)
}

func (s *Store) RevokeMediaAcquisitionConsent(
	ctx context.Context, op MediaOperation, originID string,
	request ProcessingConsentRevocationRequest,
) (MediaConsentReceipt, error) {
	s.providerEgressMu.Lock()
	defer s.providerEgressMu.Unlock()
	principal, err := normalizeConsentLabel("principal", request.Principal)
	if err != nil {
		return MediaConsentReceipt{}, err
	}
	scope, err := normalizeConsentLabel("scope", request.Scope)
	if err != nil {
		return MediaConsentReceipt{}, err
	}
	id, err := newUUIDv4()
	if err != nil {
		return MediaConsentReceipt{}, err
	}
	revokedAt := time.Now().UTC()
	revocation := ProcessingConsentRevocation{ID: id, VaultID: s.vaultID,
		Principal: principal, Scope: scope, RevokedAt: revokedAt}
	receiptRaw, err := s.withMediaOperation(ctx, op, func(tx *sql.Tx) (string, error) {
		if err := s.revokeConsentTx(ctx, tx, principal, scope, &revocation); err != nil {
			return "", err
		}
		encoded, err := canonical.Marshal(MediaConsentReceipt{OperationID: op.ID, OriginID: originID,
			Fence: revocation.Fence, RevokedAt: revokedAt.Format(timestampLayout)})
		return string(encoded), err
	})
	if err != nil {
		return MediaConsentReceipt{}, fmt.Errorf("revoking media acquisition consent: %w", err)
	}
	return decodeMediaConsentReceipt(receiptRaw)
}

func decodeMediaConsentReceipt(raw string) (MediaConsentReceipt, error) {
	var result MediaConsentReceipt
	if err := json.Unmarshal([]byte(raw), &result, json.RejectUnknownMembers(true)); err != nil {
		return MediaConsentReceipt{}, fmt.Errorf("decoding media consent receipt: %w", err)
	}
	return result, nil
}
