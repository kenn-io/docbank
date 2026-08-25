package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	c, err := Load(t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", c.Server.BindAddr)
	assert.Equal(t, 0, c.Server.APIPort)
	assert.Empty(t, c.Server.APIKey)
	assert.Equal(t, 30*time.Minute, c.Server.IdleTimeout.Std())
	assert.True(t, c.Web.Enabled)
	assert.Empty(t, c.Backup.Repo)
	assert.Zero(t, c.Backup.ZstdLevel)
	assert.Zero(t, c.Storage.PackInterval.Std())
	assert.Equal(t, int64(256<<20), c.Storage.PackMaxBytes)
	assert.NotNil(t, c.RenditionProfiles)
	assert.NotNil(t, c.EmbeddingProfiles)
	assert.NotNil(t, c.RetrievalProfiles)
	assert.NotNil(t, c.ProcessingProfiles)
}

func TestLoadParsesFile(t *testing.T) {
	dir := privateTestConfigDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(
		"[server]\nbind_addr = \"127.0.0.1\"\napi_port = 8080\napi_key = \"k\"\n"+
			"idle_timeout = \"0\"\n[web]\nenabled = false\n[backup]\nrepo = \"snapshots\"\nzstd_level = 9\n"+
			"[storage]\npack_interval = \"1h\"\npack_max_bytes = 1048576\n"), 0o600))
	c, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, 8080, c.Server.APIPort)
	assert.Equal(t, "k", c.Server.APIKey)
	assert.Equal(t, time.Duration(0), c.Server.IdleTimeout.Std())
	assert.False(t, c.Web.Enabled)
	assert.Equal(t, filepath.Join(dir, "snapshots"), c.Backup.Repo)
	assert.Equal(t, 9, c.Backup.ZstdLevel)
	assert.Equal(t, time.Hour, c.Storage.PackInterval.Std())
	assert.Equal(t, int64(1<<20), c.Storage.PackMaxBytes)
}

func TestLoadExpandsBackupRepoHome(t *testing.T) {
	dir := privateTestConfigDir(t)
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"),
		[]byte("[backup]\nrepo = \"~/backups\"\n"), 0o600))
	c, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "backups"), c.Backup.Repo)
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	dir := privateTestConfigDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"),
		[]byte("[server]\nbindaddr = \"x\"\n"), 0o600))
	_, err := Load(dir)
	require.ErrorContains(t, err, "bindaddr")
}

func TestLoadPartialFileKeepsDefaults(t *testing.T) {
	dir := privateTestConfigDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"),
		[]byte("[server]\napi_port = 8080\n"), 0o600))
	c, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, 8080, c.Server.APIPort)
	assert.Equal(t, "127.0.0.1", c.Server.BindAddr)
	assert.Equal(t, 30*time.Minute, c.Server.IdleTimeout.Std())
	assert.True(t, c.Web.Enabled)
}

func TestProcessingProfilesLoadNamedBindingsWithoutResolvingSecrets(t *testing.T) {
	dir := privateTestConfigDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`
[rendition_profiles.primary]
adapter_contract = "mistral-ocr-adapter/v1"
authorization_fingerprint = "2222222222222222222222222222222222222222222222222222222222222222"
descriptor_id = "mistral-ocr-v1"
descriptor_fingerprint = "1111111111111111111111111111111111111111111111111111111111111111"
credential_binding = "credential:ocr-primary"
deployment_fingerprint = "3333333333333333333333333333333333333333333333333333333333333333"
disclose_filename = true
disclosure_fingerprint = "4444444444444444444444444444444444444444444444444444444444444444"
max_document_bytes = 10485760
max_response_bytes = 4194304
max_units = 256
requested_artifacts = ["provider_markdown", "structured_evidence"]
trust_boundary = "processor-primary"
upload_options_fingerprint = "5555555555555555555555555555555555555555555555555555555555555555"

[embedding_profiles.semantic]
activation = "required"
authorization_fingerprint = "6666666666666666666666666666666666666666666666666666666666666666"
compatibility_id = "voyage-3-large/1024"
descriptor_id = "voyage-text-v1"
descriptor_fingerprint = "7777777777777777777777777777777777777777777777777777777777777777"
credential_binding = "credential:embedding-primary"
dimensions = 1024
disclosure_fingerprint = "8888888888888888888888888888888888888888888888888888888888888888"
document_formatter = "document/v1"
model = "voyage-3-large"
input_kind = "rendition_chunk"
max_batch_items = 32
max_input_bytes = 1048576
max_response_bytes = 1048576
metric = "cosine"
normalization = "unit_length"
query_formatter = "query/v1"
scalar_encoding = "float32"
trust_boundary = "processor-primary"

[embedding_profiles.semantic.chunk]
context_fingerprint = "9999999999999999999999999999999999999999999999999999999999999999"
formatter = "rendition-chunk/v1"
max_tokens = 800
overlap_tokens = 80
tokenizer = "voyage-3"
truncation_policy = "reject"

[retrieval_profiles.hybrid]
lexical_limit = 40
vector_limit = 60

[processing_profiles.archive]
rendition = "primary"
embeddings = ["semantic"]
retrieval = "hybrid"
attachment_policy_fingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
completeness_fingerprint = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
consent_fingerprint = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
lexical_segmenter_fingerprint = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
max_document_chars = 100000
max_segment_runes = 2000
max_unit_runes = 100000
normalizer_fingerprint = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
sanitizer_fingerprint = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
retain_sanitized_markdown = true
retain_typed_artifacts = true
trust_boundary = "vault-primary"
`), 0o600))

	cfg, err := Load(dir)
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	assert.Equal(t, "credential:ocr-primary", cfg.RenditionProfiles["primary"].CredentialBinding)
	assert.Equal(t, "credential:embedding-primary", cfg.EmbeddingProfiles["semantic"].CredentialBinding)
	assert.Equal(t, []string{"semantic"}, cfg.ProcessingProfiles["archive"].Embeddings)
	assert.Equal(t, 40, cfg.RetrievalProfiles["hybrid"].LexicalLimit)

	resolved, err := cfg.ProcessingProfile("archive")
	require.NoError(t, err)
	assert.Equal(t, "hybrid", resolved.RetrievalName)
	assert.Equal(t, 40, resolved.Retrieval.LexicalLimit())
	assert.Equal(t, 60, resolved.Retrieval.VectorLimit())
	assert.Equal(t, document.RetrievalPolicyV1{LexicalLimit: 40, VectorLimit: 60}, resolved.Document.Retrieval)
	_, _, err = document.CanonicalProfile(resolved.Document)
	require.NoError(t, err)
}

func TestProcessingProfileReturnsCanonicalPolicy(t *testing.T) {
	cfg := validProcessingConfig()
	embedding := cfg.EmbeddingProfiles["semantic"]
	embedding.Model = "model-Cafe\u0301\r\nrevision"
	cfg.EmbeddingProfiles["semantic"] = embedding

	profile, err := cfg.ProcessingProfile("archive")
	require.NoError(t, err)
	assert.Equal(t, "model-Café\nrevision", profile.Document.Embeddings[0].Model)
}

func TestProcessingProfileRejectsUnsupportedRetrievalLimit(t *testing.T) {
	cfg := validProcessingConfig()
	retrieval := cfg.RetrievalProfiles["hybrid"]
	retrieval.LexicalLimit = 1_001
	cfg.RetrievalProfiles["hybrid"] = retrieval

	_, err := cfg.ProcessingProfile("archive")
	require.ErrorContains(t, err, "lexical_limit")
}

func TestProcessingProfileRejectsOriginalFileChunkPolicy(t *testing.T) {
	cfg := validProcessingConfig()
	embedding := cfg.EmbeddingProfiles["semantic"]
	embedding.InputKind = string(document.EmbeddingInputOriginalFile)
	cfg.EmbeddingProfiles["semantic"] = embedding

	_, err := cfg.ProcessingProfile("archive")
	require.ErrorContains(t, err, "original_file")
}

func TestProcessingProfilesRejectUnknownKeys(t *testing.T) {
	for _, key := range []string{"unknown_policy", "api_key"} {
		t.Run(key, func(t *testing.T) {
			dir := privateTestConfigDir(t)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(
				"[rendition_profiles.primary]\n"+key+" = \"sk-test-do-not-persist\"\n"), 0o600))
			_, err := Load(dir)
			require.ErrorContains(t, err, key)
		})
	}
}

func TestProcessingProfilesRejectInvalidReferencesAndPolicies(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"unknown rendition", func(c *Config) {
			c.ProcessingProfiles["archive"] = withProcessing(c.ProcessingProfiles["archive"], func(p *ProcessingProfileConfig) { p.Rendition = "missing" })
		}, "rendition"},
		{"unknown embedding", func(c *Config) {
			c.ProcessingProfiles["archive"] = withProcessing(c.ProcessingProfiles["archive"], func(p *ProcessingProfileConfig) { p.Embeddings = []string{"missing"} })
		}, "embedding"},
		{"unknown retrieval", func(c *Config) {
			c.ProcessingProfiles["archive"] = withProcessing(c.ProcessingProfiles["archive"], func(p *ProcessingProfileConfig) { p.Retrieval = "missing" })
		}, "retrieval"},
		{"duplicate embedding", func(c *Config) {
			c.ProcessingProfiles["archive"] = withProcessing(c.ProcessingProfiles["archive"], func(p *ProcessingProfileConfig) { p.Embeddings = []string{"semantic", "semantic"} })
		}, "duplicated"},
		{"chunk without rendition", func(c *Config) {
			c.ProcessingProfiles["archive"] = withProcessing(c.ProcessingProfiles["archive"], func(p *ProcessingProfileConfig) { p.Rendition = ""; p.RetainSanitizedMarkdown = false })
		}, "rendition_chunk"},
		{"markdown without rendition", func(c *Config) {
			c.ProcessingProfiles["archive"] = withProcessing(c.ProcessingProfiles["archive"], func(p *ProcessingProfileConfig) { p.Rendition = ""; p.Embeddings = nil })
		}, "retained Markdown"},
		{"retrieval limit zero", func(c *Config) {
			p := c.RetrievalProfiles["hybrid"]
			p.VectorLimit = 0
			c.RetrievalProfiles["hybrid"] = p
		}, "vector_limit"},
		{"retrieval limit above maximum", func(c *Config) {
			p := c.RetrievalProfiles["hybrid"]
			p.LexicalLimit = 1_001
			c.RetrievalProfiles["hybrid"] = p
		}, "lexical_limit"},
		{"raw rendition secret", func(c *Config) {
			p := c.RenditionProfiles["primary"]
			p.CredentialBinding = "sk-test-do-not-persist"
			c.RenditionProfiles["primary"] = p
		}, "credential:"},
		{"raw embedding secret", func(c *Config) {
			p := c.EmbeddingProfiles["semantic"]
			p.CredentialBinding = "sk-test-do-not-persist"
			c.EmbeddingProfiles["semantic"] = p
		}, "credential:"},
		{"unknown embedding metric", func(c *Config) {
			p := c.EmbeddingProfiles["semantic"]
			p.Metric = "provider_default"
			c.EmbeddingProfiles["semantic"] = p
		}, "metric"},
		{"unreferenced invalid embedding", func(c *Config) {
			p := c.EmbeddingProfiles["semantic"]
			p.MaxBatchItems = 10_001
			c.EmbeddingProfiles["disabled"] = p
		}, "max batch items"},
		{"unreferenced rendition with unknown artifact role", func(c *Config) {
			p := c.RenditionProfiles["primary"]
			p.RequestedArtifacts = []string{"unknown"}
			c.RenditionProfiles["disabled"] = p
		}, "unknown"},
		{"unreferenced rendition with duplicate artifact role", func(c *Config) {
			p := c.RenditionProfiles["primary"]
			p.RequestedArtifacts = []string{"structured_evidence", "structured_evidence"}
			c.RenditionProfiles["disabled"] = p
		}, "duplicated"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validProcessingConfig()
			test.mutate(&cfg)
			require.ErrorContains(t, cfg.Validate(), test.want)
		})
	}
}

func validProcessingConfig() Config {
	return Config{
		Server: Default().Server, Web: Default().Web, Storage: Default().Storage,
		RenditionProfiles: map[string]RenditionProfileConfig{"primary": {
			AdapterContract: "adapter/v1", AuthorizationFingerprint: strings.Repeat("1", 64), CredentialBinding: "credential:ocr-primary",
			DeploymentFingerprint: strings.Repeat("2", 64), DescriptorID: "ocr-v1", DescriptorFingerprint: strings.Repeat("3", 64),
			DisclosureFingerprint: strings.Repeat("4", 64), MaxDocumentBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxUnits: 10,
			RequestedArtifacts: []string{"structured_evidence"}, TrustBoundary: "processor", UploadOptionsFingerprint: strings.Repeat("5", 64),
		}},
		EmbeddingProfiles: map[string]EmbeddingProfileConfig{"semantic": {
			Activation: "required", AuthorizationFingerprint: strings.Repeat("6", 64), CompatibilityID: "model/8",
			CredentialBinding: "credential:embedding-primary", DescriptorID: "embedding-v1", DescriptorFingerprint: strings.Repeat("7", 64),
			Dimensions: 8, DisclosureFingerprint: strings.Repeat("8", 64), DocumentFormatter: "document/v1", InputKind: "rendition_chunk",
			MaxBatchItems: 8, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20, Metric: "cosine", Model: "model",
			Normalization: "unit_length", QueryFormatter: "query/v1", ScalarEncoding: "float32", TrustBoundary: "processor",
			Chunk: EmbeddingChunkConfig{ContextFingerprint: strings.Repeat("9", 64), Formatter: "chunk/v1", MaxTokens: 100,
				OverlapTokens: 10, Tokenizer: "tokenizer", TruncationPolicy: "reject"},
		}},
		RetrievalProfiles: map[string]RetrievalProfileConfig{"hybrid": {LexicalLimit: 10, VectorLimit: 10}},
		ProcessingProfiles: map[string]ProcessingProfileConfig{"archive": {
			Rendition: "primary", Embeddings: []string{"semantic"}, Retrieval: "hybrid",
			AttachmentPolicyFingerprint: strings.Repeat("a", 64), CompletenessFingerprint: strings.Repeat("b", 64),
			ConsentFingerprint: strings.Repeat("c", 64), LexicalSegmenterFingerprint: strings.Repeat("d", 64),
			MaxDocumentChars: 100_000, MaxSegmentRunes: 100, MaxUnitRunes: 1000,
			NormalizerFingerprint:   strings.Repeat("e", 64),
			RetainSanitizedMarkdown: true, SanitizerFingerprint: strings.Repeat("f", 64), TrustBoundary: "vault",
		}},
	}
}

func withProcessing(profile ProcessingProfileConfig, mutate func(*ProcessingProfileConfig)) ProcessingProfileConfig {
	mutate(&profile)
	return profile
}

func TestLoadResolvesStoreBindings(t *testing.T) {
	dir := privateTestConfigDir(t)
	secondary := filepath.Join(dir, "secondary")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(
		"[store_bindings.archive]\n"+
			"kind = \"filesystem\"\npath = \"secondary\"\npriority = 25\n\n"+
			"[store_bindings.cold]\nkind = \"s3\"\nendpoint = \"https://objects.example.invalid\"\n"+
			"region = \"us-east-1\"\nbucket = \"documents\"\nprefix = \"vaults/main\"\n"+
			"credential_profile = \"archive\"\npriority = 100\n"), 0o600))

	cfg, err := Load(dir)
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	assert.Equal(t, StoreBindingConfig{
		Kind: "filesystem", Path: secondary, Priority: 25,
	}, cfg.StoreBindings["archive"])
	assert.Equal(t, StoreBindingConfig{
		Kind: "s3", Endpoint: "https://objects.example.invalid",
		Region: "us-east-1", Bucket: "documents", Prefix: "vaults/main",
		CredentialProfile: "archive", Priority: 100,
	}, cfg.StoreBindings["cold"])
}

func TestStoreBindingsRejectAmbiguousDefinitions(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "archive")
	for _, tc := range []struct {
		name    string
		binding StoreBindingConfig
		want    string
	}{
		{
			name: "relative filesystem path",
			binding: StoreBindingConfig{
				Kind: "filesystem", Path: "relative",
			},
			want: "must be absolute",
		},
		{
			name: "filesystem with s3 fields",
			binding: StoreBindingConfig{
				Kind: "filesystem", Path: archive, Bucket: "documents",
			},
			want: "does not accept S3",
		},
		{
			name: "s3 without credentials",
			binding: StoreBindingConfig{
				Kind: "s3", Bucket: "documents",
			},
			want: "credential_profile",
		},
		{
			name: "s3 with noncanonical prefix",
			binding: StoreBindingConfig{
				Kind: "s3", Bucket: "documents", Prefix: "../other-vault",
				CredentialProfile: "archive",
			},
			want: "namespace is invalid",
		},
		{
			name: "invalid priority",
			binding: StoreBindingConfig{
				Kind: "filesystem", Path: archive, Priority: -1,
			},
			want: "priority",
		},
		{
			name:    "unknown kind",
			binding: StoreBindingConfig{Kind: "tape"},
			want:    "kind",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.StoreBindings = map[string]StoreBindingConfig{"archive": tc.binding}
			require.ErrorContains(t, cfg.Validate(), tc.want)
		})
	}
}

func TestStoreBindingsRejectPlainHTTPS3Endpoint(t *testing.T) {
	cfg := Default()
	cfg.StoreBindings = map[string]StoreBindingConfig{
		"archive": {
			Kind: "s3", Endpoint: "http://127.0.0.1:9000", Region: "us-east-1",
			Bucket: "documents", CredentialProfile: "test", ForcePathStyle: true,
		},
	}

	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTPS")
}

func TestLoadParsesWatchedInboxesAndAppliesDefaults(t *testing.T) {
	dir := privateTestConfigDir(t)
	source := t.TempDir()
	relativeHomeSource := "~/agent-sessions"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(
		"[[watch]]\nname = \"documents\"\nsource = \""+filepath.ToSlash(source)+"\"\n"+
			"destination = \"/inbox/documents\"\nexclude = [\".tmp\", \"cache/\"]\n"+
			"settle_time = \"45s\"\nminimum_age = \"168h\"\nscan_interval = \"3s\"\n\n"+
			"[[watch]]\nname = \"sessions\"\nsource = \""+relativeHomeSource+"\"\n"+
			"destination = \"/agents/sessions\"\n"), 0o600))

	c, err := Load(dir)
	require.NoError(t, err)
	require.Len(t, c.Watches, 2)
	assert.Equal(t, filepath.Clean(source), c.Watches[0].Source)
	assert.Equal(t, "/inbox/documents", c.Watches[0].Destination)
	assert.Equal(t, 45*time.Second, c.Watches[0].SettleTime.Std())
	assert.Equal(t, 7*24*time.Hour, c.Watches[0].MinimumAge.Std())
	assert.Equal(t, 3*time.Second, c.Watches[0].ScanInterval.Std())
	assert.Equal(t, []string{".tmp", "cache/"}, c.Watches[0].Exclude)
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "agent-sessions"), c.Watches[1].Source)
	assert.Equal(t, 30*time.Second, c.Watches[1].SettleTime.Std())
	assert.Zero(t, c.Watches[1].MinimumAge.Std())
	assert.Equal(t, 5*time.Second, c.Watches[1].ScanInterval.Std())
	require.NoError(t, c.Validate())
}

func TestWatchedInboxConfigRejectsAmbiguousOrUnsafeValues(t *testing.T) {
	dir := privateTestConfigDir(t)
	source := filepath.ToSlash(t.TempDir())
	for _, tc := range []struct {
		name, body, want string
	}{
		{"relative source", "name='inbox'\nsource='relative'\ndestination='/inbox'", "must be absolute"},
		{"relative destination", "name='inbox'\nsource='" + source + "'\ndestination='inbox'", "absolute virtual path"},
		{"invalid name", "name='Bad Name'\nsource='" + source + "'\ndestination='/inbox'", "unsupported characters"},
		{"negative minimum age", "name='inbox'\nsource='" + source +
			"'\ndestination='/inbox'\nminimum_age='-1s'", "must not be negative"},
		{"duplicate name", "name='same'\nsource='" + source + "'\ndestination='/one'\n" +
			"[[watch]]\nname='same'\nsource='" + filepath.ToSlash(t.TempDir()) + "'\ndestination='/two'", "duplicated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"),
				[]byte("[[watch]]\n"+tc.body+"\n"), 0o600))
			cfg, err := Load(dir)
			if err == nil {
				err = cfg.Validate()
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name, bind, key string
		wantErr         bool
	}{
		{"loopback keyless", "127.0.0.1", "", false},
		{"localhost keyless", "localhost", "", false},
		{"ipv6 loopback keyless", "::1", "", false},
		{"loopback with key", "127.0.0.1", "k", false},
		// The API is plain HTTP: every non-loopback bind is refused, keyed
		// or not - a key on the wire in cleartext is not protection.
		{"private keyless", "192.168.1.5", "", true},
		{"private with key", "192.168.1.5", "k", true},
		{"public with key", "203.0.113.9", "k", true},
		{"wildcard keyless", "0.0.0.0", "", true},
		{"wildcard with key", "0.0.0.0", "k", true},
		{"garbage host", "not an ip", "k", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			c.Server.BindAddr, c.Server.APIKey = tc.bind, tc.key
			err := c.Validate()
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateBackupCompressionLevel(t *testing.T) {
	for _, level := range []int{0, 1, 19} {
		c := Default()
		c.Backup.ZstdLevel = level
		require.NoError(t, c.Validate())
	}
	for _, level := range []int{-1, 20} {
		c := Default()
		c.Backup.ZstdLevel = level
		require.ErrorContains(t, c.Validate(), "zstd_level")
	}
}

func TestValidateAutomaticPacking(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval time.Duration
		maxBytes int64
		want     string
	}{
		{name: "disabled", maxBytes: 256 << 20},
		{name: "enabled", interval: time.Hour, maxBytes: 1 << 20},
		{name: "negative interval", interval: -time.Second, maxBytes: 1, want: "must not be negative"},
		{name: "negative bytes", maxBytes: -1, want: "must not be negative"},
		{name: "unbounded enabled", interval: time.Hour, want: "must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			c.Storage.PackInterval = Duration(tc.interval)
			c.Storage.PackMaxBytes = tc.maxBytes
			err := c.Validate()
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}
