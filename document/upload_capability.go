package document

import "go.kenn.io/docbank/document/internal/uploadcapability"

// UploadCapability is immutable local inspection evidence carried through the
// execution boundary. Its facts omit filenames, paths, and document bytes.
// Only local media inspection can issue a valid proof; its zero value carries
// no authority. It cannot be serialized or restored from JSON.
type UploadCapability = uploadcapability.Proof

// UploadCapabilityFacts is the value copy returned by UploadCapability.Facts.
type UploadCapabilityFacts = uploadcapability.Facts

func uploadCapability(upload AuthorizedUpload) UploadCapability {
	if source, ok := upload.(interface{ CapabilityProof() UploadCapability }); ok {
		return source.CapabilityProof()
	}
	return UploadCapability{}
}

// UploadPDFPasswords returns the passwords carried by local inspection evidence.
func UploadPDFPasswords(upload AuthorizedUpload) []string {
	return uploadCapability(upload).PDFPasswords()
}
