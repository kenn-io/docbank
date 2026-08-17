package mistral

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/internal/compattest"
)

type requestFingerprintCompatibilityCase struct {
	FormatID    string          `json:"format_id"`
	Candidate   CandidateFormat `json:"candidate"`
	Options     requestOptions  `json:"options"`
	Fingerprint string          `json:"fingerprint"`
}

func TestRequestFingerprintMatchesBaseline(t *testing.T) {
	bundle, _, err := compattest.Load()
	require.NoError(t, err)
	section, ok := bundle.Sections["mistral_request_fingerprint_v2"]
	require.True(t, ok)
	assert.Equal(t, "go.kenn.io/docbank/document/mistral", section.Owner)

	var cases []requestFingerprintCompatibilityCase
	require.NoError(t, json.Unmarshal(section.Cases, &cases))
	require.Len(t, cases, len(candidateFormats))
	for _, testCase := range cases {
		t.Run(testCase.FormatID, func(t *testing.T) {
			candidate, found := CandidateFormatByID(testCase.FormatID)
			require.True(t, found)
			assert.Equal(t, testCase.Candidate, candidate)
			assert.Equal(t, testCase.Fingerprint, requestFingerprint(testCase.Candidate, testCase.Options))
		})
	}
}
