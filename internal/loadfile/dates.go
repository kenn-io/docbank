package loadfile

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	yearPattern        = regexp.MustCompile(`^[0-9]{4}$`)
	monthPattern       = regexp.MustCompile(`^([0-9]{4})-([0-9]{2})$`)
	isoDatePattern     = regexp.MustCompile(`^([0-9]{4})-([0-9]{2})-([0-9]{2})$`)
	compactDatePattern = regexp.MustCompile(`^([0-9]{4})([0-9]{2})([0-9]{2})$`)
	slashDatePattern   = regexp.MustCompile(`^([0-9]{2})/([0-9]{2})/([0-9]{4})$`)
	clockPattern       = regexp.MustCompile(`(?s)^([0-9]{2})(?::([0-9]{2})(?::([0-9]{2})(?:\.([0-9]+))?)?)?(?:(Z|[+-][0-9]{2}:[0-9]{2})| (.+))?$`)
	offsetPattern      = regexp.MustCompile(`^([+-])([0-9]{2}):([0-9]{2})$`)
	ianaNamePattern    = regexp.MustCompile(`^[A-Za-z0-9._+-]+(?:/[A-Za-z0-9._+-]+)*$`)
)

type civilTime struct {
	year, month, day          int
	hour, minute, second      int
	nanosecond                int
	precision, fraction, zone string
}

func ParseTimeClaim(raw string, profile Profile) (TimeClaim, []Diagnostic) {
	claim := TimeClaim{Raw: raw, TimezoneKind: "invalid"}
	civil, ok, diagnostics := parseSelfContainedTime(raw, profile)
	if len(diagnostics) > 0 {
		return claim, diagnostics
	}
	if ok {
		return buildTimeClaim(raw, civil, profile)
	}

	_, clock, clockDiagnostics := parseClock(raw)
	if len(clockDiagnostics) > 0 {
		return claim, clockDiagnostics
	}
	if clock {
		return claim, ambiguousTimeDiagnostic("time-only input requires an explicitly paired date")
	}
	return claim, ambiguousTimeDiagnostic("date value does not match an unambiguous declared format")
}

func parseSelfContainedTime(raw string, profile Profile) (civilTime, bool, []Diagnostic) {
	if profile.DateFormat == "" && yearPattern.MatchString(raw) {
		year, _ := strconv.Atoi(raw)
		if year > 0 {
			return civilTime{year: year, precision: "year"}, true, nil
		}
	}
	if match := monthPattern.FindStringSubmatch(raw); profile.DateFormat == "" && match != nil {
		year, month := decimal(match[1]), decimal(match[2])
		if validMonth(year, month) {
			return civilTime{year: year, month: month, precision: "month"}, true, nil
		}
	}
	if date, ok, diagnostics := parseDateToken(raw, profile); ok || len(diagnostics) > 0 {
		return date, ok, diagnostics
	}

	for _, dateWidth := range []int{10, 8} {
		if len(raw) <= dateWidth || raw[dateWidth] != 'T' && raw[dateWidth] != ' ' {
			continue
		}
		date, ok, diagnostics := parseDateToken(raw[:dateWidth], profile)
		if len(diagnostics) > 0 {
			return civilTime{}, false, diagnostics
		}
		if !ok {
			continue
		}
		clock, ok, diagnostics := parseClock(raw[dateWidth+1:])
		if len(diagnostics) > 0 {
			return civilTime{}, false, diagnostics
		}
		if !ok {
			continue
		}
		clock.year, clock.month, clock.day = date.year, date.month, date.day
		return clock, true, nil
	}
	return civilTime{}, false, nil
}

func parseDateToken(raw string, profile Profile) (civilTime, bool, []Diagnostic) {
	if profile.DateFormat != "" {
		layout := declaredDateLayout(profile.DateFormat)
		if layout == "" {
			return civilTime{}, false, ambiguousTimeDiagnostic("declared date format is unsupported")
		}
		parsed, err := time.Parse(layout, raw)
		if err != nil {
			return civilTime{}, false, nil
		}
		date := civilTime{year: parsed.Year(), month: int(parsed.Month()), day: parsed.Day(), precision: "date"}
		return date, validDate(date), nil
	}
	if match := isoDatePattern.FindStringSubmatch(raw); match != nil {
		date := civilTime{year: decimal(match[1]), month: decimal(match[2]), day: decimal(match[3]), precision: "date"}
		return date, validDate(date), nil
	}
	if match := compactDatePattern.FindStringSubmatch(raw); match != nil {
		date := civilTime{year: decimal(match[1]), month: decimal(match[2]), day: decimal(match[3]), precision: "date"}
		return date, validDate(date), nil
	}
	match := slashDatePattern.FindStringSubmatch(raw)
	if match == nil {
		return civilTime{}, false, nil
	}
	first, second, year := decimal(match[1]), decimal(match[2]), decimal(match[3])
	dayFirst := civilTime{year: year, month: second, day: first, precision: "date"}
	monthFirst := civilTime{year: year, month: first, day: second, precision: "date"}
	dayFirstValid, monthFirstValid := validDate(dayFirst), validDate(monthFirst)
	switch {
	case dayFirstValid && monthFirstValid:
		return civilTime{}, false, ambiguousTimeDiagnostic("slash date parses under both DD/MM/YYYY and MM/DD/YYYY")
	case dayFirstValid:
		return dayFirst, true, nil
	case monthFirstValid:
		return monthFirst, true, nil
	default:
		return civilTime{}, false, nil
	}
}

func parseClock(raw string) (civilTime, bool, []Diagnostic) {
	match := clockPattern.FindStringSubmatch(raw)
	if match == nil {
		return civilTime{}, false, nil
	}
	clock := civilTime{hour: decimal(match[1]), zone: match[5]}
	if match[6] != "" {
		clock.zone = match[6]
	}
	switch {
	case match[2] == "":
		clock.precision = "hour"
	case match[3] == "":
		clock.minute = decimal(match[2])
		clock.precision = "minute"
	default:
		clock.minute = decimal(match[2])
		clock.second = decimal(match[3])
		clock.precision = "second"
	}
	if match[4] != "" {
		if len(match[4]) > 9 {
			return civilTime{}, false, blockingTimeDiagnostic("fractional_precision_exceeded", "fractional seconds exceed nine digits")
		}
		clock.fraction = match[4]
		clock.nanosecond = decimal(match[4] + strings.Repeat("0", 9-len(match[4])))
		clock.precision = "fraction"
	}
	if clock.second == 60 {
		return civilTime{}, false, blockingTimeDiagnostic("leap_second_unsupported", "leap-second input cannot be normalized without a pinned recipe")
	}
	if clock.hour > 23 || clock.minute > 59 || clock.second > 59 {
		return civilTime{}, false, ambiguousTimeDiagnostic("time component is outside its valid range")
	}
	return clock, true, nil
}

func buildTimeClaim(raw string, civil civilTime, profile Profile) (TimeClaim, []Diagnostic) {
	claim := TimeClaim{
		Raw:       raw,
		DateValue: civilDateValue(civil),
		Precision: civil.precision,
		ZoneText:  profile.DeclaredTimezone,
	}
	if civil.precision == "year" || civil.precision == "month" || civil.precision == "date" {
		claim.TimezoneKind = "omitted"
		return claim, nil
	}

	zone := civil.zone
	if zone == "" {
		zone = profile.DeclaredTimezone
	}
	claim.ZoneText = zone
	switch {
	case zone == "":
		claim.TimezoneKind = "omitted"
		return claim, nil
	case zone == "Z" || zone == "UTC":
		zero := 0
		claim.TimezoneKind = "utc"
		claim.OffsetSeconds = &zero
		claim.UTCKey = utcKey(civil, 0)
		if claim.UTCKey == nil {
			return claim, blockingTimeDiagnostic("utc_key_out_of_range", "normalized UTC year is outside 0001 through 9999")
		}
		return claim, nil
	case offsetPattern.MatchString(zone):
		offset, ok := parseOffset(zone)
		if !ok {
			claim.TimezoneKind = "invalid"
			return claim, blockingTimeDiagnostic("invalid_timezone", "UTC offset is outside the supported range")
		}
		claim.TimezoneKind = "offset"
		claim.OffsetSeconds = &offset
		claim.UTCKey = utcKey(civil, offset)
		if claim.UTCKey == nil {
			return claim, blockingTimeDiagnostic("utc_key_out_of_range", "normalized UTC year is outside 0001 through 9999")
		}
		return claim, nil
	case validIANAName(zone):
		claim.TimezoneKind = "named"
		return claim, []Diagnostic{{
			Code:     "named_zone_unresolved",
			Severity: "warning",
			Detail:   "named timezone remains floating until a pinned timezone database and parser recipe are available",
		}}
	default:
		claim.TimezoneKind = "invalid"
		return claim, blockingTimeDiagnostic("invalid_timezone", "declared timezone is neither UTC, an offset, nor an IANA-style name")
	}
}

func civilDateValue(civil civilTime) string {
	switch civil.precision {
	case "year":
		return fmt.Sprintf("%04d", civil.year)
	case "month":
		return fmt.Sprintf("%04d-%02d", civil.year, civil.month)
	case "date":
		return fmt.Sprintf("%04d-%02d-%02d", civil.year, civil.month, civil.day)
	case "hour":
		return fmt.Sprintf("%04d-%02d-%02dT%02d", civil.year, civil.month, civil.day, civil.hour)
	case "minute":
		return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d", civil.year, civil.month, civil.day, civil.hour, civil.minute)
	case "second":
		return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d", civil.year, civil.month, civil.day, civil.hour, civil.minute, civil.second)
	case "fraction":
		return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d.%s", civil.year, civil.month, civil.day, civil.hour, civil.minute, civil.second, civil.fraction)
	default:
		return ""
	}
}

func utcKey(civil civilTime, offsetSeconds int) *string {
	zone := time.FixedZone("declared", offsetSeconds)
	instant := time.Date(civil.year, time.Month(civil.month), civil.day, civil.hour, civil.minute, civil.second, civil.nanosecond, zone).UTC()
	if instant.Year() < 1 || instant.Year() > 9999 {
		return nil
	}
	value := instant.Format("2006-01-02T15:04:05.000000000Z")
	return &value
}

func parseOffset(raw string) (int, bool) {
	match := offsetPattern.FindStringSubmatch(raw)
	if match == nil {
		return 0, false
	}
	hour, minute := decimal(match[2]), decimal(match[3])
	if minute > 59 || hour > 14 || hour == 14 && minute != 0 {
		return 0, false
	}
	offset := hour*60*60 + minute*60
	if match[1] == "-" {
		offset = -offset
	}
	return offset, true
}

func validIANAName(raw string) bool {
	if !ianaNamePattern.MatchString(raw) {
		return false
	}
	for segment := range strings.SplitSeq(raw, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func validMonth(year, month int) bool {
	return year > 0 && month >= 1 && month <= 12
}

func validDate(civil civilTime) bool {
	if !validMonth(civil.year, civil.month) || civil.day < 1 {
		return false
	}
	value := time.Date(civil.year, time.Month(civil.month), civil.day, 0, 0, 0, 0, time.UTC)
	return value.Year() == civil.year && int(value.Month()) == civil.month && value.Day() == civil.day
}

func decimal(raw string) int {
	value, _ := strconv.Atoi(raw)
	return value
}

func ambiguousTimeDiagnostic(detail string) []Diagnostic {
	return []Diagnostic{{Code: "package_mapping_ambiguous", Severity: "blocking", Detail: detail}}
}

func blockingTimeDiagnostic(code, detail string) []Diagnostic {
	return []Diagnostic{{Code: code, Severity: "blocking", Detail: detail}}
}

func declaredDateLayout(format string) string {
	switch format {
	case "DD/MM/YYYY":
		return "02/01/2006"
	case "MM/DD/YYYY":
		return "01/02/2006"
	case "YYYY-MM-DD":
		return "2006-01-02"
	case "YYYYMMDD":
		return "20060102"
	default:
		return ""
	}
}
