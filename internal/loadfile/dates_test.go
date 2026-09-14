package loadfile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeclaredDateOrderResolvesAmbiguityAndFractionSurvives(t *testing.T) {
	p, err := ReadProfile("dat-concordance-v1")
	require.NoError(t, err)
	p.DateFormat = "DD/MM/YYYY"
	claim, findings := ParseTimeClaim("03/04/2026", p)
	require.Empty(t, findings)
	assert.Equal(t, "2026-04-03", claim.DateValue)
	assert.Equal(t, "omitted", claim.TimezoneKind)
	assert.Nil(t, claim.UTCKey)

	p.DateFormat = "MM/DD/YYYY"
	claim, findings = ParseTimeClaim("03/04/2026", p)
	require.Empty(t, findings)
	assert.Equal(t, "2026-03-04", claim.DateValue)

	p.DateFormat = "YYYY-MM-DD"
	claim, findings = ParseTimeClaim("2026-04-03T09:15:00.123456+02:00", p)
	require.Empty(t, findings)
	assert.Equal(t, "fraction", claim.Precision)
	assert.Equal(t, "2026-04-03T09:15:00.123456+02:00", claim.Raw)
	assert.Equal(t, "2026-04-03T09:15:00.123456", claim.DateValue)
	assert.Equal(t, "2026-04-03T07:15:00.123456000Z", requireValue(t, claim.UTCKey))
}

func TestTwoDigitYearAndAmbiguousOrderBlock(t *testing.T) {
	p, err := ReadProfile("dat-concordance-v1")
	require.NoError(t, err)
	p.DateFormat, p.DeclaredTimezone = "DD/MM/YYYY", "UTC"
	claim, diagnostics := ParseTimeClaim("03/04/26", p)
	assert.Equal(t, "03/04/26", claim.Raw)
	assert.Empty(t, claim.DateValue)
	require.Len(t, diagnostics, 1)
	assert.Equal(t, "package_mapping_ambiguous", diagnostics[0].Code)
	assert.Equal(t, "blocking", diagnostics[0].Severity)

	p.DateFormat = "YYYY-MM-DD"
	exact, none := ParseTimeClaim("2026-04-03T09:15:00+02:00", p)
	assert.Empty(t, none)
	assert.Equal(t, "second", exact.Precision)
	assert.Equal(t, "offset", exact.TimezoneKind)
	assert.Equal(t, 7200, requireValue(t, exact.OffsetSeconds))
	assert.Equal(t, "2026-04-03T07:15:00.000000000Z", requireValue(t, exact.UTCKey))

	p.DateFormat = "DD/MM/YYYY"
	dateOnly, none := ParseTimeClaim("03/04/2026", p)
	assert.Empty(t, none)
	assert.Equal(t, "date", dateOnly.Precision)
	assert.Equal(t, "2026-04-03", dateOnly.DateValue)
	assert.Equal(t, "omitted", dateOnly.TimezoneKind)
	assert.Equal(t, "UTC", dateOnly.ZoneText)
	assert.Nil(t, dateOnly.UTCKey)
}

func TestYearOnlyAndMonthOnlySuppliedDatesKeepTheirPrecision(t *testing.T) {
	p, err := ReadProfile("dat-concordance-v1")
	require.NoError(t, err)
	p.DeclaredTimezone = "UTC"
	year, none := ParseTimeClaim("2026", p)
	assert.Empty(t, none)
	assert.Equal(t, "year", year.Precision)
	assert.Equal(t, "2026", year.DateValue)
	assert.Equal(t, "2026", year.Raw)
	assert.Nil(t, year.UTCKey)

	month, none := ParseTimeClaim("2026-04", p)
	assert.Empty(t, none)
	assert.Equal(t, "month", month.Precision)
	assert.Equal(t, "2026-04", month.DateValue)
	assert.Nil(t, month.UTCKey)

	partial, none := ParseTimeClaim("2026-04-03T09:15Z", p)
	assert.Empty(t, none)
	assert.Equal(t, "minute", partial.Precision)
	assert.Equal(t, "utc", partial.TimezoneKind)
	assert.Equal(t, "2026-04-03T09:15:00.000000000Z", requireValue(t, partial.UTCKey))

	for _, raw := range []string{"04/2026", "26"} {
		_, diagnostics := ParseTimeClaim(raw, p)
		require.Len(t, diagnostics, 1, raw)
		assert.Equal(t, "package_mapping_ambiguous", diagnostics[0].Code, raw)
	}
}

func TestTimeClaimSupportsAllSevenPrecisionsAndCompactDeclaredDates(t *testing.T) {
	p, err := ReadProfile("dat-concordance-v1")
	require.NoError(t, err)
	for _, tc := range []struct {
		raw, precision, dateValue string
	}{
		{raw: "2026", precision: "year", dateValue: "2026"},
		{raw: "2026-04", precision: "month", dateValue: "2026-04"},
		{raw: "2026-04-03", precision: "date", dateValue: "2026-04-03"},
		{raw: "2026-04-03T09Z", precision: "hour", dateValue: "2026-04-03T09"},
		{raw: "2026-04-03T09:15Z", precision: "minute", dateValue: "2026-04-03T09:15"},
		{raw: "2026-04-03T09:15:27Z", precision: "second", dateValue: "2026-04-03T09:15:27"},
		{raw: "2026-04-03T09:15:27.1Z", precision: "fraction", dateValue: "2026-04-03T09:15:27.1"},
		{raw: "2026-04-03T09:15:27.123456789Z", precision: "fraction", dateValue: "2026-04-03T09:15:27.123456789"},
	} {
		t.Run(tc.precision+tc.raw, func(t *testing.T) {
			claim, diagnostics := ParseTimeClaim(tc.raw, p)
			require.Empty(t, diagnostics)
			assert.Equal(t, tc.raw, claim.Raw)
			assert.Equal(t, tc.precision, claim.Precision)
			assert.Equal(t, tc.dateValue, claim.DateValue)
		})
	}

	p.DateFormat = "YYYYMMDD"
	compact, diagnostics := ParseTimeClaim("20260403 09:15:27.12", p)
	require.Empty(t, diagnostics)
	assert.Equal(t, "2026-04-03T09:15:27.12", compact.DateValue)
	assert.Equal(t, "fraction", compact.Precision)
	assert.Equal(t, "omitted", compact.TimezoneKind)
	assert.Nil(t, compact.UTCKey)
}

func TestTimeClaimDistinguishesFloatingDeclaredAndNamedZones(t *testing.T) {
	p, err := ReadProfile("dat-concordance-v1")
	require.NoError(t, err)

	floating, diagnostics := ParseTimeClaim("2026-04-03T09:15:27", p)
	require.Empty(t, diagnostics)
	assert.Equal(t, "omitted", floating.TimezoneKind)
	assert.Empty(t, floating.ZoneText)
	assert.Nil(t, floating.UTCKey)

	p.DeclaredTimezone = "-05:30"
	declared, diagnostics := ParseTimeClaim("2026-04-03T09:15", p)
	require.Empty(t, diagnostics)
	assert.Equal(t, "offset", declared.TimezoneKind)
	assert.Equal(t, "-05:30", declared.ZoneText)
	assert.Equal(t, -19800, requireValue(t, declared.OffsetSeconds))
	assert.Equal(t, "2026-04-03T14:45:00.000000000Z", requireValue(t, declared.UTCKey))

	p.DeclaredTimezone = "America/New_York"
	for _, raw := range []string{
		"2026-11-01T01:30:00",
		"2026-03-08T02:30:00",
	} {
		claim, namedDiagnostics := ParseTimeClaim(raw, p)
		assert.Equal(t, "named", claim.TimezoneKind)
		assert.Equal(t, "America/New_York", claim.ZoneText)
		assert.Nil(t, claim.UTCKey)
		require.Len(t, namedDiagnostics, 1)
		assert.Equal(t, "named_zone_unresolved", namedDiagnostics[0].Code)
		assert.Equal(t, "warning", namedDiagnostics[0].Severity)
	}

	p.DeclaredTimezone = "America/Not_A_Real_Zone"
	unknown, namedDiagnostics := ParseTimeClaim("2026-04-03T09:15:00", p)
	assert.Equal(t, "named", unknown.TimezoneKind)
	assert.Equal(t, "America/Not_A_Real_Zone", unknown.ZoneText)
	assert.Nil(t, unknown.UTCKey)
	require.Len(t, namedDiagnostics, 1)
	assert.Equal(t, "named_zone_unresolved", namedDiagnostics[0].Code)
}

func TestTimeClaimRejectsInvalidZoneLeapSecondAndExcessFraction(t *testing.T) {
	p, err := ReadProfile("dat-concordance-v1")
	require.NoError(t, err)

	for _, tc := range []struct {
		name, raw, zone, code string
	}{
		{name: "invalid zone", raw: "2026-04-03T09:15:00", zone: "Not A Zone", code: "invalid_timezone"},
		{name: "leap second", raw: "2026-04-03T09:15:60Z", code: "leap_second_unsupported"},
		{name: "excess fraction", raw: "2026-04-03T09:15:00.1234567890Z", code: "fractional_precision_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p.DeclaredTimezone = tc.zone
			claim, diagnostics := ParseTimeClaim(tc.raw, p)
			assert.Equal(t, tc.raw, claim.Raw)
			assert.Equal(t, "invalid", claim.TimezoneKind)
			assert.Nil(t, claim.UTCKey)
			require.Len(t, diagnostics, 1)
			assert.Equal(t, tc.code, diagnostics[0].Code)
			assert.Equal(t, "blocking", diagnostics[0].Severity)
		})
	}
}

func TestTimeClaimBlocksMalformedNamedZonesWithoutDiscardingCivilValue(t *testing.T) {
	p, err := ReadProfile("dat-concordance-v1")
	require.NoError(t, err)

	for _, tc := range []struct {
		name, raw, declared, zone string
	}{
		{
			name: "declared whitespace", raw: "2026-04-03T09:15:00",
			declared: "Not A/Zone", zone: "Not A/Zone",
		},
		{
			name: "declared control", raw: "2026-04-03T09:15:00",
			declared: "Area/Bad\tZone", zone: "Area/Bad\tZone",
		},
		{
			name: "inline invalid segment character",
			raw:  "2026-04-03T09:15:00 Area/Bad:Zone", zone: "Area/Bad:Zone",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p.DeclaredTimezone = tc.declared
			claim, diagnostics := ParseTimeClaim(tc.raw, p)
			assert.Equal(t, tc.raw, claim.Raw)
			assert.Equal(t, "2026-04-03T09:15:00", claim.DateValue)
			assert.Equal(t, "second", claim.Precision)
			assert.Equal(t, "invalid", claim.TimezoneKind)
			assert.Equal(t, tc.zone, claim.ZoneText)
			assert.Nil(t, claim.UTCKey)
			require.Len(t, diagnostics, 1)
			assert.Equal(t, "invalid_timezone", diagnostics[0].Code)
			assert.Equal(t, "blocking", diagnostics[0].Severity)
		})
	}
}

func TestTimeClaimBlocksUTCKeyYearRollover(t *testing.T) {
	p, err := ReadProfile("dat-concordance-v1")
	require.NoError(t, err)

	upperRaw := "9999-12-31T23:59:59-14:00"
	upper, diagnostics := ParseTimeClaim(upperRaw, p)
	assert.Equal(t, upperRaw, upper.Raw)
	assert.Equal(t, "9999-12-31T23:59:59", upper.DateValue)
	assert.Equal(t, "offset", upper.TimezoneKind)
	assert.Equal(t, "-14:00", upper.ZoneText)
	assert.Nil(t, upper.UTCKey)
	require.Len(t, diagnostics, 1)
	assert.Equal(t, "utc_key_out_of_range", diagnostics[0].Code)
	assert.Equal(t, "blocking", diagnostics[0].Severity)

	lowerRaw := "0001-01-01T00:00:00+14:00"
	lower, diagnostics := ParseTimeClaim(lowerRaw, p)
	assert.Equal(t, lowerRaw, lower.Raw)
	assert.Equal(t, "0001-01-01T00:00:00", lower.DateValue)
	assert.Equal(t, "offset", lower.TimezoneKind)
	assert.Equal(t, "+14:00", lower.ZoneText)
	assert.Nil(t, lower.UTCKey)
	require.Len(t, diagnostics, 1)
	assert.Equal(t, "utc_key_out_of_range", diagnostics[0].Code)
	assert.Equal(t, "blocking", diagnostics[0].Severity)
}

func TestDeclaredSlashDatesRespectCivilYearRange(t *testing.T) {
	p, err := ReadProfile("dat-concordance-v1")
	require.NoError(t, err)

	for _, test := range []struct {
		name       string
		dateFormat string
		raw        string
	}{
		{name: "day first date", dateFormat: "DD/MM/YYYY", raw: "31/12/0000"},
		{name: "month first date", dateFormat: "MM/DD/YYYY", raw: "12/31/0000"},
		{name: "floating", dateFormat: "DD/MM/YYYY", raw: "31/12/0000 23:59:59"},
		{name: "offset rollover", dateFormat: "DD/MM/YYYY", raw: "31/12/0000 23:59:59-14:00"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p.DateFormat = test.dateFormat
			claim, diagnostics := ParseTimeClaim(test.raw, p)
			assert.Equal(t, test.raw, claim.Raw)
			assert.Empty(t, claim.DateValue)
			assert.Nil(t, claim.UTCKey)
			require.Len(t, diagnostics, 1)
			assert.Equal(t, "package_mapping_ambiguous", diagnostics[0].Code)
			assert.Equal(t, "blocking", diagnostics[0].Severity)
		})
	}

	p.DateFormat = "DD/MM/YYYY"
	valid, diagnostics := ParseTimeClaim("01/01/0001", p)
	require.Empty(t, diagnostics)
	assert.Equal(t, "01/01/0001", valid.Raw)
	assert.Equal(t, "0001-01-01", valid.DateValue)
	assert.Equal(t, "date", valid.Precision)
	assert.Nil(t, valid.UTCKey)
}

func TestUndeclaredSlashDateOnlyAcceptsAnUnambiguousOrder(t *testing.T) {
	p, err := ReadProfile("dat-concordance-v1")
	require.NoError(t, err)

	ambiguous, diagnostics := ParseTimeClaim("03/04/2026", p)
	assert.Empty(t, ambiguous.DateValue)
	require.Len(t, diagnostics, 1)
	assert.Equal(t, "package_mapping_ambiguous", diagnostics[0].Code)

	unambiguous, diagnostics := ParseTimeClaim("13/04/2026", p)
	require.Empty(t, diagnostics)
	assert.Equal(t, "2026-04-13", unambiguous.DateValue)
}

func TestDeclaredDateFormatIsAuthoritative(t *testing.T) {
	for _, tc := range []struct{ format, raw string }{
		{"YYYY-MM-DD", "25/12/2024"},
		{"YYYYMMDD", "2024-12-25"},
		{"DD/MM/YYYY", "2024-12-25T09:15Z"},
		{"MM/DD/YYYY", "20241225"},
		{"unsupported", "2024-12-25"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			claim, diagnostics := ParseTimeClaim(tc.raw, Profile{DateFormat: tc.format})
			assert.Empty(t, claim.DateValue)
			require.Len(t, diagnostics, 1)
			assert.Equal(t, "blocking", diagnostics[0].Severity)
		})
	}
}

func TestNamedZoneAliasesRemainUnresolved(t *testing.T) {
	for _, zone := range []string{"EST", "GMT", "EST5EDT", "UTC0"} {
		t.Run(zone, func(t *testing.T) {
			claim, diagnostics := ParseTimeClaim("2024-12-25T09:15", Profile{DeclaredTimezone: zone})
			assert.Equal(t, "named", claim.TimezoneKind)
			assert.Equal(t, zone, claim.ZoneText)
			assert.Nil(t, claim.UTCKey)
			require.Len(t, diagnostics, 1)
			assert.Equal(t, "named_zone_unresolved", diagnostics[0].Code)
			assert.Equal(t, "warning", diagnostics[0].Severity)
		})
	}
}

func requireValue[T any](t *testing.T, value *T) T {
	t.Helper()
	require.NotNil(t, value)
	return *value
}
