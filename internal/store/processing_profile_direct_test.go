package store

import (
	"bytes"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestProcessingProfileDirectAcceptsCanonicalEmbeddingOnlyRecord(t *testing.T) {
	control := catalogProcessingProfile(t, true)
	require.NoError(t, ValidateProcessingProfileRecord(control),
		"the existing rendition-present control must remain accepted")

	record := processingProfileDirectEmbeddingOnlyRecord(t)
	wantCanonical := bytes.Clone(record.CanonicalProfile)
	require.Empty(t, record.RenditionDisclosureFingerprint)
	require.NoError(t, ValidateProcessingProfileRecord(record))

	normalized, err := normalizeProcessingProfileRecord(record)
	require.NoError(t, err)
	assert.Equal(t, record, normalized, "normalization must preserve exact canonical identity")
	record.CanonicalProfile[0] = '['
	assert.Equal(t, wantCanonical, []byte(normalized.CanonicalProfile),
		"normalized JSON must not alias caller-owned bytes")

	for _, test := range []struct {
		name   string
		field  string
		mutate func(*ProcessingProfileRecord)
	}{
		{name: "profile", field: "profile fingerprint", mutate: func(value *ProcessingProfileRecord) {
			value.Fingerprint = fakeHash("direct-profile")
		}},
		{name: "rendition request", field: "rendition request fingerprint", mutate: func(value *ProcessingProfileRecord) {
			value.RenditionRequestFingerprint = fakeHash("direct-rendition")
		}},
		{name: "evidence lexical", field: "evidence lexical fingerprint", mutate: func(value *ProcessingProfileRecord) {
			value.EvidenceLexicalFingerprint = fakeHash("direct-evidence")
		}},
		{name: "retention disclosure", field: "retention disclosure fingerprint", mutate: func(value *ProcessingProfileRecord) {
			value.RetentionDisclosureFingerprint = fakeHash("direct-retention")
		}},
		{name: "attachment policy", field: "attachment policy fingerprint", mutate: func(value *ProcessingProfileRecord) {
			value.AttachmentPolicyFingerprint = fakeHash("direct-attachment")
		}},
		{name: "consent", field: "consent fingerprint", mutate: func(value *ProcessingProfileRecord) {
			value.ConsentFingerprint = fakeHash("direct-consent")
		}},
		{name: "fabricated rendition disclosure", field: "rendition disclosure fingerprint", mutate: func(value *ProcessingProfileRecord) {
			value.RenditionDisclosureFingerprint = fakeHash("direct-disclosure")
		}},
		{name: "trust boundary", field: "trust boundary", mutate: func(value *ProcessingProfileRecord) {
			value.TrustBoundary = "synthetic-other-boundary"
		}},
	} {
		t.Run(test.name+" mismatch", func(t *testing.T) {
			mismatched := processingProfileDirectEmbeddingOnlyRecord(t)
			test.mutate(&mismatched)
			_, err := normalizeProcessingProfileRecord(mismatched)
			require.ErrorContains(t, err, test.field+" does not match canonical profile")
		})
	}
}

func TestProcessingProfileDirectPreservesDocumentNilRenditionRules(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(*document.ProcessingProfileV1)
		message string
	}{
		{
			name: "sanitized Markdown retention",
			mutate: func(profile *document.ProcessingProfileV1) {
				profile.Rendition = nil
			},
			message: "retained Markdown requires a rendition binding",
		},
		{
			name: "provider Markdown retention",
			mutate: func(profile *document.ProcessingProfileV1) {
				profile.Rendition = nil
				profile.RetentionDisclosure.RetainSanitizedMarkdown = false
				profile.RetentionDisclosure.RetainProviderMarkdown = true
			},
			message: "retained Markdown requires a rendition binding",
		},
		{
			name: "rendition chunk embedding",
			mutate: func(profile *document.ProcessingProfileV1) {
				profile.Rendition = nil
				profile.RetentionDisclosure.RetainSanitizedMarkdown = false
				profile.RetentionDisclosure.RetainProviderMarkdown = false
				profile.Embeddings[0].InputKind = document.EmbeddingInputRenditionChunk
			},
			message: "rendition_chunk input requires a rendition binding",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := processingProfileDirectFixture(t)
			test.mutate(&profile)
			_, _, err := document.CanonicalProfile(profile)
			require.ErrorContains(t, err, test.message)
		})
	}
}

func TestProcessingProfileDirectPersistsReopensAndRoundTripsMetadata(t *testing.T) {
	ctx := t.Context()
	record := processingProfileDirectEmbeddingOnlyRecord(t)
	normalized, err := normalizeProcessingProfileRecord(record)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "source.db")
	source, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, source.withStorageTx(ctx, func(tx *sql.Tx) error {
		return ensureProcessingProfileTx(ctx, tx, normalized)
	}))
	stored, err := loadProcessingProfile(ctx, source.db, normalized.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, normalized, stored)
	require.NoError(t, source.Close())

	reopened, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	stored, err = loadProcessingProfile(ctx, reopened.db, normalized.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, normalized, stored)

	var first, second bytes.Buffer
	require.NoError(t, reopened.ExportMetadata(ctx, &first))
	require.NoError(t, reopened.ExportMetadata(ctx, &second))
	assert.Equal(t, first.Bytes(), second.Bytes(), "unchanged profile metadata must export byte-identically")

	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(first.Bytes())))
	restoredProfile, err := loadProcessingProfile(ctx, restored.db, normalized.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, normalized, restoredProfile)
	var restoredExport bytes.Buffer
	require.NoError(t, restored.ExportMetadata(ctx, &restoredExport))
	assert.Equal(t, first.Bytes(), restoredExport.Bytes(), "metadata import must preserve exact JSONL identity")

	corrupt := mutateFirstProcessingMetadataRecord(t, first.Bytes(), metadataProcessingProfileType,
		func(fields map[string]jsontext.Value) {
			fields["rendition_disclosure_fingerprint"] = jsontext.Value(`"` + fakeHash("direct-corrupt") + `"`)
		})
	rejected := newTestStore(t)
	err = rejected.ImportMetadata(ctx, bytes.NewReader(corrupt))
	require.ErrorContains(t, err, "rendition disclosure fingerprint does not match canonical profile")
	var profiles int
	require.NoError(t, rejected.db.QueryRow(`SELECT COUNT(*) FROM processing_profiles`).Scan(&profiles))
	assert.Zero(t, profiles, "failed import must publish no partial profile")
}

func TestProcessingProfileDirectCannotEnqueueRenditionJob(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	request := renditionJobTestRequest(versions[0], catalogProcessingProfile(t, true))
	grantRenditionJobConsent(t, s, request)
	request.Profile = processingProfileDirectEmbeddingOnlyRecord(t)

	_, _, err := s.EnqueueRenditionJob(t.Context(), request)
	require.ErrorContains(t, err, "profile has no executable rendition binding")
	processingProfileDirectRequireCounts(t, s, 0, 0, 0, 0)
}

func TestProcessingProfileDirectCannotPublishRenditionAttachment(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := processingProfileDirectEmbeddingOnlyRecord(t)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	attachment := RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0],
		BuildID: build.ID, Profile: profile, AttachedAt: "2026-09-08T10:00:00.000000000Z",
	}

	err := publishRenditionForTest(t, s, attachment, "2026-09-08T10:01:00.000000000Z",
		testSHA256([]byte("direct-generation")))
	require.ErrorContains(t, err, "rendition attachment profile lacks a rendition binding")
	processingProfileDirectRequireCounts(t, s, 0, 0, 0, 0)
}

func processingProfileDirectEmbeddingOnlyRecord(t *testing.T) ProcessingProfileRecord {
	t.Helper()
	profile := processingProfileDirectFixture(t)
	profile.Rendition = nil
	profile.RetentionDisclosure.RetainProviderMarkdown = false
	profile.RetentionDisclosure.RetainSanitizedMarkdown = false
	for _, binding := range profile.Embeddings {
		require.Equal(t, document.EmbeddingInputOriginalFile, binding.InputKind)
	}
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	return ProcessingProfileRecord{
		Fingerprint: fingerprints.Profile, CanonicalProfile: jsontext.Value(canonical),
		RenditionRequestFingerprint:    fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:     fingerprints.EvidenceLexical,
		RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
		AttachmentPolicyFingerprint:    profile.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             profile.RetentionDisclosure.ConsentFingerprint,
		RenditionDisclosureFingerprint: "",
		TrustBoundary:                  profile.RetentionDisclosure.TrustBoundary,
	}
}

func processingProfileDirectFixture(t *testing.T) document.ProcessingProfileV1 {
	t.Helper()
	fixture := catalogProcessingProfile(t, true)
	var profile document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(fixture.CanonicalProfile, &profile, json.RejectUnknownMembers(true)))
	require.NotNil(t, profile.Rendition)
	require.NotEmpty(t, profile.Embeddings)
	return profile
}

func processingProfileDirectRequireCounts(
	t *testing.T, s *Store, jobs, waiters, attachments, heads int,
) {
	t.Helper()
	var gotJobs, gotWaiters, gotAttachments, gotHeads int
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM rendition_jobs),
		(SELECT COUNT(*) FROM rendition_job_waiters),
		(SELECT COUNT(*) FROM rendition_attachments),
		(SELECT COUNT(*) FROM rendition_heads)
	`).Scan(&gotJobs, &gotWaiters, &gotAttachments, &gotHeads))
	assert.Equal(t, jobs, gotJobs)
	assert.Equal(t, waiters, gotWaiters)
	assert.Equal(t, attachments, gotAttachments)
	assert.Equal(t, heads, gotHeads)
}
