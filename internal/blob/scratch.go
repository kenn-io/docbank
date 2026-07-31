package blob

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
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

func scratchCapacityBytes(blocks, blockSize uint64) int64 {
	high, low := bits.Mul64(blocks, blockSize)
	if high != 0 || low > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(low)
}

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
