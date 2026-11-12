package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDocuSealResolveRequiresSafeConfiguration(t *testing.T) {
	t.Setenv("DOCUSEAL_TEST_KEY", "synthetic-key")
	valid := DocuSealConfig{Enabled: true, Endpoint: "http://127.0.0.1:3000", APIPath: "/api", APIKeyEnv: "DOCUSEAL_TEST_KEY", TrustBoundary: "operator_network", AllowedCIDRs: []string{"127.0.0.1/32"}}
	if _, err := valid.Resolve(t.Context(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	cases := []DocuSealConfig{{}, valid, valid, valid, valid}
	cases[1].AllowedCIDRs = []string{"0.0.0.0/0"}
	cases[2].TrustBoundary = "hosted_provider"
	cases[3].Endpoint = "http://user" + ":" + "password@localhost"
	cases[4].APIKeyFile = "also-set"
	for _, tc := range cases {
		if _, err := tc.Resolve(t.Context(), t.TempDir()); err == nil {
			t.Errorf("accepted unsafe configuration: %+v", tc)
		}
	}
}

func TestDocuSealResolveRejectsInvalidCredential(t *testing.T) {
	t.Setenv("DOCUSEAL_TEST_KEY", strings.Repeat("x", 4097))
	cfg := DocuSealConfig{Enabled: true, Endpoint: "http://127.0.0.1", APIKeyEnv: "DOCUSEAL_TEST_KEY", TrustBoundary: "operator_network", AllowedCIDRs: []string{"127.0.0.1/32"}}
	if _, err := cfg.Resolve(t.Context(), t.TempDir()); err == nil {
		t.Fatal("accepted oversized credential")
	}
}

func TestDocuSealMalformedConfigIsDeferred(t *testing.T) {
	for _, section := range []string{"enabled='yes'", "enabled=true\nsurprise='secret'", "enabled=true\nallowed_cidrs=42"} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("[integrations.docuseal]\n"+section+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(root)
		if err != nil {
			t.Fatal(err)
		}
		if err = cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		if _, err = cfg.Integrations.DocuSeal.Resolve(t.Context(), root); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe result: %v", err)
		}
	}
}

func TestDocuSealConfigTOMLAndKeyRotation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOCUSEAL_TEST_KEY", "first-key")
	text := "[integrations.docuseal]\nenabled=true\nendpoint='http://127.0.0.1:3000'\napi_path='/api'\napi_key_env='DOCUSEAL_TEST_KEY'\ntrust_boundary='operator_network'\nallowed_cidrs=['127.0.0.1/32']\n"
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	a, err := cfg.Integrations.DocuSeal.Resolve(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCUSEAL_TEST_KEY", "second-key")
	b, err := cfg.Integrations.DocuSeal.Resolve(t.Context(), root)
	if err != nil || a.APIKey == b.APIKey {
		t.Fatalf("rotation failed: %v", err)
	}
}

func TestDocuSealCredentialFileIsPrivateAndBounded(t *testing.T) {
	root := t.TempDir()
	key := filepath.Join(root, "key")
	cfg := DocuSealConfig{Enabled: true, Endpoint: "http://127.0.0.1", APIKeyFile: "key", TrustBoundary: "operator_network", AllowedCIDRs: []string{"127.0.0.1/32"}}
	if err := os.WriteFile(key, []byte("synthetic-file-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Resolve(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte(strings.Repeat("x", 4097)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Resolve(t.Context(), root); err == nil {
		t.Fatal("accepted oversized file")
	}
}
