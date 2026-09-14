package mediatranscript

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestTimedArtifactRejectsDuplicatesAndPreservesSpeaker(t *testing.T) {
	a := ArtifactV1{
		ContractVersion: "media-transcript/v1",
		Origin:          "supplied",
		Provider:        "synthetic",
		Segments: []Segment{{
			Order: 0, StartMS: 0, EndMS: 1000, Speaker: "Speaker 1", Text: "synthetic cue",
		}},
	}
	raw, _, err := Marshal(a)
	require.NoError(t, err)
	b, err := Unmarshal(raw)
	require.NoError(t, err)
	require.Equal(t, a, b)

	_, err = Unmarshal([]byte(strings.Replace(string(raw),
		`"origin":"supplied"`, `"origin":"supplied","origin":"supplied"`, 1)))
	require.Error(t, err)

	a.Segments[0].EndMS = 0
	_, _, err = Marshal(a)
	require.Error(t, err)
}

func TestTimedArtifactCodecEnforcesCanonicalBounds(t *testing.T) {
	validArtifact := func() ArtifactV1 {
		return ArtifactV1{
			ContractVersion: "media-transcript/v1",
			Origin:          "generated",
			Provider:        "synthetic",
			ProviderVersion: "2026-09",
			Model:           "small",
			Language:        "en",
			Segments: []Segment{
				{Order: 0, StartMS: 0, EndMS: 1000, Text: "first"},
				{Order: 1, StartMS: 700, EndMS: 1500, Speaker: "Speaker 1", Text: "overlap"},
			},
		}
	}
	valid := validArtifact()
	raw, sum, err := Marshal(valid)
	require.NoError(t, err)
	require.Len(t, sum, 64)
	require.Equal(t, //nolint:testifylint // Exact canonical bytes are the codec contract.
		`{"contract_version":"media-transcript/v1","language":"en","model":"small","origin":"generated","provider":"synthetic","provider_version":"2026-09","segments":[{"end_ms":1000,"order":0,"start_ms":0,"text":"first"},{"end_ms":1500,"order":1,"speaker":"Speaker 1","start_ms":700,"text":"overlap"}]}`,
		string(raw),
	)

	invalidRaw := map[string][]byte{
		"unknown field":  []byte(string(raw[:len(raw)-1]) + `,"unexpected":true}`),
		"trailing bytes": append(append([]byte(nil), raw...), ' '),
		"trailing value": []byte(string(raw) + `{}`),
		"invalid span":   []byte(strings.Replace(string(raw), `"end_ms":1000`, `"end_ms":0`, 1)),
		"noncanonical":   []byte(`{"origin":"generated","contract_version":"media-transcript/v1","provider":"synthetic","segments":[{"order":0,"start_ms":0,"end_ms":1000,"text":"first"}]}`),
		"oversize":       []byte(strings.Repeat("x", MaxArtifactBytes+1)),
	}
	for name, candidate := range invalidRaw {
		t.Run(name, func(t *testing.T) {
			_, err := Unmarshal(candidate)
			require.Error(t, err)
		})
	}

	invalidValues := map[string]ArtifactV1{
		"unknown contract": func() ArtifactV1 { a := validArtifact(); a.ContractVersion = "media-transcript/v2"; return a }(),
		"unknown origin":   func() ArtifactV1 { a := validArtifact(); a.Origin = "inferred"; return a }(),
		"empty provider":   func() ArtifactV1 { a := validArtifact(); a.Provider = ""; return a }(),
		"long metadata":    func() ArtifactV1 { a := validArtifact(); a.Model = strings.Repeat("m", 129); return a }(),
		"no segments":      func() ArtifactV1 { a := validArtifact(); a.Segments = nil; return a }(),
		"wrong order":      func() ArtifactV1 { a := validArtifact(); a.Segments[0].Order = 1; return a }(),
		"regressing start": func() ArtifactV1 { a := validArtifact(); a.Segments[1].StartMS = -1; return a }(),
		"empty span":       func() ArtifactV1 { a := validArtifact(); a.Segments[0].EndMS = 0; return a }(),
		"too long":         func() ArtifactV1 { a := validArtifact(); a.Segments[0].EndMS = 86_400_001; return a }(),
		"blank text":       func() ArtifactV1 { a := validArtifact(); a.Segments[0].Text = " \n"; return a }(),
		"long speaker":     func() ArtifactV1 { a := validArtifact(); a.Segments[1].Speaker = strings.Repeat("s", 129); return a }(),
	}
	for name, candidate := range invalidValues {
		t.Run(name, func(t *testing.T) {
			_, _, err := Marshal(candidate)
			require.Error(t, err)
		})
	}
}

func TestValidateMetadataRequiresNonemptyRequiredValue(t *testing.T) {
	require.ErrorContains(t, validateMetadata("", "provider", true), "provider")
	require.NoError(t, validateMetadata("", "model", false))
}

func TestBuildTimedArtifactPreservesSpeakerInEvidenceIdentity(t *testing.T) {
	policy, err := document.NewEvidencePolicy(100)
	require.NoError(t, err)
	artifact := ArtifactV1{
		ContractVersion: "media-transcript/v1",
		Origin:          "supplied",
		Provider:        "synthetic",
		Segments: []Segment{{
			Order: 0, StartMS: 0, EndMS: 1000, Speaker: "Cafe\u0301", Text: "synthetic cue",
		}},
	}
	source, retained, err := Build(artifact, "audio", policy)
	require.NoError(t, err)
	require.Equal(t, document.EvidenceArtifactTranscript, retained.Role)
	require.Equal(t, "application/json", retained.MediaType)
	require.Equal(t, retained.SHA256, source.Artifacts[0].SHA256)
	require.Equal(t, "Cafe\u0301", source.Units[0].Speaker)

	normalized, err := document.NormalizeEvidenceV1(source, policy)
	require.NoError(t, err)
	require.Equal(t, "Café", normalized.Units[0].Speaker)
	withSpeaker := normalized.Checksum
	source.Units[0].Speaker = ""
	withoutSpeaker, err := document.NormalizeEvidenceV1(source, policy)
	require.NoError(t, err)
	require.NotEqual(t, withSpeaker, withoutSpeaker.Checksum)
	raw, _, err := document.MarshalNormalizedEvidenceV1(withoutSpeaker)
	require.NoError(t, err)
	require.NotContains(t, string(raw), `"speaker"`)
}
