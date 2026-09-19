package capselfhosted

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/providerhttp"
)

// ProbeState is the bounded result of a deployment probe.
type ProbeState string

const (
	ProbeVerified              ProbeState = "verified"
	ProbeCredentialRejected    ProbeState = "credential_rejected" //nolint:gosec // bounded probe state, not a credential.
	ProbeCredentialMissing     ProbeState = "credential_missing"  //nolint:gosec // bounded probe state, not a credential.
	ProbeDNSDenied             ProbeState = "dns_denied"
	ProbeTLSPinMismatch        ProbeState = "tls_pin_mismatch"
	ProbeTLSVerificationFailed ProbeState = "tls_verification_failed"
	ProbeRedirectUnregistered  ProbeState = "redirect_unregistered"
	ProbeContractMismatch      ProbeState = "contract_mismatch"
	ProbeProviderUnavailable   ProbeState = "provider_unavailable"
	ProbeProviderRateLimited   ProbeState = "provider_rate_limited"
)

// Evidence reports only the adapter and deployment identity plus the probe
// result. It never carries a URL, response body, or credential value.
type Evidence struct {
	AdapterContract, DeploymentRevision string
	State                               ProbeState
}

// Client probes one registered Cap deployment.
type Client struct {
	deployment Deployment
	http       *http.Client
	credential providerutil.Credential
}

var capProvider = providerutil.Provider("cap")

// NewClient validates and builds a client for one registered deployment.
func NewClient(deployment Deployment, secrets providerutil.SecretResolver, resolver providerhttp.Resolver) (*Client, error) {
	origin, scheme, host, port, err := normalizeOrigin(deployment.Origin)
	if err != nil {
		return nil, err
	}
	if deployment.Egress.Scheme != scheme || !strings.EqualFold(deployment.Egress.Host, host) ||
		deployment.Egress.Port != port {
		return nil, errors.New("cap self-hosted deployment origin and egress authority differ")
	}
	if !strings.HasPrefix(deployment.CredentialBinding, "credential:") ||
		len(strings.TrimPrefix(deployment.CredentialBinding, "credential:")) == 0 {
		return nil, errors.New("cap self-hosted credential binding must use credential:<name>")
	}
	if err := capProvider.ValidateIdentifier(strings.TrimPrefix(deployment.CredentialBinding, "credential:"), "secret binding"); err != nil {
		return nil, err
	}
	if deployment.DeploymentRevision == "" || len(deployment.DeploymentRevision) > 128 {
		return nil, errors.New("cap self-hosted deployment revision must contain 1-128 bytes")
	}
	if deployment.ProbeTimeout <= 0 || deployment.ProbeTimeout > time.Minute {
		return nil, errors.New("cap self-hosted probe timeout must be between 1ns and 1m")
	}
	transport, err := providerhttp.NewTransport(deployment.Egress, resolver)
	if err != nil {
		return nil, err
	}
	credential := providerutil.BearerCredential(strings.TrimPrefix(deployment.CredentialBinding, "credential:"), secrets)
	if err := credential.Validate(capProvider); err != nil {
		return nil, err
	}
	deployment.Origin = origin
	return &Client{deployment: deployment,
		http: providerhttp.IsolateClient(&http.Client{Transport: transport}), credential: credential}, nil
}

func normalizeOrigin(raw string) (origin, scheme, host string, port uint16, err error) {
	parsed, parseErr := url.Parse(raw)
	if parseErr != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		parsed.Opaque != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		parsed.Path != "" && parsed.Path != "/" || parsed.RawPath != "" && parsed.RawPath != "/" {
		return "", "", "", 0, errors.New("cap self-hosted origin must be an absolute root origin")
	}
	scheme = strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", "", "", 0, errors.New("cap self-hosted origin scheme must be http or https")
	}
	host = strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", "", "", 0, errors.New("cap self-hosted origin host is required")
	}
	parsedPort := parsed.Port()
	if parsedPort == "" {
		if scheme == "https" {
			port = 443
		} else {
			port = 80
		}
	} else {
		value, parseErr := strconv.ParseUint(parsedPort, 10, 16)
		if parseErr != nil || value == 0 {
			return "", "", "", 0, errors.New("cap self-hosted origin port must be between 1 and 65535")
		}
		port = uint16(value)
	}
	authorityHost := host
	if strings.Contains(host, ":") {
		authorityHost = "[" + host + "]"
	}
	origin = scheme + "://" + authorityHost
	if (scheme == "http" && port != 80) || (scheme == "https" && port != 443) {
		origin += ":" + strconv.Itoa(int(port))
	}
	return origin, scheme, host, port, nil
}

// Probe calls Cap's documented usage endpoint through the registered policy.
func (client *Client) Probe(ctx context.Context) (Evidence, error) {
	if client == nil || client.http == nil {
		return Evidence{AdapterContract: AdapterContract, State: ProbeProviderUnavailable}, nil
	}
	evidence := Evidence{AdapterContract: AdapterContract,
		DeploymentRevision: client.deployment.DeploymentRevision}
	probeContext, cancel := context.WithTimeout(ctx, client.deployment.ProbeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(probeContext, http.MethodGet,
		strings.TrimSuffix(client.deployment.Origin, "/")+probePath, nil)
	if err != nil {
		return withProbeState(evidence, ProbeProviderUnavailable), nil
	}
	if err := client.credential.Authorize(capProvider, request); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return Evidence{}, contextErr
		}
		return withProbeState(evidence, ProbeCredentialMissing), nil
	}
	response, err := client.http.Do(request)
	if response != nil {
		defer func() { _ = response.Body.Close() }()
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return Evidence{}, contextErr
	}
	if err != nil {
		return withProbeState(evidence, classifyTransportError(err)), nil
	}
	switch {
	case response.StatusCode >= 300 && response.StatusCode < 400:
		return withProbeState(evidence, ProbeRedirectUnregistered), nil
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return withProbeState(evidence, ProbeCredentialRejected), nil
	case response.StatusCode == http.StatusTooManyRequests:
		return withProbeState(evidence, ProbeProviderRateLimited), nil
	case response.StatusCode != http.StatusOK:
		if response.StatusCode >= 500 && response.StatusCode < 600 {
			return withProbeState(evidence, ProbeProviderUnavailable), nil
		}
		return withProbeState(evidence, ProbeContractMismatch), nil
	}
	body, err := providerutil.ReadBounded(response.Body, maxProbeResponseBytes)
	if contextErr := ctx.Err(); contextErr != nil {
		return Evidence{}, contextErr
	}
	if probeContext.Err() != nil {
		return withProbeState(evidence, ProbeProviderUnavailable), nil
	}
	if err != nil {
		if errors.Is(err, providerutil.ErrResponseTooLarge) {
			return withProbeState(evidence, ProbeContractMismatch), nil
		}
		return withProbeState(evidence, ProbeProviderUnavailable), nil
	}
	if !validUsageResponse(body) {
		return withProbeState(evidence, ProbeContractMismatch), nil
	}
	return withProbeState(evidence, ProbeVerified), nil
}

func withProbeState(evidence Evidence, state ProbeState) Evidence {
	evidence.State = state
	return evidence
}

func classifyTransportError(err error) ProbeState {
	if errors.Is(err, providerhttp.ErrDestinationDenied) || errors.Is(err, providerhttp.ErrAddressDenied) {
		return ProbeDNSDenied
	}
	if errors.Is(err, providerhttp.ErrCertificatePin) {
		return ProbeTLSPinMismatch
	}
	var verificationError *tls.CertificateVerificationError
	var hostnameError x509.HostnameError
	var authorityError x509.UnknownAuthorityError
	var certificateError x509.CertificateInvalidError
	if errors.As(err, &verificationError) || errors.As(err, &hostnameError) ||
		errors.As(err, &authorityError) || errors.As(err, &certificateError) {
		return ProbeTLSVerificationFailed
	}
	return ProbeProviderUnavailable
}

func validUsageResponse(body []byte) bool {
	var object map[string]jsontext.Value
	if err := json.Unmarshal(body, &object); err != nil {
		return false
	}
	data, ok := object["data"]
	return ok && data.Kind() == jsontext.KindBeginObject
}
