package openaiembed

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/providerhttp"
)

const (
	bgeM3Model = "BAAI/bge-m3"

	// PoolingCLS selects the first-token vector used by BGE-M3.
	PoolingCLS = "cls"
	// PoolingLastToken selects the final non-padding token used by Qwen3.
	PoolingLastToken = "last_token"

	// OutputDenseSingleVector excludes sparse and multi-vector outputs.
	OutputDenseSingleVector = "dense_single_vector"
	// DimensionTransformNone requires the deployment's native output width.
	DimensionTransformNone = "none"
)

// DeploymentContract pins the model-side behavior that an
// OpenAI-compatible transport cannot report or enforce itself.
type DeploymentContract struct {
	ModelFamily        string `json:"model_family"`
	WeightsRevision    string `json:"weights_revision"`
	Tokenizer          string `json:"tokenizer"`
	TokenizerRevision  string `json:"tokenizer_revision"`
	Pooling            string `json:"pooling"`
	MaxSequenceTokens  int    `json:"max_sequence_tokens"`
	OutputMode         string `json:"output_mode"`
	DimensionTransform string `json:"dimension_transform"`
}

// ReviewedProfileConfig supplies deployment-specific identity and transport
// bounds for a reviewed self-hosted embedding profile.
type ReviewedProfileConfig struct {
	Origin            string
	ServedModel       string
	DeploymentEpoch   string
	WeightsRevision   string
	TokenizerRevision string
	SecretBinding     string
	RequestTimeout    time.Duration
	MaxBatchItems     int
	MaxInputBytes     int64
	MaxRequestBytes   int64
	MaxResponseBytes  int64
	EgressPolicy      providerhttp.EgressPolicy
}

// Qwen3Model identifies one reviewed Qwen3 Embedding model size.
type Qwen3Model string

const (
	// Qwen3Embedding06B is the 0.6B model with a native 1024-dimensional output.
	Qwen3Embedding06B Qwen3Model = "Qwen/Qwen3-Embedding-0.6B"
	// Qwen3Embedding4B is the 4B model with a native 2560-dimensional output.
	Qwen3Embedding4B Qwen3Model = "Qwen/Qwen3-Embedding-4B"
	// Qwen3Embedding8B is the 8B model with a native 4096-dimensional output.
	Qwen3Embedding8B Qwen3Model = "Qwen/Qwen3-Embedding-8B"
)

// Qwen3ProfileConfig adds the model size and required retrieval instruction.
type Qwen3ProfileConfig struct {
	ReviewedProfileConfig

	Model            Qwen3Model
	QueryInstruction string
}

// BGEM3Profile builds the reviewed dense-only BGE-M3 profile.
func BGEM3Profile(config ReviewedProfileConfig) (Profile, error) {
	input, err := document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileBGEM3})
	if err != nil {
		return Profile{}, fmt.Errorf("openaiembed: build BGE-M3 input contract: %w", err)
	}
	return reviewedProfile(config, input, 1024, DeploymentContract{
		ModelFamily: bgeM3Model, WeightsRevision: config.WeightsRevision,
		Tokenizer: bgeM3Model, TokenizerRevision: config.TokenizerRevision,
		Pooling: PoolingCLS, MaxSequenceTokens: 8192,
		OutputMode: OutputDenseSingleVector, DimensionTransform: DimensionTransformNone,
	})
}

// Qwen3Profile builds one reviewed dense-only Qwen3 Embedding profile at its
// native output dimension.
func Qwen3Profile(config Qwen3ProfileConfig) (Profile, error) {
	if strings.TrimSpace(config.QueryInstruction) == "" {
		return Profile{}, errors.New("openaiembed: Qwen3 query instruction is required")
	}
	dimension := map[Qwen3Model]int{
		Qwen3Embedding06B: 1024,
		Qwen3Embedding4B:  2560,
		Qwen3Embedding8B:  4096,
	}[config.Model]
	if dimension == 0 {
		return Profile{}, fmt.Errorf("openaiembed: unsupported Qwen3 embedding model %q", config.Model)
	}
	input, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileQwen3, QueryInstruction: config.QueryInstruction,
	})
	if err != nil {
		return Profile{}, fmt.Errorf("openaiembed: build Qwen3 input contract: %w", err)
	}
	model := string(config.Model)
	return reviewedProfile(config.ReviewedProfileConfig, input, dimension, DeploymentContract{
		ModelFamily: model, WeightsRevision: config.WeightsRevision,
		Tokenizer: model, TokenizerRevision: config.TokenizerRevision,
		Pooling: PoolingLastToken, MaxSequenceTokens: 32768,
		OutputMode: OutputDenseSingleVector, DimensionTransform: DimensionTransformNone,
	})
}

func reviewedProfile(config ReviewedProfileConfig, input document.ModelInputContract, dimension int, deployment DeploymentContract) (Profile, error) {
	profile := Profile{
		Origin: config.Origin, ModelInput: input, DeploymentContract: &deployment,
		SecretBinding: config.SecretBinding, DeploymentEpoch: config.DeploymentEpoch,
		RequestTimeout: config.RequestTimeout, MaxBatchItems: config.MaxBatchItems,
		MaxInputBytes: config.MaxInputBytes, MaxRequestBytes: config.MaxRequestBytes,
		MaxResponseBytes: config.MaxResponseBytes, EgressPolicy: cloneEgressPolicy(config.EgressPolicy),
		Descriptor: document.EmbeddingDescriptor{
			ID: ProviderID, ContractVersion: document.EmbeddingProviderContractVersion,
			TrustBoundary: document.EmbeddingTrustOperatorNetwork, Model: config.ServedModel,
			ModelRevision: config.DeploymentEpoch, Dimension: dimension, Metric: document.VectorMetricCosine,
			Normalization: document.VectorNormalizationUnitLength, ScalarEncoding: ScalarEncodingFloat32,
			DocumentFormatter: DocumentFormatterV1, QueryFormatter: QueryFormatterV1,
			InputKinds:      []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk},
			CompatibilityID: input.CompatibilityID, SupportsTextQuery: true, ModelInput: input,
			SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText},
		},
	}
	fingerprint, err := PolicyFingerprint(profile)
	if err != nil {
		return Profile{}, err
	}
	profile.Descriptor.PolicyFingerprint = fingerprint
	profile.Descriptor, err = document.NewEmbeddingDescriptor(profile.Descriptor)
	if err != nil {
		return Profile{}, fmt.Errorf("openaiembed: build reviewed descriptor: %w", err)
	}
	return profile, nil
}

func validateDeploymentContract(contract *DeploymentContract, descriptor document.EmbeddingDescriptor, input document.ModelInputContract) error {
	if contract == nil {
		return nil
	}
	if !immutableGitRevision(contract.WeightsRevision) || !immutableGitRevision(contract.TokenizerRevision) {
		return errors.New("openaiembed: reviewed deployment revisions must be lowercase 40-character Git commits")
	}
	if contract.OutputMode != OutputDenseSingleVector || contract.DimensionTransform != DimensionTransformNone {
		return errors.New("openaiembed: reviewed deployment must return one native dense vector")
	}
	if descriptor.Metric != document.VectorMetricCosine || descriptor.Normalization != document.VectorNormalizationUnitLength {
		return errors.New("openaiembed: reviewed deployment requires cosine metric and unit-length vectors")
	}
	switch input.Profile {
	case document.ModelInputProfileBGEM3:
		if contract.ModelFamily != bgeM3Model || contract.Tokenizer != bgeM3Model ||
			contract.Pooling != PoolingCLS || contract.MaxSequenceTokens != 8192 || descriptor.Dimension != 1024 {
			return errors.New("openaiembed: deployment does not match the reviewed BGE-M3 dense contract")
		}
	case document.ModelInputProfileQwen3:
		dimension := map[string]int{string(Qwen3Embedding06B): 1024, string(Qwen3Embedding4B): 2560, string(Qwen3Embedding8B): 4096}[contract.ModelFamily]
		if dimension == 0 || contract.Tokenizer != contract.ModelFamily || contract.Pooling != PoolingLastToken ||
			contract.MaxSequenceTokens != 32768 || descriptor.Dimension != dimension || input.QueryInstruction == "" {
			return errors.New("openaiembed: deployment does not match a reviewed Qwen3 dense contract")
		}
	default:
		return errors.New("openaiembed: deployment contract requires a reviewed BGE-M3 or Qwen3 input profile")
	}
	return nil
}

func immutableGitRevision(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}
