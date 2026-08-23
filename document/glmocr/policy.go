package glmocr

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/url"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/ocr"
)

const (
	Provider = "glmocr"

	DefaultEndpoint             = "http://127.0.0.1:30004/glmocr/parse"
	DefaultModel                = "zai-org/GLM-OCR"
	DefaultServedModel          = "glm-ocr"
	DefaultModelRevision        = "ca5d8b3e287e52589e37c28385d9655ee4372f9d"
	DefaultModelSHA256          = "a16eb0de98d199293371c560f95f83130d2a2c9612449df16839f08ff9498815"
	DefaultSDKRevision          = "cef4d0ea120d1741f5cefe8985eee45f6c8eff1d"
	DefaultLayoutModel          = "PaddlePaddle/PP-DocLayoutV3_safetensors"
	DefaultLayoutRevision       = "97d101e6db2642e162a1d05392d1b0231c91033e"
	DefaultLayoutSHA256         = "5ea422c6cc5fe759a47e1357c35639b58173508e025a3131cbe4b6ac59e2b85e"
	DefaultEngineImage          = "vllm/vllm-openai@sha256:4f986370d7737abacc70ac17f86695acd1dc7892a02ad89ac132639d5afee0d0"
	DefaultPipelineSHA256       = "f299e93f6f928640d4aa7faceb79ed24c978f71ca33195a36dd8bc9f4855c5b0"
	DefaultPyMuPDFVersion       = "1.27.2.3"
	DefaultMuPDFVersion         = "1.27.2"
	DefaultVLLMVersion          = "0.19.0"
	DefaultEngineAdapterSHA256  = "68afc40384a9c078f07408d2b497b3249c2907d925fc9991f8a3035ccde42359"
	DefaultAdapterSHA256        = "81d6732f3e1a753eca74ff2fe9f718b7870d63e74550142eb739a3096cf0e056"
	DefaultImageRecipeSHA256    = "64e1c5f821484d1ed68e2d4d421710ac70366ea29df496b284f68144c5557cb9"
	DefaultDependencyLockSHA256 = "b8327b09b922791b91f6151d2e348cab19fac8da5c025ffec7166c393d0197ed"

	// DefaultDeploymentFingerprint identifies the complete validated local
	// inference deployment described by DefaultDeploymentIdentity.
	DefaultDeploymentFingerprint = "1d49e1c7491df09c99362307ea3ffc10c89a2c2ac7f424f9ceabf15c3fcf83f2"

	MaxDocumentBytes = int64(64 << 20)
	MaxResponseBytes = int64(512 << 20)
	MaxUnits         = 500

	canonicalPolicyVersion    = 2
	deploymentIdentityVersion = 2
	artifactDigestGitSHA1     = "git-sha1"
	artifactDigestSHA256      = "sha256"
)

// ArtifactDigest pins one file in a complete model snapshot.
type ArtifactDigest struct {
	Path      string `json:"path"`
	Algorithm string `json:"algorithm"`
	Digest    string `json:"digest"`
}

// RuntimeDependency pins one Python distribution added to the base image.
type RuntimeDependency struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// EngineEnvironment pins output-affecting and model-loading engine settings.
type EngineEnvironment struct {
	FlashInferWorkspace   string `json:"FLASHINFER_WORKSPACE_DIR"`
	HuggingFaceOffline    string `json:"HF_HUB_OFFLINE"`
	TransformersOffline   string `json:"TRANSFORMERS_OFFLINE"`
	TritonCache           string `json:"TRITON_CACHE_DIR"`
	VLLMCache             string `json:"VLLM_CACHE_ROOT"`
	VLLMCUDACompatibility string `json:"VLLM_ENABLE_CUDA_COMPATIBILITY"`
	VLLMNoUsageStats      string `json:"VLLM_NO_USAGE_STATS"`
	VLLMUsageSource       string `json:"VLLM_USAGE_SOURCE"`
}

// DeploymentIdentity contains every pinned artifact and configuration input
// included in local OCR output attribution.
type DeploymentIdentity struct {
	Version              int                   `json:"version"`
	Model                string                `json:"model"`
	ModelRevision        string                `json:"model_revision"`
	ModelFiles           [11]ArtifactDigest    `json:"model_files"`
	SDKRevision          string                `json:"sdk_revision"`
	LayoutModel          string                `json:"layout_model"`
	LayoutRevision       string                `json:"layout_revision"`
	LayoutFiles          [6]ArtifactDigest     `json:"layout_files"`
	EngineImage          string                `json:"engine_image"`
	VLLMVersion          string                `json:"vllm_version"`
	EngineAdapterSHA256  string                `json:"engine_adapter_sha256"`
	EngineCommand        [21]string            `json:"engine_command"`
	EngineEnvironment    EngineEnvironment     `json:"engine_environment"`
	AdapterSHA256        string                `json:"adapter_sha256"`
	ImageRecipeSHA256    string                `json:"image_recipe_sha256"`
	DependencyLockSHA256 string                `json:"dependency_lock_sha256"`
	PipelineConfigSHA256 string                `json:"pipeline_config_sha256"`
	RuntimeDependencies  [13]RuntimeDependency `json:"runtime_dependencies"`
	PyMuPDFVersion       string                `json:"pymupdf_version"`
	MuPDFVersion         string                `json:"mupdf_version"`
}

// DefaultDeploymentIdentity returns the deployment required by the package
// policy. The service validates locally observable members at startup.
func DefaultDeploymentIdentity() DeploymentIdentity {
	return DeploymentIdentity{
		Version: deploymentIdentityVersion, Model: DefaultModel, ModelRevision: DefaultModelRevision,
		ModelFiles: [11]ArtifactDigest{
			{Path: ".eval_results/mdpbench.yaml", Algorithm: artifactDigestGitSHA1, Digest: "ff3401de97f415491b0fd674059823e3f8510a5e"},
			{Path: ".eval_results/olmocrbench.yaml", Algorithm: artifactDigestGitSHA1, Digest: "7186fb2de9bf6b20f8fcb23bfcb31e1f7826c692"},
			{Path: ".gitattributes", Algorithm: artifactDigestGitSHA1, Digest: "a6344aac8c09253b3b630fb776ae94478aa0275b"},
			{Path: "README.md", Algorithm: artifactDigestGitSHA1, Digest: "2dcab5a6ed326dd7db202569c79c2244654ca028"},
			{Path: "chat_template.jinja", Algorithm: artifactDigestGitSHA1, Digest: "8f3b7224cc22edcb813a59ca438efc92f15749a9"},
			{Path: "config.json", Algorithm: artifactDigestGitSHA1, Digest: "46fbd1c232b7d9d0fc261cf10361350ef3af02c3"},
			{Path: "generation_config.json", Algorithm: artifactDigestGitSHA1, Digest: "0de866e8cc29b4479b56bf68300da9353bc63a71"},
			{Path: "model.safetensors", Algorithm: artifactDigestSHA256, Digest: DefaultModelSHA256},
			{Path: "preprocessor_config.json", Algorithm: artifactDigestGitSHA1, Digest: "308553695af766b3e3d05e68279d2c690e73273e"},
			{Path: "tokenizer.json", Algorithm: artifactDigestGitSHA1, Digest: "9f4a549a14a96217569648aa7627c6674ad94fe9"},
			{Path: "tokenizer_config.json", Algorithm: artifactDigestGitSHA1, Digest: "18f2106a5124ac945ee5526ac60fa75e09e97e11"},
		},
		SDKRevision: DefaultSDKRevision,
		LayoutModel: DefaultLayoutModel, LayoutRevision: DefaultLayoutRevision,
		LayoutFiles: [6]ArtifactDigest{
			{Path: ".gitattributes", Algorithm: artifactDigestGitSHA1, Digest: "a6344aac8c09253b3b630fb776ae94478aa0275b"},
			{Path: "README.md", Algorithm: artifactDigestGitSHA1, Digest: "48fc6ecdb0cec7cc38d759cdea3caaccba39ae4a"},
			{Path: "config.json", Algorithm: artifactDigestGitSHA1, Digest: "5a22928c191950850cbc0e56e43f722073e7c8da"},
			{Path: "inference.yml", Algorithm: artifactDigestGitSHA1, Digest: "ed7472400b398e0e0e032893f7986b32692980e7"},
			{Path: "model.safetensors", Algorithm: artifactDigestSHA256, Digest: DefaultLayoutSHA256},
			{Path: "preprocessor_config.json", Algorithm: artifactDigestGitSHA1, Digest: "ab66797648e5a3247eca2988e9fcd8af07a6a038"},
		},
		EngineImage: DefaultEngineImage, VLLMVersion: DefaultVLLMVersion,
		EngineAdapterSHA256: DefaultEngineAdapterSHA256,
		EngineCommand: [21]string{
			"python3", "-m", "vllm.entrypoints.cli.main", "serve", "/models/glm-ocr",
			"--host", "0.0.0.0", "--port", "30005", "--served-model-name", "glm-ocr",
			"--max-model-len", "8192", "--max-num-seqs", "4", "--gpu-memory-utilization", "0.12",
			"--speculative-config", `{"method":"mtp","num_speculative_tokens":3}`,
			"--middleware", "engine_identity.deployment_identity",
		},
		EngineEnvironment: EngineEnvironment{
			FlashInferWorkspace: "/root/.cache/flashinfer", HuggingFaceOffline: "1",
			TransformersOffline: "1", TritonCache: "/root/.cache/triton", VLLMCache: "/root/.cache/vllm",
			VLLMCUDACompatibility: "0", VLLMNoUsageStats: "1", VLLMUsageSource: "production-docker-image",
		},
		AdapterSHA256:     DefaultAdapterSHA256,
		ImageRecipeSHA256: DefaultImageRecipeSHA256, DependencyLockSHA256: DefaultDependencyLockSHA256,
		PipelineConfigSHA256: DefaultPipelineSHA256,
		RuntimeDependencies: [13]RuntimeDependency{
			{Name: "transformers", Version: "5.15.1"}, {Name: "huggingface-hub", Version: "1.28.0"},
			{Name: "hf-xet", Version: "1.6.0"}, {Name: "safetensors", Version: "0.8.0"},
			{Name: "portalocker", Version: "3.2.0"}, {Name: "PyMuPDF", Version: "1.27.2.3"},
			{Name: "pypdfium2", Version: "5.6.0"}, {Name: "Flask", Version: "3.1.2"},
			{Name: "gunicorn", Version: "23.0.0"}, {Name: "blinker", Version: "1.9.0"},
			{Name: "click", Version: "8.4.2"}, {Name: "itsdangerous", Version: "2.2.0"},
			{Name: "Werkzeug", Version: "3.1.8"},
		},
		PyMuPDFVersion: DefaultPyMuPDFVersion, MuPDFVersion: DefaultMuPDFVersion,
	}
}

// PolicyConfig fixes every input, output, and artifact bound that can affect
// local OCR evidence.
type PolicyConfig struct {
	Endpoint         string
	Model            string
	ServedModel      string
	ModelRevision    string
	SDKRevision      string
	LayoutModel      string
	LayoutRevision   string
	MaxDocumentBytes int64
	MaxResponseBytes int64
	MaxUnits         int
	NormalizePolicy  document.NormalizePolicy
}

// PolicyValues is a read-only copy of the effective local OCR policy.
type PolicyValues struct {
	Provider         string                           `json:"provider"`
	Endpoint         string                           `json:"endpoint"`
	Model            string                           `json:"model"`
	ServedModel      string                           `json:"served_model"`
	ModelRevision    string                           `json:"model_revision"`
	SDKRevision      string                           `json:"sdk_revision"`
	LayoutModel      string                           `json:"layout_model"`
	LayoutRevision   string                           `json:"layout_revision"`
	MaxDocumentBytes int64                            `json:"max_document_bytes"`
	MaxResponseBytes int64                            `json:"max_response_bytes"`
	MaxUnits         int                              `json:"max_units"`
	Normalization    document.NormalizePolicyIdentity `json:"normalization"`
	Deployment       DeploymentIdentity               `json:"deployment"`
}

// Policy is an immutable local GLM-OCR processing policy.
type Policy struct {
	values          PolicyValues
	normalizePolicy document.NormalizePolicy
	fingerprint     string
	deployment      string
	identity        ocr.Identity
}

// NewPolicy validates a loopback-only endpoint and immutable artifact pins.
func NewPolicy(config PolicyConfig) (Policy, error) {
	if config.Endpoint == "" {
		config.Endpoint = DefaultEndpoint
	}
	if config.Model == "" {
		config.Model = DefaultModel
	}
	if config.ServedModel == "" {
		config.ServedModel = DefaultServedModel
	}
	if config.ModelRevision == "" {
		config.ModelRevision = DefaultModelRevision
	}
	if config.SDKRevision == "" {
		config.SDKRevision = DefaultSDKRevision
	}
	if config.LayoutModel == "" {
		config.LayoutModel = DefaultLayoutModel
	}
	if config.LayoutRevision == "" {
		config.LayoutRevision = DefaultLayoutRevision
	}
	if err := validateEndpoint(config.Endpoint); err != nil {
		return Policy{}, err
	}
	if config.Model != DefaultModel || config.ServedModel != DefaultServedModel ||
		config.ModelRevision != DefaultModelRevision || config.SDKRevision != DefaultSDKRevision ||
		config.LayoutModel != DefaultLayoutModel || config.LayoutRevision != DefaultLayoutRevision {
		return Policy{}, errors.New("GLM-OCR policy artifacts must use the package-pinned identities")
	}
	if config.MaxDocumentBytes <= 0 || config.MaxDocumentBytes > MaxDocumentBytes ||
		config.MaxResponseBytes <= 0 || config.MaxResponseBytes > MaxResponseBytes ||
		config.MaxUnits <= 0 || config.MaxUnits > MaxUnits {
		return Policy{}, errors.New("GLM-OCR policy bounds are invalid")
	}
	normalization := config.NormalizePolicy.Identity()
	if normalization.Version == 0 {
		return Policy{}, errors.New("GLM-OCR normalization policy is invalid; use document.NewNormalizePolicy")
	}
	values := PolicyValues{
		Provider: Provider, Endpoint: config.Endpoint, Model: config.Model,
		ServedModel: config.ServedModel, ModelRevision: config.ModelRevision,
		SDKRevision: config.SDKRevision, LayoutModel: config.LayoutModel,
		LayoutRevision: config.LayoutRevision, MaxDocumentBytes: config.MaxDocumentBytes,
		MaxResponseBytes: config.MaxResponseBytes, MaxUnits: config.MaxUnits,
		Normalization: normalization, Deployment: DefaultDeploymentIdentity(),
	}
	deploymentEncoded, err := json.Marshal(values.Deployment)
	if err != nil {
		return Policy{}, fmt.Errorf("encode GLM-OCR deployment identity: %w", err)
	}
	deploymentValue := jsontext.Value(deploymentEncoded)
	if err := deploymentValue.Canonicalize(); err != nil {
		return Policy{}, fmt.Errorf("canonicalize GLM-OCR deployment identity: %w", err)
	}
	deploymentDigest := sha256.Sum256(deploymentValue)
	deploymentFingerprint := hex.EncodeToString(deploymentDigest[:])
	if deploymentFingerprint != DefaultDeploymentFingerprint {
		return Policy{}, errors.New("GLM-OCR package deployment identity is inconsistent")
	}
	payload := struct {
		PolicyValues

		Version int `json:"version"`
	}{Version: canonicalPolicyVersion, PolicyValues: values}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Policy{}, fmt.Errorf("encode GLM-OCR policy: %w", err)
	}
	digest := sha256.Sum256(encoded)
	identity, err := ocr.NewIdentity(Provider, config.Model, config.ModelRevision)
	if err != nil {
		return Policy{}, fmt.Errorf("configure GLM-OCR model identity: %w", err)
	}
	return Policy{
		values: values, normalizePolicy: config.NormalizePolicy,
		fingerprint: hex.EncodeToString(digest[:]), deployment: deploymentFingerprint, identity: identity,
	}, nil
}

func validateEndpoint(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Path != "/glmocr/parse" || parsed.Port() == "" {
		return errors.New("GLM-OCR endpoint must be an explicit loopback HTTP /glmocr/parse URL")
	}
	host := parsed.Hostname()
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("GLM-OCR endpoint must use a loopback host")
		}
	}
	return nil
}

// Values returns every effective policy value.
func (p Policy) Values() PolicyValues { return p.values }

// Fingerprint returns the stable digest of endpoint, artifacts, and bounds.
func (p Policy) Fingerprint() string { return p.fingerprint }

// Identity returns the stable provider/model identity.
func (p Policy) Identity() ocr.Identity { return p.identity }

// NormalizePolicy returns the executable normalization policy.
func (p Policy) NormalizePolicy() document.NormalizePolicy { return p.normalizePolicy }
