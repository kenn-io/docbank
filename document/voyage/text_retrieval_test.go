package voyage_test

import (
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/voyage"
)

func TestVoyageRetainedTextDocumentAndQuery(t *testing.T) {
	for _, model := range []string{"voyage-3.5", "voyage-4-large"} {
		t.Run(model, func(t *testing.T) {
			var roles []string
			endpoint, egress, resolver := voyageFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request voyageTextTestRequest
				if !assert.NoError(t, json.UnmarshalRead(r.Body, &request, json.RejectUnknownMembers(true))) {
					http.Error(w, "invalid synthetic request", http.StatusBadRequest)
					return
				}
				assert.Equal(t, "/v1/embeddings", r.URL.Path)
				assert.Equal(t, "Bearer synthetic-secret", r.Header.Get("Authorization"))
				// Strict request decoding excludes encoding_format.
				assert.False(t, request.Truncation)
				assert.Equal(t, 256, request.OutputDimension)
				assert.Equal(t, "float", request.OutputDType)
				assert.Equal(t, model, request.Model)
				role := request.InputType
				roles = append(roles, role)
				inputs := request.Input
				indices, hot := make([]int, len(inputs)), make([]int, len(inputs))
				for i := range inputs {
					indices[i] = len(inputs) - 1 - i
					hot[i] = indices[i] + 1
				}
				var response struct {
					Object string           `json:"object"`
					Model  string           `json:"model"`
					Data   []map[string]any `json:"data"`
					Usage  map[string]any   `json:"usage"`
				}
				if !assert.NoError(t, json.Unmarshal(voyageTextBody(t, model, indices, hot), &response)) {
					http.Error(w, "invalid synthetic response", http.StatusInternalServerError)
					return
				}
				for i, index := range indices {
					response.Data[i]["text"] = inputs[index]
				}

				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.MarshalWrite(w, response))
			}))
			profile := voyageTextProfile(t, voyage.EmbeddingModeText)
			profile.Endpoint, profile.EgressPolicy = endpoint, egress
			profile.DeploymentEpoch = "synthetic-text-v1"
			profile.Descriptor.Model = model
			profile.Descriptor.ModelRevision = profile.DeploymentEpoch
			profile.Descriptor.SupportsTextQuery = true
			profile = refingerprintVoyageProfile(t, profile)
			provider, err := newVoyageEmbeddingTestProvider(t, profile, embeddingSecrets{"credential:voyage": "synthetic-secret"}, resolver)
			require.NoError(t, err)
			inputs := []document.EmbeddingInput{
				{Key: "chunk-1", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "# Solar\nPanel maintenance"},
				{Key: "question", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "maintenance"},
				{Key: "chunk-2", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "Annual inspection"},
			}
			result, err := document.ExecuteEmbedding(t.Context(), provider, inputs, voyageAuthorization(profile.Descriptor))
			require.NoError(t, err)
			assert.Equal(t, []string{"document", "query"}, roles)
			require.Len(t, result.Vectors, 3)
			for i, hot := range []int{1, 1, 2} {
				assert.Equal(t, inputs[i].Key, result.Vectors[i].Key)
				assert.Equal(t, unitEmbedding(hot), result.Vectors[i].Values)
			}
		})
	}
}

func TestVoyageTextAllowsOnlyOptionalStringText(t *testing.T) {
	for _, test := range []struct {
		name, extra string
		valid       bool
	}{
		{"omitted", "", true}, {"text", `,"text":"document envelope: passage"`, true},
		{"unknown", `,"unreviewed":"synthetic"`, false}, {"wrong type", `,"text":42`, false},
		{"null text", `,"text":null`, false}, {"duplicate text", `,"text":"a","text":"b"`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := string(voyageTextBody(t, voyage.TextModel, []int{0}, []int{1}))
			body = strings.Replace(body, `"index":0`, `"index":0`+test.extra, 1)
			endpoint, egress, resolver := voyageFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			profile := voyageTextProfile(t, voyage.EmbeddingModeText)
			profile.Endpoint, profile.EgressPolicy = endpoint, egress
			profile = refingerprintVoyageProfile(t, profile)
			provider, err := newVoyageEmbeddingTestProvider(t, profile, embeddingSecrets{"credential:voyage": "synthetic-secret"}, resolver)
			require.NoError(t, err)
			_, err = provider.Embed(t.Context(), []document.EmbeddingInput{{Key: "chunk", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "passage"}}, voyageAuthorization(profile.Descriptor))
			if test.valid {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, voyage.ErrMalformedResponse)
			}
		})
	}
}

func TestVoyageTextRetrievalIdentityAndNormalization(t *testing.T) {
	profile := voyageTextProfile(t, voyage.EmbeddingModeText)
	profile.DeploymentEpoch = "synthetic-v1"
	profile.Descriptor.ModelRevision = profile.DeploymentEpoch
	profile.Descriptor.SupportsTextQuery = true
	profile = refingerprintVoyageProfile(t, profile)
	for name, mutate := range map[string]func(*voyage.EmbeddingProfile){
		"epoch mismatch": func(p *voyage.EmbeddingProfile) { p.DeploymentEpoch = "other" },
		"export alias as epoch": func(p *voyage.EmbeddingProfile) {
			p.DeploymentEpoch = voyage.HostedAliasRevision
			p.Descriptor.ModelRevision = p.DeploymentEpoch
		},
		"missing query support": func(p *voyage.EmbeddingProfile) { p.Descriptor.SupportsTextQuery = false },
		"contextual epoch":      func(p *voyage.EmbeddingProfile) { p.Mode = voyage.EmbeddingModeContextual },
		"non-unit contract":     func(p *voyage.EmbeddingProfile) { p.Descriptor.Normalization = document.VectorNormalizationNone },
		"dimensions":            func(p *voyage.EmbeddingProfile) { p.Descriptor.Dimension = 768 },
	} {
		t.Run(name, func(t *testing.T) {
			changed := profile
			mutate(&changed)
			_, err := voyage.EmbeddingPolicyFingerprint(changed)
			require.Error(t, err)
		})
	}
	changed := profile
	changed.DeploymentEpoch = "synthetic-v2"
	changed.Descriptor.ModelRevision = changed.DeploymentEpoch
	changed = refingerprintVoyageProfile(t, changed)
	assert.NotEqual(t, profile.Descriptor.PolicyFingerprint, changed.Descriptor.PolicyFingerprint)
	assert.NotEqual(t, profile.Descriptor.Fingerprint, changed.Descriptor.Fingerprint)
	for _, scale := range []float32{1, 1.00001, 2, 0} {
		t.Run(fmt.Sprint(scale), func(t *testing.T) {
			endpoint, egress, resolver := voyageFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				values := unitEmbedding(1)
				values[1] = scale
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.MarshalWrite(w, map[string]any{"object": "list", "model": profile.Descriptor.Model, "data": []map[string]any{{"object": "embedding", "index": 0, "embedding": values, "text": "passage"}}}))
			}))
			local := profile
			local.Endpoint, local.EgressPolicy = endpoint, egress
			local = refingerprintVoyageProfile(t, local)
			provider, err := newVoyageEmbeddingTestProvider(t, local, embeddingSecrets{"credential:voyage": "synthetic-secret"}, resolver)
			require.NoError(t, err)
			result, err := provider.Embed(t.Context(), []document.EmbeddingInput{{Key: "chunk", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "passage"}}, voyageAuthorization(local.Descriptor))
			if scale == 1 || scale == 1.00001 {
				require.NoError(t, err)
				assert.InDelta(t, scale, result.Vectors[0].Values[1], 0)
			} else {
				require.ErrorIs(t, err, voyage.ErrMalformedResponse)
			}
		})
	}
}

// The native text adapter sends only these members; in particular, decoding
// rejects encoding_format rather than accepting a Voyage-incompatible value.
type voyageTextTestRequest struct {
	Input           []string `json:"input"`
	Model           string   `json:"model"`
	InputType       string   `json:"input_type"`
	Truncation      bool     `json:"truncation"`
	OutputDimension int      `json:"output_dimension"`
	OutputDType     string   `json:"output_dtype"`
}
