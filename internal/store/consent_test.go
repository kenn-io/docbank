package store

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessingConsentAuthorizesOnlyExactCurrentGrant(t *testing.T) {
	s := newTestStore(t)
	request := testProviderAuthorizationRequest()

	_, err := s.AuthorizeProviderOperation(t.Context(), request)
	require.ErrorIs(t, err, ErrProcessingConsentRequired)

	grant, err := s.GrantConsent(t.Context(), ProcessingConsentGrantRequest{
		Principal: request.Principal, Scope: request.Scope,
		ProfileFingerprint:      request.ProfileFingerprint,
		DisclosureFingerprint:   request.DisclosureFingerprint,
		InputClasses:            request.InputClasses,
		RetainedArtifactClasses: request.RetainedArtifactClasses,
	})
	require.NoError(t, err)
	require.NoError(t, validateUUIDv4(grant.ID))
	require.NoError(t, validateUUIDv4(grant.ProcessingIncarnationID))
	assert.Equal(t, s.VaultID(), grant.VaultID)
	assert.Equal(t, int64(0), grant.RevocationFence)

	authorization, err := s.AuthorizeProviderOperation(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, grant.ID, authorization.GrantID)
	assert.Equal(t, grant.ProcessingIncarnationID, authorization.ProcessingIncarnationID)
	assert.Equal(t, grant.RevocationFence, authorization.RevocationFence)
}

func TestProcessingConsentRejectsExpiredAndDriftedOperations(t *testing.T) {
	tests := map[string]func(ProviderOperationAuthorizationRequest) ProviderOperationAuthorizationRequest{
		"profile drift including endpoint or deployment epoch": func(r ProviderOperationAuthorizationRequest) ProviderOperationAuthorizationRequest {
			r.ProfileFingerprint = testConsentFingerprint("profile-b")
			return r
		},
		"disclosure drift": func(r ProviderOperationAuthorizationRequest) ProviderOperationAuthorizationRequest {
			r.DisclosureFingerprint = testConsentFingerprint("disclosure-b")
			return r
		},
		"changed source or derived inputs": func(r ProviderOperationAuthorizationRequest) ProviderOperationAuthorizationRequest {
			r.InputClasses = []string{"original_file", "rendition_chunk"}
			return r
		},
		"changed retained artifact classes": func(r ProviderOperationAuthorizationRequest) ProviderOperationAuthorizationRequest {
			r.RetainedArtifactClasses = []string{"normalized_evidence", "provider_markdown"}
			return r
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			request := testProviderAuthorizationRequest()
			_, err := s.GrantConsent(t.Context(), grantRequestForAuthorization(request, nil))
			require.NoError(t, err)

			_, err = s.AuthorizeProviderOperation(t.Context(), mutate(request))
			require.ErrorIs(t, err, ErrProcessingConsentRequired)
		})
	}

	t.Run("expired grant", func(t *testing.T) {
		s := newTestStore(t)
		request := testProviderAuthorizationRequest()
		expired := time.Now().Add(-time.Minute)
		_, err := s.GrantConsent(t.Context(), grantRequestForAuthorization(request, &expired))
		require.NoError(t, err)

		_, err = s.AuthorizeProviderOperation(t.Context(), request)
		require.ErrorIs(t, err, ErrProcessingConsentExpired)
	})

	t.Run("expired current-fence grant takes precedence over revoked history", func(t *testing.T) {
		s := newTestStore(t)
		request := testProviderAuthorizationRequest()
		_, err := s.GrantConsent(t.Context(), grantRequestForAuthorization(request, nil))
		require.NoError(t, err)
		_, err = s.RevokeConsent(t.Context(), ProcessingConsentRevocationRequest{
			Principal: request.Principal, Scope: request.Scope,
		})
		require.NoError(t, err)
		expired := time.Now().Add(-time.Minute)
		_, err = s.GrantConsent(t.Context(), grantRequestForAuthorization(request, &expired))
		require.NoError(t, err)

		_, err = s.AuthorizeProviderOperation(t.Context(), request)
		require.ErrorIs(t, err, ErrProcessingConsentExpired)
	})
}

func TestProcessingConsentRevocationFencesSubmissionAndPublication(t *testing.T) {
	s := newTestStore(t)
	request := testProviderAuthorizationRequest()
	first, err := s.GrantConsent(t.Context(), grantRequestForAuthorization(request, nil))
	require.NoError(t, err)
	leased, err := s.AuthorizeProviderOperation(t.Context(), request)
	require.NoError(t, err)

	revocation, err := s.RevokeConsent(t.Context(), ProcessingConsentRevocationRequest{
		Principal: request.Principal, Scope: request.Scope,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), revocation.Fence)
	assert.Equal(t, first.ProcessingIncarnationID, revocation.ProcessingIncarnationID)

	recheck := request
	recheck.PriorAuthorization = &leased
	_, err = s.AuthorizeProviderOperation(t.Context(), recheck)
	require.ErrorIs(t, err, ErrProcessingConsentRevoked,
		"publication must recheck consent after a leased operation returns")
	assert.Equal(t, int64(0), leased.RevocationFence,
		"the earlier authorization receipt must remain immutable evidence")

	second, err := s.GrantConsent(t.Context(), grantRequestForAuthorization(request, nil))
	require.NoError(t, err)
	assert.Equal(t, revocation.Fence, second.RevocationFence)
	_, err = s.AuthorizeProviderOperation(t.Context(), request)
	require.NoError(t, err, "a grant issued after the revocation fence is current authority")
	_, err = s.AuthorizeProviderOperation(t.Context(), recheck)
	require.ErrorIs(t, err, ErrProcessingConsentRevoked,
		"a replacement grant must not revive work leased below the revocation fence")
}

func TestProcessingConsentHistoryIsAppendOnly(t *testing.T) {
	s := newTestStore(t)
	request := testProviderAuthorizationRequest()
	grant, err := s.GrantConsent(t.Context(), grantRequestForAuthorization(request, nil))
	require.NoError(t, err)
	revocation, err := s.RevokeConsent(t.Context(), ProcessingConsentRevocationRequest{
		Principal: request.Principal, Scope: request.Scope,
	})
	require.NoError(t, err)

	for _, statement := range []string{
		`UPDATE processing_consent_grants SET scope='changed' WHERE grant_id='` + grant.ID + `'`,
		`DELETE FROM processing_consent_grants WHERE grant_id='` + grant.ID + `'`,
		`UPDATE processing_consent_revocations SET scope='changed' WHERE revocation_id='` + revocation.ID + `'`,
		`DELETE FROM processing_consent_revocations WHERE revocation_id='` + revocation.ID + `'`,
	} {
		_, err := s.db.Exec(statement)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "immutable")
	}
}

func TestProcessingConsentRestorePreservesHistoryButRotatesIncarnation(t *testing.T) {
	source := newTestStore(t)
	request := testProviderAuthorizationRequest()
	grant, err := source.GrantConsent(t.Context(), grantRequestForAuthorization(request, nil))
	require.NoError(t, err)

	var snapshot bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &snapshot))
	assert.Contains(t, snapshot.String(), grant.ID)

	target, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	freshIncarnation, err := target.CurrentProcessingIncarnation(t.Context())
	require.NoError(t, err)
	require.NotEqual(t, grant.ProcessingIncarnationID, freshIncarnation.ID)

	require.NoError(t, target.ImportMetadataForRestore(t.Context(), bytes.NewReader(snapshot.Bytes())))
	current, err := target.CurrentProcessingIncarnation(t.Context())
	require.NoError(t, err)
	assert.Equal(t, freshIncarnation.ID, current.ID)
	assert.Equal(t, source.VaultID(), target.VaultID())

	_, err = target.AuthorizeProviderOperation(t.Context(), request)
	require.ErrorIs(t, err, ErrProcessingConsentRequired,
		"a restored historical grant must not authorize the fresh incarnation")
	var historical int
	require.NoError(t, target.db.QueryRow(
		`SELECT COUNT(*) FROM processing_consent_grants WHERE grant_id=?`, grant.ID,
	).Scan(&historical))
	assert.Equal(t, 1, historical)
	var restored bytes.Buffer
	require.NoError(t, target.ExportMetadata(t.Context(), &restored))
	assert.Equal(t, snapshot.String(), restored.String(),
		"the fresh local incarnation is omitted while append-only consent history round-trips exactly")
}

func testProviderAuthorizationRequest() ProviderOperationAuthorizationRequest {
	return ProviderOperationAuthorizationRequest{
		Principal: "operator:vladimir", Scope: "document-processing",
		ProfileFingerprint:      testConsentFingerprint("profile-a"),
		DisclosureFingerprint:   testConsentFingerprint("disclosure-a"),
		InputClasses:            []string{"original_file"},
		RetainedArtifactClasses: []string{"normalized_evidence"},
	}
}

func grantRequestForAuthorization(
	request ProviderOperationAuthorizationRequest, expiresAt *time.Time,
) ProcessingConsentGrantRequest {
	return ProcessingConsentGrantRequest{
		Principal: request.Principal, Scope: request.Scope,
		ProfileFingerprint:      request.ProfileFingerprint,
		DisclosureFingerprint:   request.DisclosureFingerprint,
		InputClasses:            request.InputClasses,
		RetainedArtifactClasses: request.RetainedArtifactClasses,
		ExpiresAt:               expiresAt,
	}
}

func testConsentFingerprint(value string) string { return testSHA256([]byte(value)) }
