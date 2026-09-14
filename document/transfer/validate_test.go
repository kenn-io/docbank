package transfer

import (
	"errors"
	"os"
	"strings"
	"testing"

	"encoding/json/jsontext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidationCleanupFailureInvalidatesSuccessfulRun(t *testing.T) {
	index, err := newValidationIndex()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(index.directory)) })
	index.removeAll = func(string) error { return errors.New("private /tmp/spool path") }
	validator := &packageValidator{report: Report{}, index: index}
	report, err := finishValidation(validator, nil, validator.close())
	require.Error(t, err)
	require.False(t, report.Valid)
	require.Equal(t, int64(1), report.FindingsTotal)
	require.NotContains(t, err.Error(), "/tmp/spool")
	require.NotContains(t, report.Findings[0].Detail, "/tmp/spool")
}

func TestValidationFindingsStayBounded(t *testing.T) {
	var report Report
	for range 100_000 {
		report.AddFinding(Finding{Severity: "error", Code: "unknown_field", Path: "records.jsonl"})
	}
	require.Len(t, report.Findings, 250)
	require.Equal(t, int64(100_000), report.FindingsTotal)
	require.True(t, report.FindingsTruncated)
}

func TestManifestAndTerminalCountsCannotConcealMissingRecords(t *testing.T) {
	declared := CountsV1{Record: 2}
	require.Error(t, ReconcileCounts(declared, declared, CountsV1{Record: 1}))
	require.NoError(t, ReconcileCounts(declared, declared, declared))
}

func TestSourceFieldsUseClosedKindSpecificSchemas(t *testing.T) {
	valid := []struct {
		kind Kind
		raw  string
	}{
		{KindCall, `{"duration_ms":83000}`},
		{KindVoicemail, `{"duration_ms":0}`},
		{KindCalendarEvent, `{"original_start_time":"2026-09-12T08:00:00Z","recurrence":["RRULE:FREQ=WEEKLY"],"recurring_event_id":"event-1"}`},
		{KindMeetingNote, `{"speaker_labels":["Speaker A","Speaker A","Ada"]}`},
		{KindTranscript, `{"speaker_labels":["Speaker B"]}`},
	}
	for _, test := range valid {
		require.NoError(t, ValidateSourceFields(test.kind, jsontext.Value(test.raw)), test.raw)
	}

	invalid := []struct {
		kind Kind
		raw  string
	}{
		{KindEmail, `{"duration_ms":1}`},
		{KindCall, `{}`},
		{KindCall, `{"duration_ms":2678400001}`},
		{KindCall, `{"duration_seconds":83}`},
		{KindCalendarEvent, `{"recurrence":[]}`},
		{KindCalendarEvent, `{"recurrence":["` + strings.Repeat("x", MaxRecurrenceLineBytes+1) + `"]}`},
		{KindTranscript, `{"speaker_labels":[]}`},
		{KindTranscript, `{"speaker_labels":[" "]}`},
		{KindTranscript, `{"speaker_labels":["` + strings.Repeat("x", MaxNameBytes+1) + `"]}`},
	}
	for _, test := range invalid {
		require.Error(t, ValidateSourceFields(test.kind, jsontext.Value(test.raw)), test.raw)
	}

	assert.NoError(t, ValidateSourceFields(KindEmail, nil))
}
