//go:build ignore

// processing-profile prints the dynamic descriptor identities used by the
// real-daemon screenshot harness. The embedding identity binds the loopback
// endpoint selected by the test, so it cannot be checked in as a constant.
package main

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/openaiembed"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/document/providerhttp"
)

type identities struct {
	RenditionID          string `json:"rendition_id"`
	RenditionFingerprint string `json:"rendition_fingerprint"`
	EmbeddingID          string `json:"embedding_id"`
	EmbeddingFingerprint string `json:"embedding_fingerprint"`
	CompatibilityID      string `json:"compatibility_id"`
}

func main() {
	if len(os.Args) != 2 {
		panic("usage: go run processing-profile.go <loopback-origin>")
	}
	origin := os.Args[1]
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" {
		panic("loopback origin must be an exact http://127.0.0.1:<port> URL")
	}
	port, err := strconv.ParseUint(parsed.Port(), 10, 16)
	if err != nil {
		panic(err)
	}

	renderer, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: plaintext.MaxDocumentBytes})
	if err != nil {
		panic(err)
	}
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileNomic,
	})
	if err != nil {
		panic(err)
	}
	descriptor, err := document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{
		ID:                    openaiembed.ProviderID,
		ContractVersion:       document.EmbeddingProviderContractVersion,
		PolicyFingerprint:     strings.Repeat("0", 64),
		TrustBoundary:         document.EmbeddingTrustOperatorNetwork,
		Model:                 "synthetic-model",
		ModelRevision:         "deployment-v1",
		Dimension:             2,
		Metric:                document.VectorMetricCosine,
		Normalization:         document.VectorNormalizationNone,
		ScalarEncoding:        openaiembed.ScalarEncodingFloat32,
		DocumentFormatter:     openaiembed.DocumentFormatterV1,
		QueryFormatter:        openaiembed.QueryFormatterV1,
		InputKinds:            []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk},
		CompatibilityID:       contract.CompatibilityID,
		SupportsTextQuery:     true,
		ModelInput:            contract,
		SupportedRequestModes: []document.ModelInputMode{contract.Document.Mode},
	})
	if err != nil {
		panic(err)
	}
	profile := openaiembed.Profile{
		Origin: origin, Descriptor: descriptor, ModelInput: contract,
		SecretBinding: "credential:semantic", DeploymentEpoch: "deployment-v1",
		RequestTimeout: time.Second, MaxBatchItems: 8, MaxInputBytes: 1 << 20,
		MaxRequestBytes: 1 << 20, MaxResponseBytes: 1 << 20,
		EgressPolicy: providerhttp.EgressPolicy{
			Scheme: "http", Host: "127.0.0.1", Port: uint16(port),
			AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
			ProxyMode:    providerhttp.ProxyDisabled, ConnectTimeout: time.Second,
			KeepAlive: time.Second, TLSHandshakeTimeout: time.Second,
		},
	}
	policyFingerprint, err := openaiembed.PolicyFingerprint(profile)
	if err != nil {
		panic(err)
	}
	descriptor.PolicyFingerprint, descriptor.Fingerprint = policyFingerprint, ""
	descriptor, err = document.NewEmbeddingDescriptor(descriptor)
	if err != nil {
		panic(err)
	}
	result := identities{
		RenditionID: renderer.Descriptor().ID, RenditionFingerprint: renderer.Descriptor().Fingerprint,
		EmbeddingID: descriptor.ID, EmbeddingFingerprint: descriptor.Fingerprint,
		CompatibilityID: contract.CompatibilityID,
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		panic(fmt.Errorf("encode identities: %w", err))
	}
}
