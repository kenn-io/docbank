package packstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"go.kenn.io/kit/pack"
)

var (
	// ErrStoreUnavailable reports a configured store that cannot currently be reached.
	ErrStoreUnavailable = errors.New("packstore: store unavailable")
	// ErrStoreFenced reports a binding whose ownership marker no longer matches.
	ErrStoreFenced = errors.New("packstore: store fenced")
	// ErrPhysicalMissing reports absent bytes under otherwise valid authority.
	ErrPhysicalMissing = errors.New("packstore: physical object missing")
	// ErrPhysicalCorrupt reports bytes that fail physical or content verification.
	ErrPhysicalCorrupt = errors.New("packstore: physical object corrupt")
	// ErrPhysicalAuthorityMissing reports membership with no physical candidates.
	ErrPhysicalAuthorityMissing = errors.New("packstore: physical authority missing")
)

// AttemptError retains one candidate failure without discarding evidence from
// other physical locations.
type AttemptError struct {
	Location ReadLocation
	Err      error
}

// ExhaustedError reports that every authorized physical candidate failed.
type ExhaustedError struct {
	Headline error
	Attempts []AttemptError
}

func (e *ExhaustedError) Error() string {
	if len(e.Attempts) == 1 {
		return fmt.Sprintf("%v: %v", e.Headline, e.Attempts[0].Err)
	}
	return fmt.Sprintf("%v after %d candidate attempt(s)", e.Headline, len(e.Attempts))
}

// Unwrap preserves every candidate cause for errors.Is and errors.As.
func (e *ExhaustedError) Unwrap() []error {
	errs := make([]error, 0, len(e.Attempts))
	for _, attempt := range e.Attempts {
		errs = append(errs, attempt.Err)
	}
	return errs
}

func isCandidateFailure(err error) bool {
	return errors.Is(err, ErrStoreUnavailable) ||
		errors.Is(err, ErrStoreFenced) ||
		errors.Is(err, ErrPhysicalMissing) ||
		errors.Is(err, ErrPhysicalCorrupt)
}

func newExhaustedError(attempts []AttemptError) *ExhaustedError {
	headline := ErrStoreUnavailable
	best := candidateFailureRank(headline)
	for _, attempt := range attempts {
		for _, candidate := range []error{
			ErrPhysicalCorrupt,
			ErrPhysicalMissing,
			ErrStoreFenced,
			ErrStoreUnavailable,
		} {
			if rank := candidateFailureRank(candidate); errors.Is(attempt.Err, candidate) && rank > best {
				headline = candidate
				best = rank
			}
		}
	}
	return &ExhaustedError{
		Headline: headline,
		Attempts: append([]AttemptError(nil), attempts...),
	}
}

func candidateFailureRank(err error) int {
	switch {
	case errors.Is(err, ErrPhysicalCorrupt):
		return 4
	case errors.Is(err, ErrPhysicalMissing):
		return 3
	case errors.Is(err, ErrStoreFenced):
		return 2
	case errors.Is(err, ErrStoreUnavailable):
		return 1
	default:
		return 0
	}
}

func classifyPhysicalError(err error) error {
	if err == nil || isCandidateFailure(err) {
		return err
	}
	if isPhysicalSourceNotFound(err) {
		return errors.Join(ErrPhysicalMissing, err)
	}
	if classified := ClassifyIntegrityError(err); errors.Is(classified, ErrPhysicalCorrupt) {
		return classified
	}
	if isPhysicalControlError(err) {
		return err
	}
	return errors.Join(ErrStoreUnavailable, err)
}

func isPhysicalControlError(err error) bool {
	for _, control := range []error{
		context.Canceled,
		context.DeadlineExceeded,
		io.EOF,
		fs.ErrClosed,
		pack.ErrVerificationIncomplete,
		pack.ErrStreamsActive,
		ErrInvalidPolicy,
		ErrBlobTooLarge,
		pack.ErrStreamUnsupported,
		pack.ErrStreamLimit,
	} {
		if errors.Is(err, control) {
			return true
		}
	}
	return false
}

// ClassifyRepresentationLimitError marks physical-representation limits as
// corruption while preserving structured limit evidence. Logical blob limits
// remain caller-policy errors.
func ClassifyRepresentationLimitError(err error) error {
	if err == nil || isCandidateFailure(err) {
		return err
	}
	limit, ok := errors.AsType[*LimitError](err)
	if !ok {
		return err
	}
	switch limit.Dimension {
	case LimitPackContainerBytes, LimitPackFooterBytes, LimitPackEntryCount,
		LimitBlobStoredBytes:
		return errors.Join(ErrPhysicalCorrupt, err)
	default:
		return err
	}
}

// ClassifyIntegrityError marks content and container verification failures as
// physical corruption while preserving errors that are already classified.
func ClassifyIntegrityError(err error) error {
	if err == nil || isCandidateFailure(err) {
		return err
	}
	for _, corrupt := range []error{
		ErrContentMismatch,
		pack.ErrBadMagic,
		pack.ErrUnsupportedVersion,
		pack.ErrTruncated,
		pack.ErrChecksum,
		pack.ErrCorrupt,
		pack.ErrBlobMismatch,
	} {
		if errors.Is(err, corrupt) {
			return errors.Join(ErrPhysicalCorrupt, err)
		}
	}
	return err
}
