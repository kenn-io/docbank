package document

import (
	_ "embed"
	"encoding/json/v2"
	"fmt"
	"slices"
)

// FormatMetadata describes a declared media type and filename extensions.
// Provider marks entries that belong to the existing rendition format table;
// query-only entries never authorize document processing.
type FormatMetadata struct {
	ID          string   `json:"id"`
	Family      string   `json:"family"`
	QueryFamily string   `json:"query_family"`
	MediaType   string   `json:"media_type"`
	Extensions  []string `json:"extensions"`
	UnitKind    string   `json:"unit_kind"`
	Provider    bool     `json:"provider"`
}

//go:embed format_metadata.json
var formatMetadataJSON []byte

var formatMetadata = mustFormatMetadata()

// FormatMetadataCatalog returns a defensive copy of the document-owned
// metadata table used by provider detection and query media classification.
func FormatMetadataCatalog() []FormatMetadata {
	result := make([]FormatMetadata, len(formatMetadata))
	for index, entry := range formatMetadata {
		result[index] = entry
		result[index].Extensions = slices.Clone(entry.Extensions)
	}
	return result
}

func mustFormatMetadata() []FormatMetadata {
	var entries []FormatMetadata
	if err := json.Unmarshal(formatMetadataJSON, &entries, json.RejectUnknownMembers(true)); err != nil {
		panic(fmt.Sprintf("decode embedded format metadata: %v", err))
	}
	return entries
}
