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
	cases := []DocuSealConfig{{}, valid, valid, valid, valid, valid, valid, valid, valid, valid, valid}
	cases[1].AllowedCIDRs = []string{"0.0.0.0/0"}
	cases[2].TrustBoundary = "hosted_provider"
	cases[3].Endpoint = "http://user" + ":" + "password@localhost"
	cases[4].APIKeyFile = "also-set"
	cases[5].APIPath = "/"
	cases[6].APIPath = "/api/../other"
	cases[7].APIPath = "/api?token=x"
	cases[8].APIPath = "api"
	cases[9].APIPath = "/api path"
	cases[10].APIPath = "/api\n"
	hosted := valid
	hosted.Endpoint = "https://example.test"
	hosted.TrustBoundary = "hosted_provider"
	hosted.APIPath = ""
	if _, err := hosted.Resolve(t.Context(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		if _, err := tc.Resolve(t.Context(), t.TempDir()); err == nil {
			t.Errorf("accepted unsafe configuration: %+v", tc)
		}
	}
}

func TestDocuSealMalformedConfigIsDeferred(t *testing.T) {
	t.Setenv("DOCUSEAL_TEST_KEY", "synthetic-key")
	for _, section := range []string{"enabled='yes'\nallowed_cidrs=['127.0.0.1/32']", "enabled=true\nsurprise='value'\nallowed_cidrs=['127.0.0.1/32']", "enabled=true\nallowed_cidrs=42"} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("[integrations.docuseal]\nendpoint='http://127.0.0.1'\napi_key_env='DOCUSEAL_TEST_KEY'\ntrust_boundary='operator_network'\n"+section+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(root)
		if err != nil {
			t.Fatal(err)
		}
		if err = cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		if _, err = cfg.Integrations.DocuSeal.Resolve(t.Context(), root); err == nil {
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

func TestDocuSealInvalidCredentials(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "key"), []byte("synthetic-file-token"), 0600); err != nil {
		t.Fatal(err)
	}
	valid := DocuSealConfig{Enabled: true, Endpoint: "http://127.0.0.1", APIKeyFile: "key", TrustBoundary: "operator_network", AllowedCIDRs: []string{"127.0.0.1/32"}}
	if _, err := valid.Resolve(t.Context(), root); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, value string
		file        bool
	}{
		{"oversized file", strings.Repeat("x", 4097), true}, {"control env", "part\x1ftoken", false}, {"DEL env", "part\x7ftoken", false}, {"NUL file", "part\x00token", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			cfg := DocuSealConfig{Enabled: true, Endpoint: "http://127.0.0.1", TrustBoundary: "operator_network", AllowedCIDRs: []string{"127.0.0.1/32"}}
			if tc.file {
				if err := os.WriteFile(filepath.Join(root, "key"), []byte(tc.value), 0600); err != nil {
					t.Fatal(err)
				}
				cfg.APIKeyFile = "key"
			} else {
				t.Setenv("DOCUSEAL_TEST_KEY", tc.value)
				cfg.APIKeyEnv = "DOCUSEAL_TEST_KEY"
			}
			if _, err := cfg.Resolve(t.Context(), root); err == nil {
				t.Fatal("accepted invalid credential")
			}
		})
	}
}

func TestDocuSealExplicitlyDisabledMalformedConfig(t *testing.T) {
	var cfg DocuSealConfig
	if err := cfg.UnmarshalTOML(map[string]any{"enabled": false, "unknown": true}); err != nil || !cfg.DocuSealDisabled() {
		t.Fatalf("disabled config: %+v, %v", cfg, err)
	}
	if err := cfg.UnmarshalTOML(map[string]any{"enabled": "false"}); err != nil || cfg.DocuSealDisabled() {
		t.Fatalf("malformed enabled: %+v, %v", cfg, err)
	}
}

func TestDocuSealResolveReportsSafeField(t *testing.T) {
	valid := DocuSealConfig{Enabled: true, Endpoint: "https://example.test", APIKeyEnv: "DOCUSEAL_TEST_KEY", TrustBoundary: "hosted_provider", AllowedCIDRs: []string{"127.0.0.1/32"}}
	for _, tc := range []struct {
		name string
		edit func(*DocuSealConfig)
		want string
	}{
		{"endpoint", func(d *DocuSealConfig) { d.Endpoint = "https://synthetic-secret@example.test/path" }, "invalid DocuSeal endpoint settings"},
		{"boundary", func(d *DocuSealConfig) { d.TrustBoundary = "synthetic-secret" }, "invalid DocuSeal trust_boundary settings"},
		{"hosted HTTP", func(d *DocuSealConfig) { d.Endpoint = "http://example.test" }, "invalid DocuSeal trust_boundary settings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.edit(&cfg)
			_, err := cfg.Resolve(t.Context(), t.TempDir())
			if err == nil || err.Error() != tc.want {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
}
