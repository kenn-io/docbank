package docling

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
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
func TestDoclingASRRealAudioQualification(t *testing.T) {
	endpoint := os.Getenv("DOCBANK_CI_ASR_URL")
	if endpoint == "" {
		t.Skip("requires isolated remote CI Docling ASR service")
	}
	source, err := os.ReadFile("../../.superpowers/asr-qualification/asr-spoken.m4a")
	require.NoError(t, err)
	tests := []struct {
		name, mediaType, filename, artifactName string
		source                                  []byte
	}{
		{name: "M4A AAC", mediaType: "audio/mp4", filename: "asr-spoken.m4a", artifactName: "media-transcript.json", source: source},
		{name: "MP3", mediaType: "audio/mpeg", filename: "asr-spoken.mp3", artifactName: "mp3-transcript.json", source: asrSpokenMP3},
	}
	for _, filename := range []string{"asr-spoken.mp4", "asr-large.mp4", "asr-multisegment.mp4"} {
		video, err := os.ReadFile("../../.superpowers/asr-qualification/" + filename)
		require.NoError(t, err)
		if filename == "asr-large.mp4" {
			require.Greater(t, len(video), 38<<20)
		}
		tests = append(tests, struct {
			name, mediaType, filename, artifactName string
			source                                  []byte
		}{filename, "video/mp4", filename, filename + "-transcript.json", video})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newASRFixture(t, test.mediaType, test.filename, test.source)
			measured := &asrQualificationTransport{base: http.DefaultTransport}
			client, err := NewASR(ASRProfile{
				Origin: endpoint, Descriptor: fixture.descriptor, RequestTimeout: 30 * time.Second, TotalTimeout: 5 * time.Minute,
				PollInterval: time.Second, MaxPollAttempts: 300, MaxResponseBytes: 16 << 20, MaxDocumentBytes: 64 << 20,
				MaxTranscriptChars: 100_000, AllowM4A: test.mediaType == "audio/mp4", AllowMP4: test.mediaType == "video/mp4"}, nil, &http.Client{Transport: measured})
			require.NoError(t, err)
			result, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
			require.NoError(t, err, "synthetic ASR cause: %v", errors.Unwrap(err))
			require.Len(t, result.Artifacts, 1)
			artifact, err := mediatranscript.Unmarshal(result.Artifacts[0].Payload)
			require.NoError(t, err)
			require.NotEmpty(t, artifact.Segments)
			if test.mediaType == "video/mp4" {
				require.Equal(t, "video", result.Evidence.Family)
				facts, local := fixture.capability.Facts()
				require.True(t, local)
				segments, err := mapASRTracks(measured.document, facts.DurationMS)
				require.NoError(t, err)
				require.Equal(t, segments, artifact.Segments, "provider order and timing must survive unchanged")
				if test.filename == "asr-multisegment.mp4" && len(segments) < 2 {
					t.Log("multi-segment mapping: not qualified (provider returned one segment)")
				}
			}
			var text strings.Builder
			for _, segment := range artifact.Segments {
				text.WriteString(segment.Text)
				text.WriteByte(' ')
			}
			// ASR is uncertain: the service default tiny model hears the first noun
			// imperfectly. Check several known speech terms without claiming verbatim accuracy.
			recognized := strings.ToLower(text.String())
			if test.mediaType == "video/mp4" {
				// Gate 0's tiny model missed delivery/arrives in these videos.
				// This is a loose speech check, never an accuracy claim.
				require.Contains(t, recognized, "friday")
				require.True(t, strings.Contains(recognized, "3") || strings.Contains(recognized, "three"))
			} else {
				require.Contains(t, recognized, "delivery")
				require.Contains(t, recognized, "arrives")
				require.Contains(t, recognized, "friday")
			}
			require.LessOrEqual(t, measured.responseBytes, 16<<20)
			t.Logf("source=%s response_bytes=%d frame_count=%d segment_count=%d provider_text=%q accuracy: not qualified", test.filename, measured.responseBytes, measured.frames, len(artifact.Segments), text.String())
			summary, err := json.Marshal(map[string]any{"response_bytes": measured.responseBytes, "frame_count": measured.frames,
				"segment_count": len(artifact.Segments), "multi_segment_qualified": len(artifact.Segments) >= 2, "provider_text": text.String(), "accuracy": "not qualified"})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile("../../.superpowers/asr-qualification/"+test.artifactName+".measurement.json", summary, 0600))
			require.NoError(t, os.WriteFile("../../.superpowers/asr-qualification/"+test.artifactName, result.Artifacts[0].Payload, 0600))
		})
	}
}

// Measurement observes the same bounded result consumed by the real adapter.
type asrQualificationTransport struct {
	base                  http.RoundTripper
	responseBytes, frames int
	document              []byte
}

func (transport *asrQualificationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err != nil || !strings.HasPrefix(request.URL.Path, resultPath) {
		return response, err
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
	_ = response.Body.Close()
	if err != nil {
		return nil, err
	}
	response.Body = io.NopCloser(bytes.NewReader(raw))
	transport.responseBytes = len(raw)
	var body struct {
		Document struct {
			JSONContent jsontext.Value `json:"json_content"`
		} `json:"document"`
	}
	if json.Unmarshal(raw, &body) == nil {
		transport.document = body.Document.JSONContent
		var content struct {
			Pictures []jsontext.Value `json:"pictures"`
		}
		if json.Unmarshal(transport.document, &content) == nil {
			transport.frames = len(content.Pictures)
		}
	}
	return response, nil
}
