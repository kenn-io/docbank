package cohereapi

import "math"

const MaxUsageValue = float64(1 << 50)

type Metadata struct {
	APIVersion *struct {
		Version        string `json:"version"`
		IsDeprecated   *bool  `json:"is_deprecated,omitempty"`
		IsExperimental *bool  `json:"is_experimental,omitempty"`
	} `json:"api_version,omitempty"`
	BilledUnits *struct {
		Images          *float64 `json:"images,omitempty"`
		InputTokens     *float64 `json:"input_tokens,omitempty"`
		ImageTokens     *float64 `json:"image_tokens,omitempty"`
		OutputTokens    *float64 `json:"output_tokens,omitempty"`
		SearchUnits     *float64 `json:"search_units,omitempty"`
		Classifications *float64 `json:"classifications,omitempty"`
		Pages           *float64 `json:"pages,omitempty"`
	} `json:"billed_units,omitempty"`
	Tokens *struct {
		InputTokens  *float64 `json:"input_tokens,omitempty"`
		OutputTokens *float64 `json:"output_tokens,omitempty"`
	} `json:"tokens,omitempty"`
	CachedTokens *float64 `json:"cached_tokens,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
}

func (metadata *Metadata) Valid(expectedImages int) bool {
	if metadata == nil {
		return true
	}
	if metadata.APIVersion != nil && metadata.APIVersion.Version == "" {
		return false
	}
	values := []*float64{}
	if metadata.BilledUnits != nil {
		values = append(values, metadata.BilledUnits.Images, metadata.BilledUnits.InputTokens,
			metadata.BilledUnits.ImageTokens, metadata.BilledUnits.OutputTokens,
			metadata.BilledUnits.SearchUnits, metadata.BilledUnits.Classifications, metadata.BilledUnits.Pages)
	}
	if metadata.BilledUnits != nil && metadata.BilledUnits.Images != nil && *metadata.BilledUnits.Images != float64(expectedImages) {
		return false
	}
	if expectedImages == 0 && metadata.BilledUnits != nil && metadata.BilledUnits.ImageTokens != nil &&
		*metadata.BilledUnits.ImageTokens != 0 {
		return false
	}
	if metadata.Tokens != nil {
		values = append(values, metadata.Tokens.InputTokens, metadata.Tokens.OutputTokens)
	}
	values = append(values, metadata.CachedTokens)
	for _, value := range values {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > MaxUsageValue) {
			return false
		}
	}
	return true
}
