package production

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSyntheticFixturesAreExactCanonicalPayloads(t *testing.T) {
	tests := []struct {
		name   string
		file   string
		digest string
		encode func([]byte) ([]byte, string, error)
	}{
		{"policy", "testdata/policy-v1.json", "6d79fee9baf14f7d3ad848a390c22f563f8b834920e2ab2fcdb7206320c9ca16", canonicalFixture[PolicyVersion](CanonicalPolicyVersion)},
		{"approval subject", "testdata/approval-subject-v1.json", "0a71843bb0d9a6fc84125a3657f57b1e00e4d9318f257abbad8294ed2a1a4e54", canonicalFixture[ApprovalSubject](CanonicalApprovalSubject)},
		{"players snapshot", "testdata/players-snapshot-v1.json", "7e9d9c1b9e21796793103bcbde76df8f8985fba707d0d6ea4d6d7c1c3fd48528", canonicalFixture[PlayersSnapshot](CanonicalPlayersSnapshot)},
		{"privilege log inputs", "testdata/privilege-log-inputs-v1.json", "b40059bae1ec5998b9ae6b6b5af0e6fd54a5102e3484e00705b605d888f67bb4", canonicalFixture[PrivilegeLogInputs](canonicalPrivilegeLogInputsFixture)},
		{"privilege log receipt", "testdata/privilege-log-receipt-v1.json", "2e8659868755c70ec1a5cd102879e3a9c154ea55c00f14eec665901ea4442461", canonicalFixture[PrivilegeLogReceipt](CanonicalPrivilegeLogReceipt)},
		{"artifact provenance", "testdata/artifact-provenance-v1.json", "215465619861c902b52224ee828a11ebf665d12b6018d0b602ce336840022ecd", canonicalFixture[ArtifactProvenanceReceipt](CanonicalArtifactProvenanceReceipt)},
		{"retention receipt", "testdata/retention-receipt-v1.json", "afec6b3154a10c3e625f2db5afcf9f74a5556fe71f5edc1e2e12da7dea2d6f5f", canonicalFixture[RetentionReceipt](CanonicalRetentionReceipt)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := os.ReadFile(test.file)
			require.NoError(t, err)
			raw = bytes.TrimSuffix(raw, []byte{'\n'})
			raw = bytes.TrimSuffix(raw, []byte{'\r'})
			encoded, digest, err := test.encode(raw)
			require.NoError(t, err)
			require.Equal(t, raw, encoded)
			require.Equal(t, test.digest, digest)
		})
	}
}

func canonicalPrivilegeLogInputsFixture(value PrivilegeLogInputs) ([]byte, string, error) {
	return encodeDigest(value, "privilege log inputs fixture")
}

func canonicalFixture[T any](encode func(T) ([]byte, string, error)) func([]byte) ([]byte, string, error) {
	return func(raw []byte) ([]byte, string, error) {
		var value T
		if err := json.Unmarshal(raw, &value, json.RejectUnknownMembers(true)); err != nil {
			return nil, "", err
		}
		return encode(value)
	}
}
