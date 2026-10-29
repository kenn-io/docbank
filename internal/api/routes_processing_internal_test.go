package api

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/retrieval"
)

func TestProcessingSourceIneligibleDetails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		err    error
		detail string
	}{
		{"document", processing.ErrSourceIneligible,
			"source is not eligible for the rendition profile; see the plan's rendition_ineligible_reason"},
		{"image preview", &processing.ImageSourceIneligibleError{Err: fmt.Errorf("%w: preview_missing", processing.ErrSourceIneligible)},
			"source is not eligible for the rendition profile: preview_missing"},
	} {
		for _, consent := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/consent_saved=%t", tc.name, consent), func(t *testing.T) {
				err, detail := tc.err, tc.detail
				if consent {
					err = fmt.Errorf("%w: %w", processing.ErrConsentSaved, err)
					detail = "processing consent was saved: " + detail
				}
				problem, ok := errors.AsType[*Error](fromProcessingError(err))
				require.True(t, ok)
				assert.Equal(t, http.StatusUnprocessableEntity, problem.Status)
				assert.Equal(t, "rendition_source_ineligible", problem.Code)
				assert.Equal(t, detail, problem.Detail)
			})
		}
	}
}

func TestDocumentSearchResponsePreservesTimedEvidence(t *testing.T) {
	t.Parallel()
	report := processing.SearchReport{Results: []retrieval.Result{{
		Evidence: []retrieval.EvidenceReference{
			{Kind: "rendition_segment", TimeSpan: &retrieval.MediaTimeSpan{StartMS: 0, EndMS: 1000}},
			{Kind: "embedding_input", TimeSpan: &retrieval.MediaTimeSpan{StartMS: 7000, EndMS: 9500}},
			{Kind: "rendition_segment"},
		},
	}}}

	response := fromDocumentSearchReport(report, false)
	body, err := json.Marshal(response.Results[0].Evidence)
	require.NoError(t, err)
	assert.JSONEq(t, `[
		{"kind":"rendition_segment","time_span":{"start_ms":0,"end_ms":1000}},
		{"kind":"embedding_input","time_span":{"start_ms":7000,"end_ms":9500}},
		{"kind":"rendition_segment"}
	]`, string(body))
}

func TestProcessingTerminalEventSeparatesStatusReadFailure(t *testing.T) {
	t.Parallel()
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

	// A terminal rendition error preserves the password refusal reported by the worker.
	event = processingTerminalEvent(job, processing.ErrRenditionFailed,
		processing.Status{State: "failed", FailureCode: "pdf_password_required"}, nil)
	require.NotNil(t, event.Status)
	assert.Equal(t, "failed", event.Status.State)
	assert.Equal(t, "pdf_password_required", event.Status.FailureCode)
	assert.Equal(t, "status", event.Type)
	assert.True(t, event.Terminal)
}
