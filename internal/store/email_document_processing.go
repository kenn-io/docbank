package store

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
)

// RequestEmailDocumentProcessing enqueues a request already prepared and
// verified by the processing service. It rechecks receipt and consent authority.
func (s *Store) RequestEmailDocumentProcessing(
	ctx context.Context, r document.EmailDocumentProcessingRequest,
) (document.EmailDocumentProcessingReceipt, error) {
	child, err := s.EmailDocumentProcessingChild(ctx, r)
	if err != nil {
		return document.EmailDocumentProcessingReceipt{}, err
	}
	canonical, fp, err := document.CanonicalProfile(r.Profile)
	if err != nil {
		return document.EmailDocumentProcessingReceipt{}, fmt.Errorf("%w: %w", ErrInvalidEmailDocumentRequest, err)
	}
	if r.Profile.Rendition == nil {
		return document.EmailDocumentProcessingReceipt{}, ErrInvalidEmailDocumentRequest
	}
	profile := ProcessingProfileRecord{
		Fingerprint: fp.Profile, CanonicalProfile: canonical,
		RenditionRequestFingerprint:    fp.RenditionRequest,
		EvidenceLexicalFingerprint:     fp.EvidenceLexical,
		RetentionDisclosureFingerprint: fp.RetentionDisclosure,
		AttachmentPolicyFingerprint:    r.Profile.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             r.Profile.RetentionDisclosure.ConsentFingerprint,
		RenditionDisclosureFingerprint: r.Profile.Rendition.DisclosureFingerprint,
		TrustBoundary:                  r.Profile.RetentionDisclosure.TrustBoundary,
	}
	job, waiter, err := s.EnqueueRenditionJob(ctx, RenditionJobRequest{
		ContentVersionID: child.VersionID, Profile: profile,
		ExecutionIdentity: r.ExecutionIdentity, CapturedArtifactPolicy: r.CapturedArtifactPolicy,
		Authorization: ProviderOperationAuthorizationRequest{
			Principal: r.Principal, Scope: r.Scope,
			ProfileFingerprint: fp.Profile, DisclosureFingerprint: profile.RenditionDisclosureFingerprint,
			InputClasses: r.InputClasses, RetainedArtifactClasses: r.RetainedArtifactClasses,
		},
	})
	if errors.Is(err, ErrInvalidRenditionJobRequest) {
		return document.EmailDocumentProcessingReceipt{}, fmt.Errorf("%w: %w", ErrInvalidEmailDocumentRequest, err)
	}
	if errors.Is(err, ErrRenditionJobStaleAuthority) {
		return document.EmailDocumentProcessingReceipt{}, fmt.Errorf("%w: operation %s: %w", ErrEmailDocumentConflict, r.OperationID, err)
	}
	if err != nil {
		return document.EmailDocumentProcessingReceipt{}, err
	}
	return document.EmailDocumentProcessingReceipt{JobID: job.ID, WaiterID: waiter.ID, Child: child, State: string(job.State)}, nil
}

// EmailDocumentProcessingChild resolves an exact retained occurrence before
// the processing service prepares provider-visible metadata from its bytes.
func (s *Store) EmailDocumentProcessingChild(
	ctx context.Context, r document.EmailDocumentProcessingRequest,
) (document.EmailDocumentIdentity, error) {
	if r.Order < 1 || r.Order > document.EmailDocumentMaxParts ||
		len(r.InputClasses) > 32 || len(r.RetainedArtifactClasses) > 32 ||
		len(r.CapturedArtifactPolicy) > 1<<20 || len(r.Principal) > 1024 || len(r.Scope) > 1024 {
		return document.EmailDocumentIdentity{}, ErrInvalidEmailDocumentRequest
	}
	receipt, err := s.EmailDocumentPublication(ctx, r.OperationID)
	if err != nil {
		return document.EmailDocumentIdentity{}, err
	}
	if receipt.RequestDigest != r.RequestDigest || r.Order > len(receipt.Relations) {
		return document.EmailDocumentIdentity{}, ErrEmailDocumentConflict
	}
	child := receipt.Relations[r.Order-1].Child
	if child == nil {
		return document.EmailDocumentIdentity{}, ErrEmailPartUnavailable
	}
	if r.ExecutionIdentity.Upload.SHA256 != child.SHA256 || r.ExecutionIdentity.Upload.ByteLength != child.Size {
		return document.EmailDocumentIdentity{}, ErrEmailDocumentConflict
	}
	return *child, nil
}
