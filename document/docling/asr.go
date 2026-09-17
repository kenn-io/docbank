package docling

import (
	"context"
	"encoding/json/v2"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/mediatranscript"
	"go.kenn.io/docbank/internal/canonical"
)

const (
	qualifiedASRSchemaVersion   = "1.10.0"
	qualifiedASRProviderVersion = "1.32.0"
)

// ASRProfile fixes the qualified audio contract and its transcript evidence bound.
// MaxTranscriptChars bounds generated transcript evidence independently of later processing limits.
type ASRProfile struct {
	Profile

	MaxTranscriptChars int
}

// ASRClient maps qualified Docling audio tracks to retained transcripts.
type ASRClient struct {
	client         *Client
	evidencePolicy document.EvidencePolicy
}

var _ document.RenditionProvider = (*ASRClient)(nil)

// ASRPolicyFingerprint binds the qualified versions and every transcript evidence
// bound. Use it as the ASR descriptor's PolicyFingerprint before sealing requests.
func ASRPolicyFingerprint(maxTranscriptChars int) (string, error) {
	policy, err := document.NewEvidencePolicy(maxTranscriptChars)
	if err != nil {
		return "", err
	}
	identity, err := canonical.Marshal(struct {
		Contract        string                          `json:"contract"`
		SchemaVersion   string                          `json:"schema_version"`
		ProviderVersion string                          `json:"provider_version"`
		EvidencePolicy  document.EvidencePolicyIdentity `json:"evidence_policy"`
	}{"docbank-docling-asr/v1", qualifiedASRSchemaVersion, qualifiedASRProviderVersion, policy.Identity()})
	if err != nil {
		return "", err
	}
	return providerutil.SHA256Hex(identity), nil
}

// ASRDisclosureFingerprint binds a portable ASR profile to its exact endpoint and
// deployment. Recompute it whenever the descriptor, endpoint, or deployment changes.
func ASRDisclosureFingerprint(descriptor document.RenditionDescriptor, endpoint, deployment string) string {
	return providerutil.SHA256Hex([]byte(strings.Join([]string{
		"docbank-docling-asr/v1", descriptor.ID, descriptor.Fingerprint, endpoint, deployment,
	}, "\x00")))
}

// NewASR constructs the transcript provider for the qualified WAV/MP3 deployment.
func NewASR(profile ASRProfile, secrets SecretResolver, httpClient *http.Client) (*ASRClient, error) {
	client, err := newTransportClient(profile.Profile, secrets, httpClient)
	if err != nil {
		return nil, err
	}
	descriptor := client.descriptor
	if descriptor.ReturnsMarkdown || !descriptor.ReturnsStructured ||
		len(descriptor.ArtifactRoles) != 1 || descriptor.ArtifactRoles[0] != document.EvidenceArtifactTranscript ||
		!qualifiedASRFormats(descriptor.SupportedFormats) {
		return nil, errors.New("docling: ASR profile does not match the qualified deployment")
	}
	fingerprint, err := ASRPolicyFingerprint(profile.MaxTranscriptChars)
	if err != nil || descriptor.PolicyFingerprint != fingerprint {
		return nil, errors.New("docling: ASR descriptor policy fingerprint does not match transcript bounds")
	}
	policy, err := document.NewEvidencePolicy(profile.MaxTranscriptChars)
	if err != nil {
		return nil, err
	}
	return &ASRClient{client: client, evidencePolicy: policy}, nil
}

func (client *ASRClient) Descriptor() document.RenditionDescriptor {
	if client == nil {
		return document.RenditionDescriptor{}
	}
	return client.client.Descriptor()
}

func (client *ASRClient) Render(
	ctx context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	if client == nil {
		return document.RenditionResult{}, errors.New("docling: ASR client is required")
	}
	if authorization.DescriptorFingerprint != client.client.descriptor.Fingerprint ||
		authorization.PolicyFingerprint != client.client.descriptor.PolicyFingerprint ||
		!providerutil.AllowsArtifact(authorization, document.EvidenceArtifactTranscript) {
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected,
			"authorization does not match the Docling transcript policy", nil)
	}
	durationMS, err := client.inputDuration(upload, authorization)
	if err != nil {
		return document.RenditionResult{}, err
	}
	run, err := client.client.startRender(ctx, upload, authorization)
	if err != nil {
		return document.RenditionResult{}, err
	}
	defer run.operation.Cancel()
	if run.metadata.Filename == "" {
		run.metadata.Filename = map[string]string{"audio/mpeg": "document.mp3", "audio/wav": "document.wav"}[run.metadata.MediaType]
	}
	task, result, err := client.client.convert(run, [][2]string{
		{"from_formats", "audio"}, {"to_formats", "json"}, {"target_type", "inbody"},
	})
	if err != nil {
		return document.RenditionResult{}, err
	}
	return client.buildResult(run, task, result, durationMS)
}

func (client *ASRClient) inputDuration(upload document.AuthorizedUpload, authorization document.RenditionAuthorization) (int64, error) {
	carrier, ok := upload.(interface {
		CapabilityProof() document.UploadCapability
	})
	if !ok {
		return 0, provider.Classified(document.RenditionErrorPolicyRejected, "Docling ASR requires local audio inspection", nil)
	}
	facts, local := carrier.CapabilityProof().Facts()
	metadata := upload.Metadata()
	if !local || facts.Checksum != authorization.CapabilityRecordChecksum ||
		facts.Checksum != metadata.CapabilityRecordChecksum || facts.SourceSHA256 != metadata.SHA256 ||
		facts.SourceBytes != metadata.ByteLength || facts.DescriptorFingerprint != client.client.descriptor.Fingerprint ||
		facts.MediaFamily != "audio" || facts.MediaFamily != metadata.MediaFamily || facts.MediaType != metadata.MediaType ||
		(facts.MediaType != "audio/wav" && facts.MediaType != "audio/mpeg") ||
		facts.DurationMS <= 0 || facts.DurationMS > (24*time.Hour).Milliseconds() {
		return 0, provider.Classified(document.RenditionErrorPolicyRejected, "Docling ASR audio capability does not match the upload", nil)
	}
	return facts.DurationMS, nil
}

var errASRTimingUnavailable = errors.New("timing_unavailable")

func qualifiedASRFormats(formats []document.RenditionFormatCapability) bool {
	if len(formats) != 2 {
		return false
	}
	want := map[document.RenditionFormatCapability]bool{
		{MediaFamily: "audio", MediaType: "audio/mpeg", InputKind: document.RenditionInputOriginalFile}: true,
		{MediaFamily: "audio", MediaType: "audio/wav", InputKind: document.RenditionInputOriginalFile}:  true,
	}
	for _, format := range formats {
		if !want[format] {
			return false
		}
		delete(want, format)
	}
	return len(want) == 0
}

type asrTrack struct {
	Kind  string   `json:"kind"`
	Start *float64 `json:"start_time"`
	End   *float64 `json:"end_time"`
	Voice string   `json:"voice"`
}

type asrText struct {
	Text   string     `json:"text"`
	Source []asrTrack `json:"source"`
}

// trackSpan converts provider seconds to a half-open millisecond span. Allow
// one 20 ms timestamp step at the end, but never retain time beyond the source.
func trackSpan(start, end float64, durationMS int64) (int64, int64, error) {
	duration := float64(durationMS) / 1000
	if math.IsNaN(start) || math.IsNaN(end) || math.IsInf(start, 0) || math.IsInf(end, 0) ||
		durationMS <= 0 || start < 0 || start >= duration || end <= start || end > duration+0.020 {
		return 0, 0, errors.New("invalid media track time")
	}
	return int64(math.Floor(start * 1000)), min(int64(math.Ceil(end*1000)), durationMS), nil
}

func mapASRTracks(raw []byte, durationMS int64) ([]mediatranscript.Segment, error) {
	var body struct {
		Texts []asrText `json:"texts"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &body) != nil {
		return nil, errors.New("invalid ASR response")
	}
	out := make([]mediatranscript.Segment, 0, len(body.Texts))
	for _, text := range body.Texts {
		if strings.TrimSpace(text.Text) == "" {
			continue
		}
		if len(text.Source) != 1 || text.Source[0].Kind != "track" ||
			text.Source[0].Start == nil || text.Source[0].End == nil {
			return nil, errASRTimingUnavailable
		}
		source := text.Source[0]
		start, end, err := trackSpan(*source.Start, *source.End, durationMS)
		if err != nil {
			return nil, err
		}
		out = append(out, mediatranscript.Segment{
			Order: len(out), StartMS: start, EndMS: end,
			Speaker: source.Voice, Text: text.Text,
		})
	}
	return out, nil
}

func asrDocumentVersion(raw []byte) (string, error) {
	var identity struct {
		SchemaName string `json:"schema_name"`
		Version    string `json:"version"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &identity) != nil ||
		identity.SchemaName != "DoclingDocument" || identity.Version == "" {
		return "", errors.New("invalid ASR document schema")
	}
	return identity.Version, nil
}

func (client *ASRClient) buildResult(
	run *rendering, task taskResponse, result doclingResult, durationMS int64,
) (document.RenditionResult, error) {
	if len(result.markdown) != 0 {
		return document.RenditionResult{}, provider.Malformed("Docling ASR returned unexpected Markdown", nil)
	}
	if task.status == "partial_success" || result.status == "partial_success" {
		return document.RenditionResult{}, provider.Malformed("Docling ASR returned partial success", nil)
	}
	version, err := asrDocumentVersion(result.document)
	if err != nil || version != qualifiedASRSchemaVersion {
		return document.RenditionResult{}, provider.Malformed("Docling ASR response schema is not qualified", err)
	}
	segments, err := mapASRTracks(result.document, durationMS)
	if err != nil {
		return document.RenditionResult{}, provider.Malformed("Docling ASR track evidence is invalid", err)
	}
	evidence, artifact, err := mediatranscript.Build(mediatranscript.ArtifactV1{
		ContractVersion: "media-transcript/v1", Origin: "generated",
		Provider: providerID, ProviderVersion: qualifiedASRProviderVersion,
		Segments: segments,
	}, run.authorization.MediaFamily, client.evidencePolicy)
	if err != nil {
		return document.RenditionResult{}, provider.Malformed("Docling ASR transcript is invalid", err)
	}
	if len(artifact.Payload) > run.authorization.MaxArtifactBytes {
		return document.RenditionResult{}, provider.Malformed("Docling ASR transcript exceeds authorization", nil)
	}
	receipt, err := providerutil.NewReceipt(provider, providerutil.Receipt{
		Descriptor: client.client.descriptor, Authorization: run.authorization,
		SourceSHA256: run.metadata.SHA256, OperationID: "docling-" + task.id,
		StartedAt: run.started, CompletedAt: time.Now().UTC(),
		Usage: run.usage.Rendition(int64(len(run.source)), int64(len(evidence.Units))),
	})
	if err != nil {
		return document.RenditionResult{}, err
	}
	return document.RenditionResult{
		Evidence: evidence, Artifacts: []document.RenditionArtifact{artifact}, Receipt: receipt,
	}, nil
}
