package blob

import (
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScratchCapacityBytes(t *testing.T) {
	assert.Equal(t, int64(16_384), scratchCapacityBytes(4, 4_096))
	assert.Equal(t, int64(math.MaxInt64), scratchCapacityBytes(math.MaxUint64, 4_096))
	assert.Equal(t, int64(math.MaxInt64), scratchCapacityBytes(math.MaxInt64, 2))
}

func TestScratchPreflightRejectsInsufficientSpace(t *testing.T) {
	original := inspectScratchSpace
	inspectScratchSpace = func(string) (int64, error) { return 99, nil }
	t.Cleanup(func() { inspectScratchSpace = original })

	err := requireScratchSpace(100)
	require.ErrorIs(t, err, ErrInsufficientScratch)
	var detail *ScratchSpaceError
	require.ErrorAs(t, err, &detail)
	assert.Equal(t, int64(100), detail.Required)
	assert.Equal(t, int64(99), detail.Available)
}

func TestScratchPreflightPreservesInspectionFailure(t *testing.T) {
	original := inspectScratchSpace
	inspectScratchSpace = func(string) (int64, error) {
		return 0, errors.New("stat unavailable")
	}
	t.Cleanup(func() { inspectScratchSpace = original })

	err := requireScratchSpace(1)
	require.ErrorContains(t, err, "stat unavailable")
	require.NotErrorIs(t, err, ErrInsufficientScratch)
}
