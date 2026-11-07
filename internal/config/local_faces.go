package config

import (
	"go.kenn.io/docbank/document"
	"go.kenn.io/kit/embedclient"
)

const LocalFacesProfile = "local-faces"

// LocalFaces supplies a face-only recipe without discovering or installing a runtime.
func LocalFaces() (document.ProcessingProfileV1, document.EmbeddingDescriptor, error) {
	cfg, err := Default().WithLocalText()
	if err != nil {
		return document.ProcessingProfileV1{}, document.EmbeddingDescriptor{}, err
	}
	base, err := cfg.ProcessingProfile(LocalTextProfile)
	if err != nil {
		return document.ProcessingProfileV1{}, document.EmbeddingDescriptor{}, err
	}
	compatibility := "face/" + localTextFingerprint(embedclient.FaceModel+embedclient.FaceRevision+embedclient.FaceRecipe)
	input, err := document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileCustom, CompatibilityID: compatibility, Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "{{content}}"}, Query: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "{{content}}"}})
	if err != nil {
		return document.ProcessingProfileV1{}, document.EmbeddingDescriptor{}, err
	}
	d, err := document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{ID: "local-face", ContractVersion: document.EmbeddingProviderContractVersion, PolicyFingerprint: localTextFingerprint("faces-local"), TrustBoundary: document.EmbeddingTrustLocalProcess, Model: embedclient.FaceModel, ModelRevision: embedclient.FaceRevision, Dimension: 512, Metric: document.VectorMetricL2, Normalization: "unit_length", ScalarEncoding: "float32", DocumentFormatter: embedclient.FaceRecipe, QueryFormatter: "none", InputKinds: []document.EmbeddingInputKind{document.EmbeddingInputFace}, CompatibilityID: compatibility, ModelInput: input, SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText}})
	if err != nil {
		return document.ProcessingProfileV1{}, d, err
	}
	p := base.Document
	p.Rendition = nil
	p.RetentionDisclosure.RetainSanitizedMarkdown = false
	p.Embeddings = []document.EmbeddingBindingV1{{Name: LocalFacesProfile, Activation: document.EmbeddingOptional, AuthorizationFingerprint: localTextFingerprint("faces-authorization"), DisclosureFingerprint: localTextFingerprint("faces-disclosure"), CredentialBinding: "credential:local-faces", Descriptor: document.ProviderDescriptorV1{ID: d.ID, Fingerprint: d.Fingerprint}, CompatibilityID: d.CompatibilityID, Dimensions: 512, DocumentFormatter: d.DocumentFormatter, QueryFormatter: d.QueryFormatter, Model: d.Model, ModelInput: input, Metric: d.Metric, Normalization: d.Normalization, ScalarEncoding: d.ScalarEncoding, InputKind: document.EmbeddingInputFace, MaxBatchItems: 128, MaxInputBytes: 128 * 8 << 20, MaxResponseBytes: 1 << 20, TrustBoundary: string(d.TrustBoundary)}}
	p.RetentionDisclosure.ConsentFingerprint = localTextFingerprint("faces-consent")
	_, _, err = document.CanonicalProfile(p)
	return p, d, err
}
