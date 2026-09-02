package providerutil

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/docbank/document"
)

// ErrTotalTimeout is the cancellation cause of an operation that exhausted
// its adapter total timeout.
var ErrTotalTimeout = errors.New("provider total timeout")

// Operation bounds one Render call by the caller context, the authorization
// expiry, and the adapter's total timeout.
type Operation struct {
	provider  Provider
	caller    context.Context
	ctx       context.Context
	cancel    context.CancelFunc
	expiresAt time.Time
}

// NewOperation parses the authorization expiry and derives a context bounded
// by the caller, the expiry, and totalTimeout. A zero totalTimeout applies no
// total bound beyond the expiry.
func NewOperation(
	ctx context.Context, provider Provider, expiresAt string, totalTimeout time.Duration,
) (*Operation, error) {
	expiry, err := time.Parse(TimestampForm, expiresAt)
	if err != nil {
		return nil, provider.Classified(document.RenditionErrorPolicyRejected,
			string(provider)+" authorization expiry is invalid", nil)
	}
	bounded := ctx
	cancelTotal := context.CancelFunc(func() {})
	if totalTimeout > 0 {
		bounded, cancelTotal = context.WithTimeoutCause(ctx, totalTimeout, ErrTotalTimeout)
	}
	operationCtx, cancelExpiry := context.WithDeadline(bounded, expiry)
	return &Operation{
		provider: provider, caller: ctx, ctx: operationCtx, expiresAt: expiry,
		cancel: func() {
			cancelExpiry()
			cancelTotal()
		},
	}, nil
}

// Context returns the bounded operation context.
func (operation *Operation) Context() context.Context { return operation.ctx }

// Cancel releases the operation context.
func (operation *Operation) Cancel() { operation.cancel() }

// CallerDone reports whether the caller context ended.
func (operation *Operation) CallerDone() bool { return operation.caller.Err() != nil }

// Check classifies why the operation can no longer proceed, in precedence
// order: caller cancellation, authorization expiry, total timeout, and any
// other context end. It returns nil while the operation may continue.
func (operation *Operation) Check() error {
	if err := operation.caller.Err(); err != nil {
		return operation.provider.Canceled(err)
	}
	if !time.Now().Before(operation.expiresAt) {
		return operation.provider.Expired()
	}
	if errors.Is(context.Cause(operation.ctx), ErrTotalTimeout) {
		return operation.provider.Classified(document.RenditionErrorCapacity,
			string(operation.provider)+" total timeout reached",
			fmt.Errorf("%w: %w", ErrTotalTimeout, operation.ctx.Err()))
	}
	if err := operation.ctx.Err(); err != nil {
		return operation.provider.Canceled(err)
	}
	return nil
}

// Wait sleeps for delay unless the operation ends first.
func (operation *Operation) Wait(delay time.Duration) error {
	if err := Wait(operation.ctx, delay); err != nil {
		if checkErr := operation.Check(); checkErr != nil {
			return checkErr
		}
		return operation.provider.Canceled(err)
	}
	return nil
}

// ReadUpload reads and verifies the exact authorized bytes, interrupting a
// blocked read when the operation ends.
func (operation *Operation) ReadUpload(upload document.AuthorizedUpload) ([]byte, error) {
	stopInterrupt := context.AfterFunc(operation.ctx, func() { _ = document.InterruptAuthorizedUpload(upload) })
	source, err := ReadAuthorizedUpload(operation.ctx, upload, upload.Metadata(), string(operation.provider))
	stopInterrupt()
	if err != nil {
		if operationErr := operation.Check(); operationErr != nil {
			return nil, operationErr
		}
		return nil, err
	}
	return source, nil
}
