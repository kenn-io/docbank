package glmocr_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/glmocr"
)

func TestDeploymentManifestMatchesPackageIdentity(t *testing.T) {
	encoded, err := os.ReadFile("../../deploy/glmocr/deployment.json")
	require.NoError(t, err)
	var manifest glmocr.DeploymentIdentity
	require.NoError(t, json.Unmarshal(encoded, &manifest))
	assert.Equal(t, glmocr.DefaultDeploymentIdentity(), manifest)

	canonical := jsontext.Value(encoded)
	require.NoError(t, canonical.Canonicalize())
	digest := sha256.Sum256(canonical)
	assert.Equal(t, glmocr.DefaultDeploymentFingerprint, hex.EncodeToString(digest[:]))
}

func TestPolicyFingerprintTracksArtifactsAndRequiresLoopback(t *testing.T) {
	normalize, err := document.NewNormalizePolicy(1_000_000)
	require.NoError(t, err)
	policy, err := glmocr.NewPolicy(glmocr.PolicyConfig{
		MaxDocumentBytes: 10 << 20, MaxResponseBytes: 20 << 20,
		MaxUnits: 20, NormalizePolicy: normalize,
	})
	require.NoError(t, err)

	assert.Len(t, policy.Fingerprint(), 64)
	assert.Equal(t, glmocr.DefaultModelRevision, policy.Identity().Revision)

	_, err = glmocr.NewPolicy(glmocr.PolicyConfig{
		Endpoint:         "http://192.0.2.10:30004/glmocr/parse",
		MaxDocumentBytes: 10 << 20, MaxResponseBytes: 20 << 20,
		MaxUnits: 20, NormalizePolicy: normalize,
	})
	require.ErrorContains(t, err, "loopback")

	_, err = glmocr.NewPolicy(glmocr.PolicyConfig{
		MaxDocumentBytes: 10 << 20, MaxResponseBytes: 20 << 20,
		MaxUnits: glmocr.MaxUnits + 1, NormalizePolicy: normalize,
	})
	require.ErrorContains(t, err, "bounds")
}
