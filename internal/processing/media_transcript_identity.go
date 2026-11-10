package processing

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/mediatranscript"
	"go.kenn.io/docbank/internal/store"
)

// Speech completeness excludes only the explicitly documented non-speech field.
// It does not upgrade the evidence's completeness or erase its omissions.
func transcriptSpeechState(evidence document.NormalizedEvidenceV1, truncated bool) string {
	if truncated || evidence.UnitKind != document.EvidenceUnitSegment || len(evidence.Units) == 0 {
		return statusPartial
	}
	allowed := func(omissions []document.EvidenceOmissionV1) bool {
		for _, omission := range omissions {
			if omission.Kind != document.EvidenceOmissionField || omission.Field != "non_speech_content" || omission.Locator != nil || omission.Range != nil {
				return false
			}
		}
		return true
	}
	if !allowed(evidence.Omissions) {
		return statusPartial
	}
	for _, unit := range evidence.Units {
		if strings.TrimSpace(unit.Text) == "" || !allowed(unit.Omissions) {
			return statusPartial
		}
	}
	return "complete"
}

// Read metadata only from the verified retained artifact. Legacy/supplied text
// can have another artifact contract and keeps its existing read behavior.
func (service *Service) mediaTranscriptArtifactIdentity(ctx context.Context, build store.RenditionBuildRecord, transcript *MediaTranscriptEvidence) error {
	for _, artifact := range build.Artifacts {
		if artifact.Role != string(document.EvidenceArtifactTranscript) {
			continue
		}
		if artifact.BlobHash != artifact.Checksum || artifact.Size < 1 {
			return errors.New("retained transcript artifact authority is invalid")
		}
		transcript.ArtifactSHA256 = artifact.Checksum
		raw, err := service.readRenditionArtifact(ctx, artifact, "retained transcript artifact could not be read exactly")
		if err != nil {
			return err
		}
		var identity struct {
			ContractVersion string `json:"contract_version"`
		}
		if err := json.Unmarshal(raw, &identity); err != nil {
			return errors.New("saved transcript is invalid")
		}
		switch identity.ContractVersion {
		case "supplied-transcript/v1":
			if err := document.ValidateSuppliedTranscriptArtifactV1(raw); err != nil {
				return err
			}
			transcript.SpeechState = statusPartial
		case "media-transcript/v1":
			timed, err := mediatranscript.Unmarshal(raw)
			if err != nil {
				return err
			}
			transcript.Language = timed.Language
			if timed.Origin != "generated" || transcript.Origin != "generated" {
				transcript.SpeechState = statusPartial
			}
		default:
			return errors.New("saved transcript is invalid")
		}
		return nil
	}
	// Older profiles can retain normalized evidence without the typed artifact.
	// Such evidence remains readable but is insufficient for the consumer's new contract.
	transcript.SpeechState = statusPartial
	return nil
}
