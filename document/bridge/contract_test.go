package bridge

import (
	"bytes"
	_ "embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	unitKinds, ok := contractObject(t, schema, "properties", "unit_kind")["enum"].([]any)
	require.True(t, ok)
	assert.NotContains(t, unitKinds, "time_range")
	locatorKinds, ok := contractObject(t, schema, "$defs", "locator", "properties", "kind")["enum"].([]any)
	require.True(t, ok)
	assert.NotContains(t, locatorKinds, "time_range")
	assert.Equal(t, "#/$defs/locator",
		contractObject(t, schema, "$defs", "omission", "properties", "locator")["$ref"])
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
