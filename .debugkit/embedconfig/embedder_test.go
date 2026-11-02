package embedconfig_test

import (
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
	"go.kenn.io/kit/secretref"
)

func TestEmbedderDecodesTheStandardKeys(t *testing.T) {
	var file struct {
		Search struct {
			Embeddings embedconfig.Embedder `toml:"embeddings"`
		} `toml:"search"`
	}
	meta, err := toml.Decode(`
[search.embeddings]
base_url = "https://api.example.test/v1"
model = "embed-large"
dims = 1024
api_key = { env = "EMBED_KEY" }
fingerprint_salt = "weights-2"
input_type_mode = "retrieval"
batch_size = 16
model_context_tokens = 512
max_batch_tokens = 8192
timeout_seconds = 45
trust_private_network = true
`, &file)
	require.NoError(t, err)
	assert.Empty(t, meta.Undecoded())

	embedder := file.Search.Embeddings
	require.NoError(t, embedder.Validate())
	assert.Equal(t, secretref.Ref{Env: "EMBED_KEY"}, embedder.APIKey)
	parts, err := embedder.Parts()
	require.NoError(t, err)
	assert.Equal(t, embedconfig.Model{
		Name: "embed-large", Revision: "weights-2", Dimensions: 1024,
		Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationL2,
	}, parts.Model)
	assert.Equal(t, embedconfig.InputTypeRetrieval, parts.Roles.InputType)
	assert.Equal(t, embedconfig.Deployment{
		BaseURL: "https://api.example.test/v1", TrustPrivateNetwork: true,
	}, parts.Deployment)
	assert.Equal(t, embedconfig.Batch{Items: 16, MaxTokens: 8192, InputTokenUpperBound: 512}, parts.Batch)
	assert.Equal(t, 45*time.Second, parts.Transport.Timeout)
}

func TestEmbedderLiteralRoleSettings(t *testing.T) {
	var embedder embedconfig.Embedder
	meta, err := toml.Decode(`
base_url = "https://api.example.test/v1"
model = "embed-text"
dims = 768
document_prefix = "title: none | text: "
document_suffix = " \n"
query_prefix = "task: search result | query: "
query_suffix = "\t "
request_dimensions = true
`, &embedder)
	require.NoError(t, err)
	assert.Empty(t, meta.Undecoded())
	require.NoError(t, embedder.Validate())
	parts, err := embedder.Parts()
	require.NoError(t, err)
	assert.True(t, parts.Model.RequestDimensions)
	assert.Equal(t, 768, parts.Model.Dimensions)
	for _, test := range []struct {
		role embedconfig.Role
		want string
	}{
		{embedconfig.RoleDocument, "title: none | text: hello \n"},
		{embedconfig.RoleQuery, "task: search result | query: hello\t "},
	} {
		text, err := embedmodel.Format(test.role, "hello", parts.Roles)
		require.NoError(t, err)
		assert.Equal(t, test.want, text)
	}
}

func TestEmbedderRoleSettingsRequireEndpoint(t *testing.T) {
	for _, setting := range []string{
		`document_prefix = " "`, `document_suffix = " "`,
		`query_prefix = " "`, `query_suffix = " "`, `request_dimensions = true`,
	} {
		t.Run(setting, func(t *testing.T) {
			var embedder embedconfig.Embedder
			_, err := toml.Decode(setting, &embedder)
			require.NoError(t, err)
			assert.Error(t, embedder.Validate())
		})
	}
}

func TestEmbedderPreservesDefaultIdentities(t *testing.T) {
	const oldIdentity = "75da3207794697849064648e84a8e02dd2f873ea3690159bfc067972f0e1bb37"
	const config = `base_url = "https://api.example.test/v1"
model = "embed-text"
dims = 768
`
	for _, settings := range []string{"", `document_prefix = ""
document_suffix = ""
query_prefix = ""
query_suffix = ""
request_dimensions = false
`} {
		desc := embedderDescriptor(t, config+settings)
		space, err := desc.VectorIdentity()
		require.NoError(t, err)
		assert.Equal(t, oldIdentity, space)
		input, err := desc.InputIdentity()
		require.NoError(t, err)
		assert.Equal(t, oldIdentity, input)
		gen, err := desc.Generation()
		require.NoError(t, err)
		assert.Equal(t, "ea14a3d46851bf40", gen.Fingerprint())
		assert.False(t, desc.Model.RequestDimensions)
	}
	for _, settings := range []string{
		`document_prefix = " "`, `document_suffix = " "`,
		`query_prefix = " "`, `query_suffix = " "`,
		`request_dimensions = true`, `fingerprint_salt = "weights-2"`,
	} {
		t.Run(settings, func(t *testing.T) {
			desc := embedderDescriptor(t, config+settings)
			space, err := desc.VectorIdentity()
			require.NoError(t, err)
			assert.NotEqual(t, oldIdentity, space)
			input, err := desc.InputIdentity()
			require.NoError(t, err)
			assert.NotEqual(t, oldIdentity, input)
			gen, err := desc.Generation()
			require.NoError(t, err)
			assert.NotEqual(t, "ea14a3d46851bf40", gen.Fingerprint())
		})
	}
}

func embedderDescriptor(t *testing.T, config string) embedmodel.Descriptor {
	t.Helper()
	var embedder embedconfig.Embedder
	_, err := toml.Decode(config, &embedder)
	require.NoError(t, err)
	parts, err := embedder.Parts()
	require.NoError(t, err)
	return embedmodel.Descriptor{Model: parts.Model, Roles: parts.Roles, Deployment: parts.Deployment}
}

func FuzzEmbedderLiteralAffixes(f *testing.F) {
	f.Add("title: none | text: ", "\n", "task: search result | query: ", " ", "hello")
	f.Add("", "", "", "", "")
	f.Add("\x00\n=\\", " \t", "é", "\xff", "文")
	f.Fuzz(func(t *testing.T, documentPrefix, documentSuffix, queryPrefix, querySuffix, text string) {
		parts, err := embedconfig.Embedder{
			BaseURL: "https://api.example.test/v1", Model: "embed-text", Dims: 768,
			DocumentPrefix: documentPrefix, DocumentSuffix: documentSuffix,
			QueryPrefix: queryPrefix, QuerySuffix: querySuffix,
		}.Parts()
		require.NoError(t, err)
		for _, test := range []struct {
			role embedconfig.Role
			want string
		}{
			{embedconfig.RoleDocument, documentPrefix + text + documentSuffix},
			{embedconfig.RoleQuery, queryPrefix + text + querySuffix},
		} {
			got, err := embedmodel.Format(test.role, text, parts.Roles)
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		}
	})
}

func FuzzEmbedderAffixIdentities(f *testing.F) {
	f.Add("", "", "", "", false)
	f.Add("\n=\\", "\x00", "query: ", "\xff", true)
	f.Fuzz(func(t *testing.T, documentPrefix, documentSuffix, queryPrefix, querySuffix string, requestDimensions bool) {
		embedder := embedconfig.Embedder{
			BaseURL: "https://api.example.test/v1", Model: "embed-text", Dims: 768,
			DocumentPrefix: documentPrefix, DocumentSuffix: documentSuffix,
			QueryPrefix: queryPrefix, QuerySuffix: querySuffix, RequestDimensions: requestDimensions,
		}
		identity := func(e embedconfig.Embedder) string {
			parts, err := e.Parts()
			require.NoError(t, err)
			got, err := (embedmodel.Descriptor{Model: parts.Model, Roles: parts.Roles}).VectorIdentity()
			require.NoError(t, err)
			return got
		}
		original := identity(embedder)
		for _, change := range []func(*embedconfig.Embedder){
			func(e *embedconfig.Embedder) { e.DocumentPrefix += " " },
			func(e *embedconfig.Embedder) { e.DocumentSuffix += " " },
			func(e *embedconfig.Embedder) { e.QueryPrefix += " " },
			func(e *embedconfig.Embedder) { e.QuerySuffix += " " },
			func(e *embedconfig.Embedder) { e.RequestDimensions = !e.RequestDimensions },
			func(e *embedconfig.Embedder) { e.Dims = 512 },
		} {
			changed := embedder
			change(&changed)
			assert.NotEqual(t, original, identity(changed))
		}
	})
}

func TestEmbedderPartsFillOnlyOperationalDefaults(t *testing.T) {
	parts, err := embedconfig.Embedder{
		BaseURL: "http://127.0.0.1:11434/v1", Model: "nomic", Dims: 768,
	}.Parts()
	require.NoError(t, err)
	assert.Equal(t, embedconfig.DefaultBatchItems, parts.Batch.Items)
	assert.Equal(t, embedconfig.DefaultTimeout, parts.Transport.Timeout)
	assert.Equal(t, embedconfig.InputTypeNone, parts.Roles.InputType)
	assert.False(t, parts.Deployment.PinEndpoint)
}

func TestEmbedderValidate(t *testing.T) {
	valid := embedconfig.Embedder{BaseURL: "https://api.example.test/v1", Model: "m", Dims: 8}
	with := func(change func(*embedconfig.Embedder)) embedconfig.Embedder {
		embedder := valid
		change(&embedder)
		return embedder
	}
	tests := []struct {
		name     string
		embedder embedconfig.Embedder
		valid    bool
	}{
		{name: "disabled", embedder: embedconfig.Embedder{}, valid: true},
		{name: "complete", embedder: valid, valid: true},
		{name: "setting without endpoint", embedder: embedconfig.Embedder{BatchSize: 8}},
		{name: "missing dims", embedder: with(func(e *embedconfig.Embedder) { e.Dims = 0 })},
		{name: "missing model", embedder: with(func(e *embedconfig.Embedder) { e.Model = "" })},
		{name: "two key sources", embedder: with(func(e *embedconfig.Embedder) {
			e.APIKey = secretref.Ref{Env: "EMBED_KEY", File: "~/embed.key"}
		})},
		{name: "unknown input type", embedder: with(func(e *embedconfig.Embedder) { e.InputTypeMode = "search" })},
		{name: "negative batch", embedder: with(func(e *embedconfig.Embedder) { e.BatchSize = -1 })},
		{name: "negative timeout", embedder: with(func(e *embedconfig.Embedder) { e.TimeoutSeconds = -1 })},
		{name: "half a token budget", embedder: with(func(e *embedconfig.Embedder) { e.MaxBatchTokens = 8192 })},
		{name: "plaintext public endpoint", embedder: with(func(e *embedconfig.Embedder) {
			e.BaseURL = "http://api.example.test/v1"
		})},
		{name: "plaintext trusted private endpoint", embedder: with(func(e *embedconfig.Embedder) {
			e.BaseURL, e.TrustPrivateNetwork = "http://10.1.2.3:8080/v1", true
		}), valid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.embedder.Validate()
			if test.valid {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
		})
	}
}

func TestEmbedderResolvesItsAPIKeyReference(t *testing.T) {
	t.Setenv("KIT_TEST_EMBED_KEY", "from-env")
	secret, err := embedconfig.Embedder{APIKey: secretref.Ref{Env: "KIT_TEST_EMBED_KEY"}}.ResolveAPIKey()
	require.NoError(t, err)
	assert.Equal(t, secretref.Secret{Value: "from-env", Source: "env:KIT_TEST_EMBED_KEY"}, secret)

	secret, err = embedconfig.Embedder{}.ResolveAPIKey()
	require.NoError(t, err)
	assert.Equal(t, secretref.Secret{}, secret, "an endpoint without authentication needs no key")

	t.Setenv("KIT_TEST_EMBED_KEY", "")
	_, err = embedconfig.Embedder{APIKey: secretref.Ref{Env: "KIT_TEST_EMBED_KEY"}}.ResolveAPIKey()
	require.ErrorContains(t, err, "embed api_key")
	require.ErrorContains(t, err, "KIT_TEST_EMBED_KEY")

	invalid := embedconfig.Embedder{
		BaseURL: "https://api.example.test/v1", Model: "m", Dims: 8,
		APIKey: secretref.Ref{Env: "EMBED_KEY", File: "~/embed.key"},
	}
	require.ErrorContains(t, invalid.Validate(), "embed api_key")
	_, err = invalid.ResolveAPIKey()
	require.Error(t, err)
}
