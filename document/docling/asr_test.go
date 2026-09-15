package docling

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/mediatranscript"
)

//go:embed testdata/asr-qualification.json
var asrQualification []byte

//go:embed testdata/asr-spoken.wav
var asrSpokenWAV []byte

//go:embed testdata/asr-spoken.mp3
var asrSpokenMP3 []byte

func TestDoclingASRReadsTrackSourcesAndPreservesOverlap(t *testing.T) {
	raw := []byte(`{"texts":[{"text":"telescope delivery arrives Friday at three","source":[{"kind":"track","start_time":0,"end_time":1.0001,"voice":"Speaker 1"}]},{"text":"second cue","source":[{"kind":"track","start_time":0.75,"end_time":2}]}]}`)
	got, err := mapASRTracks(raw, 3000)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, mediatranscript.Segment{Order: 0, StartMS: 0, EndMS: 1001, Speaker: "Speaker 1", Text: "telescope delivery arrives Friday at three"}, got[0])
	assert.Equal(t, mediatranscript.Segment{Order: 1, StartMS: 750, EndMS: 2000, Text: "second cue"}, got[1])

	for _, span := range [][3]float64{
		{math.NaN(), 1, 3},
		{0, math.Inf(1), 3},
		{0, 1, math.Inf(1)},
		{0, 1, 0},
		{-1, 1, 3},
		{1, 1, 3},
		{2, 4, 3},
	} {
		_, _, err = trackSpan(span[0], span[1], span[2])
		require.Error(t, err)
	}
}

func TestDoclingASRRejectsUnavailableOrAmbiguousTiming(t *testing.T) {
	for _, testCase := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "missing source", raw: `{"texts":[{"text":"untimed"}]}`, want: "timing_unavailable"},
		{name: "empty source", raw: `{"texts":[{"text":"untimed","source":[]}]}`, want: "timing_unavailable"},
		{name: "multiple spans", raw: `{"texts":[{"text":"split","source":[{"kind":"track","start_time":0,"end_time":1},{"kind":"track","start_time":2,"end_time":3}]}]}`, want: "timing_unavailable"},
		{name: "wrong source kind", raw: `{"texts":[{"text":"page","source":[{"kind":"page","start_time":0,"end_time":1}]}]}`, want: "timing_unavailable"},
		{name: "missing end", raw: `{"texts":[{"text":"open","source":[{"kind":"track","start_time":0}]}]}`, want: "timing_unavailable"},
		{name: "regressing starts", raw: `{"texts":[{"text":"later","source":[{"kind":"track","start_time":1,"end_time":2}]},{"text":"earlier","source":[{"kind":"track","start_time":0,"end_time":1}]}]}`, want: "track order regresses"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := mapASRTracks([]byte(testCase.raw), 3000)
			require.ErrorContains(t, err, testCase.want)
		})
	}
}

func TestDoclingASRQualificationPinsSyntheticSpeech(t *testing.T) {
	var qualification struct {
		Service struct {
			DoclingServe     string `json:"docling_serve"`
			Docling          string `json:"docling"`
			DoclingCore      string `json:"docling_core"`
			DoclingIBMModels string `json:"docling_ibm_models"`
		} `json:"service"`
		Schema struct {
			OpenAPISHA256         string `json:"openapi_sha256"`
			DocumentSchemaVersion string `json:"document_schema_version"`
		} `json:"schema"`
		Request struct {
			FromFormats       []string `json:"from_formats"`
			ToFormats         []string `json:"to_formats"`
			PipelineSelection string   `json:"pipeline_selection"`
			ASRModelField     string   `json:"asr_model_field"`
		} `json:"request"`
		Fixtures []struct {
			File           string `json:"file"`
			SHA256         string `json:"sha256"`
			ExpectedPhrase string `json:"expected_phrase"`
		} `json:"fixtures"`
	}
	require.NoError(t, json.Unmarshal(asrQualification, &qualification))
	assert.Equal(t, "1.32.0", qualification.Service.DoclingServe)
	assert.Equal(t, "2.124.0", qualification.Service.Docling)
	assert.Equal(t, "2.93.0", qualification.Service.DoclingCore)
	assert.Equal(t, "4.0.1", qualification.Service.DoclingIBMModels)
	assert.Equal(t, "24274f3c2e6a3981c447e64356c3efc196394961ae02523d69fd88f67c1733db", qualification.Schema.OpenAPISHA256)
	assert.Equal(t, "1.10.0", qualification.Schema.DocumentSchemaVersion)
	assert.Equal(t, []string{"audio"}, qualification.Request.FromFormats)
	assert.Equal(t, []string{"json"}, qualification.Request.ToFormats)
	assert.Equal(t, "service_auto_from_audio", qualification.Request.PipelineSelection)
	assert.Equal(t, "absent_from_schema", qualification.Request.ASRModelField)

	byName := map[string][]byte{"asr-spoken.wav": asrSpokenWAV, "asr-spoken.mp3": asrSpokenMP3}
	require.Len(t, qualification.Fixtures, 2)
	for _, fixture := range qualification.Fixtures {
		digest := sha256.Sum256(byName[fixture.File])
		assert.Equal(t, fixture.SHA256, hex.EncodeToString(digest[:]))
		assert.Equal(t, "Telescope delivery arrives Friday at 3.", fixture.ExpectedPhrase)
	}
}

func TestDoclingASRClientRequestsAudioAndBuildsTranscriptArtifact(t *testing.T) {
	for _, testCase := range []struct {
		name, mediaType, filename, taskID string
		source                            []byte
		providerEnd                       float64
		wantEnd                           int64
	}{
		{name: "WAV", mediaType: "audio/wav", filename: "asr-spoken.wav", taskID: "asr-wav", source: asrSpokenWAV, providerEnd: 3.06, wantEnd: 3060},
		{name: "MP3", mediaType: "audio/mpeg", filename: "asr-spoken.mp3", taskID: "asr-mp3", source: asrSpokenMP3, providerEnd: 3.08, wantEnd: 3080},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newASRFixture(t, testCase.mediaType, testCase.filename, testCase.source)
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case convertPath:
					assertDoclingASRSubmission(t, request, fixture.metadata, fixture.source)
					writeJSON(t, response, doclingTask(testCase.taskID, "success"))
				case resultPath + testCase.taskID:
					writeJSON(t, response, asrResultResponse(fixture.metadata.Filename, "1.10.0", testCase.providerEnd))
				default:
					http.NotFound(response, request)
				}
			}))
			t.Cleanup(server.Close)

			client := newASRClient(t, server.URL, fixture.descriptor)
			result, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
			require.NoError(t, err)
			require.Len(t, result.Evidence.Units, 1)
			assert.Equal(t, document.EvidenceUnitSegment, result.Evidence.UnitKind)
			assert.Equal(t, int64(0), result.Evidence.Units[0].Locator.Start)
			assert.Equal(t, testCase.wantEnd, result.Evidence.Units[0].Locator.End)
			assert.Equal(t, "Telescope delivery arrives Friday at 3.", result.Evidence.Units[0].Text)
			require.Len(t, result.Artifacts, 1)
			assert.Equal(t, document.EvidenceArtifactTranscript, result.Artifacts[0].Role)
			artifact, err := mediatranscript.Unmarshal(result.Artifacts[0].Payload)
			require.NoError(t, err)
			assert.Equal(t, "generated", artifact.Origin)
			assert.Equal(t, "docling.serve-v1", artifact.Provider)
			assert.Equal(t, "1.32.0", artifact.ProviderVersion)
			assert.Empty(t, artifact.Model)
		})
	}
}

func TestDoclingASRProfileRejectsUnqualifiedFormats(t *testing.T) {
	fixture := newASRFixture(t, "audio/wav", "asr-spoken.wav", asrSpokenWAV)
	for _, testCase := range []struct {
		name    string
		formats []document.RenditionFormatCapability
	}{
		{name: "missing MP3", formats: fixture.descriptor.SupportedFormats[:1]},
		{name: "unqualified OGG", formats: append(fixture.descriptor.SupportedFormats,
			document.RenditionFormatCapability{MediaFamily: "audio", MediaType: "audio/ogg", InputKind: document.RenditionInputOriginalFile})},
		{name: "non-audio", formats: []document.RenditionFormatCapability{
			{MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile},
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			descriptor := fixture.descriptor
			descriptor.SupportedFormats = testCase.formats
			descriptor.Fingerprint = ""
			descriptor, err := document.NewRenditionDescriptor(descriptor)
			require.NoError(t, err)
			_, err = New(Profile{
				Origin: "http://127.0.0.1", Descriptor: descriptor,
				ASRDocumentSchemaVersion: "1.10.0", ASRProviderVersion: "1.32.0",
				MaxTranscriptChars: 100_000,
			}, nil, http.DefaultClient)
			require.ErrorContains(t, err, "qualified deployment")
		})
	}
}

func TestDoclingASRClientRejectsUnpinnedResponseSchema(t *testing.T) {
	fixture := newASRFixture(t, "audio/mpeg", "asr-spoken.mp3", asrSpokenMP3)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case convertPath:
			writeJSON(t, response, doclingTask("asr-mp3", "success"))
		case resultPath + "asr-mp3":
			writeJSON(t, response, asrResultResponse(fixture.metadata.Filename, "1.11.0", 3.08))
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)

	client := newASRClient(t, server.URL, fixture.descriptor)
	_, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
	require.Error(t, err)
	providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok)
	assert.Equal(t, document.RenditionErrorMalformedEvidence, providerErr.Code())
}

func TestDoclingASRRejectsUnqualifiedAudioBeforeSubmission(t *testing.T) {
	for _, testCase := range []struct {
		name, mediaType, filename string
		source                    []byte
	}{
		{name: "malformed WAV", mediaType: "audio/wav", filename: "broken.wav", source: []byte("not a wave file")},
		{name: "container mismatch", mediaType: "audio/mpeg", filename: "mislabeled.mp3", source: asrSpokenWAV},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newASRFixture(t, testCase.mediaType, testCase.filename, testCase.source)
			var requests int
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				requests++
				switch request.URL.Path {
				case convertPath:
					writeJSON(t, response, doclingTask("unqualified", "success"))
				case resultPath + "unqualified":
					writeJSON(t, response, asrResultResponse(fixture.metadata.Filename, "1.10.0", 1))
				default:
					http.NotFound(response, request)
				}
			}))
			t.Cleanup(server.Close)

			client := newASRClient(t, server.URL, fixture.descriptor)
			_, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
			require.Error(t, err)
			assert.Zero(t, requests)
		})
	}
}

func newASRFixture(t *testing.T, mediaType, filename string, source []byte) fixture {
	t.Helper()
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: "docling.serve-v1", ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: "1111111111111111111111111111111111111111111111111111111111111111",
		TrustBoundary:     document.RenditionTrustOperatorNetwork,
		SupportedFormats: []document.RenditionFormatCapability{
			{MediaFamily: "audio", MediaType: "audio/mpeg", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "audio", MediaType: "audio/wav", InputKind: document.RenditionInputOriginalFile},
		},
		ReturnsStructured: true,
		ArtifactRoles:     []document.EvidenceArtifactRole{document.EvidenceArtifactTranscript},
	})
	require.NoError(t, err)
	digest := sha256.Sum256(source)
	metadata := document.AuthorizedUploadMetadata{
		Filename: filename, MediaFamily: "audio", MediaType: mediaType,
		ByteLength: int64(len(source)), SHA256: hex.EncodeToString(digest[:]),
		CapabilityRecordChecksum: "2222222222222222222222222222222222222222222222222222222222222222",
		ProviderMetadataChecksum: "3333333333333333333333333333333333333333333333333333333333333333",
		InputKind:                document.RenditionInputOriginalFile,
	}
	started := time.Now().UTC().Add(-time.Minute)
	return fixture{descriptor: descriptor, metadata: metadata, source: source, authorization: document.RenditionAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint:           descriptor.PolicyFingerprint,
		RenditionRequestFingerprint: "4444444444444444444444444444444444444444444444444444444444444444",
		SourceSHA256:                metadata.SHA256, SourceBytes: metadata.ByteLength,
		CapabilityRecordChecksum: metadata.CapabilityRecordChecksum,
		ProviderMetadataChecksum: metadata.ProviderMetadataChecksum,
		MediaFamily:              "audio", MediaType: mediaType, InputKind: document.RenditionInputOriginalFile,
		DiscloseFilename:     true,
		AllowedArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactTranscript},
		MaxArtifactBytes:     mediatranscript.MaxArtifactBytes, MaxArtifacts: 1,
		MaxTotalResultBytes: mediatranscript.MaxArtifactBytes * 2,
		AuthorizedAt:        started.Format("2006-01-02T15:04:05.000000000Z"),
		ExpiresAt:           started.Add(10 * time.Minute).Format("2006-01-02T15:04:05.000000000Z"),
	}}
}

func newASRClient(t *testing.T, origin string, descriptor document.RenditionDescriptor) *Client {
	t.Helper()
	client, err := New(Profile{
		Origin: origin, Descriptor: descriptor,
		ASRDocumentSchemaVersion: "1.10.0", ASRProviderVersion: "1.32.0",
		MaxTranscriptChars: 100_000,
		RequestTimeout:     time.Second, TotalTimeout: 2 * time.Second,
		PollInterval: time.Millisecond, MaxPollAttempts: 4,
		MaxResponseBytes: 1 << 20, MaxDocumentBytes: 1 << 20,
	}, nil, http.DefaultClient)
	require.NoError(t, err)
	return client
}

func assertDoclingASRSubmission(
	t *testing.T, request *http.Request, metadata document.AuthorizedUploadMetadata, source []byte,
) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	require.NoError(t, err)
	require.Equal(t, "multipart/form-data", mediaType)
	reader := multipart.NewReader(request.Body, params["boundary"])
	file, err := reader.NextPart()
	require.NoError(t, err)
	assert.Equal(t, "files", file.FormName())
	assert.Equal(t, metadata.Filename, file.FileName())
	gotSource, err := io.ReadAll(file)
	require.NoError(t, err)
	assert.Equal(t, source, gotSource)
	fields := make(map[string][]string)
	for {
		part, partErr := reader.NextPart()
		if errors.Is(partErr, io.EOF) {
			break
		}
		require.NoError(t, partErr)
		value, readErr := io.ReadAll(part)
		require.NoError(t, readErr)
		fields[part.FormName()] = append(fields[part.FormName()], string(value))
	}
	assert.Equal(t, map[string][]string{
		"from_formats": {"audio"}, "to_formats": {"json"}, "target_type": {"inbody"},
	}, fields)
	assert.NotContains(t, fields, "pipeline")
	assert.NotContains(t, fields, "asr_model")
}

func asrResultResponse(filename, schemaVersion string, end float64) map[string]any {
	return map[string]any{
		"status": "success", "errors": []string{},
		"document": map[string]any{
			"filename": filename, "md_content": nil,
			"json_content": map[string]any{
				"schema_name": "DoclingDocument", "version": schemaVersion,
				"origin": map[string]any{"filename": filename},
				"pages":  map[string]any{},
				"texts": []any{map[string]any{
					"text": "Telescope delivery arrives Friday at 3.",
					"source": []any{map[string]any{
						"kind": "track", "start_time": 0.0, "end_time": end,
						"identifier": nil, "voice": nil,
					}},
				}},
			},
		},
	}
}
