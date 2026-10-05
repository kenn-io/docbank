package query

import (
	"errors"
	"go.kenn.io/docbank/document"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// GPSBounds uses decimal strings so the integer-only QueryV1 number contract stays intact.
type GPSBounds struct {
	South string `json:"south"`
	West  string `json:"west"`
	North string `json:"north"`
	East  string `json:"east"`
}

var decimalCoordinate = regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]+)?$`)

func normalizeCoordinate(value string, limit float64) (string, error) {
	if len(value) > 64 || !decimalCoordinate.MatchString(value) {
		return "", errors.New("coordinate must be a bounded decimal string")
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || number < -limit || number > limit {
		return "", errors.New("coordinate outside geographic bounds")
	}
	sign := ""
	if strings.HasPrefix(value, "-") {
		sign = "-"
		value = value[1:]
	}
	whole, fraction, _ := strings.Cut(value, ".")
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	fraction = strings.TrimRight(fraction, "0")
	if whole == "0" && fraction == "" {
		sign = ""
	}
	if fraction != "" {
		whole += "." + fraction
	}
	return sign + whole, nil
}

// NormalizeGPSBounds validates a defensive copy and retains antimeridian boxes.
func NormalizeGPSBounds(value GPSBounds) (GPSBounds, error) {
	var err error
	for _, part := range []struct {
		field *string
		limit float64
	}{{&value.South, 90}, {&value.West, 180}, {&value.North, 90}, {&value.East, 180}} {
		*part.field, err = normalizeCoordinate(*part.field, part.limit)
		if err != nil {
			return GPSBounds{}, err
		}
	}
	south, _ := strconv.ParseFloat(value.South, 64)
	north, _ := strconv.ParseFloat(value.North, 64)
	if south > north {
		return GPSBounds{}, errors.New("south exceeds north")
	}
	return value, nil
}

// ParseGPSOperand validates the expression form through the typed bounds owner.
func ParseGPSOperand(value string) (GPSBounds, error) {
	parts := strings.Split(value, ",")
	if len(parts) != 4 {
		return GPSBounds{}, errors.New("GPS requires south,west,north,east")
	}
	return NormalizeGPSBounds(GPSBounds{South: parts[0], West: parts[1], North: parts[2], East: parts[3]})
}

// CaptureDateKey reads an exact calendar day as an inclusive axis boundary.
func CaptureDateKey(value string) (string, error) {
	parsed, err := time.Parse(time.DateOnly, value)
	if err != nil || parsed.Format(time.DateOnly) != value {
		return "", errors.New("capture date must be YYYY-MM-DD")
	}
	if parsed.Year() < 1 {
		return "", errors.New("capture date is outside axis domain")
	}
	return document.EventAxisKey(value, "date", "omitted", nil)
}

func validPhotoLabel(value string) bool {
	return value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= 256 && !strings.ContainsRune(value, 0)
}

func normalizePhotoFilters(value Filters) (Filters, error) {
	var err error
	for _, set := range []struct {
		values *[]string
		valid  func(string) bool
		name   string
	}{
		{&value.Kinds, func(v string) bool { return oneOf(v, "photo", "video") }, "kinds"}, {&value.Cameras, validPhotoLabel, "cameras"}, {&value.Lenses, validPhotoLabel, "lenses"}, {&value.AssetIDs, validUUIDv4, "asset_ids"},
	} {
		*set.values, err = normalizeSet(*set.values, maxIDValues, set.valid, set.name)
		if err != nil {
			return Filters{}, err
		}
	}
	for _, bound := range []*int64{value.ISOMin, value.ISOMax} {
		if bound != nil && !validSizeBound(*bound) {
			return Filters{}, errors.New("ISO must be a safe nonnegative integer")
		}
	}
	if value.ISOMin != nil {
		value.ISOMin = new(*value.ISOMin)
	}
	if value.ISOMax != nil {
		value.ISOMax = new(*value.ISOMax)
	}
	if value.ISOMin != nil && value.ISOMax != nil && *value.ISOMin > *value.ISOMax {
		return Filters{}, errors.New("iso_min exceeds iso_max")
	}
	for _, date := range []string{value.CaptureAfter, value.CaptureBefore} {
		if date != "" {
			if _, err := CaptureDateKey(date); err != nil {
				return Filters{}, err
			}
		}
	}
	if value.CaptureAfter != "" && value.CaptureBefore != "" && value.CaptureAfter >= value.CaptureBefore {
		return Filters{}, errors.New("capture_after must precede capture_before")
	}
	if value.GPSBounds != nil {
		bounds, err := NormalizeGPSBounds(*value.GPSBounds)
		if err != nil {
			return Filters{}, err
		}
		value.GPSBounds = &bounds
	}
	return value, nil
}

var capturePrecisionLayouts = map[string]string{
	"date": time.DateOnly, "hour": "2006-01-02T15", "minute": "2006-01-02T15:04",
	"second": "2006-01-02T15:04:05", "fraction": "2006-01-02T15:04:05.999999999",
}

// CaptureTimeKey derives ordering from readable source evidence; unreadable evidence has no key.
func CaptureTimeKey(normalized, precision, timezone, offset string) string {
	if normalized == "" {
		return ""
	}
	stamp := document.SourceMetadataTimestampV1{
		Raw: normalized, Normalized: normalized,
		Precision: document.SourceMetadataTimestampPrecision(precision),
		Timezone:  document.SourceMetadataTimezoneKind(timezone), Offset: offset,
	}
	if err := document.ValidateSourceMetadataTimestamp(stamp); err != nil {
		return ""
	}
	civil := normalized
	zone := document.EventTimezoneKind(timezone)
	var seconds *int
	layout := capturePrecisionLayouts[precision]
	if timezone == "utc" {
		civil = strings.TrimSuffix(civil, "Z")
	}
	if timezone == "offset" {
		civil = strings.TrimSuffix(civil, offset)
		hours, _ := strconv.Atoi(offset[1:3])
		minutes, _ := strconv.Atoi(offset[4:])
		n := (hours*60 + minutes) * 60
		if offset[0] == '-' {
			n = -n
		}
		seconds = &n
	}
	parsed, parseErr := time.Parse(layout, civil)
	if parseErr != nil {
		return ""
	}
	if parsed.Year() < 1 || parsed.Year() > 9999 {
		return ""
	}
	if seconds != nil {
		utc := parsed.Add(-time.Duration(*seconds) * time.Second)
		if utc.Year() < 1 || utc.Year() > 9999 {
			return ""
		}
	}
	if precision == "fraction" {
		layout = "2006-01-02T15:04:05.000000000"
	}
	key, keyErr := document.EventAxisKey(parsed.Format(layout), document.EventPrecision(precision), zone, seconds)
	if keyErr != nil {
		return ""
	}
	return key
}
