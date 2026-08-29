#!/usr/bin/env bash
# Run the retained operator-network processing acceptance against only a
# synthetic, disposable embedded vault. Provider values are intentionally
# accepted only through the DOCBANK_PRIVATE_PROCESSING_* environment below.
set -euo pipefail

umask 077

die() {
  printf '%s\n' "$*" >&2
  exit 78
}

require_value() {
  local name=$1
  [[ -n ${!name:-} ]] || die "private processing prerequisite missing: ${name}"
}

for required in \
  DOCBANK_PRIVATE_PROCESSING_DOCLING_URL \
  DOCBANK_PRIVATE_PROCESSING_EMBEDDING_URL \
  DOCBANK_PRIVATE_PROCESSING_EMBEDDING_API_KEY \
  DOCBANK_PRIVATE_PROCESSING_EMBEDDING_MODEL \
  DOCBANK_PRIVATE_PROCESSING_EMBEDDING_REVISION \
  DOCBANK_PRIVATE_PROCESSING_EMBEDDING_DIMENSIONS \
  DOCBANK_PRIVATE_PROCESSING_DOCLING_ALLOWED_CIDRS \
  DOCBANK_PRIVATE_PROCESSING_EMBEDDING_ALLOWED_CIDRS; do
  require_value "$required"
done

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
repo_root=$(cd -- "${script_dir}/.." && pwd -P)
[[ -f ${repo_root}/go.mod ]] || die "private processing prerequisite missing: repository go.mod"
command -v go >/dev/null 2>&1 || die "private processing prerequisite missing: go"
command -v setsid >/dev/null 2>&1 || die "private processing prerequisite missing: setsid"
setsid_path=$(command -v setsid)
module_cache=$(env -i PATH="$PATH" HOME="$HOME" GOPROXY=off go env GOMODCACHE)
[[ -d $module_cache ]] || die "private processing prerequisite missing: Go module cache"

temp_root=""
binary=""
worker_pid=""
worker_pid_file=""
worker_launching=0

cleanup() {
  local status=$?
  local cleanup_failed=0
  trap - EXIT INT TERM HUP
  if [[ -z $worker_pid && $worker_launching == 1 && -n $worker_pid_file ]]; then
    for _ in $(seq 1 50); do
      if [[ -s $worker_pid_file ]]; then
        worker_pid=$(<"$worker_pid_file")
        break
      fi
      sleep 0.1
    done
  fi
  if [[ -n $worker_pid ]]; then
    kill -TERM -- "-${worker_pid}" 2>/dev/null || true
    for _ in $(seq 1 50); do
      if ! kill -0 -- "-${worker_pid}" 2>/dev/null; then
        break
      fi
      sleep 0.1
    done
    if kill -0 -- "-${worker_pid}" 2>/dev/null; then
      kill -KILL -- "-${worker_pid}" 2>/dev/null || true
    fi
    wait "$worker_pid" 2>/dev/null || true
  fi
  if [[ -n $temp_root && -e $temp_root ]] && ! find "$temp_root" -depth -delete; then
    cleanup_failed=1
  fi
  if ((cleanup_failed)); then
    printf '%s\n' 'private processing cleanup failed' >&2
    status=1
  fi
  exit "$status"
}
trap cleanup EXIT INT TERM HUP

temp_root=$(mktemp -d "${TMPDIR:-/tmp}/docbank-private-processing.XXXXXXXX")
binary=${temp_root}/private-processing
worker_pid_file=${temp_root}/worker.pid
mkdir -p "${temp_root}/go-tmp"

cat >"${temp_root}/main.go" <<'HARNESS'
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	docbank "go.kenn.io/docbank"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/docling"
	"go.kenn.io/docbank/document/openaiembed"
	"go.kenn.io/docbank/document/providerhttp"
)

const (
	profileName       = "private-acceptance"
	bindingName       = "private-embedding"
	anchor            = "retainedprivateacceptanceanchor"
	processingJobs    = 2
	destinationDocling = "docling"
	destinationEmbedding = "embedding"
	doclingTotalTimeout = 10 * time.Minute
	embeddingRequestTimeout = 30 * time.Second
	documentEmbeddingRequests = 2
	semanticQueryRequests = 4
	localOverheadBudget = 2 * time.Minute
	acceptanceDeadline = 25 * time.Minute
)

var (
	errPublicEgress  = errors.New("public provider egress rejected")
	errMalformedCIDR = errors.New("provider allowed CIDR is malformed")
	errLiteralOrigin = errors.New("private literal provider endpoint required")
	errDestinationMismatch = errors.New("provider transport destination mismatch")
	errUnrecordedDial = errors.New("provider transport dial was unrecorded")
)

type settings struct {
	doclingURL, doclingKey, embeddingURL, embeddingKey string
	embeddingModel, embeddingRevision                 string
	embeddingDimensions                               int
	doclingCIDRs, embeddingCIDRs                      []netip.Prefix
}

type secretResolver map[string]string

func (values secretResolver) ResolveSecret(_ context.Context, name string) (string, error) {
	value := values[name]
	if value == "" {
		return "", errors.New("private processing credential is unavailable")
	}
	return value, nil
}

type destinationRecorder struct {
	mu                 sync.Mutex
	requests           map[string]int
	dials              map[string]int
	destinationClasses map[string]int
}

func newDestinationRecorder() *destinationRecorder {
	return &destinationRecorder{requests: make(map[string]int), dials: make(map[string]int),
		destinationClasses: make(map[string]int)}
}

func (recorder *destinationRecorder) request(providerClass string) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.requests[providerClass]++
}

func (recorder *destinationRecorder) dial(providerClass string, remote net.Addr,
	expected netip.AddrPort, cidrs []netip.Prefix,
) error {
	actual, err := remoteAddrPort(remote)
	if err != nil {
		return errDestinationMismatch
	}
	if !operatorAddress(actual.Addr()) {
		return errPublicEgress
	}
	if actual != expected || !containedBy(actual.Addr(), cidrs) {
		return errDestinationMismatch
	}
	destinationClass := classifyDestination(actual.Addr())
	if destinationClass == "" || providerClass != destinationDocling && providerClass != destinationEmbedding {
		return errUnrecordedDial
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.dials[providerClass]++
	recorder.destinationClasses[destinationClass]++
	return nil
}

func (recorder *destinationRecorder) summary() (int, int, string, string, error) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	requests, dials := 0, 0
	providerClasses := make([]string, 0, 2)
	for _, providerClass := range []string{destinationDocling, destinationEmbedding} {
		requests += recorder.requests[providerClass]
		dials += recorder.dials[providerClass]
		if recorder.requests[providerClass] > 0 && recorder.dials[providerClass] == 0 {
			return 0, 0, "", "", errUnrecordedDial
		}
		if recorder.dials[providerClass] > 0 {
			providerClasses = append(providerClasses, providerClass)
		}
	}
	if requests == 0 || dials == 0 || len(recorder.requests) != len(providerClasses) || len(recorder.dials) != len(providerClasses) {
		return 0, 0, "", "", errUnrecordedDial
	}
	classes := make([]string, 0, len(recorder.destinationClasses))
	for _, class := range []string{"loopback", "link_local", "private_ipv4", "cgnat", "ula"} {
		if recorder.destinationClasses[class] > 0 {
			classes = append(classes, class)
		}
	}
	if len(classes) != len(recorder.destinationClasses) || len(classes) == 0 {
		return 0, 0, "", "", errUnrecordedDial
	}
	return requests, dials, strings.Join(providerClasses, ","), strings.Join(classes, ","), nil
}

type trackingTransport struct {
	base          http.RoundTripper
	recorder      *destinationRecorder
	providerClass string
}

// literalResolver deliberately has no DNS transport. strictOrigin only admits
// the same literal address it returns here.
type literalResolver struct{ address netip.Addr }

func (resolver literalResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{resolver.address}, nil
}

func (transport trackingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.recorder.request(transport.providerClass)
	return transport.base.RoundTrip(request)
}

type syntheticRemoteAddr string

func (address syntheticRemoteAddr) Network() string { return "tcp" }
func (address syntheticRemoteAddr) String() string  { return string(address) }

func verifyDestinationRecorderBehavior() error {
	recorder := newDestinationRecorder()
	recorder.request(destinationDocling)
	if _, _, _, _, err := recorder.summary(); !errors.Is(err, errUnrecordedDial) {
		return errors.New("destination recorder accepted an unrecorded provider request")
	}
	expected := netip.MustParseAddrPort("127.0.0.1:8443")
	cidrs := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	if err := recorder.dial(destinationDocling, syntheticRemoteAddr("127.0.0.1:8444"), expected, cidrs); !errors.Is(err, errDestinationMismatch) {
		return errors.New("destination recorder accepted a mismatched remote address")
	}
	if err := recorder.dial(destinationDocling, syntheticRemoteAddr(expected.String()), expected, cidrs); err != nil {
		return errors.New("destination recorder rejected a matching private remote address")
	}
	_, _, providers, classes, err := recorder.summary()
	if err != nil || providers != destinationDocling || classes != "loopback" {
		return errors.New("destination recorder did not retain aggregate destination classes")
	}
	return nil
}

type runeTokenizer struct{}

func (runeTokenizer) Identity() document.TokenizerIdentity {
	return document.TokenizerIdentity{Name: "private-acceptance-runes", Revision: "v1", PrefixTokenCountsMonotonic: true}
}

func (runeTokenizer) Tokenize(text string, limit int) ([]document.TokenBoundary, error) {
	count := utf8.RuneCountInString(text)
	if count > limit {
		return nil, document.ErrTokenizerLimit
	}
	boundaries := make([]document.TokenBoundary, count)
	for index := range boundaries {
		boundaries[index] = document.TokenBoundary{Start: index, End: index + 1}
	}
	return boundaries, nil
}

func main() {
	if err := run(); err != nil {
		// Provider error bodies, temporary paths, and credentials are deliberately
		// not part of retained acceptance output.
		if errors.Is(err, errPublicEgress) {
			fmt.Fprintln(os.Stderr, "private processing prerequisite failed: public provider egress rejected")
		} else if errors.Is(err, errMalformedCIDR) {
			fmt.Fprintln(os.Stderr, "private processing prerequisite failed: malformed provider allowed CIDR")
		} else if errors.Is(err, errLiteralOrigin) {
			fmt.Fprintln(os.Stderr, "private processing prerequisite failed: private literal-IP provider endpoint required")
		} else if errors.Is(err, errDestinationMismatch) {
			fmt.Fprintln(os.Stderr, "private processing prerequisite failed: provider transport destination mismatch")
		} else if errors.Is(err, errUnrecordedDial) {
			fmt.Fprintln(os.Stderr, "private processing prerequisite failed: provider transport dial was unrecorded")
		} else {
			fmt.Fprintln(os.Stderr, "private processing acceptance failed")
		}
		os.Exit(1)
	}
}

func run() error {
	if err := verifyDestinationRecorderBehavior(); err != nil {
		return err
	}
	if acceptanceDeadline < requiredAcceptanceDeadline() {
		return errors.New("private processing deadline does not cover configured provider maxima")
	}
	config, err := loadSettings()
	if err != nil {
		return err
	}
	recorder := newDestinationRecorder()
	doclingTransport, err := privateTransport(config.doclingURL, config.doclingCIDRs, destinationDocling, recorder)
	if err != nil {
		return err
	}
	embeddingTransport, err := privateTransport(config.embeddingURL, config.embeddingCIDRs, destinationEmbedding, recorder)
	if err != nil {
		return err
	}

	doclingProvider, descriptor, err := newDoclingProvider(config, doclingTransport, recorder)
	if err != nil {
		return err
	}
	embeddingProvider, embeddingDescriptor, err := newEmbeddingProvider(config, embeddingTransport, recorder)
	if err != nil {
		return err
	}
	profile, err := processingProfile(descriptor, embeddingDescriptor)
	if err != nil {
		return err
	}

	root, err := os.MkdirTemp(os.Getenv("DOCBANK_PRIVATE_PROCESSING_ROOT"), "vault.XXXXXXXX")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	vault, err := docbank.New(context.Background(), docbank.Config{Root: root, Processing: docbank.ProcessingOptions{
		Profiles: map[string]docbank.ProcessingProfileConfig{profileName: {
			Profile: profile, RenditionProvider: doclingProvider,
			EmbeddingProviders: map[string]document.EmbeddingProvider{bindingName: embeddingProvider},
			EmbeddingClassifiers: map[string]docbank.EmbeddingErrorClassifier{bindingName: classifyEmbeddingError},
			Tokenizers: map[string]document.Tokenizer{bindingName: runeTokenizer{}},
		}},
		SpoolDirectory: filepath.Join(root, "spool"),
	}})
	if err != nil {
		return err
	}
	defer vault.Close()

	ctx, cancel := context.WithTimeout(context.Background(), acceptanceDeadline)
	defer cancel()
	content := []byte("# Retained private acceptance\n\n" + anchor + "\n")
	outsideContent := []byte("# Outside retained private acceptance\n\n" + anchor + "\n")
	outsideReceipt, err := vault.Put(ctx, "/outside-fence.txt", bytes.NewReader(outsideContent), docbank.PutOptions{MediaType: "text/plain"})
	if err != nil {
		return err
	}
	outsideSelector := docbank.ProcessingSelector{NodeID: outsideReceipt.Node.ID, ContentVersionID: outsideReceipt.Version.ID, Profile: profileName}
	outsidePlan, err := vault.PlanProcessing(ctx, docbank.ProcessingPlanRequest{Selector: outsideSelector})
	if err != nil || outsidePlan.Fingerprint == "" {
		return errors.New("outside-fence processing plan is unavailable")
	}
	outsideJob, err := vault.StartProcessing(ctx, docbank.StartProcessingRequest{
		PlanRequest: docbank.ProcessingPlanRequest{Selector: outsideSelector}, PlanFingerprint: outsidePlan.Fingerprint, Consent: true,
	})
	if err != nil {
		return err
	}
	outsideStatus, err := vault.ProcessingStatus(ctx, docbank.ProcessingStatusRequest{JobID: outsideJob.ID})
	if err != nil || outsideStatus.State != "completed" || outsideStatus.CompletedBindings != 1 {
		return errors.New("outside-fence processing did not complete")
	}
	receipt, err := vault.Put(ctx, "/private-acceptance.txt", bytes.NewReader(content), docbank.PutOptions{MediaType: "text/plain"})
	if err != nil {
		return err
	}
	selector := docbank.ProcessingSelector{NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, Profile: profileName}
	plan, err := vault.PlanProcessing(ctx, docbank.ProcessingPlanRequest{Selector: selector})
	if err != nil || plan.Fingerprint == "" {
		return errors.New("processing plan is unavailable")
	}
	job, err := vault.StartProcessing(ctx, docbank.StartProcessingRequest{
		PlanRequest: docbank.ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint, Consent: true,
	})
	if err != nil {
		return err
	}
	status, err := vault.ProcessingStatus(ctx, docbank.ProcessingStatusRequest{JobID: job.ID})
	if err != nil || status.State != "completed" || status.CompletedBindings != 1 {
		return errors.New("processing did not complete")
	}
	rendered, err := vault.Rendition(ctx, docbank.RenditionRequest{Selector: selector})
	if err != nil {
		return err
	}
	body, readErr := io.ReadAll(rendered.Reader)
	closeErr := rendered.Reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Contains(body, []byte(anchor)) {
		return errors.New("retained rendition is incomplete")
	}
	targetFence := docbank.DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{receipt.Version.ID}}
	outsideFence := docbank.DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{outsideReceipt.Version.ID}}
	for _, mode := range []docbank.DocumentSearchMode{docbank.DocumentSearchLexical, docbank.DocumentSearchSemantic, docbank.DocumentSearchHybrid} {
		outsideReport, outsideSearchErr := vault.SearchDocuments(ctx, docbank.DocumentSearchRequest{
			Query: anchor, Mode: mode, Profile: profileName, BindingID: bindingName, Fence: outsideFence,
		})
		if outsideSearchErr != nil || outsideReport.ActualMode != mode || len(outsideReport.Results) != 1 || outsideReport.Results[0].ContentVersionID != outsideReceipt.Version.ID {
			return errors.New("outside synthetic document did not participate in fenced retrieval")
		}
		if mode == docbank.DocumentSearchHybrid && (outsideReport.Results[0].LexicalRank == 0 || outsideReport.Results[0].SemanticRank == 0) {
			return errors.New("outside hybrid retrieval did not include lexical and semantic lanes")
		}
		targetReport, targetSearchErr := vault.SearchDocuments(ctx, docbank.DocumentSearchRequest{
			Query: anchor, Mode: mode, Profile: profileName, BindingID: bindingName, Fence: targetFence,
		})
		if targetSearchErr != nil || targetReport.ActualMode != mode || len(targetReport.Results) != 1 || targetReport.Results[0].ContentVersionID != receipt.Version.ID {
			return errors.New("requested retrieval mode did not return only the fenced synthetic document")
		}
		if mode == docbank.DocumentSearchHybrid && (targetReport.Results[0].LexicalRank == 0 || targetReport.Results[0].SemanticRank == 0) {
			return errors.New("hybrid retrieval did not include lexical and semantic lanes")
		}
	}
	requests, dials, providerClasses, destinationClasses, err := recorder.summary()
	if err != nil {
		return err
	}
	fmt.Printf("private processing acceptance: provider_transports=%s destination_classes=%s provider_requests=%d private_tcp_dials=%d docbank_provider_transport_public_egress=0 endpoint_onward_egress=not_attested\n",
		providerClasses, destinationClasses, requests, dials)
	fmt.Println("private processing acceptance: rendition=complete retrieval=lexical,semantic,hybrid")
	return nil
}

func loadSettings() (settings, error) {
	result := settings{
		doclingURL: os.Getenv("DOCBANK_PRIVATE_PROCESSING_DOCLING_URL"), doclingKey: os.Getenv("DOCBANK_PRIVATE_PROCESSING_DOCLING_API_KEY"),
		embeddingURL: os.Getenv("DOCBANK_PRIVATE_PROCESSING_EMBEDDING_URL"), embeddingKey: os.Getenv("DOCBANK_PRIVATE_PROCESSING_EMBEDDING_API_KEY"),
		embeddingModel: os.Getenv("DOCBANK_PRIVATE_PROCESSING_EMBEDDING_MODEL"), embeddingRevision: os.Getenv("DOCBANK_PRIVATE_PROCESSING_EMBEDDING_REVISION"),
	}
	var err error
	result.embeddingDimensions, err = strconv.Atoi(os.Getenv("DOCBANK_PRIVATE_PROCESSING_EMBEDDING_DIMENSIONS"))
	if err != nil || result.embeddingDimensions < 1 || result.embeddingDimensions > 1_000_000 {
		return settings{}, errors.New("embedding dimensions are invalid")
	}
	result.doclingCIDRs, err = parsePrivateCIDRs(os.Getenv("DOCBANK_PRIVATE_PROCESSING_DOCLING_ALLOWED_CIDRS"))
	if err != nil {
		return settings{}, err
	}
	result.embeddingCIDRs, err = parsePrivateCIDRs(os.Getenv("DOCBANK_PRIVATE_PROCESSING_EMBEDDING_ALLOWED_CIDRS"))
	if err != nil {
		return settings{}, err
	}
	return result, nil
}

func requiredAcceptanceDeadline() time.Duration {
	return processingJobs*doclingTotalTimeout +
		documentEmbeddingRequests*embeddingRequestTimeout +
		semanticQueryRequests*embeddingRequestTimeout +
		localOverheadBudget
}

func privateTransport(origin string, cidrs []netip.Prefix, providerClass string,
	recorder *destinationRecorder,
) (*http.Transport, error) {
	parsed, address, err := strictOrigin(origin)
	if err != nil {
		return nil, err
	}
	if !operatorAddress(address) {
		return nil, errPublicEgress
	}
	if !containedBy(address, cidrs) {
		return nil, errPublicEgress
	}
	port := uint16(443)
	if parsed.Scheme == "http" {
		port = 80
	}
	if parsed.Port() != "" {
		value, parseErr := strconv.ParseUint(parsed.Port(), 10, 16)
		if parseErr != nil || value == 0 {
			return nil, errors.New("provider port is invalid")
		}
		port = uint16(value)
	}
	expected := netip.AddrPortFrom(address, port)
	transport, err := providerhttp.NewTransport(providerhttp.EgressPolicy{
		Scheme: parsed.Scheme, Host: parsed.Hostname(), Port: port, AllowedCIDRs: cidrs,
		ProxyMode: providerhttp.ProxyDisabled,
	}, literalResolver{address: address})
	if err != nil {
		return nil, err
	}
	plainDial := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, destination string) (net.Conn, error) {
		connection, dialErr := plainDial(ctx, network, destination)
		if dialErr == nil {
			dialErr = recorder.dial(providerClass, connection.RemoteAddr(), expected, cidrs)
			if dialErr != nil {
				_ = connection.Close()
				connection = nil
			}
		}
		return connection, dialErr
	}
	tlsDial := transport.DialTLSContext
	if tlsDial != nil {
		transport.DialTLSContext = func(ctx context.Context, network, destination string) (net.Conn, error) {
			connection, dialErr := tlsDial(ctx, network, destination)
			if dialErr == nil {
				dialErr = recorder.dial(providerClass, connection.RemoteAddr(), expected, cidrs)
				if dialErr != nil {
					_ = connection.Close()
					connection = nil
				}
			}
			return connection, dialErr
		}
	}
	return transport, nil
}

func remoteAddrPort(remote net.Addr) (netip.AddrPort, error) {
	if remote == nil {
		return netip.AddrPort{}, errDestinationMismatch
	}
	if tcp, ok := remote.(*net.TCPAddr); ok {
		address, valid := netip.AddrFromSlice(tcp.IP)
		if !valid || tcp.Port < 1 || tcp.Port > 65535 {
			return netip.AddrPort{}, errDestinationMismatch
		}
		return netip.AddrPortFrom(address.Unmap(), uint16(tcp.Port)), nil
	}
	address, err := netip.ParseAddrPort(remote.String())
	if err != nil || address.Port() == 0 {
		return netip.AddrPort{}, errDestinationMismatch
	}
	return netip.AddrPortFrom(address.Addr().Unmap(), address.Port()), nil
}

func classifyDestination(address netip.Addr) string {
	address = address.Unmap()
	if address.IsLoopback() {
		return "loopback"
	}
	if address.IsLinkLocalUnicast() {
		return "link_local"
	}
	if netip.MustParsePrefix("100.64.0.0/10").Contains(address) {
		return "cgnat"
	}
	if address.Is4() && address.IsPrivate() {
		return "private_ipv4"
	}
	if address.Is6() && address.IsPrivate() {
		return "ula"
	}
	return ""
}

func strictOrigin(raw string) (*url.URL, netip.Addr, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, netip.Addr{}, errors.New("provider endpoint must be an exact credential-free HTTP(S) origin")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, netip.Addr{}, errors.New("provider endpoint must use HTTP(S)")
	}
	address, err := netip.ParseAddr(parsed.Hostname())
	if err != nil {
		return nil, netip.Addr{}, errLiteralOrigin
	}
	return parsed, address.Unmap(), nil
}

func parsePrivateCIDRs(raw string) ([]netip.Prefix, error) {
	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
	if len(parts) == 0 {
		return nil, errors.New("provider allowed CIDRs are required")
	}
	prefixes := make([]netip.Prefix, 0, len(parts))
	for _, part := range parts {
		prefix, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, errMalformedCIDR
		}
		if !privatePrefix(prefix) {
			return nil, errPublicEgress
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

func privatePrefix(prefix netip.Prefix) bool {
	for _, allowed := range []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("fc00::/7"),
		netip.MustParsePrefix("fe80::/10"), netip.MustParsePrefix("::1/128"),
	} {
		if prefix.Bits() >= allowed.Bits() && allowed.Contains(prefix.Addr()) {
			return true
		}
	}
	return false
}

func operatorAddress(address netip.Addr) bool {
	address = address.Unmap()
	return privatePrefix(netip.PrefixFrom(address, address.BitLen()))
}

func containedBy(address netip.Addr, prefixes []netip.Prefix) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func newDoclingProvider(config settings, transport *http.Transport, recorder *destinationRecorder) (document.RenditionProvider, document.RenditionDescriptor, error) {
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: "docling.serve-v1", ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: digest("private-docling-policy"), TrustBoundary: document.RenditionTrustOperatorNetwork,
		SupportedFormats: []document.RenditionFormatCapability{{MediaFamily: "text", MediaType: "text/plain", InputKind: document.RenditionInputOriginalFile}},
		ReturnsMarkdown: true, ReturnsStructured: true, ArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
	})
	if err != nil {
		return nil, document.RenditionDescriptor{}, err
	}
	var resolver docling.SecretResolver
	binding := ""
	if config.doclingKey != "" {
		binding, resolver = "docling", secretResolver{"docling": config.doclingKey}
	}
	provider, err := docling.New(docling.Profile{Origin: config.doclingURL, Descriptor: descriptor, SecretBinding: binding,
		RequestTimeout: embeddingRequestTimeout, TotalTimeout: doclingTotalTimeout, PollInterval: time.Second, MaxPollAttempts: 300,
		MaxResponseBytes: 64 << 20, MaxDocumentBytes: 1 << 20,
	}, resolver, &http.Client{Transport: trackingTransport{base: transport, recorder: recorder,
		providerClass: destinationDocling}})
	if err != nil {
		return nil, document.RenditionDescriptor{}, err
	}
	return provider, descriptor, nil
}

func newEmbeddingProvider(config settings, transport *http.Transport, recorder *destinationRecorder) (document.EmbeddingProvider, document.EmbeddingDescriptor, error) {
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic})
	if err != nil {
		return nil, document.EmbeddingDescriptor{}, err
	}
	descriptor, err := document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{
		ID: openaiembed.ProviderID, ContractVersion: document.EmbeddingProviderContractVersion,
		PolicyFingerprint: strings.Repeat("0", 64), TrustBoundary: document.EmbeddingTrustOperatorNetwork,
		Model: config.embeddingModel, ModelRevision: config.embeddingRevision, Dimension: config.embeddingDimensions,
		Metric: document.VectorMetricCosine, Normalization: document.VectorNormalizationNone, ScalarEncoding: openaiembed.ScalarEncodingFloat32,
		DocumentFormatter: openaiembed.DocumentFormatterV1, QueryFormatter: openaiembed.QueryFormatterV1,
		InputKinds: []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk}, CompatibilityID: contract.CompatibilityID,
		SupportsTextQuery: true, ModelInput: contract, SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText},
	})
	if err != nil {
		return nil, document.EmbeddingDescriptor{}, err
	}
	profile := openaiembed.Profile{Origin: config.embeddingURL, Descriptor: descriptor, ModelInput: contract, SecretBinding: "embedding",
		DeploymentEpoch: config.embeddingRevision, RequestTimeout: embeddingRequestTimeout, MaxBatchItems: 16,
		MaxInputBytes: 1 << 20, MaxRequestBytes: 1 << 20, MaxResponseBytes: 1 << 20,
	}
	policy, err := openaiembed.PolicyFingerprint(profile)
	if err != nil {
		return nil, document.EmbeddingDescriptor{}, err
	}
	descriptor.PolicyFingerprint, descriptor.Fingerprint = policy, ""
	descriptor, err = document.NewEmbeddingDescriptor(descriptor)
	if err != nil {
		return nil, document.EmbeddingDescriptor{}, err
	}
	profile.Descriptor = descriptor
	provider, err := openaiembed.New(profile, secretResolver{"embedding": config.embeddingKey}, &http.Client{Transport: trackingTransport{
		base: transport, recorder: recorder, providerClass: destinationEmbedding}})
	if err != nil {
		return nil, document.EmbeddingDescriptor{}, err
	}
	return provider, descriptor, nil
}

func processingProfile(rendition document.RenditionDescriptor, embedding document.EmbeddingDescriptor) (document.ProcessingProfileV1, error) {
	profile := document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1,
		Rendition: &document.RenditionBindingV1{AdapterContract: "docling.serve/v1", AuthorizationFingerprint: digest("docling-authorization"),
			CredentialBinding: "credential:docling", DeploymentFingerprint: digest("docling-deployment"),
			Descriptor: document.ProviderDescriptorV1{ID: rendition.ID, Fingerprint: rendition.Fingerprint}, DisclosureFingerprint: digest("docling-disclosure"),
			MaxDocumentBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxUnits: 1000, Name: "docling",
			RequestedArtifacts: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured}, TrustBoundary: string(rendition.TrustBoundary), UploadOptionsFingerprint: digest("docling-upload")},
		Embeddings: []document.EmbeddingBindingV1{{Activation: document.EmbeddingRequired, AuthorizationFingerprint: digest("embedding-authorization"),
			CompatibilityID: embedding.CompatibilityID, CredentialBinding: "credential:embedding", Descriptor: document.ProviderDescriptorV1{ID: embedding.ID, Fingerprint: embedding.Fingerprint},
			Dimensions: embedding.Dimension, DisclosureFingerprint: digest("embedding-disclosure"), DocumentFormatter: embedding.DocumentFormatter,
			InputKind: document.EmbeddingInputRenditionChunk, MaxBatchItems: 16, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20,
			Metric: embedding.Metric, Model: embedding.Model, Name: bindingName, Normalization: embedding.Normalization, QueryFormatter: embedding.QueryFormatter,
			ScalarEncoding: embedding.ScalarEncoding, TrustBoundary: string(embedding.TrustBoundary), ModelInput: embedding.ModelInput,
			Chunk: &document.EmbeddingChunkPolicyV1{ContextFingerprint: digest("chunk-context"), Formatter: "rendition-chunk/v1", MaxTokens: 512, OverlapTokens: 32,
				Tokenizer: "private-acceptance-runes@v1", TruncationPolicy: string(document.TruncationPolicyReject)}}},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{CompletenessFingerprint: digest("completeness"), LexicalSegmenterFingerprint: digest("segments"),
			MaxSegmentRunes: 1000, MaxUnitRunes: 100000, NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
			NormalizerFingerprint: digest("normalizer"), RenditionContract: document.RenditionContractV1, SanitizerFingerprint: digest("sanitizer"), SourceEvidenceContract: document.SourceEvidenceContractV1},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{AttachmentPolicyFingerprint: digest("attachments"), ConsentFingerprint: digest("consent"),
			RetainSanitizedMarkdown: true, RetainTypedArtifacts: true, TrustBoundary: "operator-network-private-acceptance"},
		Retrieval: document.RetrievalPolicyV1{LexicalLimit: 20, VectorLimit: 20},
	}
	return document.CanonicalizeProfile(profile)
}

func classifyEmbeddingError(error) (docbank.EmbeddingFailureClass, time.Duration) {
	return docbank.EmbeddingFailurePermanent, 0
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
HARNESS

# The harness has no module of its own: it compiles against this checkout's
# public API. Build tooling gets a scrubbed environment; its compiler cache,
# temporary files, and binary remain inside the private temp root. GOPROXY=off
# prevents a build from falling back to public module egress.
cat >"${temp_root}/build.sh" <<'BUILDER'
#!/usr/bin/env bash
set -euo pipefail
gofmt -w "${DOCBANK_PRIVATE_PROCESSING_ROOT}/main.go"
cd -- "${DOCBANK_PRIVATE_PROCESSING_REPO_ROOT}"
go build -tags fts5 -o "${DOCBANK_PRIVATE_PROCESSING_BINARY}" "${DOCBANK_PRIVATE_PROCESSING_ROOT}/main.go"
BUILDER

# The harness receives only the narrow provider variables documented at the
# top of this runner. Remove all inherited credential and proxy variables
# before it invokes any program.
cat >"${temp_root}/run.sh" <<'RUNNER'
#!/usr/bin/env bash
set -euo pipefail
for name in $(compgen -e); do
  case "$name" in
    DOCBANK_PRIVATE_PROCESSING_ROOT|DOCBANK_PRIVATE_PROCESSING_DOCLING_URL|DOCBANK_PRIVATE_PROCESSING_DOCLING_API_KEY|DOCBANK_PRIVATE_PROCESSING_EMBEDDING_URL|DOCBANK_PRIVATE_PROCESSING_EMBEDDING_API_KEY|DOCBANK_PRIVATE_PROCESSING_EMBEDDING_MODEL|DOCBANK_PRIVATE_PROCESSING_EMBEDDING_REVISION|DOCBANK_PRIVATE_PROCESSING_EMBEDDING_DIMENSIONS|DOCBANK_PRIVATE_PROCESSING_DOCLING_ALLOWED_CIDRS|DOCBANK_PRIVATE_PROCESSING_EMBEDDING_ALLOWED_CIDRS) ;;
    *) unset "$name" ;;
  esac
done
exec "${DOCBANK_PRIVATE_PROCESSING_ROOT}/private-processing"
RUNNER
chmod 700 "${temp_root}/build.sh" "${temp_root}/run.sh"

# Keep build and harness in one session. The worker records its session leader
# before starting any child process so cleanup can close the launch window.
cat >"${temp_root}/session.sh" <<'SESSION'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$$" >"${DOCBANK_PRIVATE_PROCESSING_WORKER_PID_FILE}"
env -i \
  PATH="${DOCBANK_PRIVATE_PROCESSING_BUILD_PATH}" \
  HOME="${DOCBANK_PRIVATE_PROCESSING_BUILD_HOME}" \
  GOMODCACHE="${DOCBANK_PRIVATE_PROCESSING_BUILD_MODULE_CACHE}" \
  GOCACHE="${DOCBANK_PRIVATE_PROCESSING_ROOT}/go-cache" \
  TMPDIR="${DOCBANK_PRIVATE_PROCESSING_ROOT}/go-tmp" \
  GOTMPDIR="${DOCBANK_PRIVATE_PROCESSING_ROOT}/go-tmp" \
  GOPROXY=off \
  DOCBANK_PRIVATE_PROCESSING_ROOT="${DOCBANK_PRIVATE_PROCESSING_ROOT}" \
  DOCBANK_PRIVATE_PROCESSING_REPO_ROOT="${DOCBANK_PRIVATE_PROCESSING_REPO_ROOT}" \
  DOCBANK_PRIVATE_PROCESSING_BINARY="${DOCBANK_PRIVATE_PROCESSING_BINARY}" \
  "${DOCBANK_PRIVATE_PROCESSING_ROOT}/build.sh"

export DOCBANK_PRIVATE_PROCESSING_ROOT
  for name in $(compgen -e); do
    case "$name" in
      DOCBANK_PRIVATE_PROCESSING_ROOT|DOCBANK_PRIVATE_PROCESSING_DOCLING_URL|DOCBANK_PRIVATE_PROCESSING_DOCLING_API_KEY|DOCBANK_PRIVATE_PROCESSING_EMBEDDING_URL|DOCBANK_PRIVATE_PROCESSING_EMBEDDING_API_KEY|DOCBANK_PRIVATE_PROCESSING_EMBEDDING_MODEL|DOCBANK_PRIVATE_PROCESSING_EMBEDDING_REVISION|DOCBANK_PRIVATE_PROCESSING_EMBEDDING_DIMENSIONS|DOCBANK_PRIVATE_PROCESSING_DOCLING_ALLOWED_CIDRS|DOCBANK_PRIVATE_PROCESSING_EMBEDDING_ALLOWED_CIDRS) ;;
      *) unset "$name" ;;
    esac
  done
exec "${DOCBANK_PRIVATE_PROCESSING_ROOT}/run.sh"
SESSION
chmod 700 "${temp_root}/session.sh"

export DOCBANK_PRIVATE_PROCESSING_ROOT="$temp_root"
export DOCBANK_PRIVATE_PROCESSING_REPO_ROOT="$repo_root"
export DOCBANK_PRIVATE_PROCESSING_BINARY="$binary"
export DOCBANK_PRIVATE_PROCESSING_WORKER_PID_FILE="$worker_pid_file"
export DOCBANK_PRIVATE_PROCESSING_BUILD_PATH="$PATH"
export DOCBANK_PRIVATE_PROCESSING_BUILD_HOME="$HOME"
export DOCBANK_PRIVATE_PROCESSING_BUILD_MODULE_CACHE="$module_cache"
worker_launching=1
"$setsid_path" "${temp_root}/session.sh" &
worker_pid=$!
worker_launching=0
wait "$worker_pid"
worker_pid=""
