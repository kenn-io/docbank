package store

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
)

// SQL reads these structural fields without decoding unrelated provider text.
// The complete profile and receipt remain unchanged in their evidence columns.
func processingProfileBindingsJSON(raw []byte) (string, error) {
	var profile document.ProcessingProfileV1
	if err := json.Unmarshal(raw, &profile); err != nil {
		return "", err
	}
	type descriptor struct {
		Fingerprint string `json:"fingerprint"`
	}
	type binding struct {
		Name       string                      `json:"name"`
		Kind       document.EmbeddingInputKind `json:"input_kind"`
		Descriptor descriptor                  `json:"descriptor"`
	}
	entries := make([]binding, len(profile.Embeddings))
	for i, b := range profile.Embeddings {
		entries[i] = binding{b.Name, b.InputKind, descriptor{b.Descriptor.Fingerprint}}
	}
	encoded, err := json.Marshal(entries)
	return string(encoded), err
}

func savedQueryPhotoScope(kind string, payload []byte) (int, error) {
	if kind != SavedQueryKindQuery {
		return 0, nil
	}
	value, err := query.Parse(payload)
	if err != nil {
		return 0, err
	}
	if len(value.Filters.Kinds) == 0 {
		return 0, nil
	}
	for _, kind := range value.Filters.Kinds {
		if kind != "photo" && kind != "video" {
			return 0, nil
		}
	}
	return 1, nil
}

// Only worker-envelope fields participate in package progress and retention.
// Other receipt bytes are opaque and have no such query fields.
func packageImportQueryJSON(raw []byte) (string, error) {
	query := struct {
		HasGaps  bool     `json:"has_gaps"`
		Gaps     []any    `json:"gaps"`
		Versions []string `json:"versions"`
	}{Gaps: []any{}, Versions: []string{}}
	var envelope map[string]jsontext.Value
	if json.Unmarshal(raw, &envelope) == nil {
		query.HasGaps = bytes.Contains(raw, []byte(`"gaps":[`)) && !bytes.Contains(raw, []byte(`"gaps":[]`))
		var gaps []jsontext.Value
		if json.Unmarshal(envelope["gaps"], &gaps) == nil {
			for _, gap := range gaps {
				var text string
				if json.Unmarshal(gap, &text) == nil {
					query.Gaps = append(query.Gaps, []byte(text))
				} else if bytes.Equal(gap, []byte("null")) {
					query.Gaps = append(query.Gaps, nil)
				} else {
					query.Gaps = append(query.Gaps, []byte(gap))
				}
			}
		}
		var member map[string]jsontext.Value
		if json.Unmarshal(envelope["member"], &member) == nil {
			var reps []struct {
				Version string `json:"content_version_id"`
			}
			if json.Unmarshal(member["representations"], &reps) == nil {
				for _, rep := range reps {
					if validateUUIDv4(rep.Version) == nil {
						query.Versions = append(query.Versions, rep.Version)
					}
				}
			}
		}
	}
	encoded, err := json.Marshal(query)
	return string(encoded), err
}
