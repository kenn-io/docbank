package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/store"
)

// NotePlaintextProfileName is reserved for explicitly consented local note text.
const NotePlaintextProfileName = "brain-local-text"

// NewNotePlaintextProfile builds the fixed in-process text profile. Construction
// neither grants consent nor enrolls an embedding or network provider.
func NewNotePlaintextProfile() (string, ProfileConfig, error) {
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: plaintext.MaxDocumentBytes})
	if err != nil {
		return "", ProfileConfig{}, err
	}
	descriptor := provider.Descriptor()
	h := func(value string) string {
		return fmt.Sprintf("%x", sha256.Sum256([]byte("docbank/brain-local-text/v1/"+value)))
	}
	profile := document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1,
		Rendition: &document.RenditionBindingV1{ //nolint:gosec // Stable anonymous identity contains no credential.
			Name: NotePlaintextProfileName, AdapterContract: "docbank-plaintext-rendition/v1",
			AuthorizationFingerprint: h("render-authorization"), CredentialBinding: "credential:brain-local-text",
			DeploymentFingerprint: h("render-deployment"),
			Descriptor:            document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint},
			DisclosureFingerprint: h("render-disclosure"), MaxDocumentBytes: plaintext.MaxDocumentBytes,
			MaxResponseBytes: MaxRenditionBytes, MaxUnits: 1,
			RequestedArtifacts: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
			TrustBoundary:      string(descriptor.TrustBoundary), UploadOptionsFingerprint: h("render-upload"),
		},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			CompletenessFingerprint: h("completeness"), LexicalSegmenterFingerprint: h("segmenter"),
			MaxDocumentChars: 16 << 20, MaxSegmentRunes: 4096, MaxUnitRunes: 16 << 20,
			NormalizedEvidenceContract: document.NormalizedEvidenceContractV1, NormalizerFingerprint: h("normalizer"),
			RenditionContract: document.RenditionContractV1, SanitizerFingerprint: h("sanitizer"),
			SourceEvidenceContract: document.SourceEvidenceContractV1,
		},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{
			AttachmentPolicyFingerprint: h("attachments"), ConsentFingerprint: h("consent"),
			RetainSanitizedMarkdown: true, TrustBoundary: "vault",
		},
		// Both candidate bounds are required by the canonical profile contract.
		// No embedding binding or vector runtime is configured.
		Retrieval: document.RetrievalPolicyV1{LexicalLimit: 100, VectorLimit: 100},
	}
	if _, _, err := document.CanonicalProfile(profile); err != nil {
		return "", ProfileConfig{}, err
	}
	return NotePlaintextProfileName, ProfileConfig{Profile: profile, RenditionProvider: provider}, nil
}

// NoteLocalTextPlan previews the fixed profile even in an empty vault. Consent
// remains ordinary scoped processing authority, read at the time of the plan.
func (service *Service) NoteLocalTextPlan(ctx context.Context) (Plan, document.ProcessingConsentRequest, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, document.ProcessingConsentRequest{}, err
	}
	profile, ok := service.profiles[NotePlaintextProfileName]
	if !ok {
		return Plan{}, document.ProcessingConsentRequest{}, ErrProfileNotConfigured
	}
	if err := validateNotePlaintextProfile(profile); err != nil {
		return Plan{}, document.ProcessingConsentRequest{}, err
	}
	plan, err := service.planForSource(ctx, Selector{Profile: NotePlaintextProfileName}, store.Node{}, store.ContentVersion{}, profile)
	if err != nil {
		return Plan{}, document.ProcessingConsentRequest{}, err
	}
	authorization := service.renditionConsentRequest(profile)
	_, err = service.catalog.AuthorizeProviderOperation(ctx, authorization)
	plan.ConsentState, err = processingConsentState(err)
	if err != nil {
		return Plan{}, document.ProcessingConsentRequest{}, err
	}
	plan.ConsentRequired = plan.ConsentState != "active"
	request := document.ProcessingConsentRequest{
		Principal: authorization.Principal, Scope: authorization.Scope,
		ProfileFingerprint: authorization.ProfileFingerprint, DisclosureFingerprint: authorization.DisclosureFingerprint,
		InputClasses: authorization.InputClasses, RetainedArtifactClasses: authorization.RetainedArtifactClasses,
	}
	return plan, request, nil
}

// Validate the actual configured execution and disclosure, not just its name.
// A caller must never grant ordinary consent to a changed reserved profile.
func validateNotePlaintextProfile(profile configuredProfile) error {
	_, fixed, err := NewNotePlaintextProfile()
	if err != nil {
		return err
	}
	want, fingerprints, err := document.CanonicalProfile(fixed.Profile)
	if err != nil {
		return err
	}
	got, _, err := document.CanonicalProfile(profile.portable)
	if err != nil || !bytes.Equal(want, got) || profile.record.Fingerprint != fingerprints.Profile ||
		profile.record.RenditionDisclosureFingerprint != fixed.Profile.Rendition.DisclosureFingerprint {
		return fmt.Errorf("%w: fixed note plaintext policy differs", ErrPlanChanged)
	}
	provider, ok := profile.provider.(*plaintext.Provider)
	if !ok || provider == nil || provider.Descriptor().Fingerprint != fixed.RenditionProvider.Descriptor().Fingerprint {
		return fmt.Errorf("%w: fixed note plaintext provider differs", ErrPlanChanged)
	}
	disclosure, err := renditionRuntimeDisclosure(fixed.RenditionDisclosure, *fixed.Profile.Rendition,
		fixed.RenditionProvider.Descriptor(), fixed.Profile.RetentionDisclosure)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(profile.renderDisclosure, disclosure) || profile.rerankingProvider != nil ||
		!reflect.ValueOf(profile.rerankingProfile).IsZero() || !reflect.ValueOf(profile.rerankingDisclosure).IsZero() ||
		profile.rerankingDeadline != 0 || profile.rerankingFailurePolicy != "" || len(profile.embedders) != 0 ||
		len(profile.embeddingRuntimes) != 0 || len(profile.embedDisclosures) != 0 || len(profile.tokenizers) != 0 ||
		len(profile.vectorSpaces) != 0 || len(profile.embeddingSimilarityFloors) != 0 {
		return fmt.Errorf("%w: fixed note plaintext runtime differs", ErrPlanChanged)
	}
	return nil
}
