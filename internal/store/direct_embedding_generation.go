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

const automaticDirectGenerationTag = "docbank/automatic-direct-file-input-generation/v1"

// DirectEmbeddingGeneration identifies the exact original-file input to a binding.
func DirectEmbeddingGeneration(version ContentVersion, profileFingerprint,
	embeddingInputFingerprint string, binding document.EmbeddingBindingV1,
) EmbeddingInputGenerationRecord {
	return EmbeddingInputGenerationRecord{
		ID:                           directEmbeddingGenerationID(version.ID, version.BlobHash, profileFingerprint, binding.Name, embeddingInputFingerprint),
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

func directEmbeddingGenerationID(versionID, sourceHash, profile, binding, inputFP string) string {
	return LengthPrefixedSHA256("docbank/direct-file-input-generation/v1", versionID, sourceHash, profile, binding, inputFP)
}

// AutomaticDirectEmbeddingGeneration retains the producer in the identity
// while keeping the original-file input and vector contracts the same.
func AutomaticDirectEmbeddingGeneration(version ContentVersion, profile, inputFP string,
	binding document.EmbeddingBindingV1,
) EmbeddingInputGenerationRecord {
	generation := DirectEmbeddingGeneration(version, profile, inputFP, binding)
	generation.ID = LengthPrefixedSHA256(automaticDirectGenerationTag, generation.ID)
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
	manual = directEmbeddingGenerationID(generation.SourceVersionID, generation.GenerationChecksum,
		generation.ProcessingProfileFingerprint, binding.Name, inputFP)
	return manual, LengthPrefixedSHA256(automaticDirectGenerationTag, manual)
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
