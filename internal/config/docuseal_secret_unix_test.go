//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDocuSealRejectsInsecureKeyFile(t *testing.T) {
	root := t.TempDir()
	key := filepath.Join(root, "key")
	if err := os.WriteFile(key, []byte("synthetic-key"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(key, 0644); err != nil {
		t.Fatal(err)
	}
	cfg := DocuSealConfig{Enabled: true, Endpoint: "http://127.0.0.1", APIKeyFile: "key", TrustBoundary: "operator_network", AllowedCIDRs: []string{"127.0.0.1/32"}}
	if _, err := cfg.Resolve(t.Context(), root); err == nil {
		t.Fatal("accepted publicly readable key")
	}
}
