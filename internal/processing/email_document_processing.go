package processing

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

// EnqueueEmailDocumentProcessing prepares an exact published child with a
// configured provider before admitting work to the daemon's rendition worker.
func (service *Service) EnqueueEmailDocumentProcessing(
	ctx context.Context, request document.EmailDocumentProcessingRequest,
) (document.EmailDocumentProcessingReceipt, error) {
	child, err := service.catalog.EmailDocumentProcessingChild(ctx, request)
	if err != nil {
		return document.EmailDocumentProcessingReceipt{}, err
	}
	_, fingerprint, err := document.CanonicalProfile(request.Profile)
	if err != nil || request.Profile.Rendition == nil {
		return document.EmailDocumentProcessingReceipt{}, store.ErrInvalidEmailDocumentRequest
	}
	profileName := ""
	for name, profile := range service.profiles {
		if profile.record.Fingerprint == fingerprint.Profile {
			profileName = name
			break
		}
	}
	if profileName == "" {
		return document.EmailDocumentProcessingReceipt{}, ErrProfileNotConfigured
	}
	node, version, profile, err := service.resolve(ctx, Selector{
		NodeID: child.NodeID, ContentVersionID: child.VersionID, Profile: profileName,
	})
	if errors.Is(err, store.ErrNotFound) {
		return document.EmailDocumentProcessingReceipt{}, fmt.Errorf(
			"%w: operation %s: child is no longer current and live", store.ErrEmailDocumentConflict, request.OperationID)
	}
	if err != nil {
		return document.EmailDocumentProcessingReceipt{}, err
	}
	prepared, err := service.prepareExecutableRendition(ctx, node, version, profile)
	if err != nil {
		return document.EmailDocumentProcessingReceipt{}, err
	}
	want, _, err := document.CanonicalRenditionExecutionIdentityV1(prepared.identity)
	if err != nil {
		return document.EmailDocumentProcessingReceipt{}, err
	}
	got, _, err := document.CanonicalRenditionExecutionIdentityV1(request.ExecutionIdentity)
	if err != nil || !bytes.Equal(want, got) {
		return document.EmailDocumentProcessingReceipt{}, fmt.Errorf(
			"%w: execution identity differs from the exact child and configured provider",
			store.ErrInvalidEmailDocumentRequest)
	}
	request.ExecutionIdentity = prepared.identity
	var receipt document.EmailDocumentProcessingReceipt
	err = service.gate.MutateContext(ctx, func() error {
		var enqueueErr error
		receipt, enqueueErr = service.catalog.RequestEmailDocumentProcessing(ctx, request)
		return enqueueErr
	})
	return receipt, err
}

// RequestEmailDocumentProcessing executes admitted work synchronously for an
// embedded vault, which does not run a background rendition worker.
func (service *Service) RequestEmailDocumentProcessing(
	ctx context.Context, request document.EmailDocumentProcessingRequest,
) (document.EmailDocumentProcessingReceipt, error) {
	receipt, err := service.EnqueueEmailDocumentProcessing(ctx, request)
	if err != nil {
		return receipt, err
	}
	_, err = service.runRenditionJob(ctx, receipt.JobID, receipt.WaiterID)
	if current, statusErr := service.catalog.RenditionJobByID(ctx, receipt.JobID); statusErr == nil {
		receipt.State = string(current.State)
	}
	return receipt, err
}
