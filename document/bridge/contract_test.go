package bridge

import (
	"bytes"
	_ "embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
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
		want     int
		setItems func(*document.SourceEvidenceV1, int)
	}{
		{
			name: "heading_path", path: []string{"$defs", "unit", "properties", "heading_path"},
			want: 64,
			setItems: func(source *document.SourceEvidenceV1, count int) {
				source.Units[0].HeadingPath = make([]string, count)
				for index := range source.Units[0].HeadingPath {
					source.Units[0].HeadingPath[index] = "heading"
				}
			},
		},
		{
			name: "geometry boxes", path: []string{"$defs", "geometry", "properties", "boxes"},
			want: 10_000,
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
			want: 10_000,
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
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, contractMaxItems(t, schema, test.path...))

			for _, boundary := range []struct {
				name    string
				count   int
				wantErr bool
			}{
				{name: "at limit", count: test.want},
				{name: "over limit", count: test.want + 1, wantErr: true},
			} {
				t.Run(boundary.name, func(t *testing.T) {
					source := contractSourceEvidence()
					test.setItems(&source, boundary.count)
					err := document.ValidateSourceEvidenceV1(source)
					if boundary.wantErr {
						require.Error(t, err)
						return
					}
					require.NoError(t, err)
				})
			}
		})
	}
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
