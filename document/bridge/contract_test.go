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

	var schema map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(sourceEvidenceSchema, &schema,
		json.RejectUnknownMembers(true)))
	assert.Equal(t, `"https://json-schema.org/draft/2020-12/schema"`, string(schema["$schema"]))
	assert.Equal(t, `"object"`, string(schema["type"]))
	assert.Equal(t, "false", string(schema["additionalProperties"]))
	assert.True(t, bytes.Contains(sourceEvidenceSchema, []byte(`"const": "source-evidence/v1"`)))
	assert.True(t, jsontext.Value(sourceEvidenceSchema).IsValid())
}
