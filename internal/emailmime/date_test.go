package emailmime

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestInterpretDateUsesOnlyFixedTimezoneProfile(t *testing.T) {
	tests := []struct {
		raw, civil, utc string
		zone            document.EmailTimezoneState
	}{{"Wed, 09 Sep 2026 12:34:56 EDT", "2026-09-09T12:34:56", "2026-09-09T16:34:56Z", document.EmailTimezoneKnownNamed}, {"09 Sep 2026 12:34:56 +0530", "2026-09-09T12:34:56", "2026-09-09T07:04:56Z", document.EmailTimezoneNumeric}, {"09 Sep 2026 12:34:56 -0000", "2026-09-09T12:34:56", "2026-09-09T12:34:56Z", document.EmailTimezoneNumeric}, {"09 Sep 2026 12:34:56 XYZ", "2026-09-09T12:34:56", "", document.EmailTimezoneUnknownNamed}, {"09 Sep 2026 12:34:56 Z", "2026-09-09T12:34:56", "", document.EmailTimezoneUnknownNamed}, {"09 Sep 2026 12:34:56", "2026-09-09T12:34:56", "", document.EmailTimezoneMissing}}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			d := &decoder{limits: Recipe().Limits}
			date := d.interpretDate("1", []parsedHeader{{index: 0, value: tc.raw}})
			require.Equal(t, document.EmailDateParsed, date.State)
			assert.Equal(t, tc.civil, *date.Civil)
			assert.Equal(t, tc.zone, date.TimezoneState)
			if tc.utc == "" {
				assert.Nil(t, date.UTC)
			} else {
				require.NotNil(t, date.UTC)
				assert.Equal(t, tc.utc, *date.UTC)
			}
		})
	}
}

func TestInterpretDateRejectsAmbiguityCalendarAndWeekday(t *testing.T) {
	d := &decoder{limits: Recipe().Limits}
	for _, raw := range []string{"09 Sep 2026 12:34:56 +1260", "31 Sep 2026 12:34:56 +0000", "Tue, 09 Sep 2026 12:34:56 +0000"} {
		date := d.interpretDate("1", []parsedHeader{{index: 0, value: raw}})
		assert.Equal(t, document.EmailDateInvalid, date.State)
		assert.Nil(t, date.UTC)
	}
	duplicate := d.interpretDate("1", []parsedHeader{{index: 0, value: "09 Sep 2026 12:34:56 +0000"}, {index: 1, value: "09 Sep 2026 12:34:56 +0000"}})
	assert.Equal(t, document.EmailDateInvalid, duplicate.State)
	assert.Equal(t, []int{0, 1}, duplicate.Fields)
}

func TestInterpretDateRetainsLeapSecondWithoutInventingInstant(t *testing.T) {
	d := &decoder{limits: Recipe().Limits}
	date := d.interpretDate("1", []parsedHeader{{index: 0, value: "09 Sep 2026 12:34:60 GMT"}})
	require.Equal(t, document.EmailDateParsed, date.State)
	assert.Equal(t, "2026-09-09T12:34:60", *date.Civil)
	assert.Nil(t, date.UTC)
	require.NotEmpty(t, date.Diagnostics)
	assert.Equal(t, document.EmailDiagnosticLeapSecondInstantUnavailable, date.Diagnostics[0].Code)
	invalidZone := d.interpretDate("1", []parsedHeader{{index: 0, value: "09 Sep 2026 12:34:60 +1260"}})
	assert.Equal(t, document.EmailDateInvalid, invalidZone.State)
}

func TestDecodeDegradesRequiredDateDiagnosticsWhenBudgetIsExhausted(t *testing.T) {
	raw := strings.Repeat("From: bad\r\n", 4095) + "Date: 09 Sep 2026 12:34:60 CET\r\n\r\nbody"
	result := decodeFixture(t, raw)
	require.Equal(t, document.EmailInventoryPartial, result.Evidence.Inventory.State)
	require.NotNil(t, result.Evidence.Inventory.Termination)
	assert.Equal(t, document.EmailDiagnosticLimit, result.Evidence.Inventory.Termination.Code)
	require.Len(t, result.Evidence.Inventory.Messages, 1)
	date := result.Evidence.Inventory.Messages[0].Date
	assert.Equal(t, document.EmailDateInvalid, date.State)
	assert.Nil(t, date.TimezoneText)
	assert.Empty(t, date.Diagnostics)
}

func TestDateDegradationDropsTimezoneOriginDiagnostic(t *testing.T) {
	d := &decoder{limits: Recipe().Limits}
	d.limits.Diagnostics = 1
	date := d.interpretDate("1", []parsedHeader{{index: 0, value: "09 Sep 2026 12:34:60 -0000"}})

	assert.Equal(t, document.EmailDateInvalid, date.State)
	assert.Nil(t, date.TimezoneText)
	assert.Empty(t, date.Diagnostics)
	assert.Equal(t, 0, d.diagnosticCount)
}

func TestEssentialDiagnosticDegradesDateBeforeEvictingRequiredDiagnostic(t *testing.T) {
	d := &decoder{limits: Recipe().Limits}
	d.limits.Diagnostics = 2
	date := d.interpretDate("1", []parsedHeader{{index: 0, value: "09 Sep 2026 12:34:60 CET"}})
	require.Equal(t, document.EmailDateParsed, date.State)
	require.Len(t, date.Diagnostics, 2)
	d.messages = []document.EmailMessageV1{{Date: date}}
	optional := []document.EmailDiagnosticV1{}

	diagnostic := d.essentialDiagnosticAt(&optional, document.EmailDiagnosticInvalidHeader, document.EmailOperationHeaders, "1.1", 0, "Content-Type header is invalid")

	require.Len(t, diagnostic, 1)
	assert.Equal(t, document.EmailDiagnosticInvalidHeader, diagnostic[0].Code)
	assert.Equal(t, document.EmailDateInvalid, d.messages[0].Date.State)
	assert.Nil(t, d.messages[0].Date.TimezoneText)
	assert.Empty(t, d.messages[0].Date.Diagnostics)
	assert.Equal(t, 1, d.diagnosticCount)
	require.NotNil(t, d.termination)
	assert.Equal(t, int64(3), *d.termination.Observed)
}

func TestEssentialDiagnosticEvictsAlternativeBeforeDegradingDate(t *testing.T) {
	d := &decoder{limits: Recipe().Limits}
	d.limits.Diagnostics = 2
	date := d.interpretDate("1", []parsedHeader{{index: 0, value: "09 Sep 2026 12:34:56 CET"}})
	require.Equal(t, document.EmailDateParsed, date.State)
	path := "1.1"
	d.messages = []document.EmailMessageV1{{Date: date, Alternatives: []document.EmailAlternativeV1{{Diagnostics: []document.EmailDiagnosticV1{{Code: document.EmailDiagnosticBodyUTF8Limit, Operation: document.EmailOperationCharset, Path: &path}}}}}}
	d.diagnosticCount = 2
	optional := []document.EmailDiagnosticV1{}

	diagnostic := d.essentialDiagnosticAt(&optional, document.EmailDiagnosticInvalidHeader, document.EmailOperationHeaders, "1.2", 0, "Content-Type header is invalid")

	require.Len(t, diagnostic, 1)
	assert.Equal(t, document.EmailDateParsed, d.messages[0].Date.State)
	require.Len(t, d.messages[0].Date.Diagnostics, 1)
	assert.Empty(t, d.messages[0].Alternatives[0].Diagnostics)
}

func TestDecodeRejectsEmbeddedSignNumericTimezone(t *testing.T) {
	result := decodeFixture(t, "Date: 09 Sep 2026 12:34:56 +-100\r\n\r\nbody")
	date := result.Evidence.Inventory.Messages[0].Date
	assert.Equal(t, document.EmailDateInvalid, date.State)
	assert.Nil(t, date.UTC)
}
