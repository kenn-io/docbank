package qmdexport

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPublishWindowsRetainedPointerFlushFailureIsPostPublication(t *testing.T) {
	// Catches losing committed selection when the real retained-handle flush
	// fails after native Windows replacement. Requires actual Windows execution.
	target := publicationTarget(t)
	old := publishOne(t, target)
	selectedNow := false
	selected, err := publishWithHooks(t.Context(), target, "synthetic", nil, syntheticReader{}, Options{}, publishHooks{
		afterCurrent: func() { selectedNow = true },
		beforeFileSync: func(file *os.File) error {
			if selectedNow {
				require.NoError(t, file.Close())
			}
			return nil
		},
	})
	var post *PostPublicationError
	require.ErrorAs(t, err, &post)
	require.Equal(t, "confirmation_incomplete", post.Code)
	require.ErrorIs(t, err, os.ErrClosed)
	require.NotEqual(t, old.GenerationID, selected.GenerationID)
	current, loadErr := LoadCurrent(target)
	require.NoError(t, loadErr)
	require.Equal(t, selected.GenerationID, current.GenerationID)
}
