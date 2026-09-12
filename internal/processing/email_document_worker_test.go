package processing

import (
	"context"
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
	"io"
	"testing"
	"time"
)

const attachmentCSV = "item,value\nattachmentquasar,42\n"
const attachmentCSVSource = "Subject: Shared subject\r\nContent-Type: multipart/mixed; boundary=m\r\n\r\n--m\r\nContent-Type: text/plain\r\n\r\nbodynebula\r\n--m\r\nContent-Type: text/csv\r\nContent-Disposition: attachment; filename=table.csv\r\n\r\n" + attachmentCSV + "\r\n--m--\r\n"

type emailWorkerUpload struct {
	io.ReadCloser

	metadata document.AuthorizedUploadMetadata
}

func (u emailWorkerUpload) Metadata() document.AuthorizedUploadMetadata { return u.metadata }

type emailRealRuntime struct {
	provider document.RenditionProvider
	blobs    *blob.Store
}

func (r emailRealRuntime) Prepare(ctx context.Context, work store.RenditionJobWork, now time.Time) (RenditionExecution, error) {
	source, _, err := r.blobs.OpenStreamContext(ctx, work.Job.SourceSHA256)
	if err != nil {
		return RenditionExecution{}, err
	}
	// The scheduler owns stable identity; only the admission time is refreshed.
	b, err := json.Marshal(work.ExecutionIdentity.Authorization)
	if err != nil {
		_ = source.Close()
		return RenditionExecution{}, err
	}
	var auth document.RenditionAuthorization
	if err = json.Unmarshal(b, &auth); err != nil {
		_ = source.Close()
		return RenditionExecution{}, err
	}
	auth.AuthorizedAt = now.UTC().Format("2006-01-02T15:04:05.000000000Z")
	auth.ExpiresAt = now.UTC().Add(time.Minute).Format("2006-01-02T15:04:05.000000000Z")
	evidence, err := document.NewEvidencePolicy(100_000)
	if err != nil {
		_ = source.Close()
		return RenditionExecution{}, err
	}
	rendition, err := document.NewRenditionPolicy(document.RenditionLimits{MaxDocumentChars: 100_000, MaxUnitRunes: 1000, MaxSegmentRunes: 100})
	if err != nil {
		_ = source.Close()
		return RenditionExecution{}, err
	}
	execution := RenditionExecution{Provider: r.provider, Upload: emailWorkerUpload{source, work.ExecutionIdentity.Upload}, Authorization: auth, EvidencePolicy: evidence, RenditionPolicy: rendition}
	return execution, nil
}

func emailProcessingRequest(t *testing.T, receipt document.EmailDocumentPublicationReceipt, descriptor document.RenditionDescriptor) document.EmailDocumentProcessingRequest {
	t.Helper()
	child := receipt.Relations[0].Child
	require.NotNil(t, child)
	profile := workerProcessingProfile(t, descriptor)
	job := workerJobRequest(child.VersionID, profile, descriptor)
	job.ExecutionIdentity.Upload.SHA256 = child.SHA256
	job.ExecutionIdentity.Upload.ByteLength = child.Size
	job.ExecutionIdentity.Upload.Filename = "table.csv"
	job.ExecutionIdentity.Upload.MediaFamily = "spreadsheet"
	job.ExecutionIdentity.Upload.MediaType = "text/csv"
	job.ExecutionIdentity.Authorization.SourceSHA256 = child.SHA256
	job.ExecutionIdentity.Authorization.SourceBytes = child.Size
	job.ExecutionIdentity.Authorization.MediaFamily = "spreadsheet"
	job.ExecutionIdentity.Authorization.MediaType = "text/csv"
	var p document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(profile.CanonicalProfile, &p))
	p.Rendition.RequestedArtifacts = descriptor.ArtifactRoles
	_, fp, err := document.CanonicalProfile(p)
	require.NoError(t, err)
	job.ExecutionIdentity.Authorization.RenditionRequestFingerprint = fp.RenditionRequest
	job.ExecutionIdentity.Authorization.AllowedArtifactRoles = descriptor.ArtifactRoles
	if len(descriptor.ArtifactRoles) == 0 {
		job.ExecutionIdentity.Authorization.MaxArtifacts = 0
		job.ExecutionIdentity.Authorization.MaxArtifactBytes = 0
	}
	return document.EmailDocumentProcessingRequest{OperationID: receipt.OperationID, RequestDigest: receipt.RequestDigest, Order: 1, Profile: p, ExecutionIdentity: job.ExecutionIdentity, CapturedArtifactPolicy: job.CapturedArtifactPolicy, Principal: job.Authorization.Principal, Scope: job.Authorization.Scope, InputClasses: job.Authorization.InputClasses, RetainedArtifactClasses: job.Authorization.RetainedArtifactClasses}
}
func grantEmailProcessingConsent(t *testing.T, s *store.Store, r document.EmailDocumentProcessingRequest) document.ProcessingConsentReceipt {
	t.Helper()
	_, fp, err := document.CanonicalProfile(r.Profile)
	require.NoError(t, err)
	grant, err := s.GrantProcessingConsent(t.Context(), document.ProcessingConsentRequest{Principal: r.Principal, Scope: r.Scope, ProfileFingerprint: fp.Profile, DisclosureFingerprint: r.Profile.Rendition.DisclosureFingerprint, InputClasses: r.InputClasses, RetainedArtifactClasses: r.RetainedArtifactClasses})
	require.NoError(t, err)
	return grant
}

func TestEmailDocumentsRemoteConsentBindingAndRevocation(t *testing.T) {
	f := newEmailPipelineFixture(t)
	target := f.add(t, "remote.eml", attachmentCSVSource, "message/rfc822")
	view, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
	require.NoError(t, err)
	receipt, err := PublishEmailDocuments(t.Context(), f.catalog, f.blobs, pipelineDocumentRequest(t, f, view, "remote-consent"))
	require.NoError(t, err)
	provider := newWorkerProvider(t)
	descriptor := provider.Descriptor()
	descriptor.Fingerprint = ""
	descriptor.TrustBoundary = document.RenditionTrustHostedProvider
	descriptor.SupportedFormats = []document.RenditionFormatCapability{{MediaFamily: "spreadsheet", MediaType: "text/csv", InputKind: document.RenditionInputOriginalFile}}
	provider.descriptor, err = document.NewRenditionDescriptor(descriptor)
	require.NoError(t, err)
	request := emailProcessingRequest(t, receipt, provider.Descriptor())
	_, fp, err := document.CanonicalProfile(request.Profile)
	require.NoError(t, err)
	for _, change := range []string{"scope", "profile", "input"} {
		grant := document.ProcessingConsentRequest{Principal: request.Principal, Scope: request.Scope, ProfileFingerprint: fp.Profile, DisclosureFingerprint: request.Profile.Rendition.DisclosureFingerprint, InputClasses: request.InputClasses, RetainedArtifactClasses: request.RetainedArtifactClasses}
		switch change {
		case "scope":
			grant.Scope = "another-scope"
		case "profile":
			grant.ProfileFingerprint = processingHash("different-profile")
		case "input":
			grant.InputClasses = []string{"other_input"}
		}
		_, err = f.catalog.GrantProcessingConsent(t.Context(), grant)
		require.NoError(t, err)
		_, err = f.catalog.RequestEmailDocumentProcessing(t.Context(), request)
		require.ErrorIs(t, err, store.ErrProcessingConsentRequired)
	}
	grantEmailProcessingConsent(t, f.catalog, request)
	job, err := f.catalog.RequestEmailDocumentProcessing(t.Context(), request)
	require.NoError(t, err)
	_, err = f.catalog.RevokeProcessingConsent(t.Context(), document.ProcessingConsentRevocationRequest{Principal: request.Principal, Scope: request.Scope})
	require.NoError(t, err)
	_, err = f.catalog.RequestEmailDocumentProcessing(t.Context(), request)
	require.ErrorIs(t, err, store.ErrProcessingConsentRevoked)
	worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: f.catalog, Blobs: f.blobs, Runtime: emailRealRuntime{provider, f.blobs}, Gate: api.NewOperationGate(), Owner: "email-consent-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond})
	require.NoError(t, err)
	worked, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, worked)
	require.Zero(t, provider.calls, "revoked work must not enter the remote provider")
	current, err := f.catalog.RenditionJobByID(t.Context(), job.JobID)
	require.NoError(t, err)
	require.Equal(t, store.RenditionJobFailed, current.State)
	require.Equal(t, store.RenditionFailureConsent, current.FailureCode)
	page, err := f.catalog.EmailDocumentRelations(t.Context(), document.EmailDocumentRelationQuery{ChildVersionID: job.Child.VersionID})
	require.NoError(t, err)
	require.Equal(t, "failed", page.Items[0].State)
	require.Equal(t, "consent", page.Items[0].Reason)
}

// MAIL04: real spreadsheet bytes go through the shipped local provider and
// worker; only its completed exact rendition may explain the search match.
func TestEmailDocumentsRealCSVProviderSearchAndConsent(t *testing.T) {
	f := newEmailPipelineFixture(t)
	target := f.add(t, "source.eml", attachmentCSVSource, "message/rfc822")
	view, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
	require.NoError(t, err)
	receipt, err := PublishEmailDocuments(t.Context(), f.catalog, f.blobs, pipelineDocumentRequest(t, f, view, "csv-publication"))
	require.NoError(t, err)
	require.Len(t, receipt.Relations, 1)
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	require.Contains(t, provider.Descriptor().SupportedFormats, document.RenditionFormatCapability{MediaFamily: "spreadsheet", MediaType: "text/csv", InputKind: document.RenditionInputOriginalFile})
	request := emailProcessingRequest(t, receipt, provider.Descriptor())
	_, err = f.catalog.RequestEmailDocumentProcessing(t.Context(), request)
	require.ErrorIs(t, err, store.ErrProcessingConsentRequired)
	hits, _, err := f.catalog.SearchPage(t.Context(), "attachmentquasar", 10)
	require.NoError(t, err)
	require.Empty(t, hits)
	grantEmailProcessingConsent(t, f.catalog, request)
	job, err := f.catalog.RequestEmailDocumentProcessing(t.Context(), request)
	require.NoError(t, err)
	page, err := f.catalog.EmailDocumentRelations(t.Context(), document.EmailDocumentRelationQuery{ChildVersionID: job.Child.VersionID})
	require.NoError(t, err)
	require.Equal(t, "pending", page.Items[0].State)
	worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: f.catalog, Blobs: f.blobs, Runtime: emailRealRuntime{provider, f.blobs}, Gate: api.NewOperationGate(), Owner: "email-csv-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond})
	require.NoError(t, err)
	worked, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, worked)
	current, err := f.catalog.RenditionJobByID(t.Context(), job.JobID)
	require.NoError(t, err)
	require.Equal(t, store.RenditionJobCompleted, current.State, "phase=%s failure=%s attempts=%d", current.Phase, current.FailureCode, current.ProviderAttempts)
	_, fp, err := document.CanonicalProfile(request.Profile)
	require.NoError(t, err)
	active, err := f.catalog.ActiveRendition(t.Context(), job.Child.VersionID, fp.Profile)
	require.NoError(t, err)
	require.Equal(t, job.JobID, active.Build.ID)
	hits, _, err = f.catalog.SearchPage(t.Context(), "attachmentquasar", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, job.Child.VersionID, hits[0].Node.CurrentVersionID)
	explained, _, err := f.catalog.SearchExplainedLexicalCandidates(t.Context(), "attachmentquasar", 10, store.SearchOptions{})
	require.NoError(t, err)
	require.Len(t, explained, 1)
	require.Equal(t, job.JobID, explained[0].BuildID)
	page, err = f.catalog.EmailDocumentRelations(t.Context(), document.EmailDocumentRelationQuery{ChildVersionID: job.Child.VersionID})
	require.NoError(t, err)
	require.Equal(t, "indexed", page.Items[0].State)
	require.Equal(t, target.Version.ID, page.Items[0].Relation.Parent.VersionID)
	body, _, err := f.catalog.SearchPage(t.Context(), "bodynebula", 10)
	require.NoError(t, err)
	require.Len(t, body, 1)
	require.Equal(t, target.Version.ID, body[0].Node.CurrentVersionID)
	t.Logf("MAIL04 provider=%s descriptor=%s child_build=%s exact child/parent and body separation verified", provider.Descriptor().ID, provider.Descriptor().Fingerprint, job.JobID)
	require.NoError(t, f.catalog.ValidateMetadata(t.Context()))
	f.emptySpool(t)
}
