package voyage_test

import (
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/voyage"
	"go.kenn.io/docbank/document/voyage/voyagetest"
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

func TestVoyageTextAllowsMissingNullOrStringText(t *testing.T) {
	for _, target := range []struct {
		model     string
		dimension int
	}{{voyage.TextModel, 256}, {"voyage-4-large", 1024}} {
		for _, test := range []struct {
			name, extra string
			valid       bool
		}{
			{"missing text", ``, true}, {"string text", `,"text":"document envelope: passage"`, true},
			{"null text", `,"text":null`, true}, {"number text", `,"text":42`, false},
			{"boolean text", `,"text":false`, false}, {"object text", `,"text":{}`, false},
			{"array text", `,"text":[]`, false}, {"duplicate text", `,"text":"a","text":"b"`, false},
			{"duplicate null text", `,"text":null,"text":null`, false}, {"unknown", `,"unreviewed":"synthetic"`, false},
		} {
			t.Run(target.model+"/"+test.name, func(t *testing.T) {
				vector := make([]float32, target.dimension)
				vector[1] = 1
				item, err := json.Marshal(map[string]any{"object": "embedding", "embedding": vector, "index": 0})
				require.NoError(t, err)
				item = append(item[:len(item)-1], test.extra+"}"...)
				body := fmt.Sprintf(`{"object":"list","data":[%s],"model":%q,"usage":{"total_tokens":2}}`, item, target.model)
				// The malformed-response retry backs off on fake time.
				synctest.Test(t, func(t *testing.T) {
					profile := voyageTextProfile(t, voyage.EmbeddingModeText)
					profile.Descriptor.Model, profile.Descriptor.Dimension = target.model, target.dimension
					profile = refingerprintVoyageProfile(t, profile)
					provider := newVoyageMemoryProvider(t, profile, embeddingSecrets{"credential:voyage": "synthetic-secret"}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(body))
					}))
					result, err := provider.Embed(t.Context(), []document.EmbeddingInput{{Key: "chunk", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "passage"}}, voyageAuthorization(profile.Descriptor))
					if test.valid {
						require.NoError(t, err)
						require.Len(t, result.Vectors, 1)
						assert.Equal(t, vector, result.Vectors[0].Values)
					} else {
						require.ErrorIs(t, err, voyage.ErrMalformedResponse)
					}
				})
			})
		}
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
			// The malformed-response retry backs off on fake time.
			synctest.Test(t, func(t *testing.T) {
				provider := newVoyageMemoryProvider(t, profile, embeddingSecrets{"credential:voyage": "synthetic-secret"}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					values := unitEmbedding(1)
					values[1] = scale
					w.Header().Set("Content-Type", "application/json")
					assert.NoError(t, json.MarshalWrite(w, map[string]any{"object": "list", "model": profile.Descriptor.Model, "data": []map[string]any{{"object": "embedding", "index": 0, "embedding": values, "text": "passage"}}}))
				}))
				result, err := provider.Embed(t.Context(), []document.EmbeddingInput{{Key: "chunk", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "passage"}}, voyageAuthorization(profile.Descriptor))
				if scale == 1 || scale == 1.00001 {
					require.NoError(t, err)
					assert.InDelta(t, scale, result.Vectors[0].Values[1], 0)
				} else {
					require.ErrorIs(t, err, voyage.ErrMalformedResponse)
				}
			})
		})
	}
}

func TestVoyageDeploymentEpochIsTextOnly(t *testing.T) {
	policy := testPolicy(t)
	manifest, err := voyagetest.SyntheticManifest(policy)
	require.NoError(t, err)
	for name, profile := range map[string]voyage.EmbeddingProfile{
		"contextual":  voyageTextProfile(t, voyage.EmbeddingModeContextual),
		"direct file": voyageDirectFileProfile(t, policy, manifest),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := voyage.EmbeddingPolicyFingerprint(profile)
			require.NoError(t, err, "the export-only profile must be valid before the epoch is added")
			profile.DeploymentEpoch = "synthetic-v1"
			_, err = voyage.EmbeddingPolicyFingerprint(profile)
			require.ErrorContains(t, err, "deployment epoch is only supported for text retrieval")
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
