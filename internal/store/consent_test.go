package store

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
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
	require.ErrorContains(t, err, "only be checked during atomic publication")
	assert.Equal(t, int64(0), leased.RevocationFence,
		"the earlier authorization receipt must remain immutable evidence")

	second, err := s.GrantConsent(t.Context(), grantRequestForAuthorization(request, nil))
	require.NoError(t, err)
	assert.Equal(t, revocation.Fence, second.RevocationFence)
	_, err = s.AuthorizeProviderOperation(t.Context(), request)
	require.NoError(t, err, "a grant issued after the revocation fence is current authority")
	_, err = s.AuthorizeProviderOperation(t.Context(), recheck)
	require.ErrorContains(t, err, "only be checked during atomic publication")
}

func TestProcessingConsentRevocationIsCheckedInPublicationTransaction(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	operation := document.RenditionAuthorization{
		RenditionRequestFingerprint: profile.RenditionRequestFingerprint,
		SourceSHA256:                catalogSourceHash,
		InputKind:                   document.RenditionInputOriginalFile,
	}
	operationChecksum, err := operation.Fingerprint()
	require.NoError(t, err)
	build := lexicalSearchBuild(s, profile, catalogBuildID, "authorized synthetic evidence")
	build.AuthorizationChecksum = operationChecksum
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	generation, err := s.StageLexicalGeneration(t.Context(), fakeHash("c9"))
	require.NoError(t, err)
	attachment := RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0],
		BuildID: build.ID, Profile: profile, AttachedAt: "2026-08-29T14:00:00.000000000Z",
	}
	head := RenditionHeadRecord{
		ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: attachment.ID, PublishedAt: "2026-08-29T14:01:00.000000000Z",
	}
	request := testProviderAuthorizationRequest()
	request.ProfileFingerprint = profile.Fingerprint
	request.DisclosureFingerprint = profile.RenditionDisclosureFingerprint
	request.RetainedArtifactClasses = []string{"normalized_evidence", "sanitized_markdown"}
	_, err = s.GrantConsent(t.Context(), grantRequestForAuthorization(request, nil))
	require.NoError(t, err)
	leased, err := s.AuthorizeProviderOperation(t.Context(), request)
	require.NoError(t, err)
	_, err = s.RevokeConsent(t.Context(), ProcessingConsentRevocationRequest{
		Principal: request.Principal, Scope: request.Scope,
	})
	require.NoError(t, err)

	publication := request
	publication.PriorAuthorization = &leased
	err = s.PublishAuthorizedRenditionAndLexicalHeads(
		t.Context(), attachment, head, generation.ID, publication, operation,
	)
	require.ErrorIs(t, err, ErrProcessingConsentRevoked)
	_, err = s.ActiveRendition(t.Context(), versions[0], profile.Fingerprint)
	require.ErrorIs(t, err, ErrNotFound)

	narrow := request
	narrow.RetainedArtifactClasses = []string{"normalized_evidence"}
	_, err = s.GrantConsent(t.Context(), grantRequestForAuthorization(narrow, nil))
	require.NoError(t, err)
	current, err := s.AuthorizeProviderOperation(t.Context(), narrow)
	require.NoError(t, err)
	publication = narrow
	publication.PriorAuthorization = &current
	err = s.PublishAuthorizedRenditionAndLexicalHeads(
		t.Context(), attachment, head, generation.ID, publication, operation,
	)
	require.ErrorContains(t, err, "retained artifact classes do not match")
	_, err = s.ActiveRendition(t.Context(), versions[0], profile.Fingerprint)
	require.ErrorIs(t, err, ErrNotFound)

	wrongInput := request
	wrongInput.InputClasses = []string{string(document.RenditionInputDerivedUpload)}
	_, err = s.GrantConsent(t.Context(), grantRequestForAuthorization(wrongInput, nil))
	require.NoError(t, err)
	current, err = s.AuthorizeProviderOperation(t.Context(), wrongInput)
	require.NoError(t, err)
	publication = wrongInput
	publication.PriorAuthorization = &current
	err = s.PublishAuthorizedRenditionAndLexicalHeads(
		t.Context(), attachment, head, generation.ID, publication, operation,
	)
	require.ErrorContains(t, err, "input classes do not match")

	_, err = s.GrantConsent(t.Context(), grantRequestForAuthorization(request, nil))
	require.NoError(t, err)
	current, err = s.AuthorizeProviderOperation(t.Context(), request)
	require.NoError(t, err)
	publication = request
	publication.PriorAuthorization = &current
	require.NoError(t, s.PublishAuthorizedRenditionAndLexicalHeads(
		t.Context(), attachment, head, generation.ID, publication, operation,
	))
	published, err := s.ActiveRendition(t.Context(), versions[0], profile.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, attachment.ID, published.Attachment.ID)
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

func TestProcessingConsentValidationRejectsCorruptRows(t *testing.T) {
	t.Run("noncanonical grant classes", func(t *testing.T) {
		s := newTestStore(t)
		grant, err := s.GrantConsent(
			t.Context(), grantRequestForAuthorization(testProviderAuthorizationRequest(), nil),
		)
		require.NoError(t, err)

		_, err = s.db.Exec(`DROP TRIGGER processing_consent_grants_immutable_update`)
		require.NoError(t, err)
		_, err = s.db.Exec(`
			UPDATE processing_consent_grants SET input_classes_json=? WHERE grant_id=?`,
			`["rendition_chunk","original_file"]`, grant.ID)
		require.NoError(t, err)
		_, err = s.db.Exec(`
			CREATE TRIGGER processing_consent_grants_immutable_update
			BEFORE UPDATE ON processing_consent_grants BEGIN
				SELECT RAISE(ABORT, 'processing consent grant records are immutable');
			END`)
		require.NoError(t, err)

		err = validateProcessingMetadataState(t.Context(), s.db)
		require.ErrorContains(t, err, "processing consent class sets are not canonical")
	})

	t.Run("invalid revocation timestamp", func(t *testing.T) {
		s := newTestStore(t)
		request := testProviderAuthorizationRequest()
		revocation, err := s.RevokeConsent(t.Context(), ProcessingConsentRevocationRequest{
			Principal: request.Principal, Scope: request.Scope,
		})
		require.NoError(t, err)

		_, err = s.db.Exec(`DROP TRIGGER processing_consent_revocations_immutable_update`)
		require.NoError(t, err)
		_, err = s.db.Exec(`
			UPDATE processing_consent_revocations SET revoked_at=? WHERE revocation_id=?`,
			`not-a-timestamp`, revocation.ID)
		require.NoError(t, err)
		_, err = s.db.Exec(`
			CREATE TRIGGER processing_consent_revocations_immutable_update
			BEFORE UPDATE ON processing_consent_revocations BEGIN
				SELECT RAISE(ABORT, 'processing consent revocation records are immutable');
			END`)
		require.NoError(t, err)

		err = validateProcessingMetadataState(t.Context(), s.db)
		require.ErrorContains(t, err, "processing consent revoked_at")
	})
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

	require.NoError(t, target.ImportMetadataForBackupRestore(t.Context(), bytes.NewReader(snapshot.Bytes())))
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
