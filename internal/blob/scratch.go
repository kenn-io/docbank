package blob

import (
	"errors"
	"fmt"
	"os"
)

// ErrInsufficientScratch reports that a remote seekable read cannot be staged
// within the currently available local temporary space.
var ErrInsufficientScratch = errors.New("insufficient local scratch space")

type ScratchSpaceError struct {
	Required  int64
	Available int64
}

func (e *ScratchSpaceError) Error() string {
	return fmt.Sprintf("%s: need %d bytes, have %d",
		ErrInsufficientScratch, e.Required, e.Available)
}

func (e *ScratchSpaceError) Unwrap() error { return ErrInsufficientScratch }

var inspectScratchSpace = availableScratchBytes

func requireScratchSpace(required int64) error {
	if required <= 0 {
		return nil
	}
	available, err := inspectScratchSpace(os.TempDir())
	if err != nil {
		return fmt.Errorf("inspect local scratch space: %w", err)
	}
	if available < required {
		return &ScratchSpaceError{Required: required, Available: available}
	}
	return nil
}
