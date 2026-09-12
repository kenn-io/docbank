package emailmime

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/docbank/document"
)

var emailDatePattern = regexp.MustCompile(`(?i)^(?:(Mon|Tue|Wed|Thu|Fri|Sat|Sun),[ \t]*)?([0-9]{1,2})[ \t]+(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[ \t]+([0-9]{4})[ \t]+([0-9]{2}):([0-9]{2})(?::([0-9]{2}))?(?:[ \t]+([^ \t]+))?$`)
var emailAlphaZonePattern = regexp.MustCompile(`^[A-Za-z]+$`)
var namedEmailZones = map[string]int{"UT": 0, "GMT": 0, "EST": -5 * 60, "EDT": -4 * 60, "CST": -6 * 60, "CDT": -5 * 60, "MST": -7 * 60, "MDT": -6 * 60, "PST": -8 * 60, "PDT": -7 * 60}
var emailMonths = map[string]time.Month{"jan": time.January, "feb": time.February, "mar": time.March, "apr": time.April, "may": time.May, "jun": time.June, "jul": time.July, "aug": time.August, "sep": time.September, "oct": time.October, "nov": time.November, "dec": time.December}

func (d *decoder) interpretDate(path string, fields []parsedHeader) document.EmailDateV1 {
	if len(fields) == 0 {
		return document.EmailDateV1{State: document.EmailDateMissing, Fields: []int{}, TimezoneState: document.EmailTimezoneMissing, Diagnostics: d.diagnostic(document.EmailDiagnosticDateMissing, document.EmailOperationDate, path, nil, "Date header is missing")}
	}
	indexes := headerIndexes(fields)
	if len(fields) > 1 {
		return document.EmailDateV1{State: document.EmailDateInvalid, Fields: indexes, TimezoneState: document.EmailTimezoneInvalid, Diagnostics: d.diagnostic(document.EmailDiagnosticDateAmbiguous, document.EmailOperationDate, path, nil, "multiple Date fields are ambiguous")}
	}
	m := emailDatePattern.FindStringSubmatch(strings.TrimSpace(fields[0].value))
	if m == nil {
		return d.invalidDate(path, indexes, "Date header syntax is invalid")
	}
	day, _ := strconv.Atoi(m[2])
	year, _ := strconv.Atoi(m[4])
	hour, _ := strconv.Atoi(m[5])
	minute, _ := strconv.Atoi(m[6])
	second := 0
	if m[7] != "" {
		second, _ = strconv.Atoi(m[7])
	}
	month := emailMonths[strings.ToLower(m[3])]
	if year < 1 || hour > 23 || minute > 59 || second > 60 {
		return d.invalidDate(path, indexes, "Date header components are invalid")
	}
	checkSecond := second
	if checkSecond == 60 {
		checkSecond = 59
	}
	civilTime := time.Date(year, month, day, hour, minute, checkSecond, 0, time.UTC)
	if civilTime.Year() != year || civilTime.Month() != month || civilTime.Day() != day {
		return d.invalidDate(path, indexes, "Date header calendar date is invalid")
	}
	if m[1] != "" && !strings.EqualFold(m[1], civilTime.Weekday().String()[:3]) {
		return d.invalidDate(path, indexes, "Date header weekday conflicts with its calendar date")
	}
	civil := fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d", year, month, day, hour, minute, second)
	zone := m[8]
	result := document.EmailDateV1{State: document.EmailDateParsed, Fields: indexes, Civil: &civil, TimezoneState: document.EmailTimezoneMissing, Diagnostics: []document.EmailDiagnosticV1{}}
	if zone == "" {
		if second == 60 {
			d.appendRequiredDateDiagnostic(&result, document.EmailDiagnosticLeapSecondInstantUnavailable, path, "leap-second instant is unavailable")
		}
		return result
	}
	result.TimezoneText = &zone
	minutes, zoneState, ok, originUnknown := parseEmailZone(zone)
	result.TimezoneState = zoneState
	if !ok {
		if zoneState == document.EmailTimezoneUnknownNamed {
			if !d.appendRequiredDateDiagnostic(&result, document.EmailDiagnosticTimezoneUnknown, path, "Date timezone is not in the fixed profile") {
				return result
			}
			if second == 60 {
				d.appendRequiredDateDiagnostic(&result, document.EmailDiagnosticLeapSecondInstantUnavailable, path, "leap-second instant is unavailable")
			}
			return result
		}
		return d.invalidDateWithZone(path, indexes, zone, "Date timezone is invalid")
	}
	if originUnknown {
		result.Diagnostics = append(result.Diagnostics, d.diagnostic(document.EmailDiagnosticTimezoneOriginUnknown, document.EmailOperationDate, path, nil, "-0000 records unknown timezone origin")...)
	}
	if second == 60 {
		d.appendRequiredDateDiagnostic(&result, document.EmailDiagnosticLeapSecondInstantUnavailable, path, "leap-second instant is unavailable")
		return result
	}
	utc := civilTime.Add(-time.Duration(minutes) * time.Minute).Format(time.RFC3339)
	result.UTC = &utc
	return result
}

func (d *decoder) appendRequiredDateDiagnostic(date *document.EmailDateV1, code document.EmailDiagnosticCode, path, detail string) bool {
	diagnostic := d.diagnostic(code, document.EmailOperationDate, path, nil, detail)
	if len(diagnostic) == 0 {
		d.diagnosticCount -= degradeEmailDate(date)
		return false
	}
	date.Diagnostics = append(date.Diagnostics, diagnostic...)
	return true
}

func degradeEmailDate(date *document.EmailDateV1) int {
	removed := 0
	for index, v := range slices.Backward(date.Diagnostics) {
		switch v.Code {
		case document.EmailDiagnosticTimezoneUnknown, document.EmailDiagnosticTimezoneOriginUnknown, document.EmailDiagnosticLeapSecondInstantUnavailable:
			removeDateDiagnosticAt(date, index)
			removed++
		default:
			continue
		}
	}
	date.State = document.EmailDateInvalid
	date.Civil = nil
	date.UTC = nil
	date.TimezoneText = nil
	date.TimezoneState = document.EmailTimezoneInvalid
	return removed
}

func removeDateDiagnosticAt(date *document.EmailDateV1, index int) {
	copy(date.Diagnostics[index:], date.Diagnostics[index+1:])
	date.Diagnostics = date.Diagnostics[:len(date.Diagnostics)-1]
}

func parseEmailZone(zone string) (int, document.EmailTimezoneState, bool, bool) {
	upper := strings.ToUpper(zone)
	if value, ok := namedEmailZones[upper]; ok {
		return value, document.EmailTimezoneKnownNamed, true, false
	}
	if len(zone) == 5 && (zone[0] == '+' || zone[0] == '-') {
		if !asciiDigits(zone[1:]) {
			return 0, document.EmailTimezoneInvalid, false, false
		}
		hours, herr := strconv.Atoi(zone[1:3])
		minutes, merr := strconv.Atoi(zone[3:])
		if herr != nil || merr != nil || hours > 23 || minutes > 59 {
			return 0, document.EmailTimezoneInvalid, false, false
		}
		offset := hours*60 + minutes
		if zone[0] == '-' {
			offset = -offset
		}
		return offset, document.EmailTimezoneNumeric, true, zone == "-0000"
	}
	if emailAlphaZonePattern.MatchString(zone) {
		return 0, document.EmailTimezoneUnknownNamed, false, false
	}
	return 0, document.EmailTimezoneInvalid, false, false
}

func asciiDigits(value string) bool {
	for _, char := range []byte(value) {
		if char < '0' || char > '9' {
			return false
		}
	}
	return value != ""
}
func (d *decoder) invalidDate(path string, indexes []int, detail string) document.EmailDateV1 {
	return document.EmailDateV1{State: document.EmailDateInvalid, Fields: indexes, TimezoneState: document.EmailTimezoneInvalid, Diagnostics: d.diagnostic(document.EmailDiagnosticDateInvalid, document.EmailOperationDate, path, nil, detail)}
}
func (d *decoder) invalidDateWithZone(path string, indexes []int, zone, detail string) document.EmailDateV1 {
	result := d.invalidDate(path, indexes, detail)
	result.TimezoneText = &zone
	return result
}
