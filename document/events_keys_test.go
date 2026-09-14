package document

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	goldenEventID      = "3c680f0a9dc9fee2ff0971c585f73f3bf77fda10368e86beffae977bf5fee550"
	goldenGenerationID = "63f4e156e8e7029ae934f4de4be47e4e9ecc686d40ad15d4b992c2390c09bcff"
)

func TestEventKeysNeverInventAnInstant(t *testing.T) {
	axis, err := EventAxisKey("2019-03", "month", "omitted", nil)
	require.NoError(t, err)
	assert.Equal(t, "2019-03-01T00:00:00.000000000", axis)

	_, ok, err := EventUTCKey("2019-03", "month", "omitted", nil)
	require.NoError(t, err)
	assert.False(t, ok, "an omitted zone has no UTC instant")

	offset := -18000
	axis, err = EventAxisKey("2019-03-04T09:30:00", "second", "offset", &offset)
	require.NoError(t, err)
	assert.Equal(t, "2019-03-04T14:30:00.000000000", axis)
	utc, ok, err := EventUTCKey("2019-03-04T09:30:00", "second", "offset", &offset)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "2019-03-04T14:30:00.000000000Z", utc)

	_, ok, err = EventUTCKey("2019-03-04T09:30:00", "second", "invalid", nil)
	require.NoError(t, err)
	assert.False(t, ok)

	_, err = EventAxisKey("2019-13-01", "date", "utc", nil)
	require.ErrorContains(t, err, "date value is not a valid civil calendar value")
}

func TestEventAndGenerationIdentityGoldenVectors(t *testing.T) {
	// Computed independently from the length-prefixed preimage. Changing either literal is a
	// contract break that re-derives every vault.
	eventID := DocumentEventID("vault-uid-1", "cv-1", "metadata/gen-1/created",
		"created", "2019-03-04", "date", "omitted", strings.Repeat("a", 64))
	require.Len(t, eventID, 64)
	assert.Equal(t, goldenEventID, eventID)

	generationID := DocumentEventGenerationID("vault-uid-1", "cv-1",
		DocumentEventsContractV1, strings.Repeat("b", 64), strings.Repeat("c", 64))
	require.Len(t, generationID, 64)
	assert.Equal(t, goldenGenerationID, generationID)

	changedSource := DocumentEventID("vault-uid-1", "cv-1", "email/a/root/header/7/date",
		"created", "2019-03-04", "date", "omitted", strings.Repeat("a", 64))
	assert.NotEqual(t, goldenEventID, changedSource)
}

func TestEventPrecisionDoesNotInventMidnight(t *testing.T) {
	for _, test := range []struct {
		value     string
		precision EventPrecision
	}{
		{"2019", "year"},
		{"2019-03", "month"},
		{"2019-03-04", "date"},
	} {
		_, ok, err := EventUTCKey(test.value, test.precision, "utc", nil)
		require.NoError(t, err)
		assert.False(t, ok, test.value)
	}

	key, ok, err := EventUTCKey("2019-03-04T09:30:00.1200", "fraction", "utc", nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "2019-03-04T09:30:00.120000000Z", key)
}

func TestEventCivilKeysValidatePrecisionAndCalendar(t *testing.T) {
	valid := []struct {
		value     string
		precision EventPrecision
		want      string
	}{
		{"2019", "year", "2019-01-01T00:00:00.000000000"},
		{"2019-02", "month", "2019-02-01T00:00:00.000000000"},
		{"2020-02-29", "date", "2020-02-29T00:00:00.000000000"},
		{"2019-03-04T09", "hour", "2019-03-04T09:00:00.000000000"},
		{"2019-03-04T09:30", "minute", "2019-03-04T09:30:00.000000000"},
		{"2019-03-04T09:30:00", "second", "2019-03-04T09:30:00.000000000"},
		{"2019-03-04T09:30:00.1200", "fraction", "2019-03-04T09:30:00.120000000"},
	}
	for _, test := range valid {
		got, err := EventAxisKey(test.value, test.precision, "omitted", nil)
		require.NoError(t, err, test.value)
		assert.Equal(t, test.want, got, test.value)
	}

	invalid := []struct {
		value     string
		precision EventPrecision
	}{
		{"2019-02-29", "date"},
		{"2019-03-04", "month"},
		{"2019-03-04T09:30", "second"},
		{"2019-03-04T09:30:60", "second"},
		{"2019-03-04T09:30:00.", "fraction"},
		{"2019-03-04T09:30:00.1234567890", "fraction"},
		{"2019-03-04", "day"},
	}
	for _, test := range invalid {
		_, err := EventAxisKey(test.value, test.precision, "omitted", nil)
		require.Error(t, err, "%s as %s", test.value, test.precision)
	}
}

func TestEventUTCKeyValidatesZoneAndOffsetCombinations(t *testing.T) {
	zero, maximum, minimum, tooHigh, tooLow := 0, 86399, -86399, 86400, -86400
	tests := []struct {
		name   string
		zone   EventTimezoneKind
		offset *int
		wantOK bool
		want   string
		bad    bool
	}{
		{"utc nil", "utc", nil, true, "2019-03-04T09:30:00.000000000Z", false},
		{"utc zero", "utc", &zero, true, "2019-03-04T09:30:00.000000000Z", false},
		{"offset maximum", "offset", &maximum, true, "2019-03-03T09:30:01.000000000Z", false},
		{"offset minimum", "offset", &minimum, true, "2019-03-05T09:29:59.000000000Z", false},
		{"offset absent", "offset", nil, false, "", true},
		{"offset too high", "offset", &tooHigh, false, "", true},
		{"offset too low", "offset", &tooLow, false, "", true},
		{"named floating", "named", nil, false, "", false},
		{"omitted floating", "omitted", nil, false, "", false},
		{"invalid floating", "invalid", nil, false, "", false},
		{"named cannot carry offset", "named", &zero, false, "", true},
		{"omitted cannot carry offset", "omitted", &zero, false, "", true},
		{"invalid cannot carry offset", "invalid", &zero, false, "", true},
		{"unknown zone", "local", nil, false, "", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok, err := EventUTCKey("2019-03-04T09:30:00", "second", test.zone, test.offset)
			if test.bad {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.wantOK, ok)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestEventUTCKeyRejectsOffsetYearOverflow(t *testing.T) {
	positive, negative := 1, -1
	_, _, err := EventUTCKey("0001-01-01T00:00:00", "second", "offset", &positive)
	require.ErrorContains(t, err, "outside four-digit year range")
	_, _, err = EventUTCKey("9999-12-31T23:59:59", "second", "offset", &negative)
	require.ErrorContains(t, err, "outside four-digit year range")
}

func TestNamedZoneFoldAndGapRemainCivil(t *testing.T) {
	for _, value := range []string{"2021-11-07T01:30:00", "2021-03-14T02:30:00"} {
		axis, err := EventAxisKey(value, "second", "named", nil)
		require.NoError(t, err)
		assert.Equal(t, value+".000000000", axis)
		utc, ok, err := EventUTCKey(value, "second", "named", nil)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Empty(t, utc)
	}
}
