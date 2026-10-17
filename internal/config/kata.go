package config

import (
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// IntegrationsConfig holds optional connections, validated only by their users.
type IntegrationsConfig struct {
	Kata KataConfig `toml:"kata"`
}

// KataConfig never contains a literal credential. Relative key files are vault-relative.
//
//nolint:recvcheck // TOML decoding mutates config; resolving a connection leaves it unchanged.
type KataConfig struct {
	Enabled        bool   `toml:"enabled"`
	Endpoint       string `toml:"endpoint"`
	APIKeyEnv      string `toml:"api_key_env"`
	APIKeyFile     string `toml:"api_key_file"`
	DefaultProject string `toml:"default_project"`
	decodeInvalid  bool
}

// UnmarshalTOML defers shape errors in this optional section until a Kata operation.
func (k *KataConfig) UnmarshalTOML(value any) error {
	table, ok := value.(map[string]any)
	if !ok {
		k.decodeInvalid = true
		return nil
	}
	for field, value := range table {
		if field == "enabled" {
			var valid bool
			k.Enabled, valid = value.(bool)
			k.decodeInvalid = k.decodeInvalid || !valid
			continue
		}
		var target *string
		switch field {
		case "endpoint":
			target = &k.Endpoint
		case "api_key_env":
			target = &k.APIKeyEnv
		case "api_key_file":
			target = &k.APIKeyFile
		case "default_project":
			target = &k.DefaultProject
		default:
			k.decodeInvalid = true
			continue
		}
		text, valid := value.(string)
		k.decodeInvalid = k.decodeInvalid || !valid
		*target = text
	}
	return nil
}

// KataConnection is resolved for one operation. Never serialize or log it.
type KataConnection struct {
	Endpoint string
	APIKey   string
	Project  string
}

// Resolve validates only this integration and reads the selected credential at use time.
func (k KataConfig) Resolve(root string) (KataConnection, error) {
	if k.decodeInvalid {
		return KataConnection{}, errors.New("integrations.kata: invalid field or type; credentials require api_key_env or api_key_file")
	}
	if !k.Enabled {
		return KataConnection{}, errors.New("kata integration is disabled; enable [integrations.kata]")
	}
	u, err := url.Parse(strings.TrimSpace(k.Endpoint))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		u.Opaque != "" || u.RawPath != "" || u.Path != "" && u.Path != "/" ||
		u.Scheme != "https" && u.Scheme != "http" {
		return KataConnection{}, errors.New("integrations.kata: invalid endpoint; use HTTPS or loopback HTTP")
	}
	if ip := net.ParseIP(u.Hostname()); u.Scheme == "http" &&
		!strings.EqualFold(u.Hostname(), "localhost") && (ip == nil || !ip.IsLoopback()) {
		return KataConnection{}, errors.New("integrations.kata: HTTP endpoint must be loopback")
	}
	if (k.APIKeyEnv == "") == (k.APIKeyFile == "") {
		return KataConnection{}, errors.New("integrations.kata: select exactly one of api_key_env or api_key_file")
	}
	key := os.Getenv(k.APIKeyEnv)
	if k.APIKeyFile != "" {
		path := k.APIKeyFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		// Inspect before opening: opening a FIFO can block before descriptor validation.
		info, statErr := os.Stat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
			return KataConnection{}, errors.New("integrations.kata: api_key_file must be a regular file of at most 4096 bytes")
		}
		// Bound credentials before reading, including special files.
		file, openErr := os.Open(path)
		if openErr != nil {
			return KataConnection{}, errors.New("integrations.kata: cannot read api_key_file")
		}
		defer func() { _ = file.Close() }()
		info, statErr = file.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
			return KataConnection{}, errors.New("integrations.kata: api_key_file must be a regular file of at most 4096 bytes")
		}
		data, readErr := io.ReadAll(io.LimitReader(file, 4097))
		if readErr != nil || len(data) > 4096 {
			return KataConnection{}, errors.New("integrations.kata: cannot read api_key_file")
		}
		key = string(data)
	}
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 4096 || strings.ContainsAny(key, "\r\n") {
		return KataConnection{}, errors.New("integrations.kata: credential is missing or invalid")
	}
	project := strings.TrimSpace(k.DefaultProject)
	if project == "" {
		project = "docbank"
	}
	if !ValidKataProject(project) {
		return KataConnection{}, errors.New("integrations.kata: invalid default_project")
	}
	return KataConnection{Endpoint: strings.TrimRight(u.String(), "/"), APIKey: key, Project: project}, nil
}

// ValidKataProject is shared by configuration and the native Kata client.
func ValidKataProject(project string) bool {
	if project == "" || len(project) > 128 || strings.TrimSpace(project) != project || strings.ContainsAny(project, "#/?\\%") {
		return false
	}
	for _, r := range project {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}
