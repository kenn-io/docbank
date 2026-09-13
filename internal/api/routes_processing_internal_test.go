package api

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/processing"
)

func TestProcessingTerminalEventSeparatesStatusReadFailure(t *testing.T) {
	job := processing.Job{ID: strings.Repeat("a", 64), AttachmentID: strings.Repeat("b", 64),
		EmbeddingJobIDs: []string{strings.Repeat("c", 64)}}
	// A failed observation does not change the outcome of completed processing.
	event := processingTerminalEvent(job, nil, processing.Status{}, errors.New("status read failed"))
	assert.Nil(t, event.Status)
	require.NotNil(t, event.Job)
	assert.Equal(t, fromProcessingJob(job), *event.Job)
	assert.Equal(t, "error", event.Type)
	require.NotNil(t, event.Error)
	assert.Equal(t, "processing_status_unavailable", event.Error.Code)
	assert.True(t, event.Terminal)
}
