package store

import "go.kenn.io/docbank/document"

// AutomaticOriginalFileConsentClass authorizes discovery to send watched MP4 originals.
const AutomaticOriginalFileConsentClass = "original_file_automatic"

// AutomaticOriginalFileDisclosureFingerprint scopes automatic consent to
// the binding even when multiple bindings share one portable disclosure.
func AutomaticOriginalFileDisclosureFingerprint(binding document.EmbeddingBindingV1) string {
	return LengthPrefixedSHA256("docbank/automatic-original-file-disclosure/v1", binding.Name, binding.DisclosureFingerprint)
}

func embeddingJobDisclosureFingerprint(binding document.EmbeddingBindingV1, inputClass string) string {
	if inputClass == AutomaticOriginalFileConsentClass {
		return AutomaticOriginalFileDisclosureFingerprint(binding)
	}
	return binding.DisclosureFingerprint
}

// DirectEmbeddingGeneration identifies the exact original-file input to a binding.
func DirectEmbeddingGeneration(version ContentVersion, profileFingerprint,
	embeddingInputFingerprint string, binding document.EmbeddingBindingV1,
) EmbeddingInputGenerationRecord {
	manual, _ := directGenerationIDs(version.ID, version.BlobHash, profileFingerprint, binding.Name, embeddingInputFingerprint)
	return EmbeddingInputGenerationRecord{
		ID:                           manual,
		SourceVersionID:              version.ID,
		ProcessingProfileFingerprint: profileFingerprint,
		EvidenceFingerprint:          LengthPrefixedSHA256("docbank/direct-file-evidence/v1", version.BlobHash),
		TokenizerFingerprint:         LengthPrefixedSHA256("docbank/direct-file-tokenizer/v1", "none"),
		ChunkPolicyFingerprint:       embeddingInputFingerprint,
		FormatterFingerprint:         LengthPrefixedSHA256("docbank/direct-file-formatter/v1", binding.DocumentFormatter, binding.ModelInput.Fingerprint),
		GenerationChecksum:           version.BlobHash,
		Inputs:                       []EmbeddingInputReference{{ID: version.ID, RenderedChecksum: version.BlobHash}},
		CreatedAt:                    version.RecordedAt}
}

// directGenerationIDs derives the manual and automatic generation IDs for one
// original-file input. Every producer and equivalence check uses this pair.
func directGenerationIDs(versionID, sourceHash, profile, binding, inputFP string) (manual, automatic string) {
	manual = LengthPrefixedSHA256("docbank/direct-file-input-generation/v1", versionID, sourceHash, profile, binding, inputFP)
	return manual, LengthPrefixedSHA256("docbank/automatic-direct-file-input-generation/v1", manual)
}

// AutomaticDirectEmbeddingGeneration retains the producer in the identity
// while keeping the original-file input and vector contracts the same.
func AutomaticDirectEmbeddingGeneration(version ContentVersion, profile, inputFP string,
	binding document.EmbeddingBindingV1,
) EmbeddingInputGenerationRecord {
	generation := DirectEmbeddingGeneration(version, profile, inputFP, binding)
	_, generation.ID = directGenerationIDs(version.ID, version.BlobHash, profile, binding.Name, inputFP)
	return generation
}

type directProducer uint8

const (
	directProducerUnknown directProducer = iota
	directProducerManual
	directProducerAutomatic
)

func equivalentEmbeddingGenerationIDs(generation EmbeddingInputGenerationRecord, binding document.EmbeddingBindingV1,
	inputFP string,
) (manual, automatic string) {
	if binding.InputKind != document.EmbeddingInputOriginalFile {
		return generation.ID, generation.ID
	}
	return directGenerationIDs(generation.SourceVersionID, generation.GenerationChecksum,
		generation.ProcessingProfileFingerprint, binding.Name, inputFP)
}

func directGenerationProducer(generation EmbeddingInputGenerationRecord, binding document.EmbeddingBindingV1,
	inputFP string,
) directProducer {
	if binding.InputKind != document.EmbeddingInputOriginalFile {
		return directProducerUnknown
	}
	manual, automatic := equivalentEmbeddingGenerationIDs(generation, binding, inputFP)
	switch generation.ID {
	case manual:
		return directProducerManual
	case automatic:
		return directProducerAutomatic
	default:
		return directProducerUnknown
	}
}

func embeddingJobConsentClass(generation EmbeddingInputGenerationRecord, binding document.EmbeddingBindingV1, inputFP string) string {
	switch directGenerationProducer(generation, binding, inputFP) {
	case directProducerAutomatic:
		return AutomaticOriginalFileConsentClass
	case directProducerManual:
		return string(document.EmbeddingInputOriginalFile)
	default:
		if binding.InputKind == document.EmbeddingInputOriginalFile {
			return ""
		}
		return string(binding.InputKind)
	}
}

func embeddingJobConsentMatches(generation EmbeddingInputGenerationRecord, binding document.EmbeddingBindingV1,
	inputFP string, inputClasses []string,
) bool {
	class := embeddingJobConsentClass(generation, binding, inputFP)
	return class != "" && len(inputClasses) == 1 && inputClasses[0] == class
}
