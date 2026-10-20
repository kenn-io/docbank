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
	for _, tc := range []struct {
		flag string
		args []string
	}{
		{"--trust-boundary", []string{"--model", "synthetic-vision", "--trust-boundary", "private", "--endpoint", "https://vision.example.com", "--deployment-fingerprint", deployment}},
		{"--trust-boundary", []string{"--model", "synthetic-vision", "--trust-boundary", "local_process", "--endpoint", "https://vision.example.com", "--deployment-fingerprint", deployment}},
		{"--endpoint", []string{"--model", "synthetic-vision", "--trust-boundary", "hosted_provider", "--deployment-fingerprint", deployment}},
		{"--endpoint", []string{"--model", "synthetic-vision", "--trust-boundary", "hosted_provider", "--endpoint", "https://vision.example.com/v1", "--deployment-fingerprint", deployment}},
		{"--endpoint", []string{"--model", "synthetic-vision", "--trust-boundary", "hosted_provider", "--endpoint", "http://vision.example.com", "--deployment-fingerprint", deployment}},
		{"--model", []string{"--trust-boundary", "hosted_provider", "--endpoint", "https://vision.example.com", "--deployment-fingerprint", deployment}},
		{"--deployment-fingerprint", []string{"--model", "synthetic-vision", "--trust-boundary", "hosted_provider", "--endpoint", "https://vision.example.com", "--deployment-fingerprint", "invalid"}},
	} {
		output, err := runCLI(t, append([]string{"processing", "vision-fingerprints"}, tc.args...)...)
		require.Error(t, err)
		require.ErrorContains(t, err, tc.flag)
		require.Equal(t, exitUsage, commandExitCode(err, true))
		require.Empty(t, output)
		require.NoDirExists(t, home)
	}
}
