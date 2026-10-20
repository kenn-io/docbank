package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/vision"
)

func TestProcessingVisionFingerprints(t *testing.T) {
	home := filepath.Join(t.TempDir(), "absent")
	t.Setenv("DOCBANK_HOME", home)
	deployment := strings.Repeat("a", 64)
	output, err := runCLI(t, "processing", "vision-fingerprints", "--model", "synthetic-vision", "--trust-boundary", "hosted_provider", "--endpoint", "https://vision.example.com", "--deployment-fingerprint", deployment)
	require.NoError(t, err)
	var fingerprints struct {
		Descriptor string `toml:"descriptor_fingerprint"`
		Disclosure string `toml:"disclosure_fingerprint"`
	}
	_, err = toml.Decode(output, &fingerprints)
	require.NoError(t, err)
	descriptor, err := vision.Descriptor("synthetic-vision", document.RenditionTrustHostedProvider)
	require.NoError(t, err)
	require.Equal(t, descriptor.Fingerprint, fingerprints.Descriptor)
	require.Equal(t, vision.DisclosureFingerprint(descriptor, "https://vision.example.com", deployment), fingerprints.Disclosure)
	require.NoDirExists(t, home)
	for _, args := range [][]string{
		{"--model", "synthetic-vision", "--trust-boundary", "hosted_provider", "--endpoint", "http://vision.example.com", "--deployment-fingerprint", strings.Repeat("a", 64)},
		{"--model", "synthetic-vision", "--trust-boundary", "hosted_provider", "--endpoint", "https://vision.example.com", "--deployment-fingerprint", "invalid"},
	} {
		_, err := runCLI(t, append([]string{"processing", "vision-fingerprints"}, args...)...)
		require.Error(t, err)
		require.Equal(t, exitUsage, commandExitCode(err, true))
		require.NoDirExists(t, home)
	}
}
