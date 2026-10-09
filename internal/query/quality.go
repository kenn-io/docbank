package query

import (
	"errors"
	"strings"
)

var errQualityScore = errors.New("quality score must be a decimal string within 0..1")

var qualityFields = withQualityFields(map[string]struct{}{})

func withQualityFields(fields map[string]struct{}) map[string]struct{} {
	for _, bound := range QualityBounds(Filters{}) {
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
	for _, pair := range [][2]**string{
		{&value.FocusMin, &value.FocusMax},
		{&value.BlurMin, &value.BlurMax},
		{&value.BrightnessMin, &value.BrightnessMax},
		{&value.FramingMin, &value.FramingMax},
		{&value.AestheticsMin, &value.AestheticsMax},
		{&value.ColorRedMin, &value.ColorRedMax},
		{&value.ColorGreenMin, &value.ColorGreenMax},
		{&value.ColorBlueMin, &value.ColorBlueMax},
	} {
		for _, bound := range pair {
			if *bound != nil {
				normalized, err := NormalizeQualityOperand(**bound)
				if err != nil {
					return Filters{}, err
				}
				*bound = new(normalized)
			}
		}
		if *pair[0] != nil && *pair[1] != nil {
			if **pair[0] > **pair[1] {
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
	return []struct {
		Field string
		Value *string
	}{
		{"focus_min", value.FocusMin},
		{"focus_max", value.FocusMax},
		{"blur_min", value.BlurMin},
		{"blur_max", value.BlurMax},
		{"brightness_min", value.BrightnessMin},
		{"brightness_max", value.BrightnessMax},
		{"framing_min", value.FramingMin},
		{"framing_max", value.FramingMax},
		{"aesthetics_min", value.AestheticsMin},
		{"aesthetics_max", value.AestheticsMax},
		{"color_red_min", value.ColorRedMin},
		{"color_red_max", value.ColorRedMax},
		{"color_green_min", value.ColorGreenMin},
		{"color_green_max", value.ColorGreenMax},
		{"color_blue_min", value.ColorBlueMin},
		{"color_blue_max", value.ColorBlueMax},
	}
}
