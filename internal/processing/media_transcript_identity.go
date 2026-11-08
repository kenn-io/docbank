package processing

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
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
		if artifact.State != store.RenditionArtifactVerified || artifact.BlobHash != artifact.Checksum || artifact.Size < 1 || artifact.Size > mediatranscript.MaxArtifactBytes {
			return errors.New("retained transcript artifact authority is invalid")
		}
		transcript.ArtifactSHA256 = artifact.Checksum
		stream, size, err := service.blobs.OpenStreamContext(ctx, artifact.BlobHash)
		if err != nil {
			return err
		}
		raw, readErr := io.ReadAll(io.LimitReader(stream, artifact.Size+1))
		verifyErr := stream.Verify()
		closeErr := stream.Close()
		if size != artifact.Size || int64(len(raw)) != artifact.Size || readErr != nil || verifyErr != nil || closeErr != nil {
			return errors.Join(errors.New("retained transcript artifact could not be read exactly"), readErr, verifyErr, closeErr)
		}
		if transcript.Origin != "generated" {
			var identity struct {
				ContractVersion string `json:"contract_version"`
			}
			if err := json.Unmarshal(raw, &identity); err != nil {
				return err
			}
			if identity.ContractVersion != "media-transcript/v1" {
				return nil
			}
		}
		timed, err := mediatranscript.Unmarshal(raw)
		if err != nil {
			return err
		}
		transcript.Language = timed.Language
		return nil
	}
	// Older profiles can retain normalized evidence without the typed artifact.
	// Such evidence remains readable but is insufficient for the consumer's new contract.
	return nil
}
