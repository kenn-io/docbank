package api

import (
	"encoding/hex"
	"errors"
	"slices"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/processing"
)

// ValidateNoteLocalTextPlan rejects any plan outside the built-in, in-process
// plaintext policy before the caller presents or grants ordinary consent.
func ValidateNoteLocalTextPlan(value NoteLocalTextPlan) error {
	invalid := errors.New("Daemon requested unsupported text indexing settings")
	name, fixed, err := processing.NewNotePlaintextProfile()
	if err != nil {
		return err
	}
	_, fingerprints, err := document.CanonicalProfile(fixed.Profile)
	if err != nil {
		return err
	}
	p, request := value.Plan, value.ConsentRequest
	if len(p.Fingerprint) != 64 {
		return invalid
	}
	if _, err := hex.DecodeString(p.Fingerprint); err != nil {
		return invalid
	}
	inputs := []string{"original_file"}
	retained := []string{"normalized_evidence", "sanitized_markdown"}
	if p.Selector.Profile != name || p.Selector.NodeID != 0 || p.Selector.ContentVersionID != "" ||
		p.ProfileFingerprint != fingerprints.Profile || len(p.Flow) != 1 ||
		!slices.Equal(p.DisclosedClasses, inputs) || !slices.Equal(p.RetainedClasses, retained) ||
		p.Estimate.SourceBytes != 0 || p.Estimate.ProviderCalls != 1 || p.Estimate.VectorSpaces != 0 || p.Estimate.Scope != "" ||
		p.RenditionIneligibleReason != "" ||
		request.Principal != "daemon:operator" || request.Scope != "document-processing" ||
		request.ProfileFingerprint != fingerprints.Profile || request.DisclosureFingerprint != fixed.Profile.Rendition.DisclosureFingerprint ||
		!slices.Equal(request.InputClasses, inputs) || !slices.Equal(request.RetainedArtifactClasses, retained) || request.ExpiresAt != nil {
		return invalid
	}
	hop := p.Flow[0]
	disclosure := hop.RuntimeDisclosure
	if hop.Capability != "rendition" || hop.ProviderID != fixed.Profile.Rendition.Descriptor.ID ||
		hop.TrustBoundary != string(document.RenditionTrustLocalProcess) || !slices.Equal(hop.InputClasses, inputs) ||
		hop.DiscloseFilename || hop.Filename != "" || disclosure.Endpoint != "in-process" ||
		disclosure.ImmediateProcessor != hop.ProviderID || disclosure.UltimateProcessor != hop.ProviderID ||
		disclosure.Deployment != fixed.Profile.Rendition.DeploymentFingerprint || disclosure.Model != "" ||
		disclosure.ModelRevision != "" || disclosure.VectorSpace != "" ||
		!slices.Equal(disclosure.MetadataClasses, []string{"byte_length", "content_hash", "detected_media_type", "synthetic_filename"}) ||
		!slices.Equal(disclosure.RetainedArtifactRoles, retained) {
		return invalid
	}
	switch p.ConsentState {
	case "active":
		if p.ConsentRequired {
			return invalid
		}
	case "required", "expired", "revoked":
		if !p.ConsentRequired {
			return invalid
		}
	default:
		return invalid
	}
	return nil
}
