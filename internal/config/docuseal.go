package config

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"

	"go.kenn.io/kit/secretref"
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
	decodeInvalid    string
	enabledInvalid   bool
	explicitDisabled bool
}

func (d *DocuSealConfig) UnmarshalTOML(value any) error {
	invalid := func(category string) {
		if d.decodeInvalid == "" || category < d.decodeInvalid {
			d.decodeInvalid = category
		}
	}
	table, ok := value.(map[string]any)
	if !ok {
		invalid("configuration")
		return nil
	}
	for k, v := range table {
		switch k {
		case "enabled":
			var ok bool
			d.Enabled, ok = v.(bool)
			d.enabledInvalid = !ok
			d.explicitDisabled = ok && !d.Enabled
			if !ok {
				invalid("enabled")
			}
		case "endpoint", "api_path", "api_key_env", "api_key_file", "trust_boundary":
			var target *string
			switch k {
			case "endpoint":
				target = &d.Endpoint
			case "api_path":
				target = &d.APIPath
			case "api_key_env":
				target = &d.APIKeyEnv
			case "api_key_file":
				target = &d.APIKeyFile
			case "trust_boundary":
				target = &d.TrustBoundary
			}
			value, valid := v.(string)
			if !valid {
				category := k
				switch k {
				case "api_path":
					category = "API path"
				case "api_key_env", "api_key_file":
					category = "credential"
				}
				invalid(category)
			}
			*target = value
		case "allowed_cidrs":
			values, ok := v.([]any)
			if !ok {
				invalid("allowed CIDR")
				continue
			}
			var out []string
			for _, item := range values {
				s, ok := item.(string)
				if !ok {
					invalid("allowed CIDR")
					continue
				}
				out = append(out, s)
			}
			d.AllowedCIDRs = out
		default:
			invalid("unknown setting")
		}
	}
	return nil
}

// DocuSealDisabled reports an absent or explicitly disabled integration.
func (d DocuSealConfig) DocuSealDisabled() bool {
	return !d.Enabled && !d.enabledInvalid && (d.explicitDisabled || d.decodeInvalid == "")
}

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
	if d.decodeInvalid != "" {
		return fail(d.decodeInvalid)
	}
	if !d.Enabled {
		return fail("configuration")
	}
	u, err := validateProviderEndpoint(d.Endpoint, "docuseal")
	if err != nil || u.Path != "" && u.Path != "/" || u.RawPath != "" {
		return fail("endpoint")
	}
	if d.TrustBoundary != "operator_network" && d.TrustBoundary != "hosted_provider" || d.TrustBoundary == "hosted_provider" && u.Scheme != "https" {
		return fail("trust_boundary")
	}
	if len(d.AllowedCIDRs) == 0 {
		return fail("allowed CIDR")
	}
	prefixes, err := parseAllowedCIDRs(d.AllowedCIDRs, "docuseal")
	if err != nil {
		return fail("allowed CIDR")
	}
	for _, prefix := range prefixes {
		if d.TrustBoundary == "operator_network" && !privateRenditionNetwork(prefix) {
			return fail("allowed CIDR")
		}
	}
	if d.APIPath != "" && (d.APIPath == "/" || d.APIPath[0] != '/' || path.Clean(d.APIPath) != d.APIPath || strings.ContainsAny(d.APIPath, "?#%\\") || strings.IndexFunc(d.APIPath, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0) {
		return fail("API path")
	}
	if (d.APIKeyEnv == "") == (d.APIKeyFile == "") {
		return fail("credential")
	}
	ref := secretref.Ref{Env: d.APIKeyEnv, File: d.APIKeyFile}
	if ref.File != "" {
		if !filepath.IsAbs(ref.File) {
			ref.File = filepath.Join(root, ref.File)
		}
		info, err := os.Stat(ref.File)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
			return fail("credential")
		}
	}
	secret, err := ref.Resolve()
	if err != nil || len(secret.Value) > 4096 {
		return fail("credential")
	}
	key := secret.Value
	key = strings.TrimSpace(key)
	if !validSecret(key) {
		return fail("credential")
	}
	return DocuSealConnection{Endpoint: strings.TrimSuffix(u.String(), "/"), APIPath: d.APIPath, APIKey: key, TrustBoundary: d.TrustBoundary, AllowedCIDRs: prefixes}, nil
}
