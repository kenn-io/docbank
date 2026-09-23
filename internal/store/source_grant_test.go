package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSourceGrantBindingRequiresExactDurableAuthority(t *testing.T) {
	version := "9c988377-87be-4bef-94fc-24e0e69831bd"
	binding := SourceGrantBinding{
		SubjectID: "synthetic:reader", CredentialKind: "test", Audience: "docbank:test",
		GrantRevision: 7, ExpiresAt: time.Now().UTC().Add(time.Hour), SourceID: version,
	}
	require.NoError(t, validateSourceGrantBinding(&binding, version))
	other := binding
	other.SourceID = "a819ff27-0e0c-466e-9cea-c463ebfac0b3"
	require.Error(t, validateSourceGrantBinding(&other, version))
	other = binding
	other.GrantRevision = 0
	require.Error(t, validateSourceGrantBinding(&other, version))
	other = binding
	other.ExpiresAt = time.Time{}
	require.Error(t, validateSourceGrantBinding(&other, version))
}

func TestSourceGrantRecheckFailsClosedOnMissingRevokedOrExpiredAuthority(t *testing.T) {
	version := "9c988377-87be-4bef-94fc-24e0e69831bd"
	now := time.Now().UTC()
	binding := &SourceGrantBinding{SubjectID: "synthetic:reader", CredentialKind: "test",
		Audience: "docbank:test", GrantRevision: 7, ExpiresAt: now.Add(time.Minute), SourceID: version}
	require.ErrorIs(t, RecheckSourceGrant(t.Context(), binding, version, now, nil), ErrSourceGrantUnavailable)
	require.ErrorIs(t, RecheckSourceGrant(t.Context(), binding, version, now,
		SourceGrantAuthorizeFunc(func(context.Context, SourceGrantBinding) error {
			return ErrSourceGrantUnavailable
		})), ErrSourceGrantUnavailable)
	require.ErrorIs(t, RecheckSourceGrant(t.Context(), binding, version, now.Add(time.Minute),
		SourceGrantAuthorizeFunc(func(context.Context, SourceGrantBinding) error { return nil })), ErrSourceGrantUnavailable)
	require.NoError(t, RecheckSourceGrant(t.Context(), nil, version, now, nil),
		"legacy and local-admin jobs remain unrestricted by a scoped grant")
}
