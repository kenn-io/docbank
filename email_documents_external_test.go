package docbank_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/plaintext"
)

type emailRecordingProvider struct {
	document.RenditionProvider

	profile document.ProcessingProfileV1
	seen    chan document.RenditionExecutionIdentityV1
}

func (p *emailRecordingProvider) Render(ctx context.Context, upload document.AuthorizedUpload,
	authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	evidence, rendition, err := document.RenditionExecutionPoliciesForProfileV1(p.profile)
	if err != nil {
		return document.RenditionResult{}, err
	}
	identity, err := document.NewRenditionExecutionIdentityV1(upload.Metadata(), authorization, evidence, rendition)
	if err != nil {
		return document.RenditionResult{}, err
	}
	p.seen <- identity
	return p.RenditionProvider.Render(ctx, upload, authorization)
}

func TestEmbeddedEmailProcessingRunsConfiguredProvider(t *testing.T) {
	plain, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	profile := embeddedProcessingProfile(t, plain.Descriptor())
	provider := &emailRecordingProvider{RenditionProvider: plain, profile: profile,
		seen: make(chan document.RenditionExecutionIdentityV1, 2)}
	options := docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
		"email": {Profile: profile, RenditionProvider: provider},
	}}
	// Obtain the public execution identity from normal processing of the same
	// synthetic bytes. This uses the production capability and upload preparation.
	seed, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(), Processing: options})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, seed.Close()) })
	const csv = "item,count\nemailquasar,42\n"
	source, err := seed.Put(t.Context(), "/source.csv", strings.NewReader(csv),
		docbank.PutOptions{MediaType: "text/csv"})
	require.NoError(t, err)
	planRequest := docbank.ProcessingPlanRequest{Selector: docbank.ProcessingSelector{
		NodeID: source.Node.ID, ContentVersionID: source.Version.ID, Profile: "email",
	}}
	plan, err := seed.PlanProcessing(t.Context(), planRequest)
	require.NoError(t, err)
	_, err = seed.StartProcessing(t.Context(), docbank.StartProcessingRequest{
		PlanRequest: planRequest, PlanFingerprint: plan.Fingerprint, Consent: true,
	})
	require.NoError(t, err)
	identity := <-provider.seen

	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(), Processing: options})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	raw := "Content-Type: text/csv\r\nContent-Disposition: attachment; filename=table.csv\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString([]byte(csv))
	mail, err := vault.Put(t.Context(), "/source.eml", strings.NewReader(raw),
		docbank.PutOptions{MediaType: "message/rfc822"})
	require.NoError(t, err)
	view, err := vault.EnsureEmailMetadata(t.Context(), mail.Version.ID)
	require.NoError(t, err)
	root, err := vault.Stat(t.Context(), "/")
	require.NoError(t, err)
	receipt, err := vault.PublishEmailDocuments(t.Context(), document.EmailDocumentPublicationRequest{
		OperationID: "embedded-email", GenerationID: view.GenerationID, AttachmentID: view.AttachmentID,
		Parent: document.EmailDocumentIdentity{NodeID: mail.Node.ID, VersionID: mail.Version.ID,
			SHA256: mail.Version.BlobHash, Size: mail.Version.Size},
		DestinationID: root.ID, DestinationRevision: root.Revision,
	})
	require.NoError(t, err)
	require.Len(t, receipt.Relations, 1)
	_, fp, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	_, err = vault.GrantProcessingConsent(t.Context(), document.ProcessingConsentRequest{
		Principal: "email-test", Scope: "attachments", ProfileFingerprint: fp.Profile,
		DisclosureFingerprint: profile.Rendition.DisclosureFingerprint,
		InputClasses:          []string{"original_file"}, RetainedArtifactClasses: []string{"normalized_evidence", "sanitized_markdown"},
	})
	require.NoError(t, err)
	job, err := vault.RequestEmailDocumentProcessing(t.Context(), document.EmailDocumentProcessingRequest{
		OperationID: receipt.OperationID, RequestDigest: receipt.RequestDigest, Order: 1,
		Profile: profile, ExecutionIdentity: identity,
		CapturedArtifactPolicy: []byte(`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`),
		Principal:              "email-test", Scope: "attachments", InputClasses: []string{"original_file"},
		RetainedArtifactClasses: []string{"normalized_evidence", "sanitized_markdown"},
	})
	require.NoError(t, err)
	require.Equal(t, "completed", job.State)
	completed := <-provider.seen
	require.Equal(t, receipt.Relations[0].Child.SHA256, completed.Upload.SHA256)
	page, err := vault.EmailDocumentRelations(t.Context(), document.EmailDocumentRelationQuery{
		ChildVersionID: receipt.Relations[0].Child.VersionID,
	})
	require.NoError(t, err)
	require.Equal(t, "indexed", page.Items[0].State)
}
