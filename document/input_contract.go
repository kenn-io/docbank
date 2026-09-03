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
	Version         int               `json:"version"`
	Profile         ModelInputProfile `json:"profile"`
	CompatibilityID string            `json:"compatibility_id"`
	Document        ModelInputEncoder `json:"document"`
	Query           ModelInputEncoder `json:"query"`
	Fingerprint     string            `json:"fingerprint"`
}

// ModelInputContractConfig constructs one canonical model-input contract.
type ModelInputContractConfig struct {
	Profile         ModelInputProfile
	CompatibilityID string
	Document        ModelInputEncoder
	Query           ModelInputEncoder
}

// NewModelInputContract resolves a reviewed built-in profile or one explicit
// custom contract. An empty config is intentional and has its own fingerprint.
func NewModelInputContract(config ModelInputContractConfig) (ModelInputContract, error) {
	contract := ModelInputContract{Version: modelInputContractVersion, Profile: config.Profile}
	switch config.Profile {
	case "":
		if config.CompatibilityID != "" || config.Document != (ModelInputEncoder{}) || config.Query != (ModelInputEncoder{}) {
			return ModelInputContract{}, errors.New("empty model-input contract cannot define encoders")
		}
	case ModelInputProfileOpenAICompatible:
		if config.CompatibilityID != "" || config.Document != (ModelInputEncoder{}) || config.Query != (ModelInputEncoder{}) {
			return ModelInputContract{}, errors.New("built-in model-input profiles cannot be overridden")
		}
		contract.CompatibilityID = "openai-compatible/text/v1"
		contract.Document = ModelInputEncoder{Mode: ModelInputModeText, Template: modelInputContentSlot}
		contract.Query = ModelInputEncoder{Mode: ModelInputModeText, Template: modelInputContentSlot}
	case ModelInputProfileVoyage:
		if config.CompatibilityID != "" || config.Document != (ModelInputEncoder{}) || config.Query != (ModelInputEncoder{}) {
			return ModelInputContract{}, errors.New("built-in model-input profiles cannot be overridden")
		}
		contract.CompatibilityID = "voyage/document-query/v1"
		contract.Document = ModelInputEncoder{Mode: ModelInputModeDocument, Template: modelInputContentSlot}
		contract.Query = ModelInputEncoder{Mode: ModelInputModeQuery, Template: modelInputContentSlot}
	case ModelInputProfileMistral:
		if config.CompatibilityID != "" || config.Document != (ModelInputEncoder{}) || config.Query != (ModelInputEncoder{}) {
			return ModelInputContract{}, errors.New("built-in model-input profiles cannot be overridden")
		}
		contract.CompatibilityID = "mistral/text/v1"
		contract.Document = ModelInputEncoder{Mode: ModelInputModeText, Template: modelInputContentSlot}
		contract.Query = ModelInputEncoder{Mode: ModelInputModeText, Template: modelInputContentSlot}
	case ModelInputProfileCustom:
		contract.CompatibilityID = config.CompatibilityID
		contract.Document = config.Document
		contract.Query = config.Query
	default:
		if strings.HasPrefix(string(config.Profile), "alias:") {
			return ModelInputContract{}, errors.New("opaque model-input aliases require an explicit compatibility contract")
		}
		return ModelInputContract{}, fmt.Errorf("unknown built-in model-input profile %q", config.Profile)
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
		canonical, err := NewModelInputContract(ModelInputContractConfig{Profile: contract.Profile})
		if err != nil || contract != (ModelInputContract{Version: canonical.Version, Profile: canonical.Profile, CompatibilityID: canonical.CompatibilityID, Document: canonical.Document, Query: canonical.Query}) {
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
		if contract.CompatibilityID != "" || contract.Document != (ModelInputEncoder{}) || contract.Query != (ModelInputEncoder{}) {
			return errors.New("empty model-input contract must have zero encoders and compatibility ID")
		}
		return nil
	}
	if err := validateCompatibilityID(contract.CompatibilityID); err != nil {
		return err
	}
	if err := validateModelInputEncoder("document", contract.Document); err != nil {
		return err
	}
	return validateModelInputEncoder("query", contract.Query)
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
