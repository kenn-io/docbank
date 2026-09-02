package providerutil

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

const (
	maxSecretBytes     = 64 << 10
	maxIdentifierBytes = 128
	// maxJobIDBytes leaves room for an adapter prefix inside a 128-byte
	// receipt operation ID.
	maxJobIDBytes = 120
)

// SecretResolver resolves the one profile-bound named credential of an
// adapter. Secret values only ever reach the fixed request header.
type SecretResolver interface {
	ResolveSecret(ctx context.Context, name string) (string, error)
}

// Credential binds an optional named secret to one request header.
type Credential struct {
	Resolver SecretResolver
	Binding  string
	Header   string
	Prefix   string
}

// BearerCredential sends the resolved secret as an Authorization bearer token.
func BearerCredential(binding string, resolver SecretResolver) Credential {
	return Credential{Resolver: resolver, Binding: binding, Header: "Authorization", Prefix: "Bearer "}
}

// APIKeyCredential sends the resolved secret verbatim in header.
func APIKeyCredential(header, binding string, resolver SecretResolver) Credential {
	return Credential{Resolver: resolver, Binding: binding, Header: header}
}

// Validate requires the binding and resolver to be paired: both present or
// both absent. A present binding must be a valid identifier.
func (credential Credential) Validate(provider Provider) error {
	if credential.Binding == "" {
		if !IsNil(credential.Resolver) {
			return fmt.Errorf("%s: secret resolver requires a named binding", provider.prefix())
		}
		return nil
	}
	if IsNil(credential.Resolver) {
		return fmt.Errorf("%s: named secret binding requires a resolver", provider.prefix())
	}
	return provider.ValidateIdentifier(credential.Binding, "secret binding")
}

// Authorize resolves the bound secret and injects it into request.
func (credential Credential) Authorize(provider Provider, request *http.Request) error {
	if credential.Binding == "" {
		return nil
	}
	secret, err := credential.Resolver.ResolveSecret(request.Context(), credential.Binding)
	if err != nil {
		return provider.Classified(document.RenditionErrorAuthentication,
			string(provider)+" credential is unavailable", err)
	}
	if secret == "" || len(secret) > maxSecretBytes || strings.ContainsAny(secret, "\r\n\x00") {
		return provider.Classified(document.RenditionErrorAuthentication,
			string(provider)+" credential is invalid", nil)
	}
	request.Header.Set(credential.Header, credential.Prefix+secret)
	return nil
}

// ValidateOrigin accepts one absolute origin without path, credentials,
// query, or fragment. HTTPS is always allowed; plaintext HTTP only inside an
// operator network.
func (provider Provider) ValidateOrigin(raw string, trust document.RenditionTrustBoundary) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Opaque != "" ||
		parsed.ForceQuery || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", fmt.Errorf("%s: origin must be one absolute origin without path, credentials, query, or fragment",
			provider.prefix())
	}
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || trust != document.RenditionTrustOperatorNetwork) {
		return "", fmt.Errorf("%s: hosted origins require HTTPS; HTTP is operator-network only", provider.prefix())
	}
	if trust != document.RenditionTrustOperatorNetwork && trust != document.RenditionTrustHostedProvider {
		return "", fmt.Errorf("%s: network origin requires an operator-network or hosted trust boundary",
			provider.prefix())
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

// ValidateIdentifier accepts 1-128 bytes of ASCII letters, digits, '.', '_',
// and '-'.
func (provider Provider) ValidateIdentifier(value, subject string) error {
	if value == "" || len(value) > maxIdentifierBytes || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) {
		return fmt.Errorf("%s: %s is invalid", provider.prefix(), subject)
	}
	for _, character := range value {
		if !identifierCharacter(character, true) {
			return fmt.Errorf("%s: %s is invalid", provider.prefix(), subject)
		}
	}
	return nil
}

// ValidatePathIdentifier accepts an identifier that is safe as one URL path
// segment: a valid identifier other than the dot segments.
func (provider Provider) ValidatePathIdentifier(value, subject string) error {
	if value == "." || value == ".." {
		return fmt.Errorf("%s: %s is invalid", provider.prefix(), subject)
	}
	return provider.ValidateIdentifier(value, subject)
}

// ValidateJobID accepts a provider job identifier that fits inside a Docbank
// receipt operation ID after an adapter prefix: 1-120 bytes of lowercase
// letters, digits, '_', and '-'.
func ValidateJobID(value string) error {
	if value == "" || len(value) > maxJobIDBytes {
		return errors.New("job ID must contain 1-120 characters")
	}
	for _, character := range value {
		if !identifierCharacter(character, false) || character == '.' {
			return errors.New("job ID contains unsupported characters")
		}
	}
	return nil
}

func identifierCharacter(character rune, allowUpper bool) bool {
	return character >= 'a' && character <= 'z' || allowUpper && character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9' || character == '.' || character == '_' || character == '-'
}
