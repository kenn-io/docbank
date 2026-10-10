package query

import (
	"errors"
	"strings"
)

var errQualityScore = errors.New("quality score must be a decimal string within 0..1")

var qualityFields = withQualityFields(map[string]struct{}{})

func withQualityFields(fields map[string]struct{}) map[string]struct{} {
	for _, bound := range qualityFilterBounds(&Filters{}) {
		fields[bound.Field] = struct{}{}
	}
	fields["unevaluated"] = struct{}{}
	return fields
}

// IsQualityField identifies scalar quality bounds and the unevaluated filter.
func IsQualityField(field string) bool {
	_, ok := qualityFields[field]
	return ok
}

// NormalizeQualityOperand retains exact decimal identity on the 0..1 scale.
func NormalizeQualityOperand(value string) (string, error) {
	if strings.HasPrefix(value, "-") {
		return "", errQualityScore
	}
	normalized, err := normalizeCoordinate(value, 1)
	if err != nil || normalized != "0" && normalized != "1" && !strings.HasPrefix(normalized, "0.") {
		return "", errQualityScore
	}
	return normalized, nil
}

func normalizeQualityFilters(value Filters) (Filters, error) {
	bounds := qualityFilterBounds(&value)
	for i := 0; i < len(bounds); i += 2 {
		pair := bounds[i : i+2]
		for _, entry := range pair {
			bound := entry.Value
			if *bound != nil {
				normalized, err := NormalizeQualityOperand(**bound)
				if err != nil {
					return Filters{}, err
				}
				*bound = new(normalized)
			}
		}
		if *pair[0].Value != nil && *pair[1].Value != nil {
			if **pair[0].Value > **pair[1].Value {
				return Filters{}, errors.New("quality minimum exceeds maximum")
			}
		}
	}
	return value, nil
}

// QualityBounds exposes the fixed scalar fields to the SQL compiler.
func QualityBounds(value Filters) []struct {
	Field string
	Value *string
} {
	bounds := qualityFilterBounds(&value)
	result := make([]struct {
		Field string
		Value *string
	}, len(bounds))
	for i, bound := range bounds {
		result[i].Field, result[i].Value = bound.Field, *bound.Value
	}
	return result
}

func qualityFilterBounds(value *Filters) []struct {
	Field string
	Value **string
} {
	// Each minimum precedes its maximum so normalization can validate pairs.
	return []struct {
		Field string
		Value **string
	}{
		{"focus_min", &value.FocusMin},
		{"focus_max", &value.FocusMax},
		{"blur_min", &value.BlurMin},
		{"blur_max", &value.BlurMax},
		{"brightness_min", &value.BrightnessMin},
		{"brightness_max", &value.BrightnessMax},
		{"framing_min", &value.FramingMin},
		{"framing_max", &value.FramingMax},
		{"aesthetics_min", &value.AestheticsMin},
		{"aesthetics_max", &value.AestheticsMax},
		{"color_red_min", &value.ColorRedMin},
		{"color_red_max", &value.ColorRedMax},
		{"color_green_min", &value.ColorGreenMin},
		{"color_green_max", &value.ColorGreenMax},
		{"color_blue_min", &value.ColorBlueMin},
		{"color_blue_max", &value.ColorBlueMax},
	}
}
