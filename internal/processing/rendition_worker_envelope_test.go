package processing

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestRenditionWorkerRetainsCanonicalEnvelope(t *testing.T) {
	fixture := newPublicationFixture(t)
	provider := newWorkerProvider(t)
	profile := workerProcessingProfile(t, provider.Descriptor())
	request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
	const metadataOnlyMarker = "HEADER-ONLY-MARKER"
	request.ExecutionIdentity.Upload.Filename = metadataOnlyMarker + ".PDF"
	grantWorkerConsent(t, fixture.catalog, request)
	job, view := runWorkerEnvelopeJob(t, fixture, provider, profile, request)
	require.Equal(t, job.ID, view.Build.ID)
	markdown := retainedWorkerMarkdown(t, fixture, view.Build)
	require.Contains(t, string(markdown), "Synthetic worker output",
		"the actual retained body is the pre-envelope positive control")
	facts, body, err := document.ParseRenditionFrontMatterV1(markdown)
	require.NoError(t, err)
	require.Equal(t, document.RenditionMarkdownContractV1, facts.Contract)
	expectedEvidence, expectedBody := workerBodyRendition(t, profile)
	require.Equal(t, expectedBody.Markdown, body)
	require.Equal(t, processingSHA256(body), facts.Rendition.BodySHA256)
	require.Equal(t, view.Build.MarkdownChecksum, processingSHA256(markdown))
	markdownRecord := workerArtifactRecord(t, view.Build, "sanitized_markdown")
	require.Equal(t, view.Build.MarkdownChecksum, markdownRecord.BlobHash)
	require.Equal(t, view.Build.MarkdownChecksum, markdownRecord.Checksum)
	require.Equal(t, int64(len(markdown)), markdownRecord.Size)
	require.Equal(t, document.RenditionMarkdownSourceV1{
		SHA256: fixture.mustSourceHash(), Format: "pdf", MediaType: "application/pdf",
	}, facts.Source)
	require.Equal(t, job.ID, facts.Rendition.BuildID)
	require.Equal(t, profile.RenditionRequestFingerprint, facts.Rendition.RenditionRequestFingerprint)
	require.Equal(t, profile.EvidenceLexicalFingerprint, facts.Rendition.EvidenceLexicalFingerprint)
	require.Equal(t, document.NormalizedEvidenceContractV1, facts.Rendition.NormalizedEvidenceContract)
	require.Equal(t, expectedBody.Completeness, facts.Rendition.Completeness)
	require.False(t, facts.Rendition.Truncated)
	require.Equal(t, document.EvidenceUnitGeneric, facts.Document.UnitKind)
	require.Equal(t, len(expectedBody.Units), facts.Document.UnitCount)
	require.Equal(t, document.RenditionNavigationOffsetBody, facts.Navigation.OffsetBase)
	require.True(t, facts.Navigation.Complete)
	require.Len(t, facts.Navigation.Entries, 1)
	require.Equal(t, expectedBody.Units[0].EvidenceUnitID, facts.Navigation.Entries[0].Key)
	require.Equal(t, document.EvidenceLocatorGeneric, facts.Navigation.Entries[0].Kind)
	require.Equal(t, 1, facts.Navigation.Entries[0].Line)
	require.Equal(t, bytes.Index(body, []byte(expectedBody.Units[0].Text)), facts.Navigation.Entries[0].Byte)

	require.Equal(t, workerUnitRecords(expectedBody), view.Build.Units)
	require.Equal(t, workerSegmentRecords(expectedBody), view.Build.LexicalSegments)
	require.Equal(t, expectedBody.EvidenceChecksum, view.Build.EvidenceChecksum)
	require.Equal(t, processingSHA256(expectedEvidence),
		workerArtifactRecord(t, view.Build, "normalized_evidence").BlobHash)
	var receipt document.RenditionReceipt
	require.NoError(t, json.Unmarshal(view.Build.ProviderReceipt, &receipt, json.RejectUnknownMembers(true)))
	require.Equal(t, "synthetic-operation", receipt.OperationID)
	require.Equal(t, fixture.mustSourceHash(), receipt.SourceSHA256)
	require.Equal(t, profile.RenditionRequestFingerprint, receipt.RenditionRequestFingerprint)

	header := markdown[:len(markdown)-len(body)]
	require.NotContains(t, string(header), metadataOnlyMarker)
	require.NotContains(t, string(header), "source.pdf")
	require.NotContains(t, string(header), request.ContentVersionID)
	require.NotContains(t, string(header), view.Attachment.ID)
	hits, _, err := fixture.catalog.SearchPage(t.Context(), metadataOnlyMarker, 10)
	require.NoError(t, err)
	require.Empty(t, hits, "header metadata must not enter body-derived lexical search")
	hits, _, err = fixture.catalog.SearchPage(t.Context(), "Synthetic worker output", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, fixture.versionID, hits[0].Node.CurrentVersionID)
	require.Equal(t, "/source.pdf", hits[0].Path)
	require.Equal(t, store.SearchMatchContent, hits[0].Match)
}

func TestSourceFormatUsesDeterministicLocalPrecedence(t *testing.T) {
	for _, test := range []struct {
		name, filename, family, mediaType, want string
	}{
		{name: "extension", filename: "synthetic.Report.PDF", family: "document", mediaType: "text/plain", want: "pdf"},
		{name: "MIME subtype", family: "document", mediaType: "application/vnd.synthetic+json; charset=utf-8", want: "vnd.synthetic+json"},
		{name: "family", family: "synthetic-family", mediaType: "not a media type", want: "synthetic-family"},
		{name: "withheld filename", family: "document", mediaType: "application/pdf; version=1", want: "pdf"},
		{name: "leading dot MIME fallback", filename: ".customer-name", family: "document", mediaType: "application/pdf", want: "pdf"},
		{name: "leading dot family fallback", filename: ".customer-name", family: "synthetic-family", mediaType: "invalid", want: "synthetic-family"},
		{name: "hidden file extension", filename: ".customer-name.PDF", family: "document", mediaType: "text/plain", want: "pdf"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, sourceFormat(test.filename, test.family, test.mediaType))
		})
	}
}

func TestRenditionWorkerSharedWaitersReuseIdenticalEnvelope(t *testing.T) {
	fixture := newPublicationFixture(t)
	provider := newWorkerProvider(t)
	profile := workerProcessingProfile(t, provider.Descriptor())
	request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
	grantWorkerConsent(t, fixture.catalog, request)
	job, firstWaiter, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	second, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(),
		"shared.pdf", fixture.mustSourceHash(), int64(len(workerSourceBytes)), "application/pdf")
	require.NoError(t, err)
	request.ContentVersionID = second.CurrentVersionID
	_, secondWaiter, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	worker := newEnvelopeWorker(t, fixture, provider)
	processed, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	require.Equal(t, 1, provider.calls)
	firstView, err := fixture.catalog.ActiveRendition(t.Context(), fixture.versionID, profile.Fingerprint)
	require.NoError(t, err)
	secondView, err := fixture.catalog.ActiveRendition(t.Context(), second.CurrentVersionID, profile.Fingerprint)
	require.NoError(t, err)
	firstBytes := retainedWorkerMarkdown(t, fixture, firstView.Build)
	secondBytes := retainedWorkerMarkdown(t, fixture, secondView.Build)
	require.Equal(t, job.ID, firstView.Build.ID)
	require.Equal(t, firstView.Build.ID, secondView.Build.ID)
	require.Equal(t, firstView.Build.Artifacts, secondView.Build.Artifacts)
	require.Equal(t, firstBytes, secondBytes)

	late, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(),
		"late.pdf", fixture.mustSourceHash(), int64(len(workerSourceBytes)), "application/pdf")
	require.NoError(t, err)
	request.ContentVersionID = late.CurrentVersionID
	_, lateWaiter, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	processed, err = worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	require.Equal(t, 1, provider.calls)
	lateView, err := fixture.catalog.ActiveRendition(t.Context(), late.CurrentVersionID, profile.Fingerprint)
	require.NoError(t, err)
	require.Equal(t, firstView.Build.ID, lateView.Build.ID)
	require.Equal(t, firstBytes, retainedWorkerMarkdown(t, fixture, lateView.Build))
	require.NotContains(t, string(firstBytes), second.CurrentVersionID)
	require.NotContains(t, string(firstBytes), late.CurrentVersionID)
	require.NotContains(t, string(firstBytes), firstWaiter.ID)
	require.NotContains(t, string(firstBytes), secondWaiter.ID)
	require.NotContains(t, string(firstBytes), lateWaiter.ID)
}

func TestRenditionWorkerReusesPublishedLegacyBodyOnlyBuild(t *testing.T) {
	fixture := newPublicationFixture(t)
	provider := newWorkerProvider(t)
	profile := workerProcessingProfile(t, provider.Descriptor())
	fixture.profile = profile
	request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
	grantWorkerConsent(t, fixture.catalog, request)
	job, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	seed, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(),
		"legacy-seed.pdf", fixture.mustSourceHash(), int64(len(workerSourceBytes)), "application/pdf")
	require.NoError(t, err)

	legacy := fixture.stageForSource(t,
		publicationIDs{"legacy-body", "legacy-attachment", "legacy-generation"},
		"Synthetic legacy lexical output", "Synthetic legacy body",
		seed.CurrentVersionID, fixture.mustSourceHash())
	legacy.Build.ID = job.ID
	legacy.Attachment.BuildID = job.ID
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)
	_, err = publisher.PublishRendition(t.Context(), legacy)
	require.NoError(t, err)
	legacyView, err := fixture.catalog.ActiveRendition(
		t.Context(), seed.CurrentVersionID, profile.Fingerprint)
	require.NoError(t, err)
	legacyBytes := retainedWorkerMarkdown(t, fixture, legacyView.Build)
	require.Equal(t, legacy.Rendition.Markdown, legacyBytes)
	_, _, err = document.ParseRenditionFrontMatterV1(legacyBytes)
	require.ErrorContains(t, err, "opening delimiter is missing")

	late, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(),
		"legacy-reuse.pdf", fixture.mustSourceHash(), int64(len(workerSourceBytes)), "application/pdf")
	require.NoError(t, err)
	request.ContentVersionID = late.CurrentVersionID
	reused, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, job.ID, reused.ID)
	require.Equal(t, store.RenditionPhaseBuildStaged, reused.Phase)
	worker := newEnvelopeWorker(t, fixture, provider)
	processed, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	require.Zero(t, provider.calls)

	lateView, err := fixture.catalog.ActiveRendition(
		t.Context(), late.CurrentVersionID, profile.Fingerprint)
	require.NoError(t, err)
	require.Equal(t, legacyView.Build.ID, lateView.Build.ID)
	require.Equal(t, legacyView.Build.Artifacts, lateView.Build.Artifacts)
	require.Equal(t, legacyBytes, retainedWorkerMarkdown(t, fixture, lateView.Build))
}

func TestRenditionWorkerWithoutMarkdownRetentionHasNoExportSource(t *testing.T) {
	fixture := newPublicationFixture(t)
	provider := newWorkerProvider(t)
	profile := mutateWorkerProfile(t, workerProcessingProfile(t, provider.Descriptor()),
		func(profile *document.ProcessingProfileV1) {
			profile.RetentionDisclosure.RetainSanitizedMarkdown = false
		})
	request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
	request.CapturedArtifactPolicy = jsontext.Value(
		`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"}],"version":1}`)
	request.Authorization.RetainedArtifactClasses = []string{"normalized_evidence"}
	grantWorkerConsent(t, fixture.catalog, request)
	_, view := runWorkerEnvelopeJob(t, fixture, provider, profile, request)
	require.Len(t, view.Build.Artifacts, 1)
	require.Equal(t, "normalized_evidence", view.Build.Artifacts[0].Role)
	sources, err := fixture.catalog.QMDExportSources(t.Context(), 10)
	require.NoError(t, err)
	require.Empty(t, sources)
}

func TestRenditionWorkerEnvelopeConsumesExistingArtifactBudget(t *testing.T) {
	fixture := newPublicationFixture(t)
	provider := newWorkerProvider(t)
	baseProfile := workerProcessingProfile(t, provider.Descriptor())
	evidence, body := workerBodyRendition(t, baseProfile)
	limit := int64(len(evidence) + len(body.Markdown))
	profile := mutateWorkerProfile(t, baseProfile, func(profile *document.ProcessingProfileV1) {
		profile.Rendition.MaxResponseBytes = limit
	})
	request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
	request.ExecutionIdentity.Authorization.MaxArtifactBytes = int(limit)
	request.ExecutionIdentity.Authorization.MaxTotalResultBytes = int(limit)
	grantWorkerConsent(t, fixture.catalog, request)
	job, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs,
		Runtime: boundedEnvelopeWorkerRuntime{provider: provider}, Gate: api.NewOperationGate(),
		Owner: "rendition-envelope-budget-test", LeaseDuration: time.Minute,
		IdleDelay: time.Millisecond, Clock: time.Now,
	})
	require.NoError(t, err)
	processed, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	require.Equal(t, 1, provider.calls)
	current, err := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	require.NotEqual(t, store.RenditionJobCompleted, current.State)
	_, err = fixture.catalog.ActiveRendition(t.Context(), fixture.versionID, profile.Fingerprint)
	require.Error(t, err)
	sources, err := fixture.catalog.QMDExportSources(t.Context(), 10)
	require.NoError(t, err)
	require.Empty(t, sources)

	enveloped, _, err := document.EnvelopeRenditionV1(body, document.RenditionEnvelopeV1{
		BuildID: job.ID, SourceSHA256: job.SourceSHA256, SourceFormat: "pdf",
		SourceMediaType: "application/pdf", RenditionRequestFingerprint: job.RenditionRequestFingerprint,
		EvidenceLexicalFingerprint: job.EvidenceLexicalFingerprint,
		NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
		UnitKind:                   document.EvidenceUnitGeneric,
	})
	require.NoError(t, err)
	require.Greater(t, int64(len(evidence)+len(enveloped.Markdown)), limit)
	_, _, err = fixture.blobs.OpenStreamContext(t.Context(), enveloped.MarkdownChecksum)
	require.Error(t, err, "validation must reject before staging the oversized envelope blob")
	_, _, err = fixture.blobs.OpenStreamContext(t.Context(), processingSHA256(evidence))
	require.Error(t, err, "validation must reject before staging normalized evidence too")
}

func runWorkerEnvelopeJob(
	t *testing.T, fixture publicationFixture, provider *workerProvider,
	profile store.ProcessingProfileRecord, request store.RenditionJobRequest,
) (store.RenditionJob, store.RenditionView) {
	t.Helper()
	job, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	worker := newEnvelopeWorker(t, fixture, provider)
	processed, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	view, err := fixture.catalog.ActiveRendition(t.Context(), request.ContentVersionID, profile.Fingerprint)
	require.NoError(t, err)
	return job, view
}

func newEnvelopeWorker(
	t *testing.T, fixture publicationFixture, provider *workerProvider,
) *RenditionWorker {
	t.Helper()
	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs,
		Runtime: workerRuntime{provider: provider}, Gate: api.NewOperationGate(),
		Owner: "rendition-envelope-test", LeaseDuration: time.Minute,
		IdleDelay: time.Millisecond, Clock: time.Now,
	})
	require.NoError(t, err)
	return worker
}

func workerBodyRendition(
	t *testing.T, profileRecord store.ProcessingProfileRecord,
) ([]byte, document.RenditionV1) {
	t.Helper()
	var profile document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(profileRecord.CanonicalProfile, &profile, json.RejectUnknownMembers(true)))
	evidencePolicy, renditionPolicy, err := document.RenditionExecutionPoliciesForProfileV1(profile)
	require.NoError(t, err)
	normalized, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1,
		Completeness:    document.EvidenceDegradedProvenance,
		Family:          "pdf", UnitKind: document.EvidenceUnitGeneric,
		Omissions: []document.SourceEvidenceOmissionV1{{
			Kind: document.EvidenceOmissionField, Field: "natural_provenance",
			Reason: "synthetic provider exposes generic provenance",
		}},
		Units: []document.SourceEvidenceUnitV1{{
			Order: 0, Text: "Synthetic worker output",
			Locator: document.SourceEvidenceLocatorV1{
				Kind: document.EvidenceLocatorGeneric, IndexOrigin: document.EvidenceIndexOriginNone,
			},
		}},
	}, evidencePolicy)
	require.NoError(t, err)
	evidence, _, err := document.MarshalNormalizedEvidenceV1(normalized)
	require.NoError(t, err)
	rendition, err := document.BuildRenditionV1(normalized, renditionPolicy)
	require.NoError(t, err)
	return evidence, rendition
}

func mutateWorkerProfile(
	t *testing.T, record store.ProcessingProfileRecord,
	mutate func(*document.ProcessingProfileV1),
) store.ProcessingProfileRecord {
	t.Helper()
	var profile document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(record.CanonicalProfile, &profile, json.RejectUnknownMembers(true)))
	mutate(&profile)
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	return store.ProcessingProfileRecord{
		Fingerprint: fingerprints.Profile, CanonicalProfile: jsontext.Value(canonical),
		RenditionRequestFingerprint:    fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:     fingerprints.EvidenceLexical,
		RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
		AttachmentPolicyFingerprint:    profile.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             profile.RetentionDisclosure.ConsentFingerprint,
		RenditionDisclosureFingerprint: profile.Rendition.DisclosureFingerprint,
		TrustBoundary:                  profile.RetentionDisclosure.TrustBoundary,
	}
}

func workerUnitRecords(rendition document.RenditionV1) []store.RenditionUnitRecord {
	records := make([]store.RenditionUnitRecord, len(rendition.Units))
	for index, unit := range rendition.Units {
		records[index] = store.RenditionUnitRecord{
			ID: unit.ID, EvidenceUnitID: unit.EvidenceUnitID, Order: unit.Order,
			Checksum: unit.Checksum, HeadingPath: append([]string{}, unit.HeadingPath...),
			Locator: unit.Locator,
		}
	}
	return records
}

func workerSegmentRecords(rendition document.RenditionV1) []store.RenditionLexicalSegmentRecord {
	records := make([]store.RenditionLexicalSegmentRecord, len(rendition.LexicalSegments))
	for index, segment := range rendition.LexicalSegments {
		records[index] = store.RenditionLexicalSegmentRecord{
			ID: segment.ID, UnitID: segment.UnitID, Order: segment.Order,
			CharStart: segment.CharStart, CharEnd: segment.CharEnd,
			Checksum: segment.Checksum, Text: segment.Text,
		}
	}
	return records
}

func retainedWorkerMarkdown(
	t *testing.T, fixture publicationFixture, build store.RenditionBuildRecord,
) []byte {
	t.Helper()
	artifact := workerArtifactRecord(t, build, "sanitized_markdown")
	stream, size, err := fixture.blobs.OpenStreamContext(t.Context(), artifact.BlobHash)
	require.NoError(t, err)
	require.Equal(t, artifact.Size, size)
	data, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Verify())
	require.NoError(t, stream.Close())
	require.Equal(t, artifact.BlobHash, processingSHA256(data))
	require.Equal(t, artifact.Checksum, processingSHA256(data))
	require.Equal(t, int64(len(data)), artifact.Size)
	return data
}

func workerArtifactRecord(
	t *testing.T, build store.RenditionBuildRecord, role string,
) store.RenditionArtifactRecord {
	t.Helper()
	for _, artifact := range build.Artifacts {
		if artifact.Role == role {
			return artifact
		}
	}
	require.FailNow(t, "retained worker artifact role is absent", role)
	return store.RenditionArtifactRecord{}
}

type boundedEnvelopeWorkerRuntime struct{ provider *workerProvider }

func (runtime boundedEnvelopeWorkerRuntime) Prepare(
	ctx context.Context, work store.RenditionJobWork, now time.Time,
) (RenditionExecution, error) {
	execution, err := (workerRuntime(runtime)).Prepare(ctx, work, now)
	if err != nil {
		return RenditionExecution{}, err
	}
	execution.Authorization.MaxArtifactBytes = work.ExecutionIdentity.Authorization.MaxArtifactBytes
	execution.Authorization.MaxTotalResultBytes = work.ExecutionIdentity.Authorization.MaxTotalResultBytes
	return execution, nil
}
