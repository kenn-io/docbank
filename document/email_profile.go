package document

import (
	"crypto/sha256"
	"encoding/hex"
)

const emailLocalCredentialBinding = "credential:" + "email-local"

func EmailBodyProfileV1(recipe EmailRecipeV1) (ProcessingProfileV1, error) {
	fingerprint, err := EmailBodyRecipeFingerprint(recipe)
	if err != nil {
		return ProcessingProfileV1{}, err
	}
	return emailBodyProfile(fingerprint), nil
}

func emailBodyProfile(recipe string) ProcessingProfileV1 {
	fp := func(field string) string {
		sum := sha256.Sum256([]byte("docbank-email-body-profile/v1\x00" + recipe + "\x00" + field))
		return hex.EncodeToString(sum[:])
	}
	return ProcessingProfileV1{
		ContractVersion: ProcessingProfileContractV1,
		Embeddings:      []EmbeddingBindingV1{},
		Rendition: &RenditionBindingV1{
			AdapterContract: "docbank-email-body/v1", AuthorizationFingerprint: fp("local-authority"),
			CredentialBinding: emailLocalCredentialBinding, DeploymentFingerprint: fp("builtin-v1"),
			Descriptor:            ProviderDescriptorV1{ID: "docbank-email-body", Fingerprint: fp("descriptor")},
			DisclosureFingerprint: fp("local-only"), MaxDocumentBytes: 128 << 20,
			MaxResponseBytes: 256 << 20, MaxUnits: 17, Name: "email-body-v1",
			RequestedArtifacts: []EvidenceArtifactRole{EvidenceArtifactStructured}, TrustBoundary: "local-vault",
			UploadOptionsFingerprint: fp("exact-email-generation"),
		},
		EvidenceLexical: EvidenceLexicalPolicyV1{
			CompletenessFingerprint: fp("selected-body-derived-generic"), LexicalSegmenterFingerprint: fp("rune4096-v1"),
			MaxDocumentChars: 64 << 20, MaxSegmentRunes: 4096, MaxUnitRunes: 4_000_000,
			NormalizedEvidenceContract: NormalizedEvidenceContractV1, NormalizerFingerprint: fp("generic-text-blocks-v1"),
			RenditionContract: RenditionContractV1, SanitizerFingerprint: fp("rendition-v1"),
			SourceEvidenceContract: SourceEvidenceContractV1,
		},
		Retrieval: RetrievalPolicyV1{LexicalLimit: 100, VectorLimit: 100},
		RetentionDisclosure: RetentionDisclosurePolicyV1{
			AttachmentPolicyFingerprint: fp("chosen-outer-body-only"), ConsentFingerprint: fp("builtin-local"),
			RetainSanitizedMarkdown: true, RetainTypedArtifacts: true, TrustBoundary: "local-vault",
		},
	}
}
