package document

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

func eventCivil(value string, precision EventPrecision) (time.Time, error) {
	layouts := map[EventPrecision]string{
		"year":   "2006",
		"month":  "2006-01",
		"date":   "2006-01-02",
		"hour":   "2006-01-02T15",
		"minute": "2006-01-02T15:04",
		"second": "2006-01-02T15:04:05",
	}
	layout, ok := layouts[precision]
	if precision == "fraction" {
		dot := strings.LastIndexByte(value, '.')
		digits := len(value) - dot - 1
		if dot != 19 || digits < 1 || digits > 9 {
			return time.Time{}, errors.New("invalid fractional precision")
		}
		layout = "2006-01-02T15:04:05." + strings.Repeat("0", digits)
		ok = true
	}
	parsed, err := time.Parse(layout, value)
	if !ok || err != nil || parsed.Year() < 1 || parsed.Year() > 9999 || parsed.Format(layout) != value {
		return time.Time{}, errors.New("date value is not a valid civil calendar value")
	}
	return parsed, nil
}

func EventUTCKey(
	value string,
	precision EventPrecision,
	zone EventTimezoneKind,
	offsetSeconds *int,
) (string, bool, error) {
	civil, err := eventCivil(value, precision)
	if err != nil {
		return "", false, err
	}
	if !ValidEventTimezoneKind(zone) {
		return "", false, errors.New("invalid timezone kind")
	}
	switch zone {
	case "offset":
		if offsetSeconds == nil || *offsetSeconds < -86399 || *offsetSeconds > 86399 {
			return "", false, errors.New("invalid offset")
		}
	case "utc":
		if offsetSeconds != nil && *offsetSeconds != 0 {
			return "", false, errors.New("UTC offset is not zero")
		}
	default:
		if offsetSeconds != nil {
			return "", false, errors.New("floating timezone kind cannot carry an offset")
		}
	}

	if precision == "year" || precision == "month" || precision == "date" ||
		(zone != "utc" && zone != "offset") {
		return "", false, nil
	}
	if zone == "offset" {
		civil = civil.Add(-time.Duration(*offsetSeconds) * time.Second)
	}
	if civil.Year() < 1 || civil.Year() > 9999 {
		return "", false, errors.New("UTC key outside four-digit year range")
	}
	return civil.Format("2006-01-02T15:04:05.000000000Z"), true, nil
}

func EventAxisKey(
	value string,
	precision EventPrecision,
	zone EventTimezoneKind,
	offsetSeconds *int,
) (string, error) {
	key, ok, err := EventUTCKey(value, precision, zone, offsetSeconds)
	if err != nil {
		return "", err
	}
	if ok {
		return strings.TrimSuffix(key, "Z"), nil
	}
	civil, err := eventCivil(value, precision)
	if err != nil {
		return "", err
	}
	return civil.Format("2006-01-02T15:04:05.000000000"), nil
}

func eventIdentity(fields ...string) string {
	hasher := sha256.New()
	for _, field := range fields {
		_, _ = fmt.Fprintf(hasher, "%d:%s", len(field), field)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func DocumentEventID(
	vaultUID, contentVersionID, sourceKey string,
	kind DateKind,
	dateValue string,
	precision EventPrecision,
	zone EventTimezoneKind,
	evidenceSHA256 string,
) string {
	return eventIdentity(vaultUID, contentVersionID, sourceKey, string(kind), dateValue,
		string(precision), string(zone), evidenceSHA256)
}

func DocumentEventGenerationID(
	vaultUID, contentVersionID, contractVersion, deriverFingerprint, inputsSHA256 string,
) string {
	return eventIdentity(vaultUID, contentVersionID, contractVersion, deriverFingerprint, inputsSHA256)
}
