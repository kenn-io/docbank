// Package qmdbridge connects an operator-hosted QMD query endpoint to exact
// current Docbank export and rendition authority.
package qmdbridge

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/internal/store"
)

const (
	maximumQueries       = 10
	maximumQueryBytes    = 64 << 10
	maximumIntentBytes   = 64 << 10
	maximumRequestBytes  = int64(8 << 20)
	maximumResponseBytes = int64(64 << 20)
	maximumSnippetBytes  = 64 << 10
	maximumTimeout       = 5 * time.Minute
	maximumTokenBytes    = 1024
)

type SecretResolver interface {
	ResolveSecret(ctx context.Context, binding string) (string, error)
}

type Authorizer interface {
	AuthorizeQMDQuery(ctx context.Context, operation Operation) error
}

type Authority interface {
	NormalizeQMDSearchScope(ctx context.Context, scope store.SearchOptions) (store.SearchOptions, error)
	RevalidateQMDExportCandidates(ctx context.Context, candidates []store.QMDExportSource,
		scope store.SearchOptions) ([]store.QMDExportLiveCandidate, error)
}

type Profile struct {
	ID                 string
	CompatibilityEpoch string
	SecretBinding      string
	EndpointPath       string
	RequestTimeout     time.Duration
	MaxQueryBytes      int
	MaxIntentBytes     int
	MaxCandidates      int
	MaxSnippetBytes    int
	MaxRequestBytes    int64
	MaxResponseBytes   int64
	EgressPolicy       providerhttp.EgressPolicy
}

type Client struct {
	profile    Profile
	secrets    SecretResolver
	authorizer Authorizer
	authority  Authority
	root       string
	endpoint   string
	http       *http.Client
}

func New(profile Profile, secrets SecretResolver, authorizer Authorizer, authority Authority,
	root string, resolver providerhttp.Resolver, supplied *http.Client,
) (*Client, error) {
	if supplied == nil || nilInterface(secrets) || nilInterface(authorizer) || nilInterface(authority) {
		return nil, errors.New("qmd bridge requires HTTP settings, named secret, authorization, and live authority")
	}
	normalized, err := normalizeProfile(profile)
	if err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(root)
	if err != nil || filesystemRoot(absolute) {
		return nil, errors.New("qmd bridge export root is invalid")
	}
	transport, err := providerhttp.NewTransport(normalized.EgressPolicy, resolver)
	if err != nil {
		return nil, errors.New("qmd bridge egress policy is invalid")
	}
	isolated := *supplied
	isolated.Transport = transport
	isolated.CheckRedirect = providerhttp.RefuseRedirects
	isolated.Jar = nil
	isolated.Timeout = 0
	authorityHost := net.JoinHostPort(normalized.EgressPolicy.Host, strconv.Itoa(int(normalized.EgressPolicy.Port)))
	endpoint := normalized.EgressPolicy.Scheme + "://" + authorityHost + normalized.EndpointPath
	return &Client{profile: normalized, secrets: secrets, authorizer: authorizer,
		authority: authority, root: absolute, endpoint: endpoint, http: &isolated}, nil
}

func filesystemRoot(value string) bool {
	volume := filepath.VolumeName(value)
	return filepath.Clean(value) == filepath.Clean(volume+string(filepath.Separator))
}

func normalizeProfile(profile Profile) (Profile, error) {
	profile.EgressPolicy.AllowedCIDRs = slices.Clone(profile.EgressPolicy.AllowedCIDRs)
	profile.EgressPolicy.TLS.SPKISHA256 = slices.Clone(profile.EgressPolicy.TLS.SPKISHA256)
	if profile.RequestTimeout == 0 {
		profile.RequestTimeout = 30 * time.Second
	}
	if profile.MaxQueryBytes == 0 {
		profile.MaxQueryBytes = 4096
	}
	if profile.MaxIntentBytes == 0 {
		profile.MaxIntentBytes = 4096
	}
	if profile.MaxCandidates == 0 {
		profile.MaxCandidates = 100
	}
	if profile.MaxSnippetBytes == 0 {
		profile.MaxSnippetBytes = 4096
	}
	if profile.MaxRequestBytes == 0 {
		profile.MaxRequestBytes = 1 << 20
	}
	if profile.MaxResponseBytes == 0 {
		profile.MaxResponseBytes = 8 << 20
	}
	if profile.EgressPolicy.ConnectTimeout == 0 {
		profile.EgressPolicy.ConnectTimeout = providerhttp.DefaultConnectTimeout
	}
	if profile.EgressPolicy.KeepAlive == 0 {
		profile.EgressPolicy.KeepAlive = providerhttp.DefaultKeepAlive
	}
	if profile.EgressPolicy.TLSHandshakeTimeout == 0 {
		profile.EgressPolicy.TLSHandshakeTimeout = providerhttp.DefaultTLSHandshakeTimeout
	}
	if profile.EgressPolicy.ProxyMode == "" {
		profile.EgressPolicy.ProxyMode = providerhttp.ProxyDisabled
	}
	if !validToken(profile.ID) || !validToken(profile.CompatibilityEpoch) || !validToken(profile.SecretBinding) ||
		!validEndpointPath(profile.EndpointPath) || profile.RequestTimeout <= 0 || profile.RequestTimeout > maximumTimeout ||
		profile.MaxQueryBytes < 1 || profile.MaxQueryBytes > maximumQueryBytes ||
		profile.MaxIntentBytes < 1 || profile.MaxIntentBytes > maximumIntentBytes ||
		profile.MaxCandidates < 1 || profile.MaxCandidates > document.MaxRetrievalCandidateLimit ||
		profile.MaxSnippetBytes < 1 || profile.MaxSnippetBytes > maximumSnippetBytes ||
		profile.MaxRequestBytes < 1 || profile.MaxRequestBytes > maximumRequestBytes ||
		profile.MaxResponseBytes < 1 || profile.MaxResponseBytes > maximumResponseBytes {
		return Profile{}, errors.New("qmd bridge profile is invalid")
	}
	return profile, nil
}

func validEndpointPath(value string) bool {
	return value != "" && len(value) <= 256 && strings.HasPrefix(value, "/") &&
		path.Clean(value) == value && !strings.ContainsAny(value, "?#\\") && utf8.ValidString(value)
}

func validToken(value string) bool {
	return value != "" && len(value) <= maximumTokenBytes && utf8.ValidString(value) &&
		value == strings.TrimSpace(value) && strings.IndexFunc(value, func(character rune) bool {
		return unicode.IsControl(character) || unicode.IsSpace(character)
	}) < 0
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func finiteUnit(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}
