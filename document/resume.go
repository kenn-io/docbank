package document

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// RenditionResumeHandle is an opaque, provider-issued durable operation
// identity. Core persists it only after the provider checkpoints it; callers
// must never derive a handle from source or job identity.
type RenditionResumeHandle struct {
	Value string
}

// RenditionResumeCheckpoint durably records a provider-issued handle before
// the provider continues work whose outcome may otherwise become ambiguous.
type RenditionResumeCheckpoint func(RenditionResumeHandle) error

// ResumableRenditionProvider is the narrow optional contract for providers
// that can continue a known durable operation without resubmitting source
// bytes. A nil handle starts new work; a non-nil handle resumes exactly that
// provider-issued operation.
type ResumableRenditionProvider interface {
	RenditionProvider
	RenderResumable(
		ctx context.Context, upload AuthorizedUpload, authorization RenditionAuthorization,
		resume *RenditionResumeHandle, checkpoint RenditionResumeCheckpoint,
	) (RenditionResult, error)
}

// ValidateRenditionProviderRequest validates the exact immutable provider and
// upload authorization against the current trusted clock.
func ValidateRenditionProviderRequest(
	provider RenditionProvider, upload AuthorizedUpload, authorization RenditionAuthorization,
) (RenditionDescriptor, error) {
	return ValidateRenditionProviderRequestAt(time.Now().UTC(), provider, upload, authorization)
}

// ValidateRenditionProviderRequestAt validates the exact immutable provider
// and upload authorization against an explicit trusted clock.
func ValidateRenditionProviderRequestAt(
	now time.Time, provider RenditionProvider, upload AuthorizedUpload,
	authorization RenditionAuthorization,
) (RenditionDescriptor, error) {
	descriptor, _, err := validateRenditionProviderRequestAt(now, provider, upload, authorization)
	return descriptor, err
}

// RenderRenditionWithResume preserves the resumable provider boundary while
// applying the same sealed upload and result checks as RenderRendition. New
// work receives the read-once sealed upload; resume work receives no source
// bytes and remains limited to the supplied provider-issued handle.
func RenderRenditionWithResume(
	ctx context.Context, provider RenditionProvider, upload AuthorizedUpload,
	authorization RenditionAuthorization, resume *RenditionResumeHandle,
	checkpoint RenditionResumeCheckpoint,
) (RenditionResult, error) {
	resumable, supportsResume := provider.(ResumableRenditionProvider)
	if !supportsResume {
		if resume != nil {
			return RenditionResult{}, errors.New("rendition provider does not support durable resume")
		}
		return RenderRendition(ctx, provider, upload, authorization)
	}
	if resume != nil {
		return renderLegacyResume(ctx, resumable, upload, authorization, *resume, checkpoint)
	}
	checked, checkpointError := checkedRenditionCheckpoint(checkpoint)
	return renderRendition(ctx, provider, upload, authorization,
		func(executionCtx context.Context, sealedUpload AuthorizedUpload, sealed RenditionAuthorization) (RenditionResult, error) {
			result, err := resumable.RenderResumable(executionCtx, sealedUpload, sealed, nil, checked)
			if durableErr := checkpointError(); durableErr != nil {
				return RenditionResult{}, durableErr
			}
			return result, err
		})
}

func renderLegacyResume(
	ctx context.Context, provider ResumableRenditionProvider, upload AuthorizedUpload,
	authorization RenditionAuthorization, resume RenditionResumeHandle,
	checkpoint RenditionResumeCheckpoint,
) (result RenditionResult, err error) {
	if err := validateRenditionResumeHandle(resume); err != nil {
		return RenditionResult{}, err
	}
	if nilInterface(upload) {
		return RenditionResult{}, errors.New("authorized upload is required")
	}
	ownedUpload := &ownedAuthorizedUpload{upload: upload}
	defer func() {
		if closeErr := ownedUpload.Close(); closeErr != nil && !errors.Is(err, closeErr) {
			err = errors.Join(err, fmt.Errorf("close authorized upload: %w", closeErr))
		}
	}()
	if err := ctx.Err(); err != nil {
		return RenditionResult{}, err
	}
	sealed := cloneRenditionAuthorization(authorization)
	descriptor, _, err := validateRenditionProviderRequestAt(time.Now().UTC(), provider, ownedUpload, sealed)
	if err != nil {
		return RenditionResult{}, err
	}
	expiresAt, err := parseRenditionTimestamp(sealed.ExpiresAt)
	if err != nil {
		return RenditionResult{}, errors.New("authorization expiry must be canonical")
	}
	executionCtx, cancel := context.WithDeadline(ctx, expiresAt)
	defer cancel()
	checked, checkpointError := checkedRenditionCheckpoint(checkpoint)
	result, err = provider.RenderResumable(executionCtx, nil, cloneRenditionAuthorization(sealed), &resume, checked)
	if contextErr := ctx.Err(); contextErr != nil {
		return RenditionResult{}, contextErr
	}
	if currentErr := validateAuthorizationCurrentAt(sealed, time.Now().UTC()); currentErr != nil {
		return RenditionResult{}, currentErr
	}
	if contextErr := executionCtx.Err(); contextErr != nil {
		return RenditionResult{}, contextErr
	}
	if durableErr := checkpointError(); durableErr != nil {
		return RenditionResult{}, durableErr
	}
	if err != nil {
		if classified := ValidateRenditionProviderError(err); classified != nil {
			return RenditionResult{}, classified
		}
		return RenditionResult{}, err
	}
	result = cloneRenditionResult(result)
	if err := validateResumedRenditionResult(descriptor, sealed, result); err != nil {
		return RenditionResult{}, err
	}
	return result, nil
}

func checkedRenditionCheckpoint(checkpoint RenditionResumeCheckpoint) (RenditionResumeCheckpoint, func() error) {
	if checkpoint == nil {
		checkpoint = func(RenditionResumeHandle) error { return nil }
	}
	var mutex sync.Mutex
	var first error
	return func(handle RenditionResumeHandle) error {
			if err := validateRenditionResumeHandle(handle); err != nil {
				mutex.Lock()
				defer mutex.Unlock()
				if first == nil {
					first = err
				}
				return err
			}
			err := checkpoint(handle)
			if err != nil {
				mutex.Lock()
				defer mutex.Unlock()
				if first == nil {
					first = err
				}
			}
			return err
		}, func() error {
			mutex.Lock()
			defer mutex.Unlock()
			return first
		}
}

func validateRenditionResumeHandle(handle RenditionResumeHandle) error {
	if handle.Value == "" || len(handle.Value) > 512 {
		return errors.New("rendition resume handle must contain 1-512 characters")
	}
	for _, char := range handle.Value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || strings.ContainsRune("-._~", char) {
			continue
		}
		return errors.New("rendition resume handle contains unsupported characters")
	}
	return nil
}
