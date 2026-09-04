package document

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
)

const (
	// ModelInputProfileOpenAICompatible is the plain-text contract used by
	// OpenAI-compatible embedding endpoints.
	ModelInputProfileOpenAICompatible ModelInputProfile = "openai-compatible/v1"
	// ModelInputProfileVoyage selects Voyage's explicit document/query modes.
	ModelInputProfileVoyage ModelInputProfile = "voyage/v1"
	// ModelInputProfileMistral is Mistral's plain-text embedding contract.
	ModelInputProfileMistral ModelInputProfile = "mistral/v1"
	// ModelInputProfileCustom permits an explicit compatibility contract.
	ModelInputProfileCustom ModelInputProfile = "custom/v1"
	// ModelInputProfileNomic uses Nomic's reviewed asymmetric search prefixes.
	ModelInputProfileNomic ModelInputProfile = "nomic/v1"
	// ModelInputProfileE5 uses E5's reviewed passage/query prefixes.
	ModelInputProfileE5 ModelInputProfile = "e5/v1"
	// ModelInputProfileBGEM3 is BGE-M3's prefix-free dense retrieval input.
	ModelInputProfileBGEM3 ModelInputProfile = "bge-m3/v1"
	// ModelInputProfileGTE is GTE's prefix-free retrieval input.
	ModelInputProfileGTE ModelInputProfile = "gte/v1"
	// ModelInputProfileQwen3 is Qwen3's prefix-free document input and optional
	// explicit query instruction contract.
	ModelInputProfileQwen3 ModelInputProfile = "qwen3/v1"
	// ModelInputProfileQueryInstruction is the reviewed query-only instruction
	// envelope for compatible models whose document role stays prefix-free.
	ModelInputProfileQueryInstruction ModelInputProfile = "query-instruction/v1"

	modelInputContractVersion = 1
	modelInputContentSlot     = "{{content}}"
)

// ModelInputProfile identifies a reviewed built-in input policy or the one
// explicit custom profile form. Provider aliases are never a durable policy.
type ModelInputProfile string

// ModelInputMode is the adapter-native role field selected by a contract.
type ModelInputMode string

const (
	ModelInputModeText     ModelInputMode = "text"
	ModelInputModeDocument ModelInputMode = "document"
	ModelInputModeQuery    ModelInputMode = "query"
)

// ModelInputEncoder renders one role with exactly one content substitution.
type ModelInputEncoder struct {
	Mode     ModelInputMode `json:"mode"`
	Template string         `json:"template"`
}

// ModelInputContract is a canonical, durable document/query formatting
// declaration. Fingerprint covers both role envelopes, including an explicit
// empty contract.
type ModelInputContract struct {
	Version          int               `json:"version"`
	Profile          ModelInputProfile `json:"profile"`
	CompatibilityID  string            `json:"compatibility_id"`
	Document         ModelInputEncoder `json:"document"`
	Query            ModelInputEncoder `json:"query"`
	QueryInstruction string            `json:"query_instruction,omitempty"`
	Fingerprint      string            `json:"fingerprint"`
}

// ModelInputContractConfig constructs one canonical model-input contract.
type ModelInputContractConfig struct {
	Profile          ModelInputProfile
	CompatibilityID  string
	Document         ModelInputEncoder
	Query            ModelInputEncoder
	QueryInstruction string
}

// builtinModelInputProfile is one reviewed model-family contract. Profiles
// that allow a query instruction render it through queryInstructionTemplate;
// every other profile rejects one so model names never imply hidden prompts.
type builtinModelInputProfile struct {
	compatibilityID     string
	documentMode        ModelInputMode
	documentPrefix      string
	queryMode           ModelInputMode
	queryPrefix         string
	queryInstruction    bool
	requiresInstruction bool
}

var builtinModelInputProfiles = map[ModelInputProfile]builtinModelInputProfile{
	ModelInputProfileOpenAICompatible: {compatibilityID: "openai-compatible/text/v1", documentMode: ModelInputModeText, queryMode: ModelInputModeText},
	ModelInputProfileVoyage:           {compatibilityID: "voyage/document-query/v1", documentMode: ModelInputModeDocument, queryMode: ModelInputModeQuery},
	ModelInputProfileMistral:          {compatibilityID: "mistral/text/v1", documentMode: ModelInputModeText, queryMode: ModelInputModeText},
	ModelInputProfileNomic:            {compatibilityID: "nomic/search/v1", documentMode: ModelInputModeText, documentPrefix: "search_document: ", queryMode: ModelInputModeText, queryPrefix: "search_query: "},
	ModelInputProfileE5:               {compatibilityID: "e5/asymmetric/v1", documentMode: ModelInputModeText, documentPrefix: "passage: ", queryMode: ModelInputModeText, queryPrefix: "query: "},
	ModelInputProfileBGEM3:            {compatibilityID: "bge-m3/text/v1", documentMode: ModelInputModeText, queryMode: ModelInputModeText},
	ModelInputProfileGTE:              {compatibilityID: "gte/text/v1", documentMode: ModelInputModeText, queryMode: ModelInputModeText},
	ModelInputProfileQwen3:            {compatibilityID: "qwen3/text/v1", documentMode: ModelInputModeText, queryMode: ModelInputModeText, queryInstruction: true},
	ModelInputProfileQueryInstruction: {compatibilityID: "query-instruction/text/v1", documentMode: ModelInputModeText, queryMode: ModelInputModeText, queryInstruction: true, requiresInstruction: true},
}

// NewModelInputContract resolves a reviewed built-in profile or one explicit
// custom contract. An empty config is intentional and has its own fingerprint.
func NewModelInputContract(config ModelInputContractConfig) (ModelInputContract, error) {
	contract := ModelInputContract{Version: modelInputContractVersion, Profile: config.Profile, QueryInstruction: config.QueryInstruction}
	hasEncoderOverrides := config.CompatibilityID != "" || config.Document != (ModelInputEncoder{}) || config.Query != (ModelInputEncoder{})
	switch config.Profile {
	case "":
		if hasEncoderOverrides || config.QueryInstruction != "" {
			return ModelInputContract{}, errors.New("empty model-input contract cannot define encoders")
		}
	case ModelInputProfileCustom:
		if config.QueryInstruction != "" {
			return ModelInputContract{}, errors.New("custom model-input contracts must encode instructions in their explicit query template")
		}
		contract.CompatibilityID = config.CompatibilityID
		contract.Document = config.Document
		contract.Query = config.Query
	default:
		builtin, ok := builtinModelInputProfiles[config.Profile]
		if !ok {
			if strings.HasPrefix(string(config.Profile), "alias:") {
				return ModelInputContract{}, errors.New("opaque model-input aliases require an explicit compatibility contract")
			}
			return ModelInputContract{}, fmt.Errorf("unknown built-in model-input profile %q", config.Profile)
		}
		if hasEncoderOverrides {
			return ModelInputContract{}, errors.New("built-in model-input profiles cannot be overridden")
		}
		if !builtin.queryInstruction && config.QueryInstruction != "" {
			return ModelInputContract{}, fmt.Errorf("%s profile cannot define a query instruction", config.Profile)
		}
		if err := validateQueryInstruction(config.QueryInstruction, builtin.requiresInstruction); err != nil {
			return ModelInputContract{}, err
		}
		contract.CompatibilityID = builtin.compatibilityID
		contract.Document = ModelInputEncoder{Mode: builtin.documentMode, Template: builtin.documentPrefix + modelInputContentSlot}
		contract.Query = ModelInputEncoder{Mode: builtin.queryMode, Template: builtin.queryPrefix + queryInstructionTemplate(config.QueryInstruction)}
	}
	if err := validateModelInputContractFields(contract); err != nil {
		return ModelInputContract{}, err
	}
	contract.Fingerprint = ""
	encoded, err := canonical.Marshal(contract)
	if err != nil {
		return ModelInputContract{}, fmt.Errorf("encode model-input contract: %w", err)
	}
	contract.Fingerprint = sha256Hex(encoded)
	return contract, nil
}

// EncodeDocument renders document text through the sealed document encoder.
func (contract ModelInputContract) EncodeDocument(content string) string {
	return strings.Replace(contract.Document.Template, modelInputContentSlot, content, 1)
}

// EncodeQuery renders query text through the sealed query encoder.
func (contract ModelInputContract) EncodeQuery(content string) string {
	return strings.Replace(contract.Query.Template, modelInputContentSlot, content, 1)
}

// modelInputRenderedLength computes an encoder's exact rendered byte length
// without allocating its template output.
func modelInputRenderedLength(encoder ModelInputEncoder, content string) (int64, error) {
	contentOffset := strings.Index(encoder.Template, modelInputContentSlot)
	if contentOffset < 0 {
		return 0, errors.New("model-input encoder does not contain content slot")
	}
	const maxInt64 = int64(^uint64(0) >> 1)
	length := int64(contentOffset)
	if int64(len(content)) > maxInt64-length {
		return 0, errors.New("model-input rendered length overflows")
	}
	length += int64(len(content))
	suffix := len(encoder.Template) - contentOffset - len(modelInputContentSlot)
	if int64(suffix) > maxInt64-length {
		return 0, errors.New("model-input rendered length overflows")
	}
	return length + int64(suffix), nil
}

func validateModelInputContract(contract ModelInputContract) error {
	if err := validateModelInputContractFields(contract); err != nil {
		return err
	}
	fingerprint := contract.Fingerprint
	contract.Fingerprint = ""
	encoded, err := canonical.Marshal(contract)
	if err != nil {
		return err
	}
	if fingerprint != sha256Hex(encoded) {
		return errors.New("model-input contract fingerprint or canonical form is invalid")
	}
	if contract.Profile != ModelInputProfileCustom && contract.Profile != "" {
		canonical, err := NewModelInputContract(ModelInputContractConfig{Profile: contract.Profile, QueryInstruction: contract.QueryInstruction})
		canonical.Fingerprint = ""
		if err != nil || contract != canonical {
			return errors.New("model-input contract is not the reviewed built-in profile")
		}
	}
	if contract.Profile == "" {
		empty, err := NewModelInputContract(ModelInputContractConfig{})
		empty.Fingerprint = ""
		if err != nil || contract != empty {
			return errors.New("empty model-input contract is not canonical")
		}
	}
	return nil
}

func validateModelInputContractFields(contract ModelInputContract) error {
	if contract.Version != modelInputContractVersion {
		return fmt.Errorf("model-input contract version must be %d", modelInputContractVersion)
	}
	if contract.Profile == "" {
		if contract.CompatibilityID != "" || contract.Document != (ModelInputEncoder{}) || contract.Query != (ModelInputEncoder{}) || contract.QueryInstruction != "" {
			return errors.New("empty model-input contract must have zero encoders and compatibility ID")
		}
		return nil
	}
	if contract.Profile == ModelInputProfileCustom && contract.QueryInstruction != "" {
		return errors.New("custom model-input contracts must encode instructions in their explicit query template")
	}
	if err := validateCompatibilityID(contract.CompatibilityID); err != nil {
		return err
	}
	if err := validateModelInputEncoder("document", contract.Document); err != nil {
		return err
	}
	return validateModelInputEncoder("query", contract.Query)
}

func validateQueryInstruction(instruction string, required bool) error {
	if required && instruction == "" {
		return errors.New("query-instruction profile requires a query instruction")
	}
	if instruction == "" {
		return nil
	}
	if !utf8.ValidString(instruction) || len(instruction) > 4096 || strings.ContainsAny(instruction, "\x00\r") || strings.Contains(instruction, modelInputContentSlot) {
		return errors.New("query instruction must be bounded valid UTF-8 text without a content slot")
	}
	return nil
}

func queryInstructionTemplate(instruction string) string {
	if instruction == "" {
		return modelInputContentSlot
	}
	return "Instruct: " + instruction + "\nQuery:" + modelInputContentSlot
}

func validateCompatibilityID(value string) error {
	if value == "" || len(value) > 128 {
		return errors.New("model-input compatibility ID must contain 1-128 characters")
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' || character == '-' || character == '.' || character == '/' {
			continue
		}
		return errors.New("model-input compatibility ID contains unsupported characters")
	}
	return nil
}

func validateModelInputEncoder(role string, encoder ModelInputEncoder) error {
	switch encoder.Mode {
	case ModelInputModeText, ModelInputModeDocument, ModelInputModeQuery:
	default:
		return fmt.Errorf("model-input %s mode is invalid", role)
	}
	if !utf8.ValidString(encoder.Template) || len(encoder.Template) > 4096 {
		return fmt.Errorf("model-input %s template must be bounded valid UTF-8", role)
	}
	if strings.Count(encoder.Template, modelInputContentSlot) != 1 {
		return fmt.Errorf("model-input %s template must contain exactly one content slot", role)
	}
	return nil
}
