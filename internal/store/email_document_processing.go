package store

import (
	"context"
	"fmt"
	"go.kenn.io/docbank/document"
)

func (s *Store) RequestEmailDocumentProcessing(ctx context.Context, r document.EmailDocumentProcessingRequest) (document.EmailDocumentProcessingReceipt, error) {
	if r.Order < 1 || r.Order > document.EmailDocumentMaxParts || len(r.InputClasses) > 32 || len(r.RetainedArtifactClasses) > 32 || len(r.CapturedArtifactPolicy) > 1<<20 || len(r.Principal) > 1024 || len(r.Scope) > 1024 {
		return document.EmailDocumentProcessingReceipt{}, ErrInvalidEmailDocumentRequest
	}
	receipt, err := s.EmailDocumentPublication(ctx, r.OperationID)
	if err != nil {
		return document.EmailDocumentProcessingReceipt{}, err
	}
	if receipt.RequestDigest != r.RequestDigest || r.Order > len(receipt.Relations) {
		return document.EmailDocumentProcessingReceipt{}, ErrEmailDocumentConflict
	}
	child := receipt.Relations[r.Order-1].Child
	if child == nil {
		return document.EmailDocumentProcessingReceipt{}, ErrEmailPartUnavailable
	}
	if r.ExecutionIdentity.Upload.SHA256 != child.SHA256 || r.ExecutionIdentity.Upload.ByteLength != child.Size {
		return document.EmailDocumentProcessingReceipt{}, ErrEmailDocumentConflict
	}
	canonical, fp, err := document.CanonicalProfile(r.Profile)
	if err != nil {
		return document.EmailDocumentProcessingReceipt{}, fmt.Errorf("%w: %w", ErrInvalidEmailDocumentRequest, err)
	}
	if r.Profile.Rendition == nil {
		return document.EmailDocumentProcessingReceipt{}, ErrInvalidEmailDocumentRequest
	}
	profile := ProcessingProfileRecord{Fingerprint: fp.Profile, CanonicalProfile: canonical, RenditionRequestFingerprint: fp.RenditionRequest, EvidenceLexicalFingerprint: fp.EvidenceLexical, RetentionDisclosureFingerprint: fp.RetentionDisclosure, AttachmentPolicyFingerprint: r.Profile.RetentionDisclosure.AttachmentPolicyFingerprint, ConsentFingerprint: r.Profile.RetentionDisclosure.ConsentFingerprint, RenditionDisclosureFingerprint: r.Profile.Rendition.DisclosureFingerprint, TrustBoundary: r.Profile.RetentionDisclosure.TrustBoundary}
	job, waiter, err := s.EnqueueRenditionJob(ctx, RenditionJobRequest{ContentVersionID: child.VersionID, Profile: profile, ExecutionIdentity: r.ExecutionIdentity, CapturedArtifactPolicy: r.CapturedArtifactPolicy, Authorization: ProviderOperationAuthorizationRequest{Principal: r.Principal, Scope: r.Scope, ProfileFingerprint: fp.Profile, DisclosureFingerprint: profile.RenditionDisclosureFingerprint, InputClasses: r.InputClasses, RetainedArtifactClasses: r.RetainedArtifactClasses}})
	if err != nil {
		return document.EmailDocumentProcessingReceipt{}, err
	}
	return document.EmailDocumentProcessingReceipt{JobID: job.ID, WaiterID: waiter.ID, Child: *child, State: string(job.State)}, nil
}
