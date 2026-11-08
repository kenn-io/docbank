package docling

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/mediatranscript"
)

// The dedicated remote CI lane runs a real pinned Docling Serve ASR deployment.
// Normal unit suites use protocol fixtures and explicitly skip this live probe.
func TestDoclingASRRealM4AQualification(t *testing.T) {
	endpoint := os.Getenv("DOCBANK_CI_ASR_URL")
	if endpoint == "" {
		t.Skip("requires isolated remote CI Docling ASR service")
	}
	source, err := os.ReadFile("../../.superpowers/asr-qualification/asr-spoken.m4a")
	require.NoError(t, err)
	fixture := newASRFixture(t, "audio/mp4", "asr-spoken.m4a", source)
	client, err := NewASR(ASRProfile{Profile: Profile{
		Origin: endpoint, Descriptor: fixture.descriptor, RequestTimeout: 30 * time.Second, TotalTimeout: 5 * time.Minute,
		PollInterval: time.Second, MaxPollAttempts: 300, MaxResponseBytes: 1 << 20, MaxDocumentBytes: 1 << 20,
	}, MaxTranscriptChars: 100_000, AllowM4A: true}, nil, http.DefaultClient)
	require.NoError(t, err)
	result, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
	require.NoError(t, err, "synthetic ASR cause: %v", errors.Unwrap(err))
	require.Len(t, result.Artifacts, 1)
	artifact, err := mediatranscript.Unmarshal(result.Artifacts[0].Payload)
	require.NoError(t, err)
	var text strings.Builder
	for _, segment := range artifact.Segments {
		text.WriteString(segment.Text)
		text.WriteByte(' ')
	}
	require.Contains(t, strings.ToLower(text.String()), "telescope")
	require.Contains(t, strings.ToLower(text.String()), "friday")
	require.NoError(t, os.WriteFile("../../.superpowers/asr-qualification/media-transcript.json", result.Artifacts[0].Payload, 0600))
}
