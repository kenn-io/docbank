package bridge

import (
	"bytes"
	_ "embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"gopkg.in/yaml.v3"
)

//go:embed openapi.yaml
var openAPIContract []byte

//go:embed source-evidence-v1.schema.json
var sourceEvidenceSchema []byte

func TestBridgeContractNormativeDocumentsAreStrictAndVersioned(t *testing.T) {
	var openAPI map[string]any
	require.NoError(t, yaml.Unmarshal(openAPIContract, &openAPI))
	assert.Equal(t, "3.1.0", openAPI["openapi"])
	paths, ok := openAPI["paths"].(map[string]any)
	require.True(t, ok)
	for _, route := range []string{
		jobsPath, jobsPath + "/{job_id}", jobsPath + "/{job_id}/artifacts/{artifact_id}",
	} {
		assert.Contains(t, paths, route)
	}
	assert.NotContains(t, string(openAPIContract), "{{",
		"the bridge contract must not define a template language")
	assert.NotContains(t, string(openAPIContract), "artifact_url",
		"artifacts are reachable only through the fixed job route")
	artifactContent := contractObject(t, openAPI, "paths",
		jobsPath+"/{job_id}/artifacts/{artifact_id}", "get", "responses", "200", "content")
	assert.Contains(t, artifactContent, "*/*")
	artifactPayload := contractObject(t, openAPI, "components", "schemas", "ArtifactPayload")
	artifactBranches, ok := artifactPayload["oneOf"].([]any)
	require.True(t, ok)
	require.Len(t, artifactBranches, 2)
	assert.Equal(t, "inline", contractObject(t, artifactBranches[0], "properties", "location")["const"])
	assert.Equal(t, "result", contractObject(t, artifactBranches[1], "properties", "location")["const"])
	manifest := contractObject(t, openAPI, "components", "schemas", "AuthorizationManifest")
	authorization := contractObject(t, manifest, "properties", "authorization")
	requiredAuthorization, ok := authorization["required"].([]any)
	require.True(t, ok)
	assert.Contains(t, requiredAuthorization, "disclose_filename")
	disclosure := contractObject(t, authorization, "properties", "disclose_filename")
	assert.Equal(t, "boolean", disclosure["type"])
	filename := contractObject(t, manifest, "properties", "source", "properties", "filename")
	assert.NotContains(t, filename, "minLength")

	conditions, ok := manifest["allOf"].([]any)
	require.True(t, ok)
	require.Len(t, conditions, 1)
	condition, ok := conditions[0].(map[string]any)
	require.True(t, ok)
	disclosed := contractObject(t, condition,
		"if", "properties", "authorization", "properties", "disclose_filename")
	assert.Equal(t, true, disclosed["const"])
	assert.Equal(t, 1, contractObject(t, condition,
		"then", "properties", "source", "properties", "filename")["minLength"])
	assert.Equal(t, 0, contractObject(t, condition,
		"else", "properties", "source", "properties", "filename")["maxLength"])

	var rawSchema map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(sourceEvidenceSchema, &rawSchema,
		json.RejectUnknownMembers(true)))
	assert.Equal(t, `"https://json-schema.org/draft/2020-12/schema"`, string(rawSchema["$schema"]))
	assert.Equal(t, `"object"`, string(rawSchema["type"]))
	assert.Equal(t, "false", string(rawSchema["additionalProperties"]))
	assert.True(t, bytes.Contains(sourceEvidenceSchema, []byte(`"const": "source-evidence/v1"`)))
	assert.True(t, jsontext.Value(sourceEvidenceSchema).IsValid())

	var schema map[string]any
	require.NoError(t, json.Unmarshal(sourceEvidenceSchema, &schema))
	description, ok := schema["description"].(string)
	require.True(t, ok)
	assert.Contains(t, description, "document.ValidateSourceEvidenceV1")
	unitKinds, ok := contractObject(t, schema, "properties", "unit_kind")["enum"].([]any)
	require.True(t, ok)
	assert.NotContains(t, unitKinds, "time_range")
	locatorKinds, ok := contractObject(t, schema, "$defs", "locator", "properties", "kind")["enum"].([]any)
	require.True(t, ok)
	assert.NotContains(t, locatorKinds, "time_range")
	assert.Equal(t, "#/$defs/locator",
		contractObject(t, schema, "$defs", "omission", "properties", "locator")["$ref"])
}

func TestBridgeContractNormativeDocumentsSourceEvidenceBoundsMatchValidator(t *testing.T) {
	var schema map[string]any
	require.NoError(t, json.Unmarshal(sourceEvidenceSchema, &schema))

	tests := []struct {
		name     string
		path     []string
		wantErr  string
		setItems func(*document.SourceEvidenceV1, int)
	}{
		{
			name: "heading_path", path: []string{"$defs", "unit", "properties", "heading_path"},
			wantErr: "heading depth",
			setItems: func(source *document.SourceEvidenceV1, count int) {
				source.Units[0].HeadingPath = make([]string, count)
				for index := range source.Units[0].HeadingPath {
					source.Units[0].HeadingPath[index] = "heading"
				}
			},
		},
		{
			name: "geometry boxes", path: []string{"$defs", "geometry", "properties", "boxes"},
			wantErr: "too many boxes",
			setItems: func(source *document.SourceEvidenceV1, count int) {
				geometry := source.Units[0].Regions[0].Geometry
				geometry.Boxes = make([]document.EvidenceBoxV1, count)
				for index := range geometry.Boxes {
					geometry.Boxes[index] = document.EvidenceBoxV1{Left: 1, Top: 1, Right: 2, Bottom: 2}
				}
			},
		},
		{
			name: "geometry polygons", path: []string{"$defs", "geometry", "properties", "polygons"},
			wantErr: "too many polygons",
			setItems: func(source *document.SourceEvidenceV1, count int) {
				geometry := source.Units[0].Regions[0].Geometry
				geometry.Polygons = make([]document.EvidencePolygonV1, count)
				for index := range geometry.Polygons {
					geometry.Polygons[index] = document.EvidencePolygonV1{Points: []document.EvidencePointV1{
						{X: 1, Y: 1}, {X: 2, Y: 1}, {X: 1, Y: 2},
					}}
				}
			},
		},
		{
			name: "polygon points", path: []string{"$defs", "polygon", "properties", "points"},
			wantErr: "too many polygon points",
			setItems: func(source *document.SourceEvidenceV1, count int) {
				source.Units[0].Regions[0].Geometry.Polygons = []document.EvidencePolygonV1{{
					Points: make([]document.EvidencePointV1, count),
				}}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limit := contractMaxItems(t, schema, test.path...)

			for _, boundary := range []struct {
				name    string
				count   int
				wantErr bool
			}{
				{name: "at limit", count: limit},
				{name: "over limit", count: limit + 1, wantErr: true},
			} {
				t.Run(boundary.name, func(t *testing.T) {
					source := contractSourceEvidence()
					test.setItems(&source, boundary.count)
					err := document.ValidateSourceEvidenceV1(source)
					if boundary.wantErr {
						require.ErrorContains(t, err, test.wantErr)
						return
					}
					require.NoError(t, err)
				})
			}
		})
	}
}

func TestBridgeContractNormativeDocumentsScalarBoundsMatchValidator(t *testing.T) {
	var schema map[string]any
	require.NoError(t, json.Unmarshal(sourceEvidenceSchema, &schema))
	type scalarBound struct {
		definition string
		field      string
		wantErr    string
		set        func(*document.SourceEvidenceV1, float64)
	}
	tests := []scalarBound{
		{"geometry", "width", "geometry frame", func(s *document.SourceEvidenceV1, n float64) { s.Units[0].Regions[0].Geometry.Width = int64(n) }},
		{"geometry", "height", "geometry frame", func(s *document.SourceEvidenceV1, n float64) { s.Units[0].Regions[0].Geometry.Height = int64(n) }},
		{"geometry", "scale", "geometry frame", func(s *document.SourceEvidenceV1, n float64) { s.Units[0].Regions[0].Geometry.Scale = int64(n) }},
		{"table", "rows", "invalid dimensions", func(s *document.SourceEvidenceV1, n float64) {
			s.Units[0].Tables = []document.SourceEvidenceTableV1{{ProviderID: "table", Rows: int(n), Columns: 1, Cells: []document.SourceEvidenceTableCellV1{}}}
		}},
		{"table", "columns", "invalid dimensions", func(s *document.SourceEvidenceV1, n float64) {
			s.Units[0].Tables = []document.SourceEvidenceTableV1{{ProviderID: "table", Rows: 1, Columns: int(n), Cells: []document.SourceEvidenceTableCellV1{}}}
		}},
		{"locator", "end", "invalid range", func(s *document.SourceEvidenceV1, n float64) {
			s.Family = "text"
			s.UnitKind = document.EvidenceUnitLine
			s.Units[0].Locator.Kind = document.EvidenceLocatorLine
			s.Units[0].Locator.IndexOrigin = document.EvidenceIndexOriginZero
			s.Units[0].Locator.Start = 0
			s.Units[0].Locator.End = int64(n)
		}},
		{"locator", "start", "invalid range", func(s *document.SourceEvidenceV1, n float64) {
			s.Completeness = document.EvidencePartial
			s.Units[0].Locator.Start = int64(n)
			s.Units[0].Locator.End = int64(n)
			s.Omissions = []document.SourceEvidenceOmissionV1{{Kind: document.EvidenceOmissionUnit, Reason: "omitted pages", Locator: &document.SourceEvidenceLocatorV1{
				Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginZero, Start: 0, End: max(0, int64(n)-1),
			}}}
			s.Units[0].Locator.IndexOrigin = document.EvidenceIndexOriginZero
			if n == 0 {
				s.Completeness = document.EvidenceComplete
				s.Omissions = nil
			}
		}},
	}
	for _, field := range []string{"minimum", "maximum", "value"} {
		tests = append(tests, scalarBound{"confidence", field, "confidence is invalid", func(s *document.SourceEvidenceV1, n float64) {
			c := &document.SourceEvidenceConfidenceV1{Interpretation: document.EvidenceConfidenceHigherIsBetter, Minimum: -1000000, Maximum: 1000000}
			switch field {
			case "minimum":
				c.Minimum = n
				c.Value = n
			case "maximum":
				c.Maximum = n
				c.Value = n
			case "value":
				c.Value = n
			}
			s.Units[0].Confidence = c
		}})
	}
	for _, test := range tests {
		t.Run(test.definition+"/"+test.field, func(t *testing.T) {
			field := contractObject(t, schema, "$defs", test.definition, "properties", test.field)
			for _, bound := range []string{"minimum", "maximum"} {
				limit, ok := field[bound].(float64)
				require.True(t, ok, "missing %s", bound)
				// Confidence scales need distinct endpoints, so the minimum cannot
				// equal the largest endpoint, nor the maximum the smallest.
				invalidEndpoint := test.definition == "confidence" &&
					(test.field == "minimum" && bound == "maximum" || test.field == "maximum" && bound == "minimum")
				t.Run(bound, func(t *testing.T) {
					source := contractSourceEvidence()
					test.set(&source, limit)
					if invalidEndpoint {
						require.ErrorContains(t, document.ValidateSourceEvidenceV1(source), test.wantErr)
					} else {
						require.NoError(t, document.ValidateSourceEvidenceV1(source))
					}
					step := 1.0
					if bound == "minimum" {
						step = -1
					}
					test.set(&source, limit+step)
					require.ErrorContains(t, document.ValidateSourceEvidenceV1(source), test.wantErr)
				})
			}
		})
	}
}

func TestBridgeContractNormativeDocumentsAggregateAndByteLimits(t *testing.T) {
	var schema map[string]any
	require.NoError(t, json.Unmarshal(sourceEvidenceSchema, &schema))
	t.Run("polygon point budget spans polygons", func(t *testing.T) {
		limit := contractMaxItems(t, schema, "$defs", "polygon", "properties", "points")
		source := contractSourceEvidence()
		geometry := source.Units[0].Regions[0].Geometry
		geometry.Polygons = []document.EvidencePolygonV1{
			{Points: make([]document.EvidencePointV1, limit/2)},
			{Points: make([]document.EvidencePointV1, limit-limit/2)},
		}
		require.NoError(t, document.ValidateSourceEvidenceV1(source))
		geometry.Polygons[1].Points = append(geometry.Polygons[1].Points, document.EvidencePointV1{})
		require.ErrorContains(t, document.ValidateSourceEvidenceV1(source), "too many polygon points")
	})
	t.Run("heading byte budget spans units", func(t *testing.T) {
		value, ok := contractObject(t, schema, "$defs", "unit", "properties", "heading_path", "items")["maxLength"].(float64)
		require.True(t, ok)
		limit := int(value)
		source := contractSourceEvidence()
		source.Units[0].HeadingPath = []string{strings.Repeat("h", limit)}
		require.NoError(t, document.ValidateSourceEvidenceV1(source))
		source.Units[0].HeadingPath[0] += "h"
		require.ErrorContains(t, document.ValidateSourceEvidenceV1(source), "heading bytes")
		source.Units[0].HeadingPath[0] = strings.Repeat("h", limit/2)
		second := source.Units[0]
		second.Order = 1
		second.Locator.Start, second.Locator.End = 2, 2
		second.HeadingPath = []string{strings.Repeat("h", limit-limit/2)}
		source.Units = append(source.Units, second)
		require.NoError(t, document.ValidateSourceEvidenceV1(source))
		source.Units[1].HeadingPath[0] += "h"
		require.ErrorContains(t, document.ValidateSourceEvidenceV1(source), "heading bytes")
	})
	t.Run("identifier limit counts UTF-8 bytes", func(t *testing.T) {
		value, ok := contractObject(t, schema, "$defs", "region", "properties", "provider_id")["maxLength"].(float64)
		require.True(t, ok)
		limit := int(value)
		source := contractSourceEvidence()
		source.Units[0].Regions[0].ProviderID = strings.Repeat("界", limit/3) + strings.Repeat("x", limit%3)
		require.NoError(t, document.ValidateSourceEvidenceV1(source))
		source.Units[0].Regions[0].ProviderID += "x"
		require.ErrorContains(t, document.ValidateSourceEvidenceV1(source), "provider ID")
	})
}

func contractMaxItems(t *testing.T, schema map[string]any, path ...string) int {
	t.Helper()
	value, ok := contractObject(t, schema, path...)["maxItems"].(float64)
	require.True(t, ok, "contract path %v has no numeric maxItems", path)
	return int(value)
}

func contractSourceEvidence() document.SourceEvidenceV1 {
	return document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1,
		Completeness:    document.EvidenceComplete,
		Family:          "pdf",
		UnitKind:        document.EvidenceUnitPage,
		Units: []document.SourceEvidenceUnitV1{{
			Order: 0,
			Locator: document.SourceEvidenceLocatorV1{
				Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne,
				Start: 1, End: 1,
			},
			Regions: []document.SourceEvidenceRegionV1{{
				Kind: document.EvidenceRegionParagraph, Order: 0,
				ProviderID: "synthetic-region",
				TextRange:  document.EvidenceTextRangeV1{Start: 0, End: 1},
				Geometry: &document.SourceEvidenceGeometryV1{
					CoordinateOrigin: document.EvidenceCoordinateTopLeft,
					CoordinateSpace:  document.EvidenceCoordinatePage,
					Height:           100,
					Orientation:      0,
					Scale:            1,
					Unit:             document.EvidenceGeometryPixel,
					Width:            100,
				},
			}},
			Text: "synthetic evidence",
		}},
	}
}

func contractObject(t *testing.T, value any, path ...string) map[string]any {
	t.Helper()
	current := value
	for _, key := range path {
		object, ok := current.(map[string]any)
		require.True(t, ok, "contract path %v is not an object", path)
		current, ok = object[key]
		require.True(t, ok, "contract path %v lacks %q", path, key)
	}
	object, ok := current.(map[string]any)
	require.True(t, ok, "contract path %v is not an object", path)
	return object
}
