package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/config"
)

func TestConfigureMediaOriginsBuildsCapPolicy(t *testing.T) {
	base := mediaOriginTestConfig()
	policies, probes, err := configureMediaOrigins(base)
	require.NoError(t, err)
	policy := policies["team-cap"]
	require.Equal(t, "https://cap.example.test", policy.ExactOrigin)
	require.NotNil(t, policy.RecognizePath)
	require.False(t, policy.AcquisitionAvailable)
	require.Contains(t, probes, "team-cap")

	mutations := []struct {
		name string
		edit func(*config.Config)
	}{
		{name: "deployment revision", edit: func(cfg *config.Config) {
			origin := cfg.MediaOrigins["team-cap"]
			origin.DeploymentRevision = "v-next"
			cfg.MediaOrigins["team-cap"] = origin
		}},
		{name: "allowed CIDRs", edit: func(cfg *config.Config) {
			origin := cfg.MediaOrigins["team-cap"]
			origin.AllowedCIDRs = []string{"127.0.0.1/32"}
			cfg.MediaOrigins["team-cap"] = origin
		}},
		{name: "SPKI pin", edit: func(cfg *config.Config) {
			origin := cfg.MediaOrigins["team-cap"]
			origin.SPKISHA256 = []string{strings.Repeat("b", 64)}
			cfg.MediaOrigins["team-cap"] = origin
		}},
		{name: "credential binding", edit: func(cfg *config.Config) {
			cfg.CredentialBindings["cap-b"] = config.CredentialBindingConfig{EnvironmentVariable: "CAP_API_KEY_B"}
			origin := cfg.MediaOrigins["team-cap"]
			origin.CredentialBinding = "credential:cap-b"
			cfg.MediaOrigins["team-cap"] = origin
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			cfg := mediaOriginTestConfig()
			mutation.edit(&cfg)
			changed, _, err := configureMediaOrigins(cfg)
			require.NoError(t, err)
			require.NotEqual(t, policy.ResolverFingerprint, changed["team-cap"].ResolverFingerprint)
		})
	}

	for _, endpoint := range []string{"https://cap.example.test.:443", "https://bücher.example:443"} {
		cfg := mediaOriginTestConfig()
		origin := cfg.MediaOrigins["team-cap"]
		origin.Endpoint = endpoint
		cfg.MediaOrigins["team-cap"] = origin
		require.Error(t, cfg.Validate(), endpoint)
	}
}

func mediaOriginTestConfig() config.Config {
	cfg := config.Default()
	cfg.CredentialBindings["cap-a"] = config.CredentialBindingConfig{EnvironmentVariable: "CAP_API_KEY"}
	cfg.MediaOrigins["team-cap"] = config.MediaOriginConfig{
		Endpoint: "HTTPS://Cap.Example.Test:443", AllowedCIDRs: []string{"127.0.0.0/8"},
		SPKISHA256: []string{strings.Repeat("a", 64)}, ProxyMode: "disabled",
		ConnectTimeout: config.Duration(time.Second), KeepAlive: config.Duration(time.Second),
		TLSHandshakeTimeout: config.Duration(time.Second),
		Provider:            "cap.self-hosted", CredentialBinding: "credential:cap-a",
		DeploymentRevision: "v-synthetic", ProbeTimeout: config.Duration(time.Second),
	}
	return cfg
}
