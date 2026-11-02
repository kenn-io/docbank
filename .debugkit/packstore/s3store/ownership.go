package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"go.kenn.io/kit/packstore"
)

const maxMarkerBytes = int64(4096)

// Ownership reads and strictly validates the live fencing marker.
func (b *Backend) Ownership(ctx context.Context) (packstore.Ownership, error) {
	owner, etag, err := b.readOwnership(ctx)
	if err != nil {
		return packstore.Ownership{}, err
	}
	expected, _ := b.expectedOwnership()
	if expected != nil && owner == *expected {
		b.setOwnership(owner, etag)
	}
	return owner, nil
}

func (b *Backend) readOwnership(ctx context.Context) (packstore.Ownership, string, error) {
	output, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.keys.ownership()),
	})
	if err != nil {
		return packstore.Ownership{}, "", classifyError("read ownership marker", err)
	}
	defer func() { _ = output.Body.Close() }()
	if output.ContentLength != nil &&
		(*output.ContentLength < 0 || *output.ContentLength > maxMarkerBytes) {
		return packstore.Ownership{}, "", errors.Join(
			packstore.ErrPhysicalCorrupt,
			fmt.Errorf("s3store: ownership marker size %d is invalid", *output.ContentLength),
		)
	}
	data, err := io.ReadAll(io.LimitReader(output.Body, maxMarkerBytes+1))
	if err != nil {
		return packstore.Ownership{}, "", classifyError("read ownership marker", err)
	}
	if int64(len(data)) > maxMarkerBytes {
		return packstore.Ownership{}, "", errors.Join(
			packstore.ErrPhysicalCorrupt,
			errors.New("s3store: ownership marker is too large"),
		)
	}
	owner, err := packstore.ParseOwnership(data)
	if err != nil {
		return packstore.Ownership{}, "", errors.Join(packstore.ErrPhysicalCorrupt, err)
	}
	return owner, aws.ToString(output.ETag), nil
}

// ReplaceOwnership conditionally creates or replaces the live marker. A
// takeover writes the new epoch first; a caller that loses the race becomes
// fenced on its next destructive admission.
func (b *Backend) ReplaceOwnership(
	ctx context.Context,
	next packstore.Ownership,
	expected *packstore.Ownership,
) error {
	encoded, err := packstore.MarshalOwnership(next)
	if err != nil {
		return err
	}
	input := &s3.PutObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.keys.ownership()),
		Body:   bytes.NewReader(encoded),
	}
	if expected == nil {
		input.IfNoneMatch = aws.String("*")
	} else {
		actual, etag, readErr := b.readOwnership(ctx)
		if readErr != nil {
			return &packstore.OwnershipMismatchError{Expected: *expected, Err: readErr}
		}
		if actual != *expected {
			return &packstore.OwnershipMismatchError{Expected: *expected, Actual: actual}
		}
		input.IfMatch = aws.String(etag)
	}
	output, err := b.client.PutObject(ctx, input)
	if err != nil {
		if statusCode(err) == http.StatusPreconditionFailed {
			actual, _, readErr := b.readOwnership(ctx)
			if expected == nil {
				return &packstore.OwnershipMismatchError{Actual: actual, Err: readErr}
			}
			return &packstore.OwnershipMismatchError{
				Expected: *expected, Actual: actual, Err: readErr,
			}
		}
		return classifyError("publish ownership marker", err)
	}
	b.setOwnership(next, aws.ToString(output.ETag))
	return nil
}

func (b *Backend) requireOwnership(ctx context.Context) (packstore.Ownership, error) {
	expected, _ := b.expectedOwnership()
	if expected == nil {
		return packstore.Ownership{}, &packstore.OwnershipMismatchError{
			Err: errors.New("s3store: backend is not attached"),
		}
	}
	actual, etag, err := b.readOwnership(ctx)
	if err != nil {
		return packstore.Ownership{}, &packstore.OwnershipMismatchError{
			Expected: *expected, Err: err,
		}
	}
	if actual != *expected {
		return packstore.Ownership{}, &packstore.OwnershipMismatchError{
			Expected: *expected, Actual: actual,
		}
	}
	b.setOwnership(actual, etag)
	return actual, nil
}

func classifyError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	switch statusCode(err) {
	case http.StatusNotFound:
		return errors.Join(packstore.ErrPhysicalMissing, fmt.Errorf("s3store: %s: %w", operation, err))
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout,
		http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return errors.Join(packstore.ErrStoreUnavailable, fmt.Errorf("s3store: %s: %w", operation, err))
	default:
		return errors.Join(packstore.ErrStoreUnavailable, fmt.Errorf("s3store: %s: %w", operation, err))
	}
}

func statusCode(err error) int {
	if response, ok := errors.AsType[*smithyhttp.ResponseError](err); ok {
		return response.HTTPStatusCode()
	}
	return 0
}
