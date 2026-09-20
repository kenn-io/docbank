package typesafe

import (
	"context"
	"crypto/x509"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"go.kenn.io/docbank/document/providerhttp"
)

type testSecrets struct{}

func (testSecrets) ResolveSecret(context.Context, string) (string, error) {
	return "synthetic-secret", nil
}

func testProfile() Profile {
	return Profile{SecretBinding: "typesafe", EgressPolicy: providerhttp.EgressPolicy{
		Scheme: "https", Host: "api.typesafe.ai", Port: 443,
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")},
	}, RequestTimeout: time.Second}
}

func TestNewPinsTypeSafeEgressAndModel(t *testing.T) {
	profile := testProfile()
	client, err := New(profile, testSecrets{}, nil, http.DefaultClient)
	if err != nil || client == nil {
		t.Fatalf("valid profile: client=%v err=%v", client, err)
	}
	for name, mutate := range map[string]func(*Profile){
		"host":  func(value *Profile) { value.EgressPolicy.Host = "api.typesafe.ai.example" },
		"port":  func(value *Profile) { value.EgressPolicy.Port = 8443 },
		"proxy": func(value *Profile) { value.EgressPolicy.ProxyMode = "proxy" },
		"roots": func(value *Profile) { value.EgressPolicy.TLS.RootCAs = x509.NewCertPool() },
		"model": func(value *Profile) { value.Model = "jev-latest" },
	} {
		candidate := profile
		mutate(&candidate)
		if _, err := New(candidate, testSecrets{}, nil, http.DefaultClient); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestPolicyFingerprintBindsBounds(t *testing.T) {
	profile := testProfile()
	base, err := PolicyFingerprint(profile)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Profile){
		"candidates":  func(value *Profile) { value.MaxCandidates = 99 },
		"query":       func(value *Profile) { value.MaxQueryBytes = 99 },
		"excerpt":     func(value *Profile) { value.MaxCandidateBytes = 99 },
		"request":     func(value *Profile) { value.MaxRequestBytes = 99 },
		"response":    func(value *Profile) { value.MaxResponseBytes = 99 },
		"concurrency": func(value *Profile) { value.MaxConcurrentCalls = 2 },
	} {
		candidate := profile
		mutate(&candidate)
		fingerprint, fingerprintErr := PolicyFingerprint(candidate)
		if fingerprintErr != nil || fingerprint == base {
			t.Errorf("%s did not change fingerprint: %v", name, fingerprintErr)
		}
	}
}

func TestPolicyFingerprintBindsEveryProfileFieldWithoutMutation(t *testing.T) {
	profile := testProfile()
	profile.RequestShape = RequestShapeBatched
	profile.RequestTimeout = time.Second
	profile.SecretBinding = "original"
	profile.EgressPolicy.AllowedCIDRs = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	base := profile
	fingerprint, err := PolicyFingerprint(profile)
	if err != nil {
		t.Fatal(err)
	}
	if profile.EgressPolicy.AllowedCIDRs[0] != base.EgressPolicy.AllowedCIDRs[0] {
		t.Fatal("profile mutated")
	}
	for name, mutate := range map[string]func(*Profile){
		"secret":  func(value *Profile) { value.SecretBinding = "changed" },
		"shape":   func(value *Profile) { value.RequestShape = RequestShapePerCandidate },
		"timeout": func(value *Profile) { value.RequestTimeout += time.Second },
		"egress": func(value *Profile) {
			value.EgressPolicy.AllowedCIDRs = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
		},
	} {
		candidate := profile
		mutate(&candidate)
		changed, changeErr := PolicyFingerprint(candidate)
		if changeErr != nil || changed == fingerprint {
			t.Errorf("%s did not change fingerprint: %v", name, changeErr)
		}
	}
}
