package document

import (
	"errors"

	"go.kenn.io/docbank/document/internal/uploadproof"
)

type VerifiedUploadProof = uploadproof.Proof
type VerifiedUploadFacts = uploadproof.Facts

// VerifiedUploadProofCarrier optionally carries filename-free authority issued
// after an upload's independent spool reinspection.
type VerifiedUploadProofCarrier interface {
	VerifiedUploadProof() (VerifiedUploadProof, bool)
}

func validateVerifiedUploadProof(proof VerifiedUploadProof, metadata AuthorizedUploadMetadata) error {
	facts := proof.Snapshot()
	if !proof.Valid() || facts.SourceBytes != metadata.ByteLength ||
		facts.SourceSHA256 != metadata.SHA256 ||
		facts.CapabilityRecordChecksum != metadata.CapabilityRecordChecksum ||
		facts.MediaFamily != metadata.MediaFamily || facts.MediaType != metadata.MediaType ||
		facts.InputKind != string(metadata.InputKind) {
		return errors.New("authorized upload proof does not match metadata")
	}
	return nil
}
