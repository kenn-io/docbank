package processing

import (
	"errors"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/mediatranscript"
)

var errTranscriptEvidenceMismatch = errors.New("transcript artifact disagrees with evidence")

func validateSuppliedTranscriptEvidence(provider, text string, evidence document.NormalizedEvidenceV1) error {
	policy, err := document.NewEvidencePolicy(document.MaxRenditionArtifactBytes)
	if err != nil {
		return err
	}
	source, _, err := document.BuildTranscriptSourceEvidenceV1(document.SuppliedTranscript{Provider: provider, Text: text}, policy)
	if err != nil {
		return err
	}
	return compareTranscriptEvidence(source, evidence, policy, true)
}

func validateTimedTranscriptEvidence(artifact mediatranscript.ArtifactV1, evidence document.NormalizedEvidenceV1, partial bool) error {
	policy, err := document.NewEvidencePolicy(document.MaxRenditionArtifactBytes)
	if err != nil {
		return err
	}
	source, _, err := mediatranscript.Build(artifact, evidence.Family, policy)
	if err != nil {
		return err
	}
	return compareTranscriptEvidence(source, evidence, policy, partial)
}

// Additional omissions may make evidence a subset of the retained transcript.
// Every returned unit must still be supported by the artifact in the same order.
func compareTranscriptEvidence(source document.SourceEvidenceV1, evidence document.NormalizedEvidenceV1, policy document.EvidencePolicy, partial bool) error {
	expected, err := document.NormalizeEvidenceV1(source, policy)
	if err != nil {
		return err
	}
	if !partial && len(expected.Units) != len(evidence.Units) {
		return errTranscriptEvidenceMismatch
	}
	index, offset := 0, 0
	for _, actual := range evidence.Units {
		found := false
		for index < len(expected.Units) {
			candidate := expected.Units[index]
			speakerMatches := actual.Speaker == candidate.Speaker || partial && actual.Speaker == ""
			locatorMatches := actual.Locator == candidate.Locator || partial && (actual.Locator.Kind == document.EvidenceLocatorGeneric ||
				actual.Locator.Kind == document.EvidenceLocatorSegment && candidate.Locator.Kind == document.EvidenceLocatorSegment &&
					actual.Locator.Start >= candidate.Locator.Start && actual.Locator.End <= candidate.Locator.End)
			matchedAt := -1
			if !partial && actual.Text == candidate.Text {
				matchedAt = 0
			} else if partial && actual.Text != "" {
				matchedAt = strings.Index(candidate.Text[offset:], actual.Text)
			}
			if matchedAt >= 0 && speakerMatches && locatorMatches {
				found = true
				offset += matchedAt + len(actual.Text)
				if !partial || offset == len(candidate.Text) {
					index++
					offset = 0
				}
				break
			}
			index++
			offset = 0
			if !partial {
				break
			}
		}
		if !found {
			return errTranscriptEvidenceMismatch
		}
	}
	return nil
}
