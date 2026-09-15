package transfer

import (
	"errors"
	"strings"

	"encoding/json/jsontext"
	"encoding/json/v2"

	"go.kenn.io/docbank/internal/canonical"
)

var (
	errInvalidSourceFields = errors.New("transfer: invalid source fields")
	errUnknownSourceField  = errors.New("transfer: unknown source field")
)

type CallSourceFieldsV1 struct {
	DurationMS *int64 `json:"duration_ms,omitzero"`
}

type CalendarSourceFieldsV1 struct {
	Recurrence        []string `json:"recurrence,omitzero"`
	RecurringEventID  string   `json:"recurring_event_id,omitzero"`
	OriginalStartTime string   `json:"original_start_time,omitzero"`
}

type TranscriptSourceFieldsV1 struct {
	SpeakerLabels []string `json:"speaker_labels"`
}

func ValidateSourceFields(kind Kind, raw jsontext.Value) error {
	if len(raw) == 0 {
		return nil
	}
	if len(raw) > MaxSourceFieldsBytes {
		return errors.New("transfer: source fields limit")
	}
	invalid := errInvalidSourceFields
	switch kind {
	case KindCall, KindVoicemail:
		value, err := canonical.Decode[CallSourceFieldsV1](raw)
		if err != nil {
			return safeSourceFieldsDecodeError(err)
		}
		if value.DurationMS == nil || *value.DurationMS < 0 || *value.DurationMS > MaxCallDurationMilliseconds {
			return invalid
		}
	case KindCalendarEvent:
		value, err := canonical.Decode[CalendarSourceFieldsV1](raw)
		if err != nil {
			return safeSourceFieldsDecodeError(err)
		}
		if len(value.Recurrence) == 0 && value.RecurringEventID == "" && value.OriginalStartTime == "" ||
			len(value.Recurrence) > MaxRecurrenceRules || len(value.RecurringEventID) > MaxRecordRefBytes || len(value.OriginalStartTime) > MaxRecordRefBytes {
			return invalid
		}
		if value.Recurrence != nil && len(value.Recurrence) == 0 {
			return invalid
		}
		for _, line := range value.Recurrence {
			if line == "" || len(line) > MaxRecurrenceLineBytes {
				return invalid
			}
		}
	case KindMeetingNote, KindTranscript:
		value, err := canonical.Decode[TranscriptSourceFieldsV1](raw)
		if err != nil {
			return safeSourceFieldsDecodeError(err)
		}
		if len(value.SpeakerLabels) == 0 || len(value.SpeakerLabels) > MaxSpeakerLabels {
			return invalid
		}
		for _, label := range value.SpeakerLabels {
			if strings.TrimSpace(label) == "" || len(label) > MaxNameBytes {
				return invalid
			}
		}
	default:
		return errUnknownSourceField
	}
	return nil
}

func safeSourceFieldsDecodeError(err error) error {
	if errors.Is(err, json.ErrUnknownName) {
		return errUnknownSourceField
	}
	return errInvalidSourceFields
}
