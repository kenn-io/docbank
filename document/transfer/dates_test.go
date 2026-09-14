package transfer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTransferDateValidationPreservesCivilPrecision(t *testing.T) {
	require.Error(t, ValidateDate(DateV1{
		Kind: DateKindEventStart, Civil: "2024-03-10", Precision: PrecisionDate,
		Timezone: TimezoneKindOmitted, Instant: "2024-03-10T00:00:00.000000000Z",
	}))
	require.NoError(t, ValidateDate(DateV1{
		Kind: DateKindEventStart, Civil: "2024-03-10", Precision: PrecisionDate,
		Timezone: TimezoneKindOmitted, Origin: OriginStartTime,
	}))
	require.NoError(t, ValidateDate(DateV1{
		Kind: DateKindEventStart, Civil: "2024-11-03T01:30", Precision: PrecisionMinute,
		Timezone: TimezoneKindNamed, TimezoneName: "America/New_York", Origin: OriginStartTime,
	}))
	require.Error(t, ValidateDate(DateV1{
		Kind: DateKindMessageSent, Instant: "2024-01-01T12:00:00.123000000Z",
		Precision: PrecisionFraction, FractionDigits: 0, Timezone: TimezoneKindUTC, Origin: OriginSentAt,
	}))
}

func TestTransferDateValidationKeepsOffsetFractionAndInvalidEvidenceHonest(t *testing.T) {
	zero := 0
	plusHour := 60
	tests := []struct {
		name    string
		date    DateV1
		wantErr bool
	}{
		{
			name: "explicit zero offset",
			date: DateV1{
				Kind: DateKindMessageSent, Instant: "2024-01-01T12:00:00.000000000Z",
				Civil: "2024-01-01T12:00:00", Precision: PrecisionSecond,
				Timezone: TimezoneKindOffset, UTCOffsetMinutes: &zero, Origin: OriginSentAt,
			},
		},
		{
			name: "positive offset",
			date: DateV1{
				Kind: DateKindEventStart, Instant: "2024-01-01T12:00:00.000000000Z",
				Civil: "2024-01-01T13:00", Precision: PrecisionMinute,
				Timezone: TimezoneKindOffset, TimezoneName: "Europe/Paris",
				UTCOffsetMinutes: &plusHour, Origin: OriginStartTime,
			},
		},
		{
			name: "fraction width preserved",
			date: DateV1{
				Kind: DateKindMessageSent, Instant: "2024-01-01T12:00:00.123000000Z",
				Civil: "2024-01-01T12:00:00.123", Precision: PrecisionFraction, FractionDigits: 3,
				Timezone: TimezoneKindUTC, Origin: OriginSentAt,
			},
		},
		{
			name: "hidden precision rejected",
			date: DateV1{
				Kind: DateKindMessageSent, Instant: "2024-01-01T12:00:00.123400000Z",
				Civil: "2024-01-01T12:00:00.123", Precision: PrecisionFraction, FractionDigits: 3,
				Timezone: TimezoneKindUTC, Origin: OriginSentAt,
			},
			wantErr: true,
		},
		{
			name: "DST fold stays floating",
			date: DateV1{
				Kind: DateKindEventStart, Civil: "2024-11-03T01:30", Precision: PrecisionMinute,
				Timezone: TimezoneKindNamed, TimezoneName: "America/New_York", Origin: OriginStartTime,
			},
		},
		{
			name: "DST gap stays floating",
			date: DateV1{
				Kind: DateKindEventStart, Civil: "2024-03-10T02:30", Precision: PrecisionMinute,
				Timezone: TimezoneKindNamed, TimezoneName: "America/New_York", Origin: OriginStartTime,
			},
		},
		{
			name: "unknown named zone stays floating",
			date: DateV1{
				Kind: DateKindEventStart, Civil: "2024-03-10T02:30", Precision: PrecisionMinute,
				Timezone: TimezoneKindNamed, TimezoneName: "Mars/Olympus_Mons", Origin: OriginStartTime,
			},
		},
		{
			name: "leap second retained as invalid evidence",
			date: DateV1{
				Kind: DateKindMessageSent, Civil: "2016-12-31T23:59:60", Precision: PrecisionSecond,
				Timezone: TimezoneKindInvalid, Origin: OriginSentAt, Raw: "2016-12-31T23:59:60Z",
				Diagnostics: []string{"leap_second"},
			},
		},
		{
			name: "invalid evidence cannot invent instant",
			date: DateV1{
				Kind: DateKindMessageSent, Instant: "2017-01-01T00:00:00.000000000Z",
				Precision: PrecisionSecond, Timezone: TimezoneKindInvalid, Origin: OriginSentAt,
				Raw: "2016-12-31T23:59:60Z", Diagnostics: []string{"leap_second"},
			},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateDate(test.date)
			if test.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestTransferDateValidationRejectsContradictoryEvidence(t *testing.T) {
	overflowOffset := 841
	valid := DateV1{
		Kind: DateKindMessageSent, Instant: "2024-01-01T12:00:00.000000000Z",
		Precision: PrecisionSecond, Timezone: TimezoneKindUTC, Origin: OriginSentAt,
	}
	cases := []DateV1{
		{},
		{Kind: DateKindEventStart, Civil: "2024-02-30", Precision: PrecisionDate, Timezone: TimezoneKindOmitted, Origin: OriginStartTime},
		{Kind: DateKindEventStart, Civil: "2024-01-01", Precision: PrecisionDate, Timezone: TimezoneKindNamed, TimezoneName: "UTC", Origin: OriginStartTime},
		{Kind: DateKindEventStart, Civil: "2024-01-01T12:00", Precision: PrecisionMinute, Timezone: TimezoneKindNamed, Origin: OriginStartTime},
		{Kind: DateKindEventStart, Civil: "2024-01-01T12:00", Precision: PrecisionMinute, Timezone: TimezoneKindOmitted, TimezoneName: "UTC", Origin: OriginStartTime},
		{Kind: DateKindEventStart, Civil: "2024-01-01T12:00", Precision: PrecisionMinute, Timezone: TimezoneKindOffset, Origin: OriginStartTime},
		{Kind: DateKindEventStart, Instant: "2024-01-01T12:00:00Z", Precision: PrecisionSecond, Timezone: TimezoneKindUTC, Origin: OriginStartTime},
		{Kind: DateKindEventStart, Civil: "2024-01-01T12:00:00", Instant: "2024-01-01T12:00:01.000000000Z", Precision: PrecisionSecond, Timezone: TimezoneKindUTC, Origin: OriginStartTime},
		{Kind: DateKindEventStart, Instant: "2024-01-01T12:00:00.000000000Z", Precision: PrecisionSecond, Timezone: TimezoneKindOffset, UTCOffsetMinutes: &overflowOffset, Origin: OriginStartTime},
		{Kind: DateKindEventStart, Precision: PrecisionSecond, Timezone: TimezoneKindInvalid, Origin: OriginStartTime, Raw: "bad"},
	}
	for _, date := range cases {
		require.Error(t, ValidateDate(date), "%+v", date)
	}
	require.NoError(t, ValidateDate(valid))
}
