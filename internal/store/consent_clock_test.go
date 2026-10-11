package store

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProcessingConsentRenewalOrdersClockCollisions(t *testing.T) {
	t.Parallel()
	for _, offset := range []time.Duration{0, -time.Second, time.Second} {
		t.Run(offset.String(), func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			request := testProviderAuthorizationRequest()
			authority, err := normalizeConsentAuthority(request)
			require.NoError(t, err)
			issuedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			ids := []string{"ffffffff-ffff-4fff-8fff-ffffffffffff", "00000000-0000-4000-8000-000000000001"}
			for index, id := range ids {
				at := issuedAt
				if index > 0 {
					at = at.Add(offset)
				}
				grant := ProcessingConsentGrant{ID: id, ConsentSetID: id, VaultID: s.VaultID(),
					Principal: request.Principal, Scope: request.Scope,
					ProfileFingerprint: request.ProfileFingerprint, DisclosureFingerprint: request.DisclosureFingerprint,
					InputClasses: request.InputClasses, RetainedArtifactClasses: request.RetainedArtifactClasses,
					IssuedAt: at}
				err := s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
					return s.grantConsentTx(t.Context(), tx, authority, at.Format(timestampLayout), nil, &grant)
				})
				require.NoError(t, err)
				authorization, err := s.AuthorizeProviderOperation(t.Context(), request)
				require.NoError(t, err)
				require.Equal(t, id, authorization.GrantID, "renewal must select the last committed grant")
				expected := at
				if index > 0 && !at.After(issuedAt) {
					expected = issuedAt.Add(time.Nanosecond)
				}
				require.Equal(t, expected, grant.IssuedAt)
				var stored string
				require.NoError(t, s.db.QueryRowContext(t.Context(),
					`SELECT issued_at FROM processing_consent_grants WHERE grant_id=?`, id).Scan(&stored))
				require.Equal(t, expected.Format(timestampLayout), stored)
			}
		})
	}
}
