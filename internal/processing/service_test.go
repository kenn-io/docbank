package processing

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

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

func TestProcessingServicePlanFingerprintSealsDisclosure(t *testing.T) {
	plan := Plan{VaultUID: "00000000-0000-4000-8000-000000000001",
		Selector:           Selector{NodeID: 1, ContentVersionID: "00000000-0000-4000-8000-000000000002", Profile: "private"},
		ProfileFingerprint: frontmatterHashForService("profile"),
		Flow: []FlowHop{{Capability: "rendition", ProviderID: "local", TrustBoundary: "local_process",
			InputClasses: []string{"original_file"}}}, RetainedClasses: []string{"sanitized_markdown"},
		ConsentRequired: true}
	first, err := planFingerprint(plan)
	require.NoError(t, err)
	second, err := planFingerprint(plan)
	require.NoError(t, err)
	require.Equal(t, first, second)
	plan.Flow[0].TrustBoundary = "hosted_provider"
	changed, err := planFingerprint(plan)
	require.NoError(t, err)
	require.NotEqual(t, first, changed)
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
