package document

import (
	"errors"
	"go.kenn.io/docbank/internal/canonical"
)

const EmailPDFContract = "email-pdf-v1"

type EmailPDFRequest struct {
	VersionID    string `json:"version_id"`
	GenerationID string `json:"generation_id"`
	Paper        string `json:"paper"`
	Consent      bool   `json:"consent"`
}

type EmailPDFReceiptV1 struct {
	Source             EmailDocumentIdentity `json:"source"`
	AttachmentID       string                `json:"attachment_id"`
	BuildID            string                `json:"build_id"`
	ProfileFingerprint string                `json:"profile_fingerprint"`
	Binding            EmailPDFBindingV1     `json:"binding"`
	Output             EmailPDFOutputV1      `json:"output"`
}

type EmailPDFJob struct {
	JobID              string             `json:"job_id"`
	VersionID          string             `json:"version_id"`
	ProfileFingerprint string             `json:"profile_fingerprint"`
	State              string             `json:"state"`
	Receipt            *EmailPDFReceiptV1 `json:"receipt,omitempty"`
}

type EmailPDFRecipeV1 struct {
	Contract        string `json:"contract"`
	RendererVersion string `json:"renderer_version"`
	RendererSHA256  string `json:"renderer_sha256"`
	WorkerSHA256    string `json:"worker_sha256"`
	FontsSHA256     string `json:"fonts_sha256"`
	Paper           string `json:"paper"`
}

// EmailPDFBindingV1 pins the decoded representation as well as the complete
// v1 policy: sanitized expanded outer body, verified raster CIDs, inventory
// only, Noto fonts, en/C.UTF-8, UTC, portrait/12mm, two 512MiB workers,
// 60 seconds, 1000 pages, 256MiB, and fixed PDF metadata where supported.
type EmailPDFBindingV1 struct {
	GenerationID       string           `json:"generation_id"`
	GenerationChecksum string           `json:"generation_checksum"`
	Recipe             EmailPDFRecipeV1 `json:"recipe"`
}

type EmailPDFOutputV1 struct {
	BodyPath   string `json:"body_path"`
	BodySHA256 string `json:"body_sha256"`
	BodySize   int64  `json:"body_size"`
	PDFSHA256  string `json:"pdf_sha256"`
	PDFSize    int64  `json:"pdf_size"`
	Pages      int64  `json:"pages"`
}

func ValidateEmailPDFBinding(b EmailPDFBindingV1) error {
	if !canonical.IsSHA256Hex(b.Recipe.WorkerSHA256) {
		return errors.New("email PDF worker fingerprint is invalid")
	}
	if !canonical.IsSHA256Hex(b.GenerationID) || !canonical.IsSHA256Hex(b.GenerationChecksum) || b.Recipe.Contract != EmailPDFContract || !canonical.IsSHA256Hex(b.Recipe.RendererSHA256) || !canonical.IsSHA256Hex(b.Recipe.FontsSHA256) || b.Recipe.RendererVersion == "" || len(b.Recipe.RendererVersion) > 128 || (b.Recipe.Paper != "A4" && b.Recipe.Paper != "Letter") {
		return errors.New("invalid exact email PDF recipe")
	}
	return nil
}

func EmailPDFDescriptor(recipe EmailPDFRecipeV1) (RenditionDescriptor, error) {
	fp, err := componentFingerprint("email_pdf_runtime", recipe)
	if err != nil {
		return RenditionDescriptor{}, err
	}
	return NewRenditionDescriptor(RenditionDescriptor{ID: EmailPDFContract, ContractVersion: RenditionProviderContractVersion, PolicyFingerprint: fp, TrustBoundary: RenditionTrustLocalProcess, SupportedFormats: []RenditionFormatCapability{{MediaFamily: "mail", MediaType: "message/rfc822", InputKind: RenditionInputOriginalFile}}, ReturnsStructured: true, ArtifactRoles: []EvidenceArtifactRole{EvidenceArtifactPDF}})
}

func EmailPDFProfile(b EmailPDFBindingV1) (ProcessingProfileV1, error) {
	if err := ValidateEmailPDFBinding(b); err != nil {
		return ProcessingProfileV1{}, err
	}
	fp, err := componentFingerprint("email_pdf_binding", b)
	if err != nil {
		return ProcessingProfileV1{}, err
	}
	d, err := EmailPDFDescriptor(b.Recipe)
	if err != nil {
		return ProcessingProfileV1{}, err
	}
	p := emailBodyProfile(fp)
	p.Rendition.AdapterContract = EmailPDFContract
	p.Rendition.Name = EmailPDFContract
	p.Rendition.Descriptor = ProviderDescriptorV1{ID: d.ID, Fingerprint: d.Fingerprint}
	p.Rendition.TrustBoundary = string(RenditionTrustLocalProcess)
	p.RetentionDisclosure.TrustBoundary = string(RenditionTrustLocalProcess)
	p.Rendition.RequestedArtifacts = []EvidenceArtifactRole{EvidenceArtifactPDF}
	p.Rendition.EmailPDF = &b
	return p, nil
}
