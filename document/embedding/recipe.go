package embedding

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	recipeVersion      = 1
	inputFormatVersion = 1

	defaultMaxInputRunes     = 8_000
	defaultMaxFilenameRunes  = 256
	defaultMaxTitleRunes     = 512
	defaultMaxHeadingRunes   = 512
	defaultMaxPartitionRunes = 16_000
	defaultMaxSections       = 128
	defaultMaxSectionRunes   = 4_000
)

// RepresentationMode selects which document representations enter an
// embedding plan.
type RepresentationMode string

const (
	// RepresentationRaw embeds normalized source chunks without distillation.
	RepresentationRaw RepresentationMode = "raw"
	// RepresentationDistilled embeds only validated derived sections.
	RepresentationDistilled RepresentationMode = "distilled"
	// RepresentationCombined embeds both normalized chunks and derived sections.
	RepresentationCombined RepresentationMode = "combined"
)

// DistillationConfig fixes the behavior and bounds of an optional
// pre-embedding distillation step. Endpoint and credentials are deliberately
// excluded; endpoint-sensitive consent uses EgressIdentity.
type DistillationConfig struct {
	Provider              string `json:"provider"`
	Model                 string `json:"model"`
	ModelRevision         string `json:"model_revision"`
	PromptTemplateVersion int    `json:"prompt_template_version"`
	MaxPartitionRunes     int    `json:"max_partition_runes"`
	MaxSections           int    `json:"max_sections"`
	MaxSectionRunes       int    `json:"max_section_runes"`
}

// RecipeConfig configures deterministic embedding-plan construction.
type RecipeConfig struct {
	Mode             RepresentationMode
	MaxInputRunes    int
	MaxFilenameRunes int
	MaxTitleRunes    int
	MaxHeadingRunes  int
	Distillation     *DistillationConfig
}

// RecipeValues is a read-only copy of every value that defines plan output.
type RecipeValues struct {
	Version            int                 `json:"version"`
	InputFormatVersion int                 `json:"input_format_version"`
	Mode               RepresentationMode  `json:"mode"`
	MaxInputRunes      int                 `json:"max_input_runes"`
	MaxFilenameRunes   int                 `json:"max_filename_runes"`
	MaxTitleRunes      int                 `json:"max_title_runes"`
	MaxHeadingRunes    int                 `json:"max_heading_runes"`
	Distillation       *DistillationConfig `json:"distillation,omitempty"`
}

// Recipe is an immutable document embedding recipe.
type Recipe struct {
	values RecipeValues
	digest string
}

// NewRecipe validates and constructs a recipe. Zero bounds select conservative
// defaults. Raw is the default mode and does not permit distillation settings.
func NewRecipe(config RecipeConfig) (Recipe, error) {
	if config.Mode == "" {
		config.Mode = RepresentationRaw
	}
	if config.MaxInputRunes == 0 {
		config.MaxInputRunes = defaultMaxInputRunes
	}
	if config.MaxFilenameRunes == 0 {
		config.MaxFilenameRunes = defaultMaxFilenameRunes
	}
	if config.MaxTitleRunes == 0 {
		config.MaxTitleRunes = defaultMaxTitleRunes
	}
	if config.MaxHeadingRunes == 0 {
		config.MaxHeadingRunes = defaultMaxHeadingRunes
	}
	if config.MaxInputRunes < 1 || config.MaxFilenameRunes < 1 || config.MaxTitleRunes < 1 || config.MaxHeadingRunes < 1 {
		return Recipe{}, errors.New("embedding recipe bounds must be positive")
	}
	if config.Mode != RepresentationRaw && config.Mode != RepresentationDistilled && config.Mode != RepresentationCombined {
		return Recipe{}, fmt.Errorf("unsupported embedding representation mode %q", config.Mode)
	}

	distillation, hasDistillation, err := normalizeDistillation(config.Mode, config.Distillation)
	if err != nil {
		return Recipe{}, err
	}
	var distillationValues *DistillationConfig
	if hasDistillation {
		distillationValues = &distillation
	}
	values := RecipeValues{
		Version: recipeVersion, InputFormatVersion: inputFormatVersion, Mode: config.Mode,
		MaxInputRunes: config.MaxInputRunes, MaxFilenameRunes: config.MaxFilenameRunes,
		MaxTitleRunes: config.MaxTitleRunes, MaxHeadingRunes: config.MaxHeadingRunes,
		Distillation: distillationValues,
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return Recipe{}, fmt.Errorf("encode embedding recipe: %w", err)
	}
	return Recipe{values: values, digest: fingerprint(encoded)}, nil
}

func normalizeDistillation(mode RepresentationMode, config *DistillationConfig) (DistillationConfig, bool, error) {
	if mode == RepresentationRaw {
		if config != nil {
			return DistillationConfig{}, false, errors.New("raw embedding recipe cannot configure distillation")
		}
		return DistillationConfig{}, false, nil
	}
	if config == nil {
		return DistillationConfig{}, false, errors.New("distilled and combined embedding recipes require distillation settings")
	}
	values := *config
	if values.MaxPartitionRunes == 0 {
		values.MaxPartitionRunes = defaultMaxPartitionRunes
	}
	if values.MaxSections == 0 {
		values.MaxSections = defaultMaxSections
	}
	if values.MaxSectionRunes == 0 {
		values.MaxSectionRunes = defaultMaxSectionRunes
	}
	if values.Provider == "" || values.Model == "" || values.ModelRevision == "" || values.PromptTemplateVersion < 1 {
		return DistillationConfig{}, false, errors.New("distillation provider, model, revision, and prompt template version are required")
	}
	for _, identity := range [...]struct{ name, value string }{
		{name: "distillation provider", value: values.Provider},
		{name: "distillation model", value: values.Model},
		{name: "distillation model revision", value: values.ModelRevision},
	} {
		if err := validateIdentityText(identity.name, identity.value); err != nil {
			return DistillationConfig{}, false, err
		}
	}
	if values.MaxPartitionRunes < 1 || values.MaxSections < 1 || values.MaxSectionRunes < 1 {
		return DistillationConfig{}, false, errors.New("distillation bounds must be positive")
	}
	return values, true, nil
}

// Values returns a deep copy of every effective recipe value.
func (r Recipe) Values() RecipeValues {
	values := r.values
	if r.values.Distillation != nil {
		distillation := *r.values.Distillation
		values.Distillation = &distillation
	}
	return values
}

// CanonicalJSON returns the canonical recipe identity.
func (r Recipe) CanonicalJSON() ([]byte, error) {
	if !r.valid() {
		return nil, errors.New("embedding recipe is invalid; use NewRecipe")
	}
	encoded, err := json.Marshal(r.values)
	if err != nil {
		return nil, fmt.Errorf("encode embedding recipe: %w", err)
	}
	return encoded, nil
}

// Fingerprint returns lowercase SHA-256 over CanonicalJSON.
func (r Recipe) Fingerprint() string { return r.digest }

func (r Recipe) valid() bool { return r.digest != "" }

func fingerprint(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
