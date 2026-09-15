package docling

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/mediatranscript"
)

const (
	qualifiedASRSchemaVersion   = "1.10.0"
	qualifiedASRProviderVersion = "1.32.0"
	maxASRDurationMS            = int64(86_400_000)
)

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

func trackSpan(start, end, duration float64) (int64, int64, error) {
	if math.IsNaN(start) || math.IsNaN(end) || math.IsNaN(duration) ||
		math.IsInf(start, 0) || math.IsInf(end, 0) || math.IsInf(duration, 0) ||
		duration <= 0 || duration > float64(maxASRDurationMS)/1000 ||
		start < 0 || end <= start || end > duration {
		return 0, 0, errors.New("invalid media track time")
	}
	return int64(math.Floor(start * 1000)), int64(math.Ceil(end * 1000)), nil
}

func mapASRTracks(raw []byte, durationMS int64) ([]mediatranscript.Segment, error) {
	if len(raw) > mediatranscript.MaxArtifactBytes {
		return nil, errors.New("ASR response byte_limit")
	}
	if durationMS <= 0 || durationMS > maxASRDurationMS {
		return nil, errors.New("invalid media duration")
	}
	var body struct {
		Texts []asrText `json:"texts"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &body) != nil {
		return nil, errors.New("invalid ASR response")
	}
	if len(body.Texts) == 0 || len(body.Texts) > 25_000 {
		return nil, errors.New("segment_limit")
	}
	out := make([]mediatranscript.Segment, 0, len(body.Texts))
	for _, text := range body.Texts {
		if len(text.Source) != 1 || text.Source[0].Kind != "track" ||
			text.Source[0].Start == nil || text.Source[0].End == nil {
			return nil, errASRTimingUnavailable
		}
		source := text.Source[0]
		start, end, err := trackSpan(*source.Start, *source.End, float64(durationMS)/1000)
		if err != nil {
			return nil, err
		}
		if len(out) > 0 && start < out[len(out)-1].StartMS {
			return nil, errors.New("track order regresses")
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

func asrDurationMS(
	source []byte,
	metadata document.AuthorizedUploadMetadata,
	authorization document.RenditionAuthorization,
) (int64, error) {
	filename := metadata.Filename
	if filename == "" {
		switch metadata.MediaType {
		case "audio/wav", "audio/x-wav":
			filename = "document.wav"
		case "audio/mpeg":
			filename = "document.mp3"
		default:
			return 0, errors.New("unsupported ASR audio type")
		}
	}
	policy := media.InspectionPolicy{
		Filename: filename, DeclaredMediaType: metadata.MediaType,
		ExpectedBytes: metadata.ByteLength, ExpectedSHA256: metadata.SHA256,
		DescriptorFingerprint: authorization.DescriptorFingerprint,
		ProfileFingerprint:    authorization.PolicyFingerprint,
		DisclosureFingerprint: metadata.ProviderMetadataChecksum,
		InputKind:             authorization.InputKind,
		MaxSourceBytes:        metadata.ByteLength, MaxExpandedBytes: 1, MaxEntryBytes: 1,
		MaxEntries: 1, MaxNestingDepth: 1, MaxTextLines: 1, MaxCharacters: 1,
		MaxRecords: 1, MaxPages: 1, MaxSlides: 1, MaxSheets: 1, MaxCells: 1,
		MaxSpineItems: 1, MaxResources: 1, MaxDurationMS: maxASRDurationMS,
	}
	record, err := media.InspectCapability(bytes.NewReader(source), policy)
	if err != nil {
		return 0, err
	}
	if !record.Eligible || record.MediaFamily != "audio" ||
		record.Measurements.DurationMS <= 0 || record.Measurements.DurationMS > maxASRDurationMS {
		return 0, errors.New("ASR audio duration is unavailable")
	}
	return record.Measurements.DurationMS, nil
}
