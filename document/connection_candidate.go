package document

// SameVectorSpace admits a stored-vector comparison only when both selected
// bindings identify the same nonempty vector space. The fingerprint covers the
// model, revision, dimensions, metric, and model-input recipe.
func SameVectorSpace(source, candidate string) bool { return source != "" && source == candidate }

// LocateConnectionInput maps a sealed chunk span to the exact UTF-8 rendition
// body range for its evidence unit. It declines a mapping if the retained body
// does not contain precisely the generated input content at that range.
func LocateConnectionInput(body []byte, navigation []RenditionNavigationEntryV1,
	unitKeys []string, input GeneratedEmbeddingInput,
) (int, int, bool) {
	if input.Content == "" || input.SourceSpan.UnitIndex < 0 || input.SourceSpan.UnitIndex >= len(unitKeys) {
		return 0, 0, false
	}
	key := unitKeys[input.SourceSpan.UnitIndex]
	for i, entry := range navigation {
		if entry.Key != key {
			continue
		}
		limit := len(body)
		if i+1 < len(navigation) {
			limit = navigation[i+1].Byte
		}
		if entry.Byte < 0 || limit > len(body) || entry.Byte >= limit {
			return 0, 0, false
		}
		start, end, err := RuneRangeToPassageBytes(body[entry.Byte:limit],
			input.SourceSpan.CharStart, input.SourceSpan.CharEnd)
		if err != nil || string(body[entry.Byte+start:entry.Byte+end]) != input.Content {
			return 0, 0, false
		}
		return entry.Byte + start, entry.Byte + end, true
	}
	return 0, 0, false
}
