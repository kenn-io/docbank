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
	Enabled       bool     `toml:"enabled"`
	Endpoint      string   `toml:"endpoint"`
	APIPath       string   `toml:"api_path"`
	APIKeyEnv     string   `toml:"api_key_env"`
	APIKeyFile    string   `toml:"api_key_file"`
	TrustBoundary string   `toml:"trust_boundary"`
	AllowedCIDRs  []string `toml:"allowed_cidrs"`
	decodeInvalid bool
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
		case "endpoint", "api_path", "api_key_env", "api_key_file", "trust_boundary":
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
			}
		case "allowed_cidrs":
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
			d.AllowedCIDRs = out
		default:
			d.decodeInvalid = true
		}
	}
	return nil
}

// DocuSealDisabled reports an absent or explicitly disabled integration.
func (d DocuSealConfig) DocuSealDisabled() bool { return !d.Enabled && !d.decodeInvalid }

// DocuSealConnection holds per-operation secret material and must not be logged or serialized.
type DocuSealConnection struct {
	Endpoint, APIPath, APIKey, TrustBoundary string
	AllowedCIDRs                             []netip.Prefix
}

func (d DocuSealConfig) Resolve(ctx context.Context, root string) (DocuSealConnection, error) {
	fail := func(category string) (DocuSealConnection, error) {
		return DocuSealConnection{}, errors.New("invalid DocuSeal " + category + " settings")
	}
	if ctx.Err() != nil {
		return DocuSealConnection{}, ctx.Err()
	}
	if !d.Enabled || d.decodeInvalid {
		return fail("configuration")
	}
	var prefixes []netip.Prefix
	egress := ProviderEgressConfig{Endpoint: d.Endpoint, AllowedCIDRs: d.AllowedCIDRs, ProxyMode: "disabled", ConnectTimeout: Duration(30 * time.Second), KeepAlive: Duration(30 * time.Second), TLSHandshakeTimeout: Duration(10 * time.Second)}
	u, err := validateProviderEgressConfigWithPrefixes(egress, "docuseal", &prefixes)
	if err != nil || u.Path != "" && u.Path != "/" || u.RawPath != "" || d.TrustBoundary != "operator_network" && d.TrustBoundary != "hosted_provider" || d.TrustBoundary == "hosted_provider" && u.Scheme != "https" {
		return fail("endpoint or trust boundary")
	}
	for _, prefix := range prefixes {
		if d.TrustBoundary == "operator_network" && !privateRenditionNetwork(prefix) {
			return fail("allowed CIDR")
		}
	}
	if d.APIPath != "" && (d.APIPath == "/" || d.APIPath[0] != '/' || path.Clean(d.APIPath) != d.APIPath || strings.ContainsAny(d.APIPath, "?#%\\")) {
		return fail("API path")
	}
	if (d.APIKeyEnv == "") == (d.APIKeyFile == "") {
		return fail("credential")
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
			return fail("credential")
		}
		info, e := f.Stat()
		if e != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
			_ = f.Close()
			return fail("credential")
		}
		b, e := io.ReadAll(io.LimitReader(f, 4097))
		_ = f.Close()
		if e != nil || len(b) > 4096 {
			return fail("credential")
		}
		key = string(b)
	}
	if len(key) > 4096 {
		return fail("credential")
	}
	key = strings.TrimSpace(key)
	if !validSecret(key) {
		return fail("credential")
	}
	return DocuSealConnection{Endpoint: strings.TrimSuffix(u.String(), "/"), APIPath: d.APIPath, APIKey: key, TrustBoundary: d.TrustBoundary, AllowedCIDRs: prefixes}, nil
}
