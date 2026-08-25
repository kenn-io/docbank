package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrProcessingConsentRequired = errors.New("processing consent is required")
	ErrProcessingConsentExpired  = errors.New("processing consent has expired")
	ErrProcessingConsentRevoked  = errors.New("processing consent has been revoked")
)

// ProcessingIncarnation is the local authority boundary for provider access.
// Restores keep historical consent records but start with a new incarnation.
type ProcessingIncarnation struct {
	ID        string
	CreatedAt time.Time
}

// ProcessingConsentGrantRequest binds one consent grant to exact policy and
// disclosure identities plus the byte and retention classes it permits.
type ProcessingConsentGrantRequest struct {
	Principal               string
	Scope                   string
	ProfileFingerprint      string
	DisclosureFingerprint   string
	InputClasses            []string
	RetainedArtifactClasses []string
	ExpiresAt               *time.Time
}

// ProcessingConsentGrant is immutable authority within one vault incarnation.
type ProcessingConsentGrant struct {
	ID                      string
	VaultID                 string
	ProcessingIncarnationID string
	Principal               string
	Scope                   string
	ProfileFingerprint      string
	DisclosureFingerprint   string
	InputClasses            []string
	RetainedArtifactClasses []string
	RevocationFence         int64
	IssuedAt                time.Time
	ExpiresAt               *time.Time
}

// ProcessingConsentRevocationRequest advances the fence for one principal and scope.
type ProcessingConsentRevocationRequest struct {
	Principal string
	Scope     string
}

// ProcessingConsentRevocation is an immutable scope-fence advancement.
type ProcessingConsentRevocation struct {
	ID                      string
	VaultID                 string
	ProcessingIncarnationID string
	Principal               string
	Scope                   string
	Fence                   int64
	RevokedAt               time.Time
}

// ProviderOperationAuthorizationRequest describes the exact outbound operation
// and retained outputs that core is about to authorize.
type ProviderOperationAuthorizationRequest struct {
	Principal               string
	Scope                   string
	ProfileFingerprint      string
	DisclosureFingerprint   string
	InputClasses            []string
	RetainedArtifactClasses []string
	// PriorAuthorization is set when a leased operation rechecks authority
	// immediately before publication. A later replacement grant cannot revive
	// work authorized below an earlier revocation fence.
	PriorAuthorization *ProviderOperationAuthorization
}

// ProviderOperationAuthorization is an immutable receipt. It is evidence of a
// successful check, not a bearer capability: publication must authorize again.
type ProviderOperationAuthorization struct {
	GrantID                 string
	ProcessingIncarnationID string
	RevocationFence         int64
	AuthorizedAt            time.Time
}

type normalizedConsentAuthority struct {
	principal, scope, profile, disclosure string
	inputs, retained                      []string
	inputsJSON, retainedJSON              string
}

func ensureProcessingIncarnationTx(tx *sql.Tx) error {
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM current_processing_incarnation`).Scan(&count); err != nil {
		return fmt.Errorf("checking processing incarnation: %w", err)
	}
	if count == 1 {
		return nil
	}
	if count != 0 {
		return fmt.Errorf("processing incarnation pointer has %d rows", count)
	}
	id, err := newUUIDv4()
	if err != nil {
		return fmt.Errorf("creating processing incarnation: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO processing_incarnations(incarnation_id,created_at) VALUES(?,?)`,
		id, nowRFC3339()); err != nil {
		return fmt.Errorf("creating processing incarnation: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO current_processing_incarnation(singleton,incarnation_id) VALUES(1,?)`, id); err != nil {
		return fmt.Errorf("activating processing incarnation: %w", err)
	}
	return nil
}

func (s *Store) CurrentProcessingIncarnation(ctx context.Context) (ProcessingIncarnation, error) {
	var value ProcessingIncarnation
	var created string
	err := s.db.QueryRowContext(ctx, `
		SELECT i.incarnation_id,i.created_at
		FROM current_processing_incarnation c
		JOIN processing_incarnations i ON i.incarnation_id=c.incarnation_id
		WHERE c.singleton=1`).Scan(&value.ID, &created)
	if err != nil {
		return ProcessingIncarnation{}, fmt.Errorf("reading current processing incarnation: %w", err)
	}
	value.CreatedAt, err = time.Parse(timestampLayout, created)
	if err != nil {
		return ProcessingIncarnation{}, fmt.Errorf("reading current processing incarnation timestamp: %w", err)
	}
	return value, nil
}

func (s *Store) GrantConsent(
	ctx context.Context, request ProcessingConsentGrantRequest,
) (ProcessingConsentGrant, error) {
	authority, err := normalizeConsentAuthority(ProviderOperationAuthorizationRequest{
		Principal: request.Principal, Scope: request.Scope,
		ProfileFingerprint:      request.ProfileFingerprint,
		DisclosureFingerprint:   request.DisclosureFingerprint,
		InputClasses:            request.InputClasses,
		RetainedArtifactClasses: request.RetainedArtifactClasses,
	})
	if err != nil {
		return ProcessingConsentGrant{}, err
	}
	id, err := newUUIDv4()
	if err != nil {
		return ProcessingConsentGrant{}, fmt.Errorf("granting processing consent: %w", err)
	}
	issuedAt := time.Now().UTC()
	issuedRaw := issuedAt.Format(timestampLayout)
	var expiresRaw any
	var expiresAt *time.Time
	if request.ExpiresAt != nil {
		value := request.ExpiresAt.UTC()
		expiresRaw = value.Format(timestampLayout)
		expiresAt = &value
	}
	grant := ProcessingConsentGrant{
		ID: id, VaultID: s.vaultID, Principal: authority.principal, Scope: authority.scope,
		ProfileFingerprint: authority.profile, DisclosureFingerprint: authority.disclosure,
		InputClasses: authority.inputs, RetainedArtifactClasses: authority.retained,
		IssuedAt: issuedAt, ExpiresAt: expiresAt,
	}
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		incarnationID, err := currentProcessingIncarnationIDTx(ctx, tx)
		if err != nil {
			return err
		}
		grant.ProcessingIncarnationID = incarnationID
		grant.RevocationFence, err = consentRevocationFenceTx(ctx, tx, s.vaultID,
			incarnationID, authority.principal, authority.scope)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO processing_consent_grants(
				grant_id,vault_uid,incarnation_id,principal,scope,profile_fingerprint,
				disclosure_fingerprint,input_classes_json,retained_classes_json,
				revocation_fence,issued_at,expires_at
			) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, grant.ID, grant.VaultID,
			grant.ProcessingIncarnationID, grant.Principal, grant.Scope,
			grant.ProfileFingerprint, grant.DisclosureFingerprint, authority.inputsJSON,
			authority.retainedJSON, grant.RevocationFence, issuedRaw, expiresRaw)
		return err
	})
	if err != nil {
		return ProcessingConsentGrant{}, fmt.Errorf("granting processing consent: %w", err)
	}
	return grant, nil
}

func (s *Store) RevokeConsent(
	ctx context.Context, request ProcessingConsentRevocationRequest,
) (ProcessingConsentRevocation, error) {
	principal, err := normalizeConsentLabel("principal", request.Principal)
	if err != nil {
		return ProcessingConsentRevocation{}, err
	}
	scope, err := normalizeConsentLabel("scope", request.Scope)
	if err != nil {
		return ProcessingConsentRevocation{}, err
	}
	id, err := newUUIDv4()
	if err != nil {
		return ProcessingConsentRevocation{}, fmt.Errorf("revoking processing consent: %w", err)
	}
	revokedAt := time.Now().UTC()
	result := ProcessingConsentRevocation{
		ID: id, VaultID: s.vaultID, Principal: principal, Scope: scope, RevokedAt: revokedAt,
	}
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		result.ProcessingIncarnationID, err = currentProcessingIncarnationIDTx(ctx, tx)
		if err != nil {
			return err
		}
		prior, err := consentRevocationFenceTx(ctx, tx, s.vaultID,
			result.ProcessingIncarnationID, principal, scope)
		if err != nil {
			return err
		}
		result.Fence = prior + 1
		_, err = tx.ExecContext(ctx, `
			INSERT INTO processing_consent_revocations(
				revocation_id,vault_uid,incarnation_id,principal,scope,fence,revoked_at
			) VALUES(?,?,?,?,?,?,?)`, result.ID, result.VaultID,
			result.ProcessingIncarnationID, principal, scope, result.Fence,
			revokedAt.Format(timestampLayout))
		return err
	})
	if err != nil {
		return ProcessingConsentRevocation{}, fmt.Errorf("revoking processing consent: %w", err)
	}
	return result, nil
}

func (s *Store) AuthorizeProviderOperation(
	ctx context.Context, request ProviderOperationAuthorizationRequest,
) (ProviderOperationAuthorization, error) {
	authority, err := normalizeConsentAuthority(request)
	if err != nil {
		return ProviderOperationAuthorization{}, err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ProviderOperationAuthorization{}, fmt.Errorf("authorizing provider operation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	incarnationID, err := currentProcessingIncarnationIDTx(ctx, tx)
	if err != nil {
		return ProviderOperationAuthorization{}, err
	}
	fence, err := consentRevocationFenceTx(ctx, tx, s.vaultID, incarnationID,
		authority.principal, authority.scope)
	if err != nil {
		return ProviderOperationAuthorization{}, err
	}
	if request.PriorAuthorization != nil &&
		(request.PriorAuthorization.ProcessingIncarnationID != incarnationID ||
			request.PriorAuthorization.RevocationFence != fence) {
		return ProviderOperationAuthorization{}, ErrProcessingConsentRevoked
	}
	query := `
		SELECT grant_id,revocation_fence,expires_at
		FROM processing_consent_grants
		WHERE vault_uid=? AND incarnation_id=? AND principal=? AND scope=?
		  AND profile_fingerprint=? AND disclosure_fingerprint=?
		  AND input_classes_json=? AND retained_classes_json=?`
	args := []any{s.vaultID, incarnationID,
		authority.principal, authority.scope, authority.profile, authority.disclosure,
		authority.inputsJSON, authority.retainedJSON}
	if request.PriorAuthorization != nil {
		query += ` AND grant_id=?`
		args = append(args, request.PriorAuthorization.GrantID)
	}
	query += ` ORDER BY issued_at DESC,grant_id DESC`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return ProviderOperationAuthorization{}, fmt.Errorf("authorizing provider operation: %w", err)
	}
	defer func() { _ = rows.Close() }()
	foundCurrentFence, foundRevoked := false, false
	for rows.Next() {
		var grantID string
		var grantFence int64
		var expires sql.NullString
		if err := rows.Scan(&grantID, &grantFence, &expires); err != nil {
			return ProviderOperationAuthorization{}, fmt.Errorf("authorizing provider operation: %w", err)
		}
		if grantFence != fence {
			foundRevoked = true
			continue
		}
		foundCurrentFence = true
		if expires.Valid {
			expiry, err := time.Parse(timestampLayout, expires.String)
			if err != nil {
				return ProviderOperationAuthorization{}, fmt.Errorf("authorizing provider operation: invalid grant expiry: %w", err)
			}
			if !expiry.After(now) {
				continue
			}
		}
		if err := tx.Commit(); err != nil {
			return ProviderOperationAuthorization{}, fmt.Errorf("authorizing provider operation: %w", err)
		}
		return ProviderOperationAuthorization{
			GrantID: grantID, ProcessingIncarnationID: incarnationID,
			RevocationFence: fence, AuthorizedAt: now,
		}, nil
	}
	if err := rows.Err(); err != nil {
		return ProviderOperationAuthorization{}, fmt.Errorf("authorizing provider operation: %w", err)
	}
	if foundCurrentFence {
		return ProviderOperationAuthorization{}, ErrProcessingConsentExpired
	}
	if foundRevoked {
		return ProviderOperationAuthorization{}, ErrProcessingConsentRevoked
	}
	return ProviderOperationAuthorization{}, ErrProcessingConsentRequired
}

func currentProcessingIncarnationIDTx(
	ctx context.Context, tx interface {
		QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	},
) (string, error) {
	var id string
	if err := tx.QueryRowContext(ctx,
		`SELECT incarnation_id FROM current_processing_incarnation WHERE singleton=1`).Scan(&id); err != nil {
		return "", fmt.Errorf("reading current processing incarnation: %w", err)
	}
	return id, nil
}

func consentRevocationFenceTx(
	ctx context.Context, tx interface {
		QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	},
	vaultID, incarnationID, principal, scope string,
) (int64, error) {
	var fence int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(fence),0) FROM processing_consent_revocations
		WHERE vault_uid=? AND incarnation_id=? AND principal=? AND scope=?`,
		vaultID, incarnationID, principal, scope).Scan(&fence); err != nil {
		return 0, fmt.Errorf("reading processing consent revocation fence: %w", err)
	}
	return fence, nil
}

func normalizeConsentAuthority(
	request ProviderOperationAuthorizationRequest,
) (normalizedConsentAuthority, error) {
	principal, err := normalizeConsentLabel("principal", request.Principal)
	if err != nil {
		return normalizedConsentAuthority{}, err
	}
	scope, err := normalizeConsentLabel("scope", request.Scope)
	if err != nil {
		return normalizedConsentAuthority{}, err
	}
	if err := validateCatalogSHA256(request.ProfileFingerprint, "processing consent profile fingerprint"); err != nil {
		return normalizedConsentAuthority{}, err
	}
	if err := validateCatalogSHA256(request.DisclosureFingerprint, "processing consent disclosure fingerprint"); err != nil {
		return normalizedConsentAuthority{}, err
	}
	inputs, inputsJSON, err := normalizeConsentClasses("input", request.InputClasses, true)
	if err != nil {
		return normalizedConsentAuthority{}, err
	}
	retained, retainedJSON, err := normalizeConsentClasses("retained artifact", request.RetainedArtifactClasses, false)
	if err != nil {
		return normalizedConsentAuthority{}, err
	}
	return normalizedConsentAuthority{
		principal: principal, scope: scope, profile: request.ProfileFingerprint,
		disclosure: request.DisclosureFingerprint, inputs: inputs, retained: retained,
		inputsJSON: inputsJSON, retainedJSON: retainedJSON,
	}, nil
}

func normalizeConsentLabel(subject, value string) (string, error) {
	if !utf8.ValidString(value) || value == "" || len(value) > 256 || strings.TrimSpace(value) != value {
		return "", fmt.Errorf("processing consent %s must be 1 to 256 UTF-8 bytes without surrounding whitespace", subject)
	}
	return value, nil
}

func normalizeConsentClasses(subject string, values []string, required bool) ([]string, string, error) {
	if (required && len(values) == 0) || len(values) > 64 {
		return nil, "", fmt.Errorf("processing consent %s classes have an invalid count", subject)
	}
	result := slices.Clone(values)
	if result == nil {
		result = []string{}
	}
	for _, value := range result {
		if !utf8.ValidString(value) || value == "" || len(value) > 128 || strings.TrimSpace(value) != value {
			return nil, "", fmt.Errorf("processing consent %s class must be 1 to 128 UTF-8 bytes without surrounding whitespace", subject)
		}
	}
	slices.Sort(result)
	result = slices.Compact(result)
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, "", fmt.Errorf("encoding processing consent %s classes: %w", subject, err)
	}
	return result, string(encoded), nil
}
