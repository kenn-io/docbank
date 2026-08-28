package openaiembed

import (
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/providerhttp"
)

const (
	testWeightsRevision   = "0123456789abcdef0123456789abcdef01234567"
	testTokenizerRevision = "89abcdef0123456789abcdef0123456789abcdef"
)

func TestBGEM3ProfilePinsReviewedDenseContract(t *testing.T) {
	profile, err := BGEM3Profile(reviewedProfileConfig())
	require.NoError(t, err)
	assert.Equal(t, "BAAI/bge-m3", profile.DeploymentContract.ModelFamily)
	assert.Equal(t, "BAAI/bge-m3", profile.DeploymentContract.Tokenizer)
	assert.Equal(t, PoolingCLS, profile.DeploymentContract.Pooling)
	assert.Equal(t, 8192, profile.DeploymentContract.MaxSequenceTokens)
	assert.Equal(t, OutputDenseSingleVector, profile.DeploymentContract.OutputMode)
	assert.Equal(t, DimensionTransformNone, profile.DeploymentContract.DimensionTransform)
	assert.Equal(t, 1024, profile.Descriptor.Dimension)
	assert.Equal(t, document.VectorNormalizationUnitLength, profile.Descriptor.Normalization)
	assert.Equal(t, "passage", profile.ModelInput.EncodeDocument("passage"))
	assert.Equal(t, "question", profile.ModelInput.EncodeQuery("question"))
	_, err = New(profile, nil, &http.Client{})
	require.NoError(t, err)
}

func TestQwen3ProfilePinsReviewedDenseContracts(t *testing.T) {
	for _, testCase := range []struct {
		model     Qwen3Model
		dimension int
	}{
		{Qwen3Embedding06B, 1024},
		{Qwen3Embedding4B, 2560},
		{Qwen3Embedding8B, 4096},
	} {
		t.Run(string(testCase.model), func(t *testing.T) {
			profile, err := Qwen3Profile(Qwen3ProfileConfig{
				ReviewedProfileConfig: reviewedProfileConfig(), Model: testCase.model,
				QueryInstruction: "Retrieve supporting passages",
			})
			require.NoError(t, err)
			assert.Equal(t, string(testCase.model), profile.DeploymentContract.ModelFamily)
			assert.Equal(t, string(testCase.model), profile.DeploymentContract.Tokenizer)
			assert.Equal(t, PoolingLastToken, profile.DeploymentContract.Pooling)
			assert.Equal(t, 32768, profile.DeploymentContract.MaxSequenceTokens)
			assert.Equal(t, testCase.dimension, profile.Descriptor.Dimension)
			assert.Equal(t, "passage", profile.ModelInput.EncodeDocument("passage"))
			assert.Equal(t, "Instruct: Retrieve supporting passages\nQuery:question", profile.ModelInput.EncodeQuery("question"))
			_, err = New(profile, nil, &http.Client{})
			require.NoError(t, err)
		})
	}
}

func TestReviewedProfilesRejectAmbiguousOrMutableIdentity(t *testing.T) {
	for name, build := range map[string]func() error{
		"mutable weights": func() error {
			config := reviewedProfileConfig()
			config.WeightsRevision = "main"
			_, err := BGEM3Profile(config)
			return err
		},
		"mutable tokenizer": func() error {
			config := reviewedProfileConfig()
			config.TokenizerRevision = "main"
			_, err := BGEM3Profile(config)
			return err
		},
		"missing qwen instruction": func() error {
			_, err := Qwen3Profile(Qwen3ProfileConfig{ReviewedProfileConfig: reviewedProfileConfig(), Model: Qwen3Embedding06B})
			return err
		},
		"unknown qwen model": func() error {
			_, err := Qwen3Profile(Qwen3ProfileConfig{ReviewedProfileConfig: reviewedProfileConfig(), Model: "Qwen/Qwen3-Embedding-2B", QueryInstruction: "Retrieve passages"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) { require.Error(t, build()) })
	}
}

func TestDeploymentContractIsFingerprintBoundAndDenseOnly(t *testing.T) {
	base, err := BGEM3Profile(reviewedProfileConfig())
	require.NoError(t, err)
	baseFingerprint := base.Descriptor.PolicyFingerprint

	changed := reviewedProfileConfig()
	changed.TokenizerRevision = "fedcba9876543210fedcba9876543210fedcba98"
	profile, err := BGEM3Profile(changed)
	require.NoError(t, err)
	assert.NotEqual(t, baseFingerprint, profile.Descriptor.PolicyFingerprint)

	for name, mutate := range map[string]func(*DeploymentContract){
		"sparse output":       func(contract *DeploymentContract) { contract.OutputMode = "sparse" },
		"multi-vector output": func(contract *DeploymentContract) { contract.OutputMode = "multi_vector" },
		"wrong pooling":       func(contract *DeploymentContract) { contract.Pooling = PoolingLastToken },
		"dimension transform": func(contract *DeploymentContract) { contract.DimensionTransform = "truncate" },
	} {
		t.Run(name, func(t *testing.T) {
			mutated := base
			contract := *base.DeploymentContract
			mutate(&contract)
			mutated.DeploymentContract = &contract
			_, err := PolicyFingerprint(mutated)
			require.Error(t, err)
		})
	}

	for name, mutate := range map[string]func(*document.EmbeddingDescriptor){
		"metric": func(descriptor *document.EmbeddingDescriptor) { descriptor.Metric = document.VectorMetricDotProduct },
		"normalization": func(descriptor *document.EmbeddingDescriptor) {
			descriptor.Normalization = document.VectorNormalizationNone
		},
	} {
		t.Run(name, func(t *testing.T) {
			mutated := base
			mutated.Descriptor = canonicalMutatedDescriptor(t, base.Descriptor, mutate)
			_, err := PolicyFingerprint(mutated)
			require.Error(t, err)
		})
	}
}

func TestReviewedProfileDoesNotAliasCallerEgressPolicy(t *testing.T) {
	config := reviewedProfileConfig()
	config.Origin = "https://model.local"
	config.EgressPolicy = providerhttp.EgressPolicy{
		Scheme: "https", Host: "model.local", Port: 443,
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("10.1.0.0/16"), netip.MustParsePrefix("10.0.0.0/8")},
		TLS:          providerhttp.TLSPolicy{SPKISHA256: []string{strings.Repeat("b", 64), strings.Repeat("a", 64)}},
	}
	wantCIDRs := slices.Clone(config.EgressPolicy.AllowedCIDRs)
	wantPins := slices.Clone(config.EgressPolicy.TLS.SPKISHA256)

	profile, err := BGEM3Profile(config)
	require.NoError(t, err)
	assert.Equal(t, wantCIDRs, config.EgressPolicy.AllowedCIDRs)
	assert.Equal(t, wantPins, config.EgressPolicy.TLS.SPKISHA256)

	config.EgressPolicy.AllowedCIDRs[0] = netip.MustParsePrefix("192.0.2.0/24")
	config.EgressPolicy.TLS.SPKISHA256[0] = strings.Repeat("c", 64)
	assert.NotEqual(t, config.EgressPolicy.AllowedCIDRs, profile.EgressPolicy.AllowedCIDRs)
	assert.NotEqual(t, config.EgressPolicy.TLS.SPKISHA256, profile.EgressPolicy.TLS.SPKISHA256)
	_, err = New(profile, nil, &http.Client{})
	require.NoError(t, err)
}

func TestLegacyGenericProfileFingerprintIsStable(t *testing.T) {
	profile := testProfile(t, modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic}))
	assert.Equal(t, "8c5ca6d380549ccb86894384cd006d136481968ea3b84a5a762d3054c1d46848", profile.Descriptor.PolicyFingerprint)
}

func reviewedProfileConfig() ReviewedProfileConfig {
	return ReviewedProfileConfig{
		Origin: "http://127.0.0.1:11434", ServedModel: "operator-model", DeploymentEpoch: "deployment-2026-08-28",
		WeightsRevision: testWeightsRevision, TokenizerRevision: testTokenizerRevision,
	}
}
