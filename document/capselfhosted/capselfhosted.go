// Package capselfhosted recognizes storage-neutral self-hosted Cap references
// and probes the documented deployment endpoint.
package capselfhosted

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"time"

	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/internal/canonical"
)

const (
	// Provider identifies the self-hosted Cap adapter.
	Provider = "cap.self-hosted"
	// AdapterContract identifies the Cap adapter contract.
	AdapterContract = "cap-self-hosted-adapter/v1"
	// IdentityContract identifies the Cap video identity contract.
	IdentityContract      = "cap-self-hosted-video-id/v1"
	probePath             = "/api/developer/v1/usage"
	maxProbeResponseBytes = 64 << 10
	maxVideoIDBytes       = 128
)

// RecognizeSharePath accepts only Cap's share and embed route forms.
func RecognizeSharePath(escapedPath string) (identity string, ok bool) {
	prefix := "/s/"
	switch {
	case strings.HasPrefix(escapedPath, "/s/"):
	case strings.HasPrefix(escapedPath, "/embed/"):
		prefix = "/embed/"
	default:
		return "", false
	}
	id := strings.TrimPrefix(escapedPath, prefix)
	if id == "" || len(id) > maxVideoIDBytes {
		return "", false
	}
	for _, character := range id {
		if character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' || character == '_' || character == '-' {
			continue
		}
		return "", false
	}
	return IdentityContract + ":" + id, true
}

// Deployment describes one operator-registered Cap deployment.
type Deployment struct {
	Origin                                string
	Egress                                providerhttp.EgressPolicy
	CredentialBinding, DeploymentRevision string
	ProbeTimeout                          time.Duration
}

type egressIdentity struct {
	Scheme              string   `json:"scheme"`
	Host                string   `json:"host"`
	Port                uint16   `json:"port"`
	AllowedCIDRs        []string `json:"allowed_cidrs"`
	ProxyMode           string   `json:"proxy_mode"`
	SPKISHA256          []string `json:"spki_sha256"`
	ConnectTimeout      int64    `json:"connect_timeout_nanos"`
	KeepAlive           int64    `json:"keep_alive_nanos"`
	TLSHandshakeTimeout int64    `json:"tls_handshake_timeout_nanos"`
}

type deploymentIdentity struct {
	AdapterContract    string         `json:"adapter_contract"`
	Origin             string         `json:"origin"`
	DeploymentRevision string         `json:"deployment_revision"`
	CredentialBinding  string         `json:"credential_binding"`
	Egress             egressIdentity `json:"egress"`
}

func deploymentEgressIdentity(policy providerhttp.EgressPolicy) egressIdentity {
	cidrs := make([]string, len(policy.AllowedCIDRs))
	for index, prefix := range policy.AllowedCIDRs {
		cidrs[index] = prefix.Masked().String()
	}
	slices.Sort(cidrs)
	pins := slices.Clone(policy.TLS.SPKISHA256)
	slices.Sort(pins)
	return egressIdentity{Scheme: policy.Scheme, Host: strings.ToLower(policy.Host), Port: policy.Port,
		AllowedCIDRs: cidrs, ProxyMode: string(policy.ProxyMode), SPKISHA256: pins,
		ConnectTimeout: int64(policy.ConnectTimeout), KeepAlive: int64(policy.KeepAlive),
		TLSHandshakeTimeout: int64(policy.TLSHandshakeTimeout)}
}

// Fingerprint returns the public identity of the registered deployment.
func (deployment Deployment) Fingerprint() (string, error) {
	encoded, err := canonical.Marshal(deploymentIdentity{
		AdapterContract: AdapterContract, Origin: deployment.Origin,
		DeploymentRevision: deployment.DeploymentRevision, CredentialBinding: deployment.CredentialBinding,
		Egress: deploymentEgressIdentity(deployment.Egress),
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// ContractFingerprint returns the SHA-256 identity of a contract string.
func ContractFingerprint(contract string) string {
	digest := sha256.Sum256([]byte(contract))
	return hex.EncodeToString(digest[:])
}
