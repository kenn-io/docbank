package packstore

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// Move copies one authorized source candidate into a destination backend and
// independently verifies the destination read-back. It never changes catalog
// authority or retires the source.
func Move(
	ctx context.Context,
	source ReadBackend,
	destination Backend,
	request MoveRequest,
) (receipt MoveReceipt, resultErr error) {
	if ctx == nil {
		return MoveReceipt{}, errors.New("packstore: nil context")
	}
	if source == nil {
		return MoveReceipt{}, errors.New("packstore: nil move source backend")
	}
	if destination == nil {
		return MoveReceipt{}, errors.New("packstore: nil move destination backend")
	}
	if err := request.Identity.Validate(); err != nil {
		return MoveReceipt{}, err
	}
	if err := request.Source.Validate(); err != nil {
		return MoveReceipt{}, err
	}
	if identified, ok := source.(interface{ StoreID() StoreID }); ok {
		if sourceID := identified.StoreID(); sourceID != request.Source.StoreID {
			return MoveReceipt{}, fmt.Errorf(
				"packstore: move source %q does not match backend %q",
				request.Source.StoreID,
				sourceID,
			)
		}
	}
	if request.Destination == "" || destination.StoreID() != request.Destination {
		return MoveReceipt{}, fmt.Errorf(
			"packstore: move destination %q does not match backend %q",
			request.Destination,
			destination.StoreID(),
		)
	}
	if request.Source.Pack != nil && request.Source.Pack.Hash != request.Identity.Hash {
		return MoveReceipt{}, errors.New("packstore: move source hash does not match identity")
	}
	stream, sourceSize, err := openBackendStream(
		ctx,
		source,
		request.Identity.Hash,
		request.Source,
	)
	if err != nil {
		return MoveReceipt{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, stream.Close()) }()
	if sourceSize != request.Identity.Size {
		return MoveReceipt{}, fmt.Errorf(
			"%w: move source size %d does not match identity %d",
			ErrPhysicalCorrupt,
			sourceSize,
			request.Identity.Size,
		)
	}
	published, err := destination.PublishLoose(
		ctx,
		request.Identity.Hash,
		stream,
		PublishOptions{
			ExpectedSize: request.Identity.Size,
			SizeKnown:    true,
			Durability:   DurablePublication,
			Dedup:        VerifyFullHash,
			MaxBytes:     request.Identity.Size,
		},
	)
	if err != nil {
		return MoveReceipt{}, err
	}
	if err := stream.Verify(); err != nil {
		return MoveReceipt{}, classifyPhysicalError(err)
	}
	readback, readbackSize, err := destination.OpenLoose(
		ctx,
		request.Identity.Hash,
		published.Location,
	)
	if err != nil {
		return MoveReceipt{}, err
	}
	if readback == nil {
		return MoveReceipt{}, errors.New("packstore: destination returned a nil read-back stream")
	}
	defer func() { resultErr = errors.Join(resultErr, readback.Close()) }()
	if readbackSize != request.Identity.Size {
		return MoveReceipt{}, fmt.Errorf(
			"%w: destination size %d does not match identity %d",
			ErrPhysicalCorrupt,
			readbackSize,
			request.Identity.Size,
		)
	}
	written, err := io.Copy(io.Discard, readback)
	if err != nil {
		return MoveReceipt{}, classifyPhysicalError(err)
	}
	if written != request.Identity.Size {
		return MoveReceipt{}, fmt.Errorf(
			"%w: destination read-back copied %d bytes, expected %d",
			ErrPhysicalCorrupt,
			written,
			request.Identity.Size,
		)
	}
	if err := readback.Verify(); err != nil {
		return MoveReceipt{}, classifyPhysicalError(err)
	}
	return MoveReceipt{
		Destination: ReadLocation{
			StoreID: published.StoreID, Generation: published.Generation,
			Loose: &published.Location,
		},
		Verified: true,
		Created:  published.Created,
	}, nil
}
