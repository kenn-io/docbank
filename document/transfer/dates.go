package transfer

import (
	"errors"
	"strings"
	"time"
)

func ValidateDate(date DateV1) error {
	return validateDate(date, true)
}

func validateDate(date DateV1, requireKind bool) error {
	invalid := errors.New("transfer: invalid date evidence")
	if requireKind && !ValidDateKind(date.Kind) || !requireKind && date.Kind != "" ||
		!ValidPrecision(date.Precision) ||
		!ValidTimezoneKind(date.Timezone) || !ValidOrigin(date.Origin) {
		return invalid
	}
	if date.Precision == PrecisionFraction {
		if date.FractionDigits < 1 || date.FractionDigits > 9 {
			return invalid
		}
	} else if date.FractionDigits != 0 {
		return invalid
	}
	if date.UTCOffsetMinutes != nil && (*date.UTCOffsetMinutes < -840 || *date.UTCOffsetMinutes > 840) {
		return invalid
	}

	layout := "2006-01-02"
	switch date.Precision {
	case PrecisionDate:
	case PrecisionHour:
		layout += "T15"
	case PrecisionMinute:
		layout += "T15:04"
	case PrecisionSecond:
		layout += "T15:04:05"
	case PrecisionFraction:
		layout += "T15:04:05." + strings.Repeat("0", date.FractionDigits)
	}

	if date.Timezone == TimezoneKindInvalid {
		if date.Instant != "" || date.Civil == "" || date.Raw == "" || len(date.Diagnostics) == 0 {
			return invalid
		}
		return nil
	}
	if date.Precision == PrecisionDate && date.Timezone != TimezoneKindOmitted {
		return invalid
	}

	if date.Timezone == TimezoneKindOmitted || date.Timezone == TimezoneKindNamed {
		if date.Instant != "" || date.UTCOffsetMinutes != nil || date.Civil == "" {
			return invalid
		}
		if date.Timezone == TimezoneKindNamed {
			if date.TimezoneName == "" {
				return invalid
			}
		} else if date.TimezoneName != "" {
			return invalid
		}
		parsed, err := time.Parse(layout, date.Civil)
		if err != nil || parsed.Format(layout) != date.Civil {
			return invalid
		}
		return nil
	}

	if date.Timezone == TimezoneKindUTC && date.TimezoneName != "" {
		return invalid
	}
	parsed, err := time.Parse("2006-01-02T15:04:05.000000000Z", date.Instant)
	if err != nil || parsed.Format("2006-01-02T15:04:05.000000000Z") != date.Instant {
		return invalid
	}
	if date.Timezone == TimezoneKindOffset && date.UTCOffsetMinutes == nil {
		return invalid
	}
	if date.Timezone == TimezoneKindUTC && date.UTCOffsetMinutes != nil && *date.UTCOffsetMinutes != 0 {
		return invalid
	}
	offset := 0
	if date.UTCOffsetMinutes != nil {
		offset = *date.UTCOffsetMinutes
	}
	local := parsed.In(time.FixedZone("source", offset*60))
	civil := local.Format(layout)
	if date.Civil != "" && date.Civil != civil {
		return invalid
	}
	reparsed, err := time.ParseInLocation(layout, civil, local.Location())
	if err != nil || !reparsed.Equal(parsed) {
		return invalid
	}
	return nil
}
