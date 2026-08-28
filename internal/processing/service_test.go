package processing

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

func TestProcessingServiceSourceFenceIsBoundedCanonicalAuthority(t *testing.T) {
	ids := []string{"00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000001"}
	normalized, err := normalizeFenceIDs(ids)
	require.NoError(t, err)
	require.Equal(t, []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"}, normalized)
	require.Equal(t, []string{"00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000001"}, ids)

	_, err = normalizeFenceIDs(nil)
	require.ErrorContains(t, err, "between 1")
	_, err = normalizeFenceIDs([]string{ids[0], ids[0]})
	require.ErrorContains(t, err, "duplicate")
	_, err = normalizeFenceIDs(make([]string, MaxSourceFenceIDs+1))
	require.ErrorContains(t, err, strconv.Itoa(MaxSourceFenceIDs))
}

func TestProcessingServicePlanFingerprintSealsCompleteRuntimeDisclosure(t *testing.T) {
	plan := Plan{VaultUID: "00000000-0000-4000-8000-000000000001",
		Selector:           Selector{NodeID: 1, ContentVersionID: "00000000-0000-4000-8000-000000000002", Profile: "private"},
		ProfileFingerprint: frontmatterHashForService("profile"),
		Flow: []FlowHop{{Capability: "rendition", ProviderID: "local", TrustBoundary: "local_process",
			InputClasses: []string{"original_file"}}}, RetainedClasses: []string{"sanitized_markdown"},
		ConsentRequired: true}
	encoded, err := json.Marshal(plan)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(encoded, &wire))
	flow, ok := wire["Flow"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, flow)
	hop, ok := flow[0].(map[string]any)
	require.True(t, ok)
	hop["RuntimeDisclosure"] = map[string]any{
		"ImmediateProcessor": "docbank plaintext adapter",
		"UltimateProcessor":  "docbank process",
		"Endpoint":           "in-process",
		"Deployment":         frontmatterHashForService("deployment"),
		"Model":              "plain-text",
		"ModelRevision":      "builtin-1",
		"VectorSpace":        "not-applicable",
		"MetadataClasses":    []any{"byte_length", "content_hash", "detected_media_type", "synthetic_filename"},
		"RetainedArtifactRoles": []any{
			"normalized_evidence", "sanitized_markdown",
		},
	}
	decode := func(value map[string]any) Plan {
		t.Helper()
		body, marshalErr := json.Marshal(value)
		require.NoError(t, marshalErr)
		var result Plan
		require.NoError(t, json.Unmarshal(body, &result, json.RejectUnknownMembers(true)))
		return result
	}
	baseline, err := planFingerprint(decode(wire))
	require.NoError(t, err)

	for _, testCase := range []struct {
		name  string
		field string
		value any
	}{
		{name: "endpoint", field: "Endpoint", value: "https://processor.example/v2"},
		{name: "deployment", field: "Deployment", value: frontmatterHashForService("new-deployment")},
		{name: "model revision", field: "ModelRevision", value: "builtin-2"},
		{name: "metadata class", field: "MetadataClasses", value: []any{"byte_length", "content_hash"}},
		{name: "retention", field: "RetainedArtifactRoles", value: []any{"normalized_evidence"}},
		{name: "vector space", field: "VectorSpace", value: frontmatterHashForService("vector-space")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			body, marshalErr := json.Marshal(wire)
			require.NoError(t, marshalErr)
			var changedWire map[string]any
			require.NoError(t, json.Unmarshal(body, &changedWire))
			changedFlow, flowOK := changedWire["Flow"].([]any)
			require.True(t, flowOK)
			require.NotEmpty(t, changedFlow)
			changedHop, hopOK := changedFlow[0].(map[string]any)
			require.True(t, hopOK)
			changedDisclosure, disclosureOK := changedHop["RuntimeDisclosure"].(map[string]any)
			require.True(t, disclosureOK)
			changedDisclosure[testCase.field] = testCase.value
			changed, fingerprintErr := planFingerprint(decode(changedWire))
			require.NoError(t, fingerprintErr)
			require.NotEqual(t, baseline, changed)
		})
	}
}

func TestProcessingServiceCoverageReportsRebuildWhilePreviousGenerationServes(t *testing.T) {
	fixture := newPublicationFixture(t)
	provider := newWorkerProvider(t)
	profile := workerProcessingProfile(t, provider.Descriptor())
	fixture.profile = profile
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)
	_, err = publisher.PublishRendition(t.Context(), fixture.stage(t,
		publicationIDs{"coverage-old-build", "coverage-old-attachment", "coverage-old-generation"},
		"old searchable evidence", "old markdown",
	))
	require.NoError(t, err)

	var portable document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(profile.CanonicalProfile, &portable, json.RejectUnknownMembers(true)))
	gate := newWorkerTestGate()
	service, err := NewService(ServiceConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: gate,
		SpoolDirectory: filepath.Join(t.TempDir(), "spool"),
		Profiles: map[string]ProfileConfig{"private": {
			Profile: portable, RenditionProvider: provider,
		}},
	})
	require.NoError(t, err)

	request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
	grantWorkerConsent(t, fixture.catalog, request)
	job, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)

	coverage, err := service.Coverage(t.Context(), "private", SourceFence{
		VaultUID: fixture.catalog.VaultID(), ContentVersionIDs: []string{fixture.versionID},
	})
	require.NoError(t, err)
	assert.Equal(t, "rebuilding", coverage.Renditions.State)
	assert.Equal(t, 1, coverage.Renditions.Rebuilding)
	assert.Equal(t, 1, coverage.Renditions.PreviousServing)
	assert.Zero(t, coverage.Renditions.Complete)

	now := time.Now().UTC()
	claim, err := fixture.catalog.ClaimRenditionJob(
		t.Context(), job.ID, "coverage-service-test", now, 5*time.Minute,
	)
	require.NoError(t, err)
	require.NoError(t, fixture.catalog.MarkRenditionJobFailed(
		t.Context(), claim, store.RenditionFailureTerminal, now.Add(time.Second),
	))
	coverage, err = service.Coverage(t.Context(), "private", SourceFence{
		VaultUID: fixture.catalog.VaultID(), ContentVersionIDs: []string{fixture.versionID},
	})
	require.NoError(t, err)
	assert.Equal(t, "complete", coverage.Renditions.State)
	assert.Equal(t, 1, coverage.Renditions.Complete)
	assert.Zero(t, coverage.Renditions.Rebuilding)
	assert.Zero(t, coverage.Renditions.PreviousServing)
}

func TestInspectionPolicyCanonicalizesDeclaredMediaTypeForDurableReplay(t *testing.T) {
	profile := configuredProfile{
		portable: document.ProcessingProfileV1{Rendition: &document.RenditionBindingV1{
			MaxDocumentBytes: 1024, DisclosureFingerprint: strings.Repeat("1", 64),
		}},
		record: store.ProcessingProfileRecord{Fingerprint: strings.Repeat("2", 64)},
		provider: inertRenditionProvider{descriptor: document.RenditionDescriptor{
			Fingerprint: strings.Repeat("3", 64),
		}},
	}
	policy := inspectionPolicy("document.txt", store.ContentVersion{
		ID: "00000000-0000-4000-8000-000000000001", BlobHash: strings.Repeat("4", 64),
		Size: 12, MimeType: "text/plain; charset=utf-8",
	}, profile)
	require.Equal(t, "text/plain", policy.DeclaredMediaType)
}

type inertRenditionProvider struct{ descriptor document.RenditionDescriptor }

func (provider inertRenditionProvider) Descriptor() document.RenditionDescriptor {
	return provider.descriptor
}
func (inertRenditionProvider) Render(context.Context, document.AuthorizedUpload,
	document.RenditionAuthorization,
) (document.RenditionResult, error) {
	return document.RenditionResult{}, nil
}

func BenchmarkProcessingServiceSourceFence4096(b *testing.B) {
	ids := make([]string, MaxSourceFenceIDs)
	for index := range ids {
		ids[index] = fmt.Sprintf("00000000-0000-4000-8000-%012d", index)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := normalizeFenceIDs(ids); err != nil {
			b.Fatal(err)
		}
	}
}

func frontmatterHashForService(value string) string {
	return fmt.Sprintf("%064s", value)
}
