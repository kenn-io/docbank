package config

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// DocuSealConfig defers optional connection validation until use.
//
//nolint:recvcheck // TOML decoding mutates the config.
type DocuSealConfig struct {
	Enabled          bool     `toml:"enabled"`
	Endpoint         string   `toml:"endpoint"`
	APIPath          string   `toml:"api_path"`
	APIKeyEnv        string   `toml:"api_key_env"`
	APIKeyFile       string   `toml:"api_key_file"`
	TrustBoundary    string   `toml:"trust_boundary"`
	AllowedCIDRs     []string `toml:"allowed_cidrs"`
	DownloadOrigins  []string `toml:"download_origins"`
	ImportPath       string   `toml:"import_path"`
	PollInterval     string   `toml:"poll_interval"`
	MaxDocumentBytes int64    `toml:"max_document_bytes"`
	decodeInvalid    bool
}

func (d *DocuSealConfig) UnmarshalTOML(value any) error {
	table, ok := value.(map[string]any)
	if !ok {
		d.decodeInvalid = true
		return nil
	}
	for k, v := range table {
		switch k {
		case "enabled":
			var ok bool
			d.Enabled, ok = v.(bool)
			d.decodeInvalid = d.decodeInvalid || !ok
		case "endpoint", "api_path", "api_key_env", "api_key_file", "trust_boundary", "import_path", "poll_interval":
			s, ok := v.(string)
			if !ok {
				d.decodeInvalid = true
				continue
			}
			switch k {
			case "endpoint":
				d.Endpoint = s
			case "api_path":
				d.APIPath = s
			case "api_key_env":
				d.APIKeyEnv = s
			case "api_key_file":
				d.APIKeyFile = s
			case "trust_boundary":
				d.TrustBoundary = s
			case "import_path":
				d.ImportPath = s
			case "poll_interval":
				d.PollInterval = s
			}
		case "allowed_cidrs", "download_origins":
			values, ok := v.([]any)
			if !ok {
				d.decodeInvalid = true
				continue
			}
			var out []string
			for _, item := range values {
				s, ok := item.(string)
				if !ok {
					d.decodeInvalid = true
					continue
				}
				out = append(out, s)
			}
			if k == "allowed_cidrs" {
				d.AllowedCIDRs = out
			} else {
				d.DownloadOrigins = out
			}
		case "max_document_bytes":
			n, ok := v.(int64)
			if !ok {
				d.decodeInvalid = true
			} else {
				d.MaxDocumentBytes = n
			}
		default:
			d.decodeInvalid = true
		}
	}
	return nil
}

// DocuSealConnection holds per-operation secret material and must not be logged or serialized.
type DocuSealConnection struct {
	Endpoint, APIPath, APIKey, TrustBoundary, ImportPath string
	AllowedCIDRs                                         []netip.Prefix
	DownloadOrigins                                      []string
	PollInterval                                         time.Duration
	MaxDocumentBytes                                     int64
}

func (d DocuSealConfig) Resolve(ctx context.Context, root string) (DocuSealConnection, error) {
	fail := func() (DocuSealConnection, error) {
		return DocuSealConnection{}, errors.New("invalid or disabled [integrations.docuseal]; check endpoint, egress and credential settings")
	}
	if ctx.Err() != nil {
		return DocuSealConnection{}, ctx.Err()
	}
	if !d.Enabled || d.decodeInvalid {
		return fail()
	}
	egress := ProviderEgressConfig{Endpoint: d.Endpoint, AllowedCIDRs: d.AllowedCIDRs, ProxyMode: "disabled", ConnectTimeout: Duration(30 * time.Second), KeepAlive: Duration(30 * time.Second), TLSHandshakeTimeout: Duration(10 * time.Second)}
	u, err := validateProviderEgressConfig(egress, "docuseal")
	if err != nil || u.Path != "" && u.Path != "/" || u.RawPath != "" || d.TrustBoundary != "operator_network" && d.TrustBoundary != "hosted_provider" || d.TrustBoundary == "hosted_provider" && u.Scheme != "https" {
		return fail()
	}
	var prefixes []netip.Prefix
	for _, s := range d.AllowedCIDRs {
		p, e := netip.ParsePrefix(s)
		if e != nil || d.TrustBoundary == "operator_network" && !privateRenditionNetwork(p) {
			return fail()
		}
		prefixes = append(prefixes, p)
	}
	if d.APIPath != "" && (d.APIPath == "/" || d.APIPath[0] != '/' || path.Clean(d.APIPath) != d.APIPath || strings.ContainsAny(d.APIPath, "?#%\\")) {
		return fail()
	}
	for _, origin := range d.DownloadOrigins {
		v, e := validateProviderEndpoint(origin, "docuseal")
		if e != nil || v.Path != "" && v.Path != "/" || v.RawPath != "" {
			return fail()
		}
	}
	if (d.APIKeyEnv == "") == (d.APIKeyFile == "") {
		return fail()
	}
	var key string
	if d.APIKeyEnv != "" {
		key = os.Getenv(d.APIKeyEnv)
	} else {
		file := d.APIKeyFile
		if !filepath.IsAbs(file) {
			file = filepath.Join(root, file)
		}
		f, e := openSecret(file)
		if e != nil {
			return fail()
		}
		info, e := f.Stat()
		if e != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
			_ = f.Close()
			return fail()
		}
		b, e := io.ReadAll(io.LimitReader(f, 4097))
		_ = f.Close()
		if e != nil || len(b) > 4096 {
			return fail()
		}
		key = string(b)
	}
	if len(key) > 4096 {
		return fail()
	}
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 4096 || strings.ContainsAny(key, "\r\n") || strings.Contains(u.String(), key) {
		return fail()
	}
	importPath := d.ImportPath
	if importPath == "" {
		importPath = "/Contracts/Signed"
	}
	if !strings.HasPrefix(importPath, "/") || path.Clean(importPath) != importPath {
		return fail()
	}
	interval := 5 * time.Minute
	if d.PollInterval != "" {
		interval, err = time.ParseDuration(d.PollInterval)
		if err != nil || interval <= 0 {
			return fail()
		}
	}
	limit := d.MaxDocumentBytes
	if limit == 0 {
		limit = 64 << 20
	}
	if limit < 0 || limit > 1<<30 {
		return fail()
	}
	return DocuSealConnection{Endpoint: strings.TrimSuffix(u.String(), "/"), APIPath: d.APIPath, APIKey: key, TrustBoundary: d.TrustBoundary, AllowedCIDRs: prefixes, DownloadOrigins: d.DownloadOrigins, ImportPath: importPath, PollInterval: interval, MaxDocumentBytes: limit}, nil
}
