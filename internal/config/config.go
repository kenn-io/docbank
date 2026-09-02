// Package config loads the optional $DOCBANK_HOME/config.toml. Every value
// has a default; the file's absence is not an error. There are no per-field
// env or flag overrides — the only environment knob is DOCBANK_HOME.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/embedding"
	"go.kenn.io/docbank/internal/storenamespace"
)

// Duration is a time.Duration that unmarshals from a TOML string such as
// "30m"; "0" disables the associated timeout.
type Duration time.Duration

// UnmarshalText parses a duration string, rejecting negative durations.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", string(b), err)
	}
	if v < 0 {
		return fmt.Errorf("invalid duration %q: must not be negative", string(b))
	}
	*d = Duration(v)
	return nil
}

// Std returns d as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// ServerConfig configures the docbank API daemon's listen address and idle
// shutdown behavior.
type ServerConfig struct {
	BindAddr    string   `toml:"bind_addr"`    // default "127.0.0.1"
	APIPort     int      `toml:"api_port"`     // default 0 (ephemeral)
	APIKey      string   `toml:"api_key"`      // default "" (ephemeral per-run key on loopback)
	IdleTimeout Duration `toml:"idle_timeout"` // default 30m; background daemons only
}

// WebConfig controls the built-in web UI.
type WebConfig struct {
	Enabled bool `toml:"enabled"` // default true
}

// BackupConfig configures the default immutable snapshot repository and its
// compression policy. An empty Repo keeps backup commands available through
// an explicit request path without silently choosing storage under the vault.
type BackupConfig struct {
	Repo      string `toml:"repo"`
	ZstdLevel int    `toml:"zstd_level"`
}

// StorageConfig controls optional daemon-owned physical storage work. Packing
// is non-destructive to logical document authority; GC and repack remain
// explicit operator actions.
type StorageConfig struct {
	PackInterval Duration `toml:"pack_interval"`
	PackMaxBytes int64    `toml:"pack_max_bytes"`
}

// StoreBindingConfig names one machine-local physical storage namespace.
// Credentials, endpoints, and filesystem paths remain deployment
// configuration and never enter portable vault metadata.
type StoreBindingConfig struct {
	Kind              string `toml:"kind"`
	Path              string `toml:"path"`
	Endpoint          string `toml:"endpoint"`
	Region            string `toml:"region"`
	Bucket            string `toml:"bucket"`
	Prefix            string `toml:"prefix"`
	CredentialProfile string `toml:"credential_profile"`
	Priority          int    `toml:"priority"`
	ForcePathStyle    bool   `toml:"force_path_style"`
}

// WatchConfig describes one daemon-owned local inbox. Name and each relative
// source path form the stable, portable source identity; Source itself is a
// machine-local location and is intentionally not archive metadata.
type WatchConfig struct {
	Name         string   `toml:"name"`
	Source       string   `toml:"source"`
	Destination  string   `toml:"destination"`
	SettleTime   Duration `toml:"settle_time"`
	MinimumAge   Duration `toml:"minimum_age"`
	ScanInterval Duration `toml:"scan_interval"`
	Exclude      []string `toml:"exclude"`
}

// RenditionProfileConfig names a deployment binding for one pinned rendition
// descriptor. CredentialBinding is resolved only by the eventual provider
// adapter, never while loading config.toml.
type RenditionProfileConfig struct {
	AdapterContract          string   `toml:"adapter_contract"`
	AuthorizationFingerprint string   `toml:"authorization_fingerprint"`
	CredentialBinding        string   `toml:"credential_binding"`
	DeploymentFingerprint    string   `toml:"deployment_fingerprint"`
	DescriptorID             string   `toml:"descriptor_id"`
	DescriptorFingerprint    string   `toml:"descriptor_fingerprint"`
	DiscloseFilename         bool     `toml:"disclose_filename"`
	DisclosureFingerprint    string   `toml:"disclosure_fingerprint"`
	MaxDocumentBytes         int64    `toml:"max_document_bytes"`
	MaxResponseBytes         int64    `toml:"max_response_bytes"`
	MaxUnits                 int      `toml:"max_units"`
	RequestedArtifacts       []string `toml:"requested_artifacts"`
	TrustBoundary            string   `toml:"trust_boundary"`
	UploadOptionsFingerprint string   `toml:"upload_options_fingerprint"`
}

// EmbeddingChunkConfig pins rendition-chunk input generation.
type EmbeddingChunkConfig struct {
	ContextFingerprint string `toml:"context_fingerprint"`
	Formatter          string `toml:"formatter"`
	MaxTokens          int    `toml:"max_tokens"`
	OverlapTokens      int    `toml:"overlap_tokens"`
	Tokenizer          string `toml:"tokenizer"`
	TruncationPolicy   string `toml:"truncation_policy"`
}

// EmbeddingProfileConfig names a deployment binding for one pinned embedding
// descriptor and semantic input kind.
type EmbeddingProfileConfig struct {
	Activation               string               `toml:"activation"`
	AuthorizationFingerprint string               `toml:"authorization_fingerprint"`
	Chunk                    EmbeddingChunkConfig `toml:"chunk"`
	CompatibilityID          string               `toml:"compatibility_id"`
	CredentialBinding        string               `toml:"credential_binding"`
	DescriptorID             string               `toml:"descriptor_id"`
	DescriptorFingerprint    string               `toml:"descriptor_fingerprint"`
	Dimensions               int                  `toml:"dimensions"`
	DisclosureFingerprint    string               `toml:"disclosure_fingerprint"`
	DocumentFormatter        string               `toml:"document_formatter"`
	InputKind                string               `toml:"input_kind"`
	MaxBatchItems            int                  `toml:"max_batch_items"`
	MaxInputBytes            int64                `toml:"max_input_bytes"`
	MaxResponseBytes         int64                `toml:"max_response_bytes"`
	Metric                   string               `toml:"metric"`
	Model                    string               `toml:"model"`
	Normalization            string               `toml:"normalization"`
	QueryFormatter           string               `toml:"query_formatter"`
	ScalarEncoding           string               `toml:"scalar_encoding"`
	TrustBoundary            string               `toml:"trust_boundary"`
}

// RetrievalProfileConfig contains bounded candidate limits for one named
// retrieval policy.
type RetrievalProfileConfig struct {
	LexicalLimit int `toml:"lexical_limit"`
	VectorLimit  int `toml:"vector_limit"`
}

// ProcessingProfileConfig assembles named rendition, embedding, and retrieval
// policies without copying provider credentials into portable policy.
type ProcessingProfileConfig struct {
	AttachmentPolicyFingerprint string   `toml:"attachment_policy_fingerprint"`
	CompletenessFingerprint     string   `toml:"completeness_fingerprint"`
	ConsentFingerprint          string   `toml:"consent_fingerprint"`
	Embeddings                  []string `toml:"embeddings"`
	LexicalSegmenterFingerprint string   `toml:"lexical_segmenter_fingerprint"`
	MaxDocumentChars            int      `toml:"max_document_chars"`
	MaxSegmentRunes             int      `toml:"max_segment_runes"`
	MaxUnitRunes                int      `toml:"max_unit_runes"`
	NormalizedEvidenceContract  string   `toml:"normalized_evidence_contract"`
	NormalizerFingerprint       string   `toml:"normalizer_fingerprint"`
	Rendition                   string   `toml:"rendition"`
	RenditionContract           string   `toml:"rendition_contract"`
	RetainProviderMarkdown      bool     `toml:"retain_provider_markdown"`
	RetainSanitizedMarkdown     bool     `toml:"retain_sanitized_markdown"`
	RetainTypedArtifacts        bool     `toml:"retain_typed_artifacts"`
	Retrieval                   string   `toml:"retrieval"`
	SanitizerFingerprint        string   `toml:"sanitizer_fingerprint"`
	SourceEvidenceContract      string   `toml:"source_evidence_contract"`
	TrustBoundary               string   `toml:"trust_boundary"`
}

// ResolvedProcessingProfile pairs the portable document profile with the
// executable retrieval policy built from the same limits. The retrieval
// limits are copied into the document profile, so they are part of the
// profile fingerprint; changing them creates a new profile identity.
type ResolvedProcessingProfile struct {
	Document      document.ProcessingProfileV1
	RetrievalName string
	Retrieval     embedding.RetrievalPolicy
}

// Config is the full contents of config.toml.
type Config struct {
	Server             ServerConfig                       `toml:"server"`
	Web                WebConfig                          `toml:"web"`
	Backup             BackupConfig                       `toml:"backup"`
	Storage            StorageConfig                      `toml:"storage"`
	StoreBindings      map[string]StoreBindingConfig      `toml:"store_bindings"`
	RenditionProfiles  map[string]RenditionProfileConfig  `toml:"rendition_profiles"`
	EmbeddingProfiles  map[string]EmbeddingProfileConfig  `toml:"embedding_profiles"`
	RetrievalProfiles  map[string]RetrievalProfileConfig  `toml:"retrieval_profiles"`
	ProcessingProfiles map[string]ProcessingProfileConfig `toml:"processing_profiles"`
	Watches            []WatchConfig                      `toml:"watch"`
}

// Default returns the configuration used when config.toml is absent.
func Default() Config {
	return Config{
		Server: ServerConfig{
			BindAddr:    "127.0.0.1",
			IdleTimeout: Duration(30 * time.Minute),
		},
		Web:                WebConfig{Enabled: true},
		Storage:            StorageConfig{PackMaxBytes: 256 << 20},
		RenditionProfiles:  make(map[string]RenditionProfileConfig),
		EmbeddingProfiles:  make(map[string]EmbeddingProfileConfig),
		RetrievalProfiles:  make(map[string]RetrievalProfileConfig),
		ProcessingProfiles: make(map[string]ProcessingProfileConfig),
	}
}

// Load reads <root>/config.toml, returning Default() if the file does not
// exist. An unrecognized key is treated as a typo and rejected.
func Load(root string) (Config, error) {
	c := Default()
	path := filepath.Join(root, "config.toml")
	file, err := openConfig(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("loading %s: %w", path, err)
	}
	md, decodeErr := toml.NewDecoder(file).Decode(&c)
	closeErr := file.Close()
	if decodeErr != nil {
		return Config{}, fmt.Errorf("loading %s: %w", path, decodeErr)
	}
	if closeErr != nil {
		return Config{}, fmt.Errorf("loading %s: %w", path, closeErr)
	}
	if undec := md.Undecoded(); len(undec) > 0 {
		return Config{}, fmt.Errorf("loading %s: unknown key %q (typo?)", path, undec[0].String())
	}
	if err := resolveBackupRepo(root, &c.Backup); err != nil {
		return Config{}, fmt.Errorf("loading %s: %w", path, err)
	}
	if err := resolveStoreBindings(root, &c); err != nil {
		return Config{}, fmt.Errorf("loading %s: %w", path, err)
	}
	if err := resolveWatches(&c); err != nil {
		return Config{}, fmt.Errorf("loading %s: %w", path, err)
	}
	return c, nil
}

func resolveStoreBindings(root string, c *Config) error {
	home, homeErr := os.UserHomeDir()
	for name, binding := range c.StoreBindings {
		if binding.Kind != "filesystem" || binding.Path == "" {
			continue
		}
		path := binding.Path
		if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
			if homeErr != nil {
				return fmt.Errorf("resolving [store_bindings.%s] path %q: %w",
					name, path, homeErr)
			}
			if path == "~" {
				path = home
			} else {
				path = filepath.Join(home, strings.TrimLeft(path[1:], `/\`))
			}
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("resolving [store_bindings.%s] path %q: %w",
				name, binding.Path, err)
		}
		binding.Path = filepath.Clean(absolute)
		c.StoreBindings[name] = binding
	}
	return nil
}

const (
	defaultWatchSettleTime   = 30 * time.Second
	defaultWatchScanInterval = 5 * time.Second
)

func resolveWatches(c *Config) error {
	home, homeErr := os.UserHomeDir()
	names := make(map[string]struct{}, len(c.Watches))
	sources := make(map[string]string, len(c.Watches))
	for i := range c.Watches {
		watch := &c.Watches[i]
		if _, exists := names[watch.Name]; exists {
			return fmt.Errorf("[[watch]] name %q is duplicated", watch.Name)
		}
		names[watch.Name] = struct{}{}
		if watch.Source == "~" || strings.HasPrefix(watch.Source, "~/") ||
			strings.HasPrefix(watch.Source, `~\`) {
			if homeErr != nil {
				return fmt.Errorf("resolving [[watch]] %q source %q: %w",
					watch.Name, watch.Source, homeErr)
			}
			if watch.Source == "~" {
				watch.Source = home
			} else {
				watch.Source = filepath.Join(home, strings.TrimLeft(watch.Source[1:], `/\`))
			}
		}
		if watch.Source != "" && !filepath.IsAbs(watch.Source) {
			return fmt.Errorf("[[watch]] %q source %q must be absolute or start with ~/",
				watch.Name, watch.Source)
		}
		if watch.Source != "" {
			abs, err := filepath.Abs(watch.Source)
			if err != nil {
				return fmt.Errorf("resolving [[watch]] %q source %q: %w",
					watch.Name, watch.Source, err)
			}
			watch.Source = filepath.Clean(abs)
			if prior, exists := sources[watch.Source]; exists {
				return fmt.Errorf("[[watch]] %q and %q use the same source %q",
					prior, watch.Name, watch.Source)
			}
			sources[watch.Source] = watch.Name
		}
		if watch.SettleTime.Std() == 0 {
			watch.SettleTime = Duration(defaultWatchSettleTime)
		}
		if watch.ScanInterval.Std() == 0 {
			watch.ScanInterval = Duration(defaultWatchScanInterval)
		}
	}
	return nil
}

func resolveBackupRepo(root string, backup *BackupConfig) error {
	if backup.Repo == "" {
		return nil
	}
	repo := backup.Repo
	if repo == "~" || strings.HasPrefix(repo, "~/") || strings.HasPrefix(repo, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolving [backup] repo %q: %w", repo, err)
		}
		if repo == "~" {
			repo = home
		} else {
			repo = filepath.Join(home, strings.TrimLeft(repo[1:], `/\`))
		}
	}
	if !filepath.IsAbs(repo) {
		repo = filepath.Join(root, repo)
	}
	abs, err := filepath.Abs(repo)
	if err != nil {
		return fmt.Errorf("resolving [backup] repo %q: %w", backup.Repo, err)
	}
	backup.Repo = filepath.Clean(abs)
	return nil
}

// Validate enforces the bind policy: loopback only. The API is plain HTTP,
// so any non-loopback bind — even a keyed, private-network one — would put
// the API key and vault contents on the wire in cleartext. Remote access
// goes through an SSH tunnel or VPN to the loopback listener until the
// daemon grows TLS. An unset api_key stays valid: the daemon generates and
// self-publishes an ephemeral key rather than serving unauthenticated (see
// cmd/docbank/daemon.go).
func (c Config) Validate() error {
	if c.Backup.ZstdLevel != 0 && (c.Backup.ZstdLevel < 1 || c.Backup.ZstdLevel > 19) {
		return fmt.Errorf("[backup] zstd_level %d: want 0 or 1-19", c.Backup.ZstdLevel)
	}
	if c.Storage.PackMaxBytes < 0 {
		return errors.New("[storage] pack_max_bytes must not be negative")
	}
	if c.Storage.PackInterval.Std() < 0 {
		return errors.New("[storage] pack_interval must not be negative")
	}
	if c.Storage.PackInterval.Std() > 0 && c.Storage.PackMaxBytes == 0 {
		return errors.New("[storage] pack_max_bytes must be positive when pack_interval is enabled")
	}
	if err := validateStoreBindings(c.StoreBindings); err != nil {
		return err
	}
	if err := validateProcessingProfiles(c); err != nil {
		return err
	}
	for _, watch := range c.Watches {
		if err := validateWatch(watch); err != nil {
			return err
		}
	}
	host := c.Server.BindAddr
	if isLoopbackHost(host) {
		return nil
	}
	if net.ParseIP(host) == nil {
		return fmt.Errorf("[server] bind_addr %q: not an IP address or localhost", host)
	}
	return fmt.Errorf("[server] bind_addr %q: the API is plain HTTP, so binds are "+
		"loopback-only; reach a remote docbank through an SSH tunnel or VPN", host)
}

var storeBindingNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

var lowercaseSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var credentialReferencePattern = regexp.MustCompile(`^credential:[a-z][a-z0-9_-]{0,62}$`)

func validateProcessingProfiles(c Config) error {
	for name, profile := range c.RenditionProfiles {
		prefix := fmt.Sprintf("[rendition_profiles.%s]", name)
		if err := validateProfileName(name, prefix); err != nil {
			return err
		}
		if err := validateRenditionProfileConfig(profile, prefix); err != nil {
			return err
		}
	}
	for name, profile := range c.EmbeddingProfiles {
		prefix := fmt.Sprintf("[embedding_profiles.%s]", name)
		if err := validateProfileName(name, prefix); err != nil {
			return err
		}
		if err := validateEmbeddingProfileConfig(profile, prefix); err != nil {
			return err
		}
	}
	for name, profile := range c.RetrievalProfiles {
		prefix := fmt.Sprintf("[retrieval_profiles.%s]", name)
		if err := validateProfileName(name, prefix); err != nil {
			return err
		}
		if err := validateRetrievalProfileConfig(profile, prefix); err != nil {
			return err
		}
	}
	for name := range c.ProcessingProfiles {
		prefix := fmt.Sprintf("[processing_profiles.%s]", name)
		if err := validateProfileName(name, prefix); err != nil {
			return err
		}
		assembled, err := c.assembleProcessingProfile(name)
		if err != nil {
			return err
		}
		if _, _, err := document.CanonicalProfile(assembled.Document); err != nil {
			return fmt.Errorf("%s is invalid: %w", prefix, err)
		}
	}
	return nil
}

func validateRetrievalProfileConfig(profile RetrievalProfileConfig, prefix string) error {
	if profile.LexicalLimit <= 0 || profile.LexicalLimit > embedding.MaxCandidateLimit {
		return fmt.Errorf("%s lexical_limit must be between 1 and %d", prefix, embedding.MaxCandidateLimit)
	}
	if profile.VectorLimit <= 0 || profile.VectorLimit > embedding.MaxCandidateLimit {
		return fmt.Errorf("%s vector_limit must be between 1 and %d", prefix, embedding.MaxCandidateLimit)
	}
	return nil
}

func validateRenditionProfileConfig(profile RenditionProfileConfig, prefix string) error {
	for field, value := range map[string]string{
		"adapter_contract": profile.AdapterContract, "descriptor_id": profile.DescriptorID,
		"trust_boundary": profile.TrustBoundary,
	} {
		if value == "" {
			return fmt.Errorf("%s %s is required", prefix, field)
		}
	}
	for field, value := range map[string]string{
		"authorization_fingerprint": profile.AuthorizationFingerprint, "deployment_fingerprint": profile.DeploymentFingerprint,
		"descriptor_fingerprint": profile.DescriptorFingerprint, "disclosure_fingerprint": profile.DisclosureFingerprint,
		"upload_options_fingerprint": profile.UploadOptionsFingerprint,
	} {
		if !lowercaseSHA256Pattern.MatchString(value) {
			return fmt.Errorf("%s %s must be a lowercase SHA-256 value", prefix, field)
		}
	}
	if !credentialReferencePattern.MatchString(profile.CredentialBinding) {
		return fmt.Errorf("%s credential_binding must use credential:<name>", prefix)
	}
	if profile.MaxDocumentBytes <= 0 || profile.MaxDocumentBytes > 1<<40 {
		return fmt.Errorf("%s max document bytes must be between 1 and %d", prefix, int64(1<<40))
	}
	if profile.MaxResponseBytes <= 0 || profile.MaxResponseBytes > 1<<30 {
		return fmt.Errorf("%s max response bytes must be between 1 and %d", prefix, int64(1<<30))
	}
	if profile.MaxUnits <= 0 || profile.MaxUnits > 1_000_000 {
		return fmt.Errorf("%s max units must be between 1 and 1000000", prefix)
	}
	if len(profile.RequestedArtifacts) == 0 || len(profile.RequestedArtifacts) > 4 {
		return fmt.Errorf("%s requested_artifacts must contain 1-4 roles", prefix)
	}
	seenArtifacts := make(map[string]struct{}, len(profile.RequestedArtifacts))
	for _, role := range profile.RequestedArtifacts {
		switch document.EvidenceArtifactRole(role) {
		case document.EvidenceArtifactImage, document.EvidenceArtifactMarkdown,
			document.EvidenceArtifactStructured, document.EvidenceArtifactTranscript:
		default:
			return fmt.Errorf("%s requested artifact role %q is unknown", prefix, role)
		}
		if _, exists := seenArtifacts[role]; exists {
			return fmt.Errorf("%s requested artifact role %q is duplicated", prefix, role)
		}
		seenArtifacts[role] = struct{}{}
	}
	return nil
}

func validateEmbeddingProfileConfig(profile EmbeddingProfileConfig, prefix string) error {
	for field, value := range map[string]string{
		"compatibility_id": profile.CompatibilityID, "descriptor_id": profile.DescriptorID,
		"document_formatter": profile.DocumentFormatter, "metric": profile.Metric, "model": profile.Model,
		"normalization": profile.Normalization, "query_formatter": profile.QueryFormatter,
		"scalar_encoding": profile.ScalarEncoding, "trust_boundary": profile.TrustBoundary,
	} {
		if value == "" {
			return fmt.Errorf("%s %s is required", prefix, field)
		}
	}
	for field, value := range map[string]string{
		"authorization_fingerprint": profile.AuthorizationFingerprint, "descriptor_fingerprint": profile.DescriptorFingerprint,
		"disclosure_fingerprint": profile.DisclosureFingerprint,
	} {
		if !lowercaseSHA256Pattern.MatchString(value) {
			return fmt.Errorf("%s %s must be a lowercase SHA-256 value", prefix, field)
		}
	}
	if !credentialReferencePattern.MatchString(profile.CredentialBinding) {
		return fmt.Errorf("%s credential_binding must use credential:<name>", prefix)
	}
	if profile.Activation != string(document.EmbeddingOptional) && profile.Activation != string(document.EmbeddingRequired) {
		return fmt.Errorf("%s activation must be optional or required", prefix)
	}
	if profile.InputKind != string(document.EmbeddingInputOriginalFile) && profile.InputKind != string(document.EmbeddingInputRenditionChunk) {
		return fmt.Errorf("%s input_kind must be original_file or rendition_chunk", prefix)
	}
	if profile.Dimensions <= 0 || profile.Dimensions > 1_000_000 {
		return fmt.Errorf("%s dimensions must be between 1 and 1000000", prefix)
	}
	if profile.MaxBatchItems <= 0 || profile.MaxBatchItems > 10_000 {
		return fmt.Errorf("%s max batch items must be between 1 and 10000", prefix)
	}
	if profile.MaxInputBytes <= 0 || profile.MaxInputBytes > 1<<30 {
		return fmt.Errorf("%s max input bytes must be between 1 and %d", prefix, int64(1<<30))
	}
	if profile.MaxResponseBytes <= 0 || profile.MaxResponseBytes > 1<<30 {
		return fmt.Errorf("%s max response bytes must be between 1 and %d", prefix, int64(1<<30))
	}
	if profile.InputKind == string(document.EmbeddingInputRenditionChunk) {
		if profile.Chunk.MaxTokens <= 0 || profile.Chunk.MaxTokens > 1_000_000 ||
			profile.Chunk.OverlapTokens < 0 || profile.Chunk.OverlapTokens >= profile.Chunk.MaxTokens ||
			profile.Chunk.Tokenizer == "" || profile.Chunk.Formatter == "" || profile.Chunk.TruncationPolicy == "" ||
			!lowercaseSHA256Pattern.MatchString(profile.Chunk.ContextFingerprint) {
			return fmt.Errorf("%s chunk policy is invalid", prefix)
		}
	} else if profile.Chunk != (EmbeddingChunkConfig{}) {
		return fmt.Errorf("%s original_file input must not define chunk policy", prefix)
	}
	return nil
}

// ProcessingProfile assembles one named portable profile and its deployment
// retrieval policy without resolving credential references or contacting providers.
func (c Config) ProcessingProfile(name string) (ResolvedProcessingProfile, error) {
	profile, err := c.assembleProcessingProfile(name)
	if err != nil {
		return ResolvedProcessingProfile{}, err
	}
	canonical, err := document.CanonicalizeProfile(profile.Document)
	if err != nil {
		return ResolvedProcessingProfile{}, fmt.Errorf("processing profile %q is invalid: %w", name, err)
	}
	profile.Document = canonical
	return profile, nil
}

func (c Config) assembleProcessingProfile(name string) (ResolvedProcessingProfile, error) {
	configured, exists := c.ProcessingProfiles[name]
	if !exists {
		return ResolvedProcessingProfile{}, fmt.Errorf("processing profile %q is not defined", name)
	}
	if configured.Retrieval == "" {
		return ResolvedProcessingProfile{}, fmt.Errorf("[processing_profiles.%s] retrieval is required", name)
	}
	retrievalConfig, exists := c.RetrievalProfiles[configured.Retrieval]
	if !exists {
		return ResolvedProcessingProfile{}, fmt.Errorf("[processing_profiles.%s] retrieval %q is not defined", name, configured.Retrieval)
	}
	if err := validateRetrievalProfileConfig(
		retrievalConfig, fmt.Sprintf("[retrieval_profiles.%s]", configured.Retrieval),
	); err != nil {
		return ResolvedProcessingProfile{}, err
	}
	retrieval, err := embedding.NewRetrievalPolicy(retrievalConfig.LexicalLimit, retrievalConfig.VectorLimit)
	if err != nil {
		return ResolvedProcessingProfile{}, fmt.Errorf("[processing_profiles.%s] retrieval %q: %w", name, configured.Retrieval, err)
	}
	profile := document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1,
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			CompletenessFingerprint:     configured.CompletenessFingerprint,
			LexicalSegmenterFingerprint: configured.LexicalSegmenterFingerprint,
			MaxDocumentChars:            configured.MaxDocumentChars,
			MaxSegmentRunes:             configured.MaxSegmentRunes, MaxUnitRunes: configured.MaxUnitRunes,
			NormalizedEvidenceContract: valueOr(configured.NormalizedEvidenceContract, document.NormalizedEvidenceContractV1),
			NormalizerFingerprint:      configured.NormalizerFingerprint,
			RenditionContract:          valueOr(configured.RenditionContract, document.RenditionContractV1),
			SanitizerFingerprint:       configured.SanitizerFingerprint,
			SourceEvidenceContract:     valueOr(configured.SourceEvidenceContract, document.SourceEvidenceContractV1),
		},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{
			AttachmentPolicyFingerprint: configured.AttachmentPolicyFingerprint,
			ConsentFingerprint:          configured.ConsentFingerprint, RetainProviderMarkdown: configured.RetainProviderMarkdown,
			RetainSanitizedMarkdown: configured.RetainSanitizedMarkdown, RetainTypedArtifacts: configured.RetainTypedArtifacts,
			TrustBoundary: configured.TrustBoundary,
		},
		Retrieval: document.RetrievalPolicyV1{
			LexicalLimit: retrievalConfig.LexicalLimit, VectorLimit: retrievalConfig.VectorLimit,
		},
	}
	if configured.Rendition != "" {
		rendition, exists := c.RenditionProfiles[configured.Rendition]
		if !exists {
			return ResolvedProcessingProfile{}, fmt.Errorf("[processing_profiles.%s] rendition %q is not defined", name, configured.Rendition)
		}
		profile.Rendition = renditionDocumentBinding(configured.Rendition, rendition)
	}
	seen := make(map[string]struct{}, len(configured.Embeddings))
	for _, bindingName := range configured.Embeddings {
		if _, exists := seen[bindingName]; exists {
			return ResolvedProcessingProfile{}, fmt.Errorf("[processing_profiles.%s] embedding %q is duplicated", name, bindingName)
		}
		seen[bindingName] = struct{}{}
		binding, exists := c.EmbeddingProfiles[bindingName]
		if !exists {
			return ResolvedProcessingProfile{}, fmt.Errorf("[processing_profiles.%s] embedding %q is not defined", name, bindingName)
		}
		if err := validateEmbeddingProfileConfig(binding, fmt.Sprintf("[embedding_profiles.%s]", bindingName)); err != nil {
			return ResolvedProcessingProfile{}, err
		}
		if binding.InputKind == string(document.EmbeddingInputRenditionChunk) && profile.Rendition == nil {
			return ResolvedProcessingProfile{}, fmt.Errorf("[processing_profiles.%s] rendition_chunk embedding %q requires rendition", name, bindingName)
		}
		profile.Embeddings = append(profile.Embeddings, embeddingDocumentBinding(bindingName, binding))
	}
	if profile.Rendition == nil && (configured.RetainSanitizedMarkdown || configured.RetainProviderMarkdown) {
		return ResolvedProcessingProfile{}, fmt.Errorf("[processing_profiles.%s] retained Markdown requires rendition", name)
	}
	return ResolvedProcessingProfile{
		Document: profile, RetrievalName: configured.Retrieval, Retrieval: retrieval,
	}, nil
}

func renditionDocumentBinding(name string, profile RenditionProfileConfig) *document.RenditionBindingV1 {
	roles := make([]document.EvidenceArtifactRole, len(profile.RequestedArtifacts))
	for index, role := range profile.RequestedArtifacts {
		roles[index] = document.EvidenceArtifactRole(role)
	}
	return &document.RenditionBindingV1{
		AdapterContract: profile.AdapterContract, AuthorizationFingerprint: profile.AuthorizationFingerprint,
		CredentialBinding: profile.CredentialBinding, DeploymentFingerprint: profile.DeploymentFingerprint,
		Descriptor:       document.ProviderDescriptorV1{ID: profile.DescriptorID, Fingerprint: profile.DescriptorFingerprint},
		DiscloseFilename: profile.DiscloseFilename, DisclosureFingerprint: profile.DisclosureFingerprint,
		MaxDocumentBytes: profile.MaxDocumentBytes, MaxResponseBytes: profile.MaxResponseBytes, MaxUnits: profile.MaxUnits,
		Name: name, RequestedArtifacts: roles, TrustBoundary: profile.TrustBoundary,
		UploadOptionsFingerprint: profile.UploadOptionsFingerprint,
	}
}

func embeddingDocumentBinding(name string, profile EmbeddingProfileConfig) document.EmbeddingBindingV1 {
	result := document.EmbeddingBindingV1{
		Activation: document.EmbeddingActivation(profile.Activation), AuthorizationFingerprint: profile.AuthorizationFingerprint,
		CompatibilityID: profile.CompatibilityID, CredentialBinding: profile.CredentialBinding,
		Descriptor: document.ProviderDescriptorV1{ID: profile.DescriptorID, Fingerprint: profile.DescriptorFingerprint},
		Dimensions: profile.Dimensions, DisclosureFingerprint: profile.DisclosureFingerprint,
		DocumentFormatter: profile.DocumentFormatter, InputKind: document.EmbeddingInputKind(profile.InputKind),
		MaxBatchItems: profile.MaxBatchItems, MaxInputBytes: profile.MaxInputBytes, MaxResponseBytes: profile.MaxResponseBytes,
		Metric: profile.Metric, Model: profile.Model, Name: name, Normalization: profile.Normalization,
		QueryFormatter: profile.QueryFormatter, ScalarEncoding: profile.ScalarEncoding, TrustBoundary: profile.TrustBoundary,
	}
	if profile.InputKind == string(document.EmbeddingInputRenditionChunk) {
		result.Chunk = &document.EmbeddingChunkPolicyV1{
			ContextFingerprint: profile.Chunk.ContextFingerprint, Formatter: profile.Chunk.Formatter,
			MaxTokens: profile.Chunk.MaxTokens, OverlapTokens: profile.Chunk.OverlapTokens,
			Tokenizer: profile.Chunk.Tokenizer, TruncationPolicy: profile.Chunk.TruncationPolicy,
		}
	}
	return result
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func validateProfileName(name, prefix string) error {
	if !storeBindingNamePattern.MatchString(name) {
		return fmt.Errorf("%s name must start with a lowercase letter and contain only lowercase letters, digits, _ or -", prefix)
	}
	return nil
}

func validateStoreBindings(bindings map[string]StoreBindingConfig) error {
	for name, binding := range bindings {
		prefix := fmt.Sprintf("[store_bindings.%s]", name)
		if !storeBindingNamePattern.MatchString(name) {
			return fmt.Errorf("%s name must start with a lowercase letter and contain only lowercase letters, digits, _ or -", prefix)
		}
		if binding.Priority < 0 || binding.Priority > 1_000_000 {
			return fmt.Errorf("%s priority must be between 0 and 1000000", prefix)
		}
		switch binding.Kind {
		case "filesystem":
			if binding.Path == "" {
				return fmt.Errorf("%s path is required", prefix)
			}
			if !filepath.IsAbs(binding.Path) {
				return fmt.Errorf("%s path %q must be absolute", prefix, binding.Path)
			}
			if binding.Endpoint != "" || binding.Region != "" || binding.Bucket != "" ||
				binding.Prefix != "" || binding.CredentialProfile != "" ||
				binding.ForcePathStyle {
				return fmt.Errorf("%s filesystem binding does not accept S3 fields", prefix)
			}
		case "s3":
			if binding.Path != "" {
				return fmt.Errorf("%s S3 binding does not accept path", prefix)
			}
			if binding.Bucket == "" {
				return fmt.Errorf("%s bucket is required", prefix)
			}
			if binding.CredentialProfile == "" {
				return fmt.Errorf("%s credential_profile is required", prefix)
			}
			if binding.Endpoint != "" {
				endpoint, err := url.ParseRequestURI(binding.Endpoint)
				if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
					return fmt.Errorf("%s endpoint %q must be an absolute URL", prefix, binding.Endpoint)
				}
				if !strings.EqualFold(endpoint.Scheme, "https") {
					return fmt.Errorf("%s endpoint %q must use HTTPS", prefix, binding.Endpoint)
				}
			}
			if _, err := storenamespace.CanonicalS3(storenamespace.S3Binding{
				Endpoint: binding.Endpoint,
				Region:   binding.Region,
				Bucket:   binding.Bucket,
				Prefix:   binding.Prefix,
			}); err != nil {
				return fmt.Errorf("%s namespace is invalid: %w", prefix, err)
			}
		default:
			return fmt.Errorf("%s kind %q must be filesystem or s3", prefix, binding.Kind)
		}
	}
	return nil
}

func validateWatch(watch WatchConfig) error {
	if watch.Name == "" || len(watch.Name) > 64 {
		return errors.New("[[watch]] name must contain 1-64 characters")
	}
	for _, char := range watch.Name {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') ||
			strings.ContainsRune("-_.", char) {
			continue
		}
		return fmt.Errorf("[[watch]] name %q contains unsupported characters", watch.Name)
	}
	if watch.Source == "" {
		return fmt.Errorf("[[watch]] %q source is required", watch.Name)
	}
	if !filepath.IsAbs(watch.Source) {
		return fmt.Errorf("[[watch]] %q source %q is not absolute", watch.Name, watch.Source)
	}
	if !strings.HasPrefix(watch.Destination, "/") {
		return fmt.Errorf("[[watch]] %q destination %q must be an absolute virtual path",
			watch.Name, watch.Destination)
	}
	if watch.SettleTime.Std() <= 0 {
		return fmt.Errorf("[[watch]] %q settle_time must be positive", watch.Name)
	}
	if watch.MinimumAge.Std() < 0 {
		return fmt.Errorf("[[watch]] %q minimum_age must not be negative", watch.Name)
	}
	if watch.ScanInterval.Std() <= 0 {
		return fmt.Errorf("[[watch]] %q scan_interval must be positive", watch.Name)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
